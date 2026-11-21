// 浏览器扩展类渠道：飞书网页文档（feishu-web）。
//
// 和另外三种渠道都不一样：它没有凭据要填。登录态在用户自己的浏览器里，
// mcp-hub 只和本机的扩展配对（配对 token 由配置页面生成、存进系统密码管理器，
// 不是这条渠道实例的字段）。所以它的描述符没有 SecretField，Save 也不要求凭据。
package channels

import (
	"context"
	"errors"
	"fmt"

	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/feishuweb"
)

// KindBrowser 是浏览器扩展渠道。
const KindBrowser = "browser"

// FeishuWebType 是飞书网页文档渠道的类型 id。
const FeishuWebType = "feishu-web"

func feishuWebDescriptor() Descriptor {
	return Descriptor{
		Type:    FeishuWebType,
		Name:    "飞书网页文档（浏览器扩展）",
		Icon:    "📄",
		Kind:    KindBrowser,
		Version: 1,
		Tags:    []string{"office"},
		Summary: "借你浏览器里已登录的飞书网页版读云文档 · 只读，禁止复制的文档不读 · 不用建应用",
		// 配对 token 存在这个分段下（见 webui/feishuweb.go），不属于某条渠道实例。
		Integration:  FeishuWebType,
		AccountField: "name",
		Available:    true,
		Tools:        []string{"feishu_web_read_doc", "feishu_web_recent", "feishu_web_search"},
		ToolInfo: []ToolMeta{
			{Name: "feishu_web_read_doc", Title: "读取飞书文档", Desc: "按链接读一篇云文档或知识库页面，转成 Markdown"},
			{Name: "feishu_web_recent", Title: "最近的飞书文档", Desc: "浏览器里开着的和读过的飞书文档"},
			{Name: "feishu_web_search", Title: "检索飞书文档", Desc: "尚未实现，会返回 not_implemented"},
		},
		// 没有写工具，也就没有写开关。
		Fields: []Field{
			{
				Name: "name", Label: "名称", Kind: KindText, Required: true, Default: "本机浏览器",
				Help: "只用来在列表里区分，随便起",
			},
		},
	}
}

// BrowserEnabled 报告是否配置并启用了飞书网页文档渠道。MCP 进程据此决定
// 要不要注册那三个工具。
func (m *Manager) BrowserEnabled() (bool, error) {
	views, err := m.List()
	if err != nil {
		return false, err
	}
	for _, v := range views {
		if v.Descriptor.Kind == KindBrowser && v.Instance.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// testBrowser 是「测试并保存」：扩展连上了就算通。
func (m *Manager) testBrowser(ctx context.Context) (any, error) {
	if m.BrowserCheck == nil {
		return nil, fmt.Errorf("只能在配置页面里检查浏览器扩展")
	}
	return m.BrowserCheck(ctx)
}

// pairingRef 是扩展配对 token 在系统密码管理器里的位置。
var pairingRef = credstore.Ref{Integration: FeishuWebType, Account: "pairing-token"}

// PairingToken 取当前配对 token。没配对过返回空串。
func (m *Manager) PairingToken() (string, error) {
	v, err := m.creds.Secret(pairingRef).Value()
	if errors.Is(err, credstore.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// NewPairing 生成新的配对 token 并存进系统密码管理器，旧的随之作废。
func (m *Manager) NewPairing() (string, error) {
	tok := feishuweb.NewPairingToken()
	if err := m.creds.Put(pairingRef, tok); err != nil {
		return "", err
	}
	return tok, nil
}
