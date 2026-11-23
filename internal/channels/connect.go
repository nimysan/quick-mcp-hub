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
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// 连接会话的阶段。大多数渠道只有授权一步；飞书在没有 App ID 时先走
// 「一键创建应用」，再走用户授权 —— 两步各有自己的链接和码，前端要在
// 阶段切换时换掉显示的内容。
const (
	PhaseRegister  = "register"
	PhaseAuthorize = "authorize"
	// PhasePaste 是隐式授权：用户在浏览器里授权完，把跳转后的地址粘回来。
	PhasePaste = "paste"
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
	// phase 和 cur 是当前阶段及其链接。上面那几个导出字段是开始时的快照
	// （随 start 响应序列化），之后的变化只走 status，免得和序列化抢锁。
	phase string
	cur   oauthdev.Pending
	// fill 是要回填到表单上的值（一键建好的 App ID）。授权中途失败时用户
	// 再点一次连接，带着它就会接着授权，而不是又建一个应用。
	fill map[string]string
	// onToken 和 pasteState 只有隐式授权的会话有：用户把授权后的地址粘回来、
	// 解析出令牌后调用 onToken；pasteState 是授权页上带出去的 state。
	onToken    func(ctx context.Context, tok oauthdev.TokenSet) (string, any, error)
	pasteState string
}

// ConnectStatus 是前端轮询时拿到的东西。
type ConnectStatus struct {
	State    ConnectState `json:"state"`
	Error    string       `json:"error,omitempty"`
	Account  string       `json:"account,omitempty"`
	Identity any          `json:"identity,omitempty"`
	// Phase 和下面三项是当前阶段要给用户看的东西，阶段切换时会变。
	Phase                   string            `json:"phase,omitempty"`
	UserCode                string            `json:"userCode,omitempty"`
	VerificationURI         string            `json:"verificationUri,omitempty"`
	VerificationURIComplete string            `json:"verificationUriComplete,omitempty"`
	Fill                    map[string]string `json:"fill,omitempty"`
}

func (s *ConnectSession) status() ConnectStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ConnectStatus{State: s.state, Error: s.errMsg, Account: s.account, Identity: s.identity,
		Phase: s.phase, UserCode: s.cur.UserCode, VerificationURI: s.cur.VerificationURI,
		VerificationURIComplete: s.cur.VerificationURIComplete, Fill: s.fill}
}

// advance 切到下一个阶段。
func (s *ConnectSession) advance(phase string, p oauthdev.Pending, fill map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase, s.cur = phase, p
	if fill != nil {
		s.fill = fill
	}
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
	// 复制一份表单值：前端可能在授权期间继续改表单，而这次授权该用点下
	// 按钮那一刻的值。
	snapshot := make(map[string]string, len(values))
	for k, v := range values {
		snapshot[k] = v
	}

	// 重新授权一条已有连接时，App ID 以那条连接为准。它缺了 App ID 时
	// 不能顺手走一键建应用 —— 那会在企业里多建一个应用，而原来那条连接
	// 仍然是坏的。
	if o := spec.OAuth; o.RegisterURL != "" && o.ClientIDField != "" {
		if acc := strings.TrimSpace(values[d.AccountField]); acc != "" && strings.TrimSpace(settings[o.ClientIDField]) == "" {
			if in, ok, err := m.store.Get(typ, acc); err == nil && ok && in.Settings[o.ClientIDField] != "" {
				settings[o.ClientIDField] = in.Settings[o.ClientIDField]
				snapshot[o.ClientIDField] = in.Settings[o.ClientIDField]
			} else if ok {
				label := o.ClientIDField
				if f, found := spec.Field(o.ClientIDField); found {
					label = f.Label
				}
				return nil, fmt.Errorf("这条连接缺少 %s：请先在「配置 → 高级设置」里填上，再重新授权", label)
			}
		}
	}

	// 没填 App ID、而渠道支持一键建应用：先建应用，再授权。
	if reg, ok := registration(spec, settings); ok {
		return m.startWithRegistration(d, spec, settings, snapshot, reg)
	}
	if spec.OAuth.Flow == httpchan.FlowImplicit {
		return m.startImplicit(d, spec, snapshot)
	}

	cfg, err := spec.OAuthConfig(settings)
	if err != nil {
		return nil, err
	}
	if spec.OAuth.ClientSecret != nil {
		if cfg.ClientSecret, err = m.clientSecret(d, spec, cfg.ClientID, values); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTTL)
	pending, err := cfg.Start(ctx, m.oauthHTTP())
	if err != nil {
		cancel()
		return nil, err
	}
	sess := m.newSession(pending, PhaseAuthorize, cancel)
	go func() {
		defer cancel()
		tok, err := cfg.Await(ctx, m.oauthHTTP(), pending)
		if err != nil {
			sess.finish("", nil, err)
			return
		}
		account, identity, err := m.adoptToken(ctx, d, spec, snapshot, tok)
		if err == nil && spec.OAuth.ClientSecret != nil {
			m.forgetPendingApp(d, cfg.ClientID)
		}
		sess.finish(account, identity, err)
	}()
	return sess, nil
}

// startImplicit 开始一次隐式授权：拼好授权页地址交给前端，然后等用户把
// 授权后的地址粘回来（CompleteConnect）。这里没有后台轮询 —— 令牌在浏览器
// 地址栏里，本程序拿不到，只能等用户送回来。
func (m *Manager) startImplicit(d Descriptor, spec httpchan.Spec, snapshot map[string]string) (*ConnectSession, error) {
	imp, err := spec.Implicit()
	if err != nil {
		return nil, err
	}
	state := oauthdev.NewState()
	link, err := imp.URL(state)
	if err != nil {
		return nil, err
	}
	pending := oauthdev.Pending{
		VerificationURI:         spec.OAuth.AuthorizeURL,
		VerificationURIComplete: link,
		ExpiresAt:               time.Now().Add(connectTTL),
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTTL)
	sess := m.newSession(pending, PhasePaste, cancel)
	sess.pasteState = state
	sess.onToken = func(_ context.Context, tok oauthdev.TokenSet) (string, any, error) {
		return m.adoptToken(ctx, d, spec, snapshot, tok)
	}
	return sess, nil
}

// CompleteConnect 收下隐式授权粘回来的地址：解析出令牌、问出身份、保存。
//
// 粘错了（没授权就复制、复制了别的页面、state 对不上）只报错、不结束会话，
// 让用户改了再交一次；拿着令牌访问对方失败才算这次连接失败。
func (m *Manager) CompleteConnect(id, pasted string) (ConnectStatus, error) {
	m.connMu.Lock()
	sess, ok := m.conns[id]
	m.connMu.Unlock()
	if !ok {
		return ConnectStatus{}, fmt.Errorf("连接会话不存在或已过期，请重新点连接")
	}
	sess.mu.Lock()
	want := sess.pasteState
	hasPaste := sess.onToken != nil || sess.pasteState != ""
	sess.mu.Unlock()
	if !hasPaste {
		return ConnectStatus{}, fmt.Errorf("这次连接不需要粘贴地址")
	}
	tok, err := oauthdev.ParsePasted(pasted, want, time.Now())
	if err != nil {
		return ConnectStatus{}, err
	}
	// 认领：同一个会话被连点两次「完成」时，只能有一次去保存。
	sess.mu.Lock()
	onToken := sess.onToken
	sess.onToken = nil
	sess.mu.Unlock()
	if onToken == nil {
		return sess.status(), nil
	}
	account, identity, err := onToken(context.Background(), tok)
	sess.finish(account, identity, err)
	return sess.status(), nil
}

// newSession 登记一个连接会话。
func (m *Manager) newSession(p oauthdev.Pending, phase string, cancel context.CancelFunc) *ConnectSession {
	sess := &ConnectSession{
		ID:                      newSessionID(),
		UserCode:                p.UserCode,
		VerificationURI:         p.VerificationURI,
		VerificationURIComplete: p.VerificationURIComplete,
		ExpiresAt:               p.ExpiresAt,
		state:                   ConnectPending,
		created:                 time.Now(),
		cancel:                  cancel,
		phase:                   phase,
		cur:                     p,
	}
	m.connMu.Lock()
	if m.conns == nil {
		m.conns = map[string]*ConnectSession{}
	}
	m.sweepLocked()
	m.conns[sess.ID] = sess
	m.connMu.Unlock()
	return sess
}

// registration 判断这次连接要不要先一键建应用。
func registration(spec httpchan.Spec, settings map[string]string) (oauthdev.AppRegistration, bool) {
	o := spec.OAuth
	if o == nil || o.RegisterURL == "" || o.ClientIDField == "" {
		return oauthdev.AppRegistration{}, false
	}
	if strings.TrimSpace(settings[o.ClientIDField]) != "" {
		return oauthdev.AppRegistration{}, false
	}
	return oauthdev.AppRegistration{
		URL: o.RegisterURL, Name: o.RegisterName, Desc: o.RegisterDesc, UserScopes: o.Scopes,
	}, true
}

// startWithRegistration 是两段式连接：扫码建应用 → 扫码授权个人身份。
func (m *Manager) startWithRegistration(d Descriptor, spec httpchan.Spec, settings, snapshot map[string]string,
	reg oauthdev.AppRegistration) (*ConnectSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTTL)
	pending, err := reg.Begin(ctx, m.oauthHTTP())
	if err != nil {
		cancel()
		return nil, err
	}
	sess := m.newSession(pending, PhaseRegister, cancel)
	idField := spec.OAuth.ClientIDField
	go func() {
		defer cancel()
		app, err := reg.Await(ctx, m.oauthHTTP(), pending)
		if err != nil {
			sess.finish("", nil, err)
			return
		}
		// 应用一建好就把密钥存下来，再往下走授权。授权这一步可能要等企业
		// 管理员审批应用，用户会隔一阵再回来点连接 —— 那时密钥必须还在，
		// 否则只能再建一个应用。
		fill := map[string]string{idField: app.ClientID}
		if err := m.rememberPendingApp(d, app); err != nil {
			sess.finish("", nil, fmt.Errorf("应用已建好（App ID %s），但应用密钥没能存进%s: %w", app.ClientID, credstore.BackendName(), err))
			return
		}
		settings[idField] = app.ClientID
		snapshot[idField] = app.ClientID
		cfg, err := spec.OAuthConfig(settings)
		if err != nil {
			sess.finish("", nil, err)
			return
		}
		cfg.ClientSecret = app.ClientSecret
		p2, err := cfg.Start(ctx, m.oauthHTTP())
		if err != nil {
			sess.advance(PhaseAuthorize, oauthdev.Pending{}, fill)
			sess.finish("", nil, fmt.Errorf("应用已建好（App ID %s），但个人授权没能开始：%w。"+
				"如果企业要求管理员审批应用，等审批通过后再点一次连接即可，不会重复建应用", app.ClientID, err))
			return
		}
		sess.advance(PhaseAuthorize, p2, fill)
		tok, err := cfg.Await(ctx, m.oauthHTTP(), p2)
		if err != nil {
			sess.finish("", nil, err)
			return
		}
		account, identity, err := m.adoptToken(ctx, d, spec, snapshot, tok)
		if err == nil {
			m.forgetPendingApp(d, app.ClientID)
		}
		sess.finish(account, identity, err)
	}()
	return sess, nil
}

// pendingAppRef 是「已建好、还没授权成功」的应用密钥的存放位置。授权成功后
// 密钥随令牌组一起存进那条连接的条目，这一条就删掉。
func pendingAppRef(d Descriptor, appID string) credstore.Ref {
	return credstore.Ref{Integration: d.Integration, Account: "app:" + appID}
}

func (m *Manager) rememberPendingApp(d Descriptor, app oauthdev.AppCredentials) error {
	return m.creds.Put(pendingAppRef(d, app.ClientID), app.ClientSecret)
}

func (m *Manager) forgetPendingApp(d Descriptor, appID string) {
	_ = m.creds.Delete(pendingAppRef(d, appID))
}

// clientSecret 找出这次授权要用的应用密钥。按顺序：
//
//  1. 表单上刚填的（用 IT 建好的应用）
//  2. 已保存连接的令牌组里带着的（重新授权）
//  3. 一键建好、还没授权成功的应用（等完管理员审批回来接着连）
//
// 都没有就要用户填，而不是带着空密钥去打授权服务 —— 那样的报错是
// 「invalid_client」，用户看不出是自己少填了一项。
func (m *Manager) clientSecret(d Descriptor, spec httpchan.Spec, appID string, values map[string]string) (string, error) {
	cs := spec.OAuth.ClientSecret
	if v := strings.TrimSpace(values[cs.Name]); v != "" {
		return v, nil
	}
	if acc := strings.TrimSpace(values[d.AccountField]); acc != "" {
		if stored, err := m.creds.Secret(credstore.Ref{Integration: d.Integration, Account: acc}).Value(); err == nil {
			if tok, ok := oauthdev.Parse(stored); ok && tok.ClientSecret != "" {
				return tok.ClientSecret, nil
			}
		}
	}
	v, err := m.creds.Secret(pendingAppRef(d, appID)).Value()
	if err == nil && v != "" {
		return v, nil
	}
	if err != nil && !errors.Is(err, credstore.ErrNotFound) {
		return "", err
	}
	return "", fmt.Errorf("请填写 %s", cs.Label)
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
