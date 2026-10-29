// OAuth 设备流的「连接」按钮在服务端的那一半。
//
// 为什么轮询放在服务端而不是让前端每隔几秒打一次：授权服务规定了最小轮询
// 间隔，收到 slow_down 还要把间隔加 5 秒。这套节流逻辑只有一份实现才可靠，
// 而且放在服务端的话，用户切走页面再回来，授权照样在进行。
package channels

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/oauthdev"
)

// connectTTL 一个连接会话最长活多久。设备码本身 15 分钟过期，
// 留一点余量用于前端读到终态。
const connectTTL = 20 * time.Minute

// ConnectState 是连接会话的状态。
type ConnectState string

const (
	ConnectPending ConnectState = "pending"
	ConnectOK      ConnectState = "ok"
	ConnectFailed  ConnectState = "error"
)

// ConnectSession 是一次「点连接」的全过程。
type ConnectSession struct {
	ID string `json:"id"`
	// UserCode 是给用户看的 8 位码，VerificationURI 是去哪输。
	UserCode                string    `json:"userCode"`
	VerificationURI         string    `json:"verificationUri"`
	VerificationURIComplete string    `json:"verificationUriComplete,omitempty"`
	ExpiresAt               time.Time `json:"expiresAt"`

	mu       sync.Mutex
	state    ConnectState
	errMsg   string
	account  string
	identity any
	created  time.Time
	cancel   context.CancelFunc
}

// ConnectStatus 是前端轮询时拿到的东西。
type ConnectStatus struct {
	State    ConnectState `json:"state"`
	Error    string       `json:"error,omitempty"`
	Account  string       `json:"account,omitempty"`
	Identity any          `json:"identity,omitempty"`
}

func (s *ConnectSession) status() ConnectStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ConnectStatus{State: s.state, Error: s.errMsg, Account: s.account, Identity: s.identity}
}

func (s *ConnectSession) finish(account string, identity any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state, s.errMsg = ConnectFailed, err.Error()
		return
	}
	s.state, s.account, s.identity = ConnectOK, account, identity
}

// StartConnect 开始一次设备流授权：向授权服务要码，然后在后台轮询。
//
// values 是表单上已填的值（可能只有 client id）。账号名不用用户填 ——
// 授权完成后向对方系统问一次身份就知道了。
func (m *Manager) StartConnect(typ string, values map[string]string) (*ConnectSession, error) {
	d, ok := ByType(typ)
	if !ok {
		return nil, fmt.Errorf("未知渠道类型 %q", typ)
	}
	if d.OAuth == nil {
		return nil, fmt.Errorf("渠道 %s 不支持「连接」授权", d.Name)
	}
	spec, ok := SpecByID(typ)
	if !ok {
		return nil, fmt.Errorf("渠道 %s 的声明已不在目录里", typ)
	}

	// 这里只需要够算出设备流参数的那部分设置，所以不走 Validate ——
	// 它会因为账号字段还空着而报错，而账号正是待会要问出来的东西。
	settings := map[string]string{}
	for _, f := range d.Fields {
		if f.Name == d.SecretField {
			continue
		}
		if v, ok := values[f.Name]; ok {
			settings[f.Name] = v
		}
	}
	cfg, err := spec.OAuthConfig(settings)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTTL)
	pending, err := cfg.Start(ctx, m.oauthHTTP())
	if err != nil {
		cancel()
		return nil, err
	}

	sess := &ConnectSession{
		ID:                      newSessionID(),
		UserCode:                pending.UserCode,
		VerificationURI:         pending.VerificationURI,
		VerificationURIComplete: pending.VerificationURIComplete,
		ExpiresAt:               pending.ExpiresAt,
		state:                   ConnectPending,
		created:                 time.Now(),
		cancel:                  cancel,
	}

	m.connMu.Lock()
	if m.conns == nil {
		m.conns = map[string]*ConnectSession{}
	}
	m.sweepLocked()
	m.conns[sess.ID] = sess
	m.connMu.Unlock()

	// 复制一份表单值：前端可能在授权期间继续改表单，而这次授权该用点下
	// 按钮那一刻的值。
	snapshot := make(map[string]string, len(values))
	for k, v := range values {
		snapshot[k] = v
	}

	go func() {
		defer cancel()
		tok, err := cfg.Await(ctx, m.oauthHTTP(), pending)
		if err != nil {
			sess.finish("", nil, err)
			return
		}
		account, identity, err := m.adoptToken(ctx, d, spec, snapshot, tok)
		sess.finish(account, identity, err)
	}()
	return sess, nil
}

// ConnectStatus 查一次连接会话的状态。
func (m *Manager) ConnectStatus(id string) (ConnectStatus, error) {
	m.connMu.Lock()
	sess, ok := m.conns[id]
	m.connMu.Unlock()
	if !ok {
		return ConnectStatus{}, fmt.Errorf("连接会话不存在或已过期，请重新点连接")
	}
	return sess.status(), nil
}

// CancelConnect 放弃一次连接（用户关掉了弹窗）。
func (m *Manager) CancelConnect(id string) {
	m.connMu.Lock()
	sess, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	m.connMu.Unlock()
	if ok && sess.cancel != nil {
		sess.cancel()
	}
}

// adoptToken 拿到令牌之后的收尾：问出身份、存凭据、落实例。
//
// 账号名从对方系统问出来而不是让用户填：用户填的名字和令牌实际属于谁可能
// 不一致，而凭据是按账号名定位的 —— 填错就等于把令牌存到另一个条目下。
func (m *Manager) adoptToken(ctx context.Context, d Descriptor, spec httpchan.Spec,
	values map[string]string, tok oauthdev.TokenSet) (string, any, error) {

	stored, err := tok.Marshal()
	if err != nil {
		return "", nil, err
	}

	settings := map[string]string{}
	for _, f := range d.Fields {
		if f.Name == d.SecretField {
			continue
		}
		if v, ok := values[f.Name]; ok {
			settings[f.Name] = v
		}
	}
	// 用新令牌打一次 test 接口，既验证令牌可用，又顺手拿到身份。
	rt := httpchan.Runtime{
		Spec:     spec,
		Account:  "(授权中)",
		Settings: settings,
		Secret:   func() (string, error) { return stored, nil },
	}
	identity, err := (&httpchan.Client{HTTP: m.oauthHTTP()}).Test(ctx, rt)
	if err != nil {
		return "", nil, fmt.Errorf("授权成功了，但用新令牌访问 %s 失败: %w", d.Name, err)
	}

	account := pickIdentityField(identity, d.AccountField)
	if account == "" {
		// 问不出来就只能让用户自己填，但要说清是哪个字段没拿到。
		return "", identity, fmt.Errorf("授权成功了，但没能从 %s 的响应里读出 %s，请手工填写账号后保存",
			d.Name, d.AccountField)
	}

	// 复用常规保存路径：凭据进密码管理器、实例进配置文件，顺序和校验都一样。
	save := make(map[string]string, len(values)+2)
	for k, v := range values {
		save[k] = v
	}
	save[d.AccountField] = account
	save[d.SecretField] = stored
	if _, err := m.Save(d.Type, save); err != nil {
		return "", identity, fmt.Errorf("授权成功了，但保存渠道失败: %w", err)
	}
	return account, identity, nil
}

// pickIdentityField 从 test 接口的结果里取账号名。
//
// 结果的键是声明里写的字段路径（如 "login"），所以直接按名字取；取不到就
// 返回空，由调用方报告，而不是在这里猜一个。
//
// 只有一条记录的数组按对象处理：有的系统没有「查我自己」这个接口，身份要
// 走批量接口问（云之家的 getPersonInfos 不传 openId 时就返回只含自己的
// 一条数组）。对「给这份凭据起个名字」来说，一条记录的数组和一个对象是
// 同一件事；不认它的后果是连接成功了却报「问不出账号名」。
func pickIdentityField(identity any, field string) string {
	if arr, ok := identity.([]any); ok && len(arr) == 1 {
		identity = arr[0]
	}
	m, ok := identity.(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := m[field]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SaveSecretFunc 返回一个把刷新后的令牌写回密码管理器的函数。
func (m *Manager) saveSecretFunc(d Descriptor, account string) func(string) error {
	ref := credstore.Ref{Integration: d.Integration, Account: account}
	return func(v string) error { return m.creds.Put(ref, v) }
}

// oauthHTTP 是授权和身份查询用的 HTTP 客户端。可被测试替换。
func (m *Manager) oauthHTTP() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// sweepLocked 清掉过期会话。调用前必须持有 connMu。
func (m *Manager) sweepLocked() {
	cutoff := time.Now().Add(-connectTTL)
	for id, s := range m.conns {
		if s.created.Before(cutoff) {
			if s.cancel != nil {
				s.cancel()
			}
			delete(m.conns, id)
		}
	}
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机数读不出来时退回时间戳：会话 id 只是本机内存里的句柄，
		// 不是凭据，但也不该可预测，所以这是降级而非等价替换。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
