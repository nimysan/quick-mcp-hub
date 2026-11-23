package channels

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/mailbox"
	"github.com/nimysan/mcp-hub/internal/mailprovider"
	"github.com/nimysan/mcp-hub/internal/oauthdev"
	"github.com/nimysan/mcp-hub/internal/wasmplug"
)

// Manager 把渠道实例（store）、凭据（credstore）和具体渠道的连通性测试
// 串在一起，是页面和 MCP 启动逻辑共用的入口。
type Manager struct {
	store *Store
	creds *credstore.Store

	// HTTP 非空时用于 OAuth 授权和身份查询，测试里指向 httptest。
	HTTP *http.Client

	// Plugins 是 wasm 插件运行时。为空表示本进程不执行插件渠道
	// （例如只跑配置页面时也要装上它，否则「测试并保存」用不了）。
	Plugins *wasmplug.Registry

	// BrowserCheck 由配置页面装上：检查浏览器扩展是否连着（飞书网页文档渠道的
	// 「测试」）。MCP 进程里为空 —— 那边没有扩展连接。
	BrowserCheck func(ctx context.Context) (any, error)

	connMu sync.Mutex
	conns  map[string]*ConnectSession
}

func NewManager(store *Store, creds *credstore.Store) *Manager {
	return &Manager{store: store, creds: creds}
}

func (m *Manager) Store() *Store { return m.store }

// View 是渠道列表给页面看的一条记录，不含凭据。
type View struct {
	Descriptor Descriptor `json:"descriptor"`
	Instance   Instance   `json:"instance"`
	HasSecret  bool       `json:"hasSecret"`
	// Auth 只在凭据是设备流令牌组时出现，手工填的长期令牌没有这些信息。
	Auth *AuthInfo `json:"auth,omitempty"`
}

// AuthInfo 是一份 OAuth 令牌组的非凭据元数据。
//
// 这里一个字节的令牌都不出现 —— 页面只需要知道「还能用多久」「会不会自己
// 续期」，而令牌本身到了前端就等于到了任何能读这个页面的东西手上。
type AuthInfo struct {
	// ObtainedAt 是最后一次授权或续期的时间。刷新时会重置，所以它回答的
	// 是「上一次换令牌是什么时候」，不是「什么时候连上的」。
	ObtainedAt time.Time `json:"obtainedAt,omitempty"`
	// ExpiresAt 是当前访问令牌的到期时间。它通常只有两小时 —— 这个数字
	// 单独摆出来会让人以为要隔两小时重新授权一次，所以页面必须连着
	// CanRefresh 一起说。
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	// CanRefresh 为真表示有可用的刷新令牌，访问令牌到期时程序自己会换一张。
	CanRefresh bool `json:"canRefresh"`
	// Stale 为真表示当前访问令牌已经该换了（到期前 60 秒就算）。
	// CanRefresh 也为真时这不是故障，只是「下次调用时会先续一下」。
	Stale bool `json:"stale"`
}

// authInfo 从已存的凭据里取出令牌元数据。不是令牌组就返回 nil。
func authInfo(secret string, now time.Time) *AuthInfo {
	tok, ok := oauthdev.Parse(secret)
	if !ok {
		return nil
	}
	return &AuthInfo{
		ObtainedAt: tok.ObtainedAt,
		ExpiresAt:  tok.ExpiresAt,
		CanRefresh: tok.CanRefresh(now),
		Stale:      tok.Stale(now),
	}
}

// List 把已配置的实例和它们的描述符拼在一起返回。
func (m *Manager) List() ([]View, error) {
	instances, err := m.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(instances))
	for _, in := range instances {
		d, ok := ByType(in.Type)
		if !ok {
			continue // 配置里有，但这个版本已不认识的类型，跳过
		}
		if d.SecretField == "" {
			// 本机邮件这类渠道没有凭据，不去问密码管理器。
			out = append(out, View{Descriptor: d, Instance: in})
			continue
		}
		sec := m.creds.Secret(credstore.Ref{Integration: d.Integration, Account: in.Account})
		raw, secErr := sec.Value()
		v := View{Descriptor: d, Instance: in, HasSecret: secErr == nil}
		if secErr == nil {
			// 凭据本来就要读出来才能算 hasSecret，所以这里不多一次钥匙串访问。
			v.Auth = authInfo(raw, time.Now())
		}
		out = append(out, v)
	}
	return out, nil
}

// Save 校验表单、把密码类字段写进系统密码管理器、把其余字段写进配置文件。
// 顺序很重要：先写凭据，成功了再写配置，避免出现「配置里有渠道但凭据缺失」。
func (m *Manager) Save(typ string, values map[string]string) (Instance, error) {
	d, ok := ByType(typ)
	if !ok {
		return Instance{}, fmt.Errorf("未知渠道类型 %q", typ)
	}
	if !d.Available {
		return Instance{}, fmt.Errorf("渠道 %s 尚未实现", d.Name)
	}

	account, secret, settings, err := d.Validate(values)
	if err != nil {
		return Instance{}, err
	}
	if d.SecretField == "" {
		// 没有凭据的渠道（本机邮件、飞书网页文档）：只写配置文件，不碰密码管理器。
		in := Instance{Type: typ, Account: account, Settings: settings, Enabled: true}
		if err := m.store.Upsert(in); err != nil {
			return Instance{}, err
		}
		return in, nil
	}
	m.keepAppID(d, account, settings)
	if err := m.checkAppID(d, account, secret, settings); err != nil {
		return Instance{}, err
	}

	ref := credstore.Ref{Integration: d.Integration, Account: account}
	// 编辑时密码留空表示「不改」，此时必须钥匙串里本来就有。
	if secret != "" {
		if err := m.creds.Put(ref, secret); err != nil {
			return Instance{}, err
		}
	} else if _, err := m.creds.Secret(ref).Value(); err != nil {
		label := d.SecretField
		if f, ok := d.Field(d.SecretField); ok {
			label = f.Label
		}
		return Instance{}, fmt.Errorf("%s 未填写，且%s里也没有现成凭据", label, credstore.BackendName())
	}

	in := Instance{Type: typ, Account: account, Settings: settings, Enabled: true}
	if err := m.store.Upsert(in); err != nil {
		return Instance{}, err
	}
	return in, nil
}

// keepAppID 只对飞书这类「应用密钥跟着令牌存」的渠道生效：一次保存没带上
// App ID 时（详情页的权限开关只提交开关本身），沿用这条连接原来的 App ID。
// 丢了它，令牌两小时后续期时就找不到应用，连接直接失效。
func (m *Manager) keepAppID(d Descriptor, account string, settings map[string]string) {
	spec, ok := SpecByID(d.Type)
	if !ok || spec.OAuth == nil || spec.OAuth.ClientSecret == nil || spec.OAuth.ClientIDField == "" {
		return
	}
	field := spec.OAuth.ClientIDField
	if settings[field] != "" {
		return
	}
	if old, ok, err := m.store.Get(d.Type, account); err == nil && ok && old.Settings[field] != "" {
		settings[field] = old.Settings[field]
	}
}

// checkAppID 让 App ID 始终和这条连接的授权一致。
//
// 令牌是哪个应用签发的，续期就只能用那个应用 —— 配置页上填的 App ID 只是
// 显示和兜底。两个一键建出来的应用 ID 前缀几乎一样，用户填错一个很常见，
// 所以这里直接改成签发令牌的那个，而不是报错让用户自己去找。
func (m *Manager) checkAppID(d Descriptor, account, newSecret string, settings map[string]string) error {
	spec, ok := SpecByID(d.Type)
	if !ok || spec.OAuth == nil || spec.OAuth.ClientSecret == nil || spec.OAuth.ClientIDField == "" {
		return nil
	}
	stored := newSecret
	if stored == "" {
		v, err := m.creds.Secret(credstore.Ref{Integration: d.Integration, Account: account}).Value()
		if err != nil {
			return nil
		}
		stored = v
	}
	tok, ok := oauthdev.Parse(stored)
	if !ok {
		return nil
	}
	if issuer := tok.IssuerClientID(); issuer != "" {
		settings[spec.OAuth.ClientIDField] = issuer
	}
	return nil
}

// Delete 删除渠道实例及其凭据。
func (m *Manager) Delete(typ, account string) error {
	d, ok := ByType(typ)
	if !ok {
		return fmt.Errorf("未知渠道类型 %q", typ)
	}
	// 先删配置：即便凭据删除失败，也不会留下一个指向它的渠道。
	if err := m.store.Delete(typ, account); err != nil {
		return err
	}
	if d.SecretField == "" {
		return nil // 没有凭据可删
	}
	return m.creds.Delete(credstore.Ref{Integration: d.Integration, Account: account})
}

// Test 验证一个渠道实例能否连通。
// values 非空时用表单里的临时值测（含刚填、尚未保存的密码）；
// 为空时用已保存的实例 + 密码管理器里的凭据测。
func (m *Manager) Test(ctx context.Context, typ, account string, values map[string]string) error {
	d, ok := ByType(typ)
	if !ok {
		return fmt.Errorf("未知渠道类型 %q", typ)
	}

	switch d.Kind {
	case KindHTTP:
		rt, err := m.httpRuntime(d, account, values)
		if err != nil {
			return err
		}
		if _, err := new(httpchan.Client).Test(ctx, rt); err != nil {
			return err
		}
		return nil
	case KindPlugin:
		if _, err := m.testPlugin(ctx, d, account, values); err != nil {
			return err
		}
		return nil
	case KindLocalMail:
		_, err := m.localMailTest(ctx, d, account, values)
		return err
	case KindBrowser:
		_, err := m.testBrowser(ctx)
		return err
	case KindShareLink:
		_, err := m.testShareLink(ctx)
		return err
	case KindMail:
		cfg, err := m.mailConfig(d, account, values)
		if err != nil {
			return err
		}
		imapErr, smtpErr := mailbox.SelfTest(ctx, cfg)
		// 不再加 "IMAP: " / "SMTP: " 前缀 —— mailbox 返回的错误里已经说了是
		// 哪一侧，再包一层会得到「IMAP: IMAP 登录失败」这种读起来像坏了的提示。
		if imapErr != nil {
			return imapErr
		}
		if smtpErr != nil {
			return smtpErr
		}
		return nil
	default:
		return fmt.Errorf("渠道 %s 暂不支持连通性测试", d.Name)
	}
}

// Identify 测试连通并返回对方系统报回来的身份信息（如 GitHub 的 login），
// 让用户在页面上看到「以谁的身份连上了」，而不只是一个绿勾。
func (m *Manager) Identify(ctx context.Context, typ, account string, values map[string]string) (any, error) {
	d, ok := ByType(typ)
	if !ok {
		return nil, fmt.Errorf("未知渠道类型 %q", typ)
	}
	if d.Kind == KindPlugin {
		return m.testPlugin(ctx, d, account, values)
	}
	if d.Kind == KindLocalMail {
		s, err := m.localMailTest(ctx, d, account, values)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	if d.Kind == KindBrowser {
		return m.testBrowser(ctx)
	}
	if d.Kind != KindHTTP {
		return nil, m.Test(ctx, typ, account, values)
	}
	rt, err := m.httpRuntime(d, account, values)
	if err != nil {
		return nil, err
	}
	return new(httpchan.Client).Test(ctx, rt)
}

// CheckSteps 分步检查一个渠道，而不是只回「成或败」。
//
// 为什么需要它：合成一句的话，失败时用户只知道「连不上」——分不清是网络不通、
// 凭据错了，还是其中一个服务单独坏了，而这三种的处理完全不同。设计稿上的
// 「连接检查」就是三条分开的。
//
// 只有邮件渠道能真分三步（IMAP 可达 / 凭据 / SMTP 可达）。声明式 HTTPS 渠道
// 的一次调用里，可达性和鉴权是同一个请求，物理上分不开 —— 那种情况下如实回
// 一条，不假装分了三步。
func (m *Manager) CheckSteps(ctx context.Context, typ, account string, values map[string]string) ([]mailbox.CheckStep, error) {
	d, ok := ByType(typ)
	if !ok {
		return nil, fmt.Errorf("未知渠道类型 %q", typ)
	}
	if d.Kind == KindMail {
		cfg, err := m.mailConfig(d, account, values)
		if err != nil {
			return nil, err
		}
		return mailbox.Check(ctx, cfg), nil
	}
	if d.Kind == KindLocalMail {
		// 每个来源一条：Apple Mail 没授权和 Outlook 能读是两件分开的事。
		return m.localMailProbe(ctx, d, account, values)
	}
	// 其余渠道：跑原来那条单步检查，包装成一条 —— 与其编出三条假的分步，
	// 不如如实说「这一种只能整体检查」。
	step := mailbox.CheckStep{Key: "connect", Label: "连接与凭据验证"}
	start := time.Now()
	if _, err := m.Identify(ctx, typ, account, values); err != nil {
		step.State, step.Error = "fail", err.Error()
	} else {
		step.State = "ok"
	}
	step.Took = time.Since(start).Milliseconds()
	return []mailbox.CheckStep{step}, nil
}

// HTTPGroup 是同一个声明式渠道类型下的全部已启用实例。
//
// 按类型分组是为了工具注册：两个 GitHub 账号不应该变成两套同名工具
// （后注册的会覆盖前一个），而是一套工具加一个 account 参数。
type HTTPGroup struct {
	Spec     httpchan.Spec
	Runtimes []httpchan.Runtime
}

// HTTPGroups 把所有已启用的声明式渠道按类型分组，供 MCP 启动时注册工具用。
// 分组顺序和每组内的实例顺序都来自 store 的排序，所以是稳定的。
func (m *Manager) HTTPGroups() ([]HTTPGroup, error) {
	views, err := m.List()
	if err != nil {
		return nil, err
	}
	var order []string
	byType := map[string][]httpchan.Runtime{}
	for _, v := range views {
		if v.Descriptor.Kind != KindHTTP || !v.Instance.Enabled {
			continue
		}
		rt, err := m.httpRuntime(v.Descriptor, v.Instance.Account, nil)
		if err != nil {
			return nil, err
		}
		if _, seen := byType[v.Instance.Type]; !seen {
			order = append(order, v.Instance.Type)
		}
		byType[v.Instance.Type] = append(byType[v.Instance.Type], rt)
	}
	out := make([]HTTPGroup, 0, len(order))
	for _, typ := range order {
		rts := byType[typ]
		out = append(out, HTTPGroup{Spec: rts[0].Spec, Runtimes: rts})
	}
	return out, nil
}

// httpRuntime 把一个声明式渠道（已保存实例或表单临时值）变成 httpchan.Runtime。
func (m *Manager) httpRuntime(d Descriptor, account string, values map[string]string) (httpchan.Runtime, error) {
	spec, ok := SpecByID(d.Type)
	if !ok {
		return httpchan.Runtime{}, fmt.Errorf("渠道 %s 的声明已不在目录里", d.Type)
	}

	var in Instance
	if len(values) > 0 {
		acc, _, settings, err := d.Validate(values)
		if err != nil {
			return httpchan.Runtime{}, err
		}
		if account == "" {
			account = acc
		}
		in = Instance{Type: d.Type, Account: account, Settings: settings}
	} else {
		got, ok, err := m.store.Get(d.Type, account)
		if err != nil {
			return httpchan.Runtime{}, err
		}
		if !ok {
			return httpchan.Runtime{}, fmt.Errorf("找不到渠道 %s/%s", d.Type, account)
		}
		in = got
	}

	ref := credstore.Ref{Integration: d.Integration, Account: in.Account}
	secret := m.creds.Secret(ref)
	saveSecret := m.saveSecretFunc(d, in.Account)
	reload := secret.Reload
	// 表单测试时密码可能是刚填、还没入库的，用一个临时 handle 承载它。
	if v, ok := values[d.SecretField]; ok && v != "" {
		secret = credstore.Literal(ref, v)
		// 这条实例可能还没保存过，写回去等于凭空造一条凭据，所以不给写回口。
		saveSecret = nil
		reload = nil
	}

	return httpchan.Runtime{
		Spec:       spec,
		Account:    in.Account,
		Settings:   in.Settings,
		Secret:     secret.Value,
		SaveSecret: saveSecret,
		Reload:     reload,
	}, nil
}

// MailConfigs 把所有已启用的邮件渠道装配成 mailbox 要的 Config，
// 供 MCP 启动时注册工具用。
func (m *Manager) MailConfigs() ([]config.Config, error) {
	views, err := m.List()
	if err != nil {
		return nil, err
	}
	var out []config.Config
	for _, v := range views {
		if v.Descriptor.Kind != KindMail || !v.Instance.Enabled {
			continue
		}
		cfg, err := m.mailConfig(v.Descriptor, v.Instance.Account, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, nil
}

// mailConfig 把一个邮件渠道（已保存实例或表单临时值）变成 mailbox.Config。
func (m *Manager) mailConfig(d Descriptor, account string, values map[string]string) (config.Config, error) {
	var in Instance
	if len(values) > 0 {
		_, _, settings, err := d.Validate(values)
		if err != nil {
			return config.Config{}, err
		}
		in = Instance{Type: d.Type, Account: account, Settings: settings}
	} else {
		got, ok, err := m.store.Get(d.Type, account)
		if err != nil {
			return config.Config{}, err
		}
		if !ok {
			return config.Config{}, fmt.Errorf("找不到渠道 %s/%s", d.Type, account)
		}
		in = got
	}

	ref := credstore.Ref{Integration: d.Integration, Account: account}
	var secret *credstore.Secret
	// 表单测试时密码可能是刚填、还没入库的，用一个临时 handle 承载它。
	if v, ok := values[d.SecretField]; ok && v != "" {
		secret = credstore.Literal(ref, v)
	} else {
		secret = m.creds.Secret(ref)
	}

	// 这家服务商是否需要放宽 TLS，由服务商表决定，不是用户的开关 ——
	// 它是那台服务器的客观属性（见 mailprovider.Provider.LegacyRSAKeyExchange）。
	legacyRSA := false
	if p, ok := mailprovider.ByType(d.Type); ok {
		legacyRSA = p.LegacyRSAKeyExchange
	}

	return config.FromMailChannel(config.MailChannelInput{
		User:                 account,
		Secret:               secret,
		IMAPHost:             in.Setting(d, "imapHost"),
		IMAPPort:             in.SettingInt(d, "imapPort", 993),
		SMTPHost:             in.Setting(d, "smtpHost"),
		SMTPPort:             in.SettingInt(d, "smtpPort", 465),
		AllowSend:            in.SettingBool(d, "allowSend"),
		ProviderName:         d.Name,
		SecretLabel:          secretLabelOf(d),
		LegacyRSAKeyExchange: legacyRSA,
	}), nil
}

// secretLabelOf 取这个渠道对凭据的叫法（「授权码」/「邮箱密码」）。
//
// 从表单字段上读，而不是另开一个字段：页面上显示的就是这个标签，错误提示
// 用同一个词，用户才能对上 —— 「授权码不正确」对着一个写着「授权码」的
// 输入框才说得通。
func secretLabelOf(d Descriptor) string {
	for _, f := range d.Fields {
		if f.Kind == KindPassword {
			return f.Label
		}
	}
	return "密码"
}
