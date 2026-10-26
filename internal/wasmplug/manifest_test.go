package wasmplug

import (
	"strings"
	"testing"
)

// manifest 是「签名过的那一半」，所以它的拒绝规则就是信任边界本身：
// 一份恶意 manifest 能把用户的凭据指向别处，而模块无法给自己扩权。

func validManifest() Manifest {
	return Manifest{
		ABI: 1, ID: "demo", Name: "示例", Version: 1,
		Module:       "demo.wasm",
		AllowedHosts: []string{"api.example.com"},
		BaseURL:      "https://api.example.com",
		Auth:         Auth{Kind: "bearer"},
		Account:      Credential{Name: "login", Label: "账号"},
		Secret:       Credential{Name: "token", Label: "令牌"},
		Tools: []Tool{
			{Name: "demo_read", Description: "读点东西", ReadOnly: true},
			{Name: "demo_write", Description: "写点东西"},
		},
	}
}

func TestManifestAccepted(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("合法 manifest 被拒: %v", err)
	}
}

func TestManifestRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"ABI 不是本程序支持的版本", func(m *Manifest) { m.ABI = 2 }, "需要升级客户端"},
		{"ABI 缺失", func(m *Manifest) { m.ABI = 0 }, "ABI"},
		{"id 带大写", func(m *Manifest) { m.ID = "Demo" }, "只能用小写"},
		{"id 太短", func(m *Manifest) { m.ID = "d" }, "长度"},
		{"没有显示名", func(m *Manifest) { m.Name = "" }, "缺少显示名"},
		{"版本号为 0", func(m *Manifest) { m.Version = 0 }, "version 必须从 1 起"},
		{"模块名带路径", func(m *Manifest) { m.Module = "../../evil.wasm" }, "不能带路径"},
		{"模块名缺失", func(m *Manifest) { m.Module = "" }, "没有指定模块文件"},
		{"allowedHosts 为空", func(m *Manifest) {
			m.AllowedHosts = nil
			m.BaseURL = ""
			m.BaseURLField = "site"
		}, "必须声明 allowedHosts"},
		{"allowedHosts 用通配符", func(m *Manifest) {
			m.AllowedHosts = []string{"*.example.com"}
		}, "不支持通配符"},
		{"allowedHosts 带协议", func(m *Manifest) {
			m.AllowedHosts = []string{"https://api.example.com"}
		}, "不要带协议或路径"},
		{"baseUrl 是明文 http", func(m *Manifest) { m.BaseURL = "http://api.example.com" }, "必须是 https"},
		{"baseUrl 含占位符", func(m *Manifest) {
			m.BaseURL = "https://{tenant}.example.com"
			m.AllowedHosts = []string{"{tenant}.example.com"}
		}, "不能含占位符"},
		{"baseUrl 主机不在白名单内", func(m *Manifest) {
			m.BaseURL = "https://other.example.com"
		}, "不在 allowedHosts 内"},
		{"既没有 baseUrl 也没有字段", func(m *Manifest) {
			m.BaseURL = ""
			m.BaseURLField = ""
		}, "必须给出 baseUrl 或 baseUrlField"},
		{"没有工具", func(m *Manifest) { m.Tools = nil }, "没有声明任何工具"},
		{"工具名没有渠道前缀", func(m *Manifest) {
			m.Tools[0].Name = "read_stuff"
		}, "必须以 demo_ 开头"},
		{"工具名重复", func(m *Manifest) {
			m.Tools[1].Name = m.Tools[0].Name
		}, "重复"},
		{"工具没有描述", func(m *Manifest) { m.Tools[0].Description = "" }, "缺少 description"},
		{"认证方式不认识", func(m *Manifest) { m.Auth = Auth{Kind: "oauth-magic"} }, "不支持"},
		{"header 认证没给头名", func(m *Manifest) { m.Auth = Auth{Kind: "header"} }, "必须给出 auth.name"},
		{"账号和凭据同名", func(m *Manifest) { m.Secret.Name = m.Account.Name }, "不能是同一个字段"},
		{"账号字段名不合法", func(m *Manifest) { m.Account.Name = "1bad" }, "必须是合法字段名"},
		{"凭据缺 label", func(m *Manifest) { m.Secret.Label = "" }, "都要有 label"},
		{"额外字段与凭据撞名", func(m *Manifest) {
			m.Fields = []Field{{Name: "token", Label: "撞名"}}
		}, "与保留字段冲突"},
		{"额外字段占用写开关名", func(m *Manifest) {
			m.Fields = []Field{{Name: AllowWriteField, Label: "撞名"}}
		}, "与保留字段冲突"},
		{"baseUrlField 指向不存在的字段", func(m *Manifest) {
			m.BaseURL = ""
			m.BaseURLField = "site"
			m.AllowedHosts = []string{"api.example.com"}
		}, "在 fields 里不存在"},
		{"参数占用分派参数名", func(m *Manifest) {
			m.Tools[0].Params = []Param{{Name: ReservedAccountParam}}
		}, "保留参数名"},
		{"参数名不合法", func(m *Manifest) {
			m.Tools[0].Params = []Param{{Name: "has-dash"}}
		}, "参数名"},
		{"参数重复", func(m *Manifest) {
			m.Tools[0].Params = []Param{{Name: "a"}, {Name: "a"}}
		}, "重复"},
		{"参数类型不支持", func(m *Manifest) {
			m.Tools[0].Params = []Param{{Name: "a", Type: "int64"}}
		}, "类型"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validManifest()
			c.mut(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("本该被拒却通过了")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("拒绝原因 = %q，期望包含 %q", err.Error(), c.want)
			}
		})
	}
}

func TestWriteToolsListed(t *testing.T) {
	got := validManifest().WriteTools()
	if len(got) != 1 || got[0] != "demo_write" {
		t.Fatalf("WriteTools = %v，期望只有 demo_write", got)
	}
}
