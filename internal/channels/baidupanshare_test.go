package channels

import "testing"

// 这条渠道的形状有几处是刻意的，回归时要守住。
func TestBaiduPanShareDescriptor(t *testing.T) {
	d, ok := ByType(BaiduPanShareType)
	if !ok {
		t.Fatalf("%s 没有出现在 All() 里", BaiduPanShareType)
	}
	if d.Kind != KindShareLink {
		t.Errorf("Kind = %q，想要 %q", d.Kind, KindShareLink)
	}
	// 没有凭据：链接和提取码是调用参数，不是配置。有 SecretField 会让
	// Save 去要一份不存在的密码，并在密码管理器里留下空条目。
	if d.SecretField != "" {
		t.Errorf("这条渠道不该有 SecretField，却有 %q", d.SecretField)
	}
	// 只读渠道不该有写开关，否则页面上会出现一个打开了也没用的勾。
	if len(d.WriteTools) != 0 {
		t.Errorf("不该有写工具，却有 %v", d.WriteTools)
	}
	if len(d.Tools) != 1 || d.Tools[0] != "baidupan_share_list" {
		t.Errorf("工具清单不对：%v", d.Tools)
	}
	if !d.Available {
		t.Error("应当在所有平台可用：它只发 HTTP，不依赖本机程序")
	}
	// 和 baidupan（开放平台那条）必须是两条不同的渠道。
	if BaiduPanShareType == "baidupan" {
		t.Error("type id 和开放平台那条撞了")
	}
	if _, ok := ByType("baidupan"); !ok {
		t.Error("开放平台那条渠道不见了 —— 新渠道不该顶掉它")
	}
}

func TestBaiduPanShareToolInfoMatchesTools(t *testing.T) {
	d, _ := ByType(BaiduPanShareType)
	// ToolInfo 是页面上展示的说明，少一条就会有工具没有解释。
	if len(d.ToolInfo) != len(d.Tools) {
		t.Fatalf("ToolInfo %d 条，Tools %d 条", len(d.ToolInfo), len(d.Tools))
	}
	for i, m := range d.ToolInfo {
		if m.Name != d.Tools[i] {
			t.Errorf("第 %d 条说明的名字是 %q，工具是 %q", i, m.Name, d.Tools[i])
		}
		if m.Title == "" || m.Desc == "" {
			t.Errorf("%s 的说明不全", m.Name)
		}
	}
}
