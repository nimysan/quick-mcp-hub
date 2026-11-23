// 分享链接类渠道：百度网盘分享（baidupan-share）。
//
// 和另外几种渠道都不一样：它**没有任何要保存的东西**。链接和提取码是每次调用
// 时给的参数，不是配置；也没有凭据、没有本机程序、没有要配对的扩展。所以这条
// 渠道实例只相当于一个开关 ——「允许模型读分享链接」。
//
// 它和 baidupan（catalog/baidupan.json）是两条渠道，别混：那条走开放平台
// OpenAPI，以使用者本人的身份操作**他自己的**网盘；这条走匿名分享接口，读的是
// **别人分享出来的**链接。身份模型和风险都不同，合成一条会让写开关的语义含混。
package channels

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nimysan/mcp-hub/internal/baidupanshare"
)

// KindShareLink 是「公开分享链接」类渠道：无凭据、无本机依赖，只有开关。
const KindShareLink = "sharelink"

// BaiduPanShareType 是百度网盘分享渠道的类型 id。
const BaiduPanShareType = "baidupan-share"

const baiduShareNameField = "name"

func baiduPanShareDescriptor() Descriptor {
	return Descriptor{
		Type:    BaiduPanShareType,
		Name:    "百度网盘分享链接（只读）",
		Icon:    "🔗",
		Kind:    KindShareLink,
		Version: 1,
		Tags:    []string{"storage"},
		Summary: "给一个分享链接和提取码，列出里面有哪些文件和目录 · 只读，不下载、不转存 · 不用账号，也不用授权",
		// 没有凭据，Integration 只用来占位：这条渠道不往密码管理器里写任何东西。
		Integration:  BaiduPanShareType,
		AccountField: baiduShareNameField,
		Available:    true,
		Tools:        []string{"baidupan_share_list"},
		ToolInfo: []ToolMeta{
			{Name: "baidupan_share_list", Title: "列出分享链接里的文件", Desc: "按链接和提取码列出一个百度网盘分享里的文件和目录，可逐级进入子目录"},
		},
		// 没有写工具，也就没有写开关。
		Fields: []Field{{
			Name: baiduShareNameField, Label: "名称", Kind: KindText, Required: true,
			Default: "百度网盘分享",
			Help:    "只用来在列表里认出这条连接，一般不用改",
		}},
	}
}

// ShareLinkEnabled 报告是否配置并启用了分享链接类渠道。MCP 进程据此决定要不要
// 注册那个工具。
func (m *Manager) ShareLinkEnabled() (bool, error) {
	views, err := m.List()
	if err != nil {
		return false, err
	}
	for _, v := range views {
		if v.Descriptor.Kind == KindShareLink && v.Instance.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// testShareLink 是「测试并保存」。
//
// 这条渠道没有凭据可验，所以能验的只有一件事：这台机器够不够得着
// pan.baidu.com。这不是走形式 —— 公司网络封了外网、或 DNS 不通时，用户会在
// 真正用工具时才撞上，而那时报出来的是一个看不懂的网络错误。
func (m *Manager) testShareLink(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, baidupanshare.Host+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", baidupanshare.UserAgent)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 pan.baidu.com：%w", err)
	}
	defer resp.Body.Close()
	// 不挑状态码：首页可能回 302 或 403，能建连说明网络这一关过了，
	// 而这正是这个测试唯一能证明的事。
	return map[string]any{"host": baidupanshare.Host, "reachable": true}, nil
}
