package channels

import (
	"context"
	"fmt"

	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/mailbox"
)

// Manager 把渠道实例（store）、凭据（credstore）和具体渠道的连通性测试
// 串在一起，是页面和 MCP 启动逻辑共用的入口。
type Manager struct {
	store *Store
	creds *credstore.Store
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
		sec := m.creds.Secret(credstore.Ref{Integration: d.Integration, Account: in.Account})
		_, secErr := sec.Value()
		out = append(out, View{Descriptor: d, Instance: in, HasSecret: secErr == nil})
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
	case KindMail:
		cfg, err := m.mailConfig(d, account, values)
		if err != nil {
			return err
		}
		imapErr, smtpErr := mailbox.SelfTest(ctx, cfg)
		if imapErr != nil {
			return fmt.Errorf("IMAP: %w", imapErr)
		}
		if smtpErr != nil {
			return fmt.Errorf("SMTP: %w", smtpErr)
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
	if d.Kind != KindHTTP {
		return nil, m.Test(ctx, typ, account, values)
	}
	rt, err := m.httpRuntime(d, account, values)
	if err != nil {
		return nil, err
	}
	return new(httpchan.Client).Test(ctx, rt)
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
	spec, ok := httpchan.ByType(d.Type)
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
	// 表单测试时密码可能是刚填、还没入库的，用一个临时 handle 承载它。
	if v, ok := values[d.SecretField]; ok && v != "" {
		secret = credstore.Literal(ref, v)
	}

	return httpchan.Runtime{
		Spec:     spec,
		Account:  in.Account,
		Settings: in.Settings,
		Secret:   secret.Value,
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

	return config.FromMailChannel(config.MailChannelInput{
		User:         account,
		Secret:       secret,
		IMAPHost:     in.Setting(d, "imapHost"),
		IMAPPort:     in.SettingInt(d, "imapPort", 993),
		SMTPHost:     in.Setting(d, "smtpHost"),
		SMTPPort:     in.SettingInt(d, "smtpPort", 465),
		AllowSend:    in.SettingBool(d, "allowSend"),
		ProviderName: d.Name,
	}), nil
}
