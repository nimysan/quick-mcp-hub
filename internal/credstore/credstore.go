// Package credstore 把凭据存在操作系统自己的密码管理器里，并以惰性方式取出。
//
// 后端由 go-keyring 按平台选择，都不需要 cgo，所以 CGO_ENABLED=0 的
// 交叉编译不受影响：
//
//	macOS   -> Keychain（exec /usr/bin/security）
//	Windows -> Credential Manager（wincred / DPAPI）
//	Linux   -> Secret Service（D-Bus，需要 gnome-keyring / KWallet 之类）
//
// 设计上的三条约束：
//
//  1. 惰性。每个集成一个独立 handle，第一次真正用到它的 tool 被调用时才读
//     钥匙串。有 8 个集成而本次会话只用邮件时，另外 7 份凭据不会被解出来
//     放进内存。
//  2. 每进程只读一次。MCP stdio server 是常驻进程，客户端 spawn 一次就复用，
//     所以 handle 内部用 sync.Once 缓存，不存在「每次调用都查钥匙串」的开销，
//     也避免在锁屏时于半路弹出授权框。
//  3. 凭据只进不出。本包不提供任何把明文返回给模型的路径，Value 只给宿主
//     代码用；错误信息里绝不带凭据内容。
package credstore

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// ErrNotFound 表示该集成的凭据既不在钥匙串里，也没有环境变量兜底。
var ErrNotFound = errors.New("凭据未配置")

// Ref 唯一标识一份凭据：哪个集成、哪个账号。
//
// 同一个集成可以有多个账号（两个 QQ 邮箱、一个 163），它们是不同的 Ref，
// 各自独立惰性取用。
type Ref struct {
	Integration string // 集成名，如 "mail"、"jira"
	Account     string // 账号标识，如 "12345@qq.com"
}

func (r Ref) String() string { return r.Integration + "/" + r.Account }

func (r Ref) valid() error {
	if strings.TrimSpace(r.Integration) == "" || strings.TrimSpace(r.Account) == "" {
		return fmt.Errorf("集成名和账号都不能为空")
	}
	return nil
}

// Store 是一组凭据的惰性取用入口。namespace 用来给钥匙串里的条目加前缀，
// 避免和机器上别的程序撞 service 名。
type Store struct {
	namespace string

	mu      sync.Mutex
	secrets map[Ref]*Secret
}

// New 创建一个 Store。namespace 建议用程序名，如 "mcp-hub"。
func New(namespace string) *Store {
	return &Store{namespace: namespace, secrets: make(map[Ref]*Secret)}
}

// service 是写进钥匙串的 service 字段，形如 "mcp-hub:mail"。
// 按集成分段，这样 DeleteAll 能按集成清理，钥匙串里也看得出归属。
func (s *Store) service(r Ref) string { return s.namespace + ":" + r.Integration }

// Secret 是一份凭据的惰性句柄。取得 handle 不会访问钥匙串；
// 第一次调用 Value 才真正去读，之后复用结果（含错误）。
//
// 并发安全：多个 tool 同时首次调用只会触发一次后端读取。
//
// 句柄是跨调用点共享的（见 Store.Secret），所以 Store.Put 必须把新值
// 推进已发出的句柄里 —— 否则 OAuth 刷新换了新令牌之后，常驻的 MCP
// 进程会一直读到旧的那个，每次调用都去刷一遍，而很多提供方刷新时会
// 轮换 refresh_token，第二次就用了一个已作废的。
type Secret struct {
	ref     Ref
	service string
	envKeys []string

	mu     sync.Mutex
	loaded bool
	val    string
	src    string
	err    error
}

// Ref 返回这份凭据的标识。
func (s *Secret) Ref() Ref { return s.ref }

// Literal 构造一个已持有明文的 Secret，不经过系统密码管理器。
// 用于「测试尚未保存的表单」：页面刚填的密码还没入库，用它承载即可。
// Source 报告为 "form"。
func Literal(ref Ref, value string) *Secret {
	return &Secret{ref: ref, val: value, src: "form", loaded: true}
}

// override 把新值推进句柄。Put 成功后调用，让已经拿着这个句柄的调用方
// 立刻看到新凭据，而不是继续用缓存里的旧值。
func (s *Secret) override(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val, s.src, s.err, s.loaded = value, "keychain", nil, true
}

// forget 丢掉缓存，下次 Value 重新读后端。Delete 之后调用。
func (s *Secret) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.val, s.src, s.err, s.loaded = "", "", nil, false
}

// Source 返回凭据实际来自哪里（"keychain" 或 "env:XXX"），
// 仅在 Value 成功后有意义。用于日志和 -selftest，不含凭据本身。
func (s *Secret) Source() string { return s.src }

// Loaded 报告是否已经取过（不触发读取）。
func (s *Secret) Loaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// Value 取出凭据明文。第一次调用会访问系统密码管理器。
//
// 优先级：系统密码管理器 > 环境变量兜底。钥匙串里有就用钥匙串，
// 这样「把凭据搬进钥匙串」这个迁移只要 -set-secret 一次，
// 不必同时去 Quick 的面板里删环境变量。
func (s *Secret) Value() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.val, s.err
	}
	s.loaded = true

	v, err := keyring.Get(s.service, s.ref.Account)
	if err == nil && v != "" {
		s.val, s.src = v, "keychain"
		return s.val, s.err
	}
	keyringErr := err

	for _, k := range s.envKeys {
		if ev := strings.TrimSpace(os.Getenv(k)); ev != "" {
			s.val, s.src = ev, "env:"+k
			return s.val, s.err
		}
	}

	switch {
	case keyringErr == nil || errors.Is(keyringErr, keyring.ErrNotFound):
		s.err = fmt.Errorf("%w: %s 不在%s中，也没有环境变量 %s；用 -set-secret 写入",
			ErrNotFound, s.ref, BackendName(), strings.Join(s.envKeys, " / "))
	default:
		// 后端本身出错（Linux 无 Secret Service、钥匙串被锁等）。
		// 这里只回错误类型，绝不回凭据内容。
		s.err = fmt.Errorf("读取%s失败（%s）: %w", BackendName(), s.ref, keyringErr)
	}
	return s.val, s.err
}

// Secret 返回该 Ref 的惰性句柄，不访问钥匙串。
// 同一个 Ref 多次调用返回同一个句柄，所以缓存是跨调用点共享的。
//
// envKeys 是环境变量兜底名，按顺序尝试。给 CI、容器和 Linux 无
// Secret Service 的场景留后路；留空则只认钥匙串。
func (s *Store) Secret(r Ref, envKeys ...string) *Secret {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec, ok := s.secrets[r]; ok {
		return sec
	}
	sec := &Secret{ref: r, service: s.service(r), envKeys: envKeys}
	s.secrets[r] = sec
	return sec
}

// Put 把凭据写入系统密码管理器，并在本地索引里登记（索引不含凭据）。
func (s *Store) Put(r Ref, secret string) error {
	if err := r.valid(); err != nil {
		return err
	}
	if secret == "" {
		return fmt.Errorf("凭据不能为空")
	}
	if err := keyring.Set(s.service(r), r.Account, secret); err != nil {
		return fmt.Errorf("写入%s失败: %w", BackendName(), err)
	}
	// 把新值推进已发出的句柄：它们是共享的，不更新就会继续返回旧凭据。
	s.mu.Lock()
	if sec, ok := s.secrets[r]; ok {
		sec.override(secret)
	}
	s.mu.Unlock()
	return s.indexAdd(r)
}

// Delete 删除系统密码管理器里的凭据，并从索引中移除。
func (s *Store) Delete(r Ref) error {
	if err := r.valid(); err != nil {
		return err
	}
	err := keyring.Delete(s.service(r), r.Account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("删除%s条目失败: %w", BackendName(), err)
	}
	// 同理：删掉之后，已发出的句柄不能继续返回缓存里的那份凭据。
	s.mu.Lock()
	if sec, ok := s.secrets[r]; ok {
		sec.forget()
	}
	s.mu.Unlock()
	return s.indexRemove(r)
}

// BackendName 返回当前平台的密码管理器名称，用于日志和提示。
func BackendName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS 钥匙串"
	case "windows":
		return "Windows 凭据管理器"
	case "linux":
		return "Secret Service (D-Bus)"
	default:
		return "系统密码管理器"
	}
}
