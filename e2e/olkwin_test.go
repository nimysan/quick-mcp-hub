package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 新版 Outlook for Windows（source=outlook_new_win）和经典版同时启用的 stdio 冒烟，只在 Windows 上跑。
// MCPHUB_E2E_OLK_DIR=<olk 的 IndexedDB 目录>（SSM 以 SYSTEM 运行时必须显式给），
// MCPHUB_E2E_PST_DIR=<放 .pst/.ost 样本的目录>。断言测试邮箱里用户自造的两封 mcp-hub-test 信；
// 只打印数量、时间和标记，不打印正文和地址。
const olkSource = "outlook_new_win"

func TestLocalMailWindowsNewAndClassic(t *testing.T) {
	winOnly(t)
	olkDir := os.Getenv("MCPHUB_E2E_OLK_DIR")
	if olkDir == "" {
		t.Skip("设 MCPHUB_E2E_OLK_DIR=<...\\Olk\\EBWebView\\Default\\IndexedDB> 才跑")
	}
	files := sampleFiles(t)
	var ost string
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f), ".ost") {
			ost = f
		}
	}
	if ost == "" {
		t.Fatal("样本目录里没有 .ost")
	}
	bin := binPath(t)
	h := mustStart(t, bin, []inst{localInst(true, map[string]string{
		olkSource: "true", olkSource + "_dir": olkDir,
		winSource: "true", winSource + "_dir": ost,
	})})

	ts := h.tools(t)
	if n := localNames(ts); len(n) != 3 {
		t.Fatalf("本机邮件工具 = %v", n)
	}
	for _, x := range ts {
		if !strings.HasPrefix(x.Name, "localmail_") {
			continue
		}
		for _, want := range []string{olkSource, winSource, "离线缓存的天数和文件夹"} {
			if !strings.Contains(x.Description, want) {
				t.Errorf("%s 的说明里没有 %q", x.Name, want)
			}
		}
	}
	t.Logf("tools/list：%d 个工具，本机邮件 %v", len(ts), localNames(ts))

	var fo foldersOut
	if isErr, text := h.call(t, "localmail_list_folders", nil, &fo); isErr {
		t.Fatalf("list_folders: %s", text)
	}
	for _, g := range fo.Sources {
		if g.Error != "" {
			t.Errorf("%s 列文件夹出错：%s", g.Source, g.Error)
		}
		t.Logf("%s：%d 个文件夹", g.Source, len(g.Folders))
	}
	if len(fo.Sources) != 2 {
		t.Errorf("来源分组 %d 个，期望 2", len(fo.Sources))
	}

	// 新版 Outlook：两封测试信
	for _, c := range []struct {
		subject string
		hhmm    string
		attach  bool
	}{{"mcp-hub-test 111", "21:54", true}, {"mcp-hub-test 222", "21:53", false}} {
		var p struct {
			page
			Messages []struct {
				summary
				HasAttachment bool `json:"has_attachment"`
			} `json:"messages"`
		}
		if isErr, text := h.call(t, "localmail_search", map[string]any{"source": olkSource, "subject": c.subject}, &p); isErr {
			t.Fatalf("search %q: %s", c.subject, text)
		}
		found := false
		for _, m := range p.Messages {
			if m.Date.Local().Format("2006-01-02 15:04") != "2026-11-21 "+c.hhmm {
				continue
			}
			found = true
			if m.HasAttachment != c.attach {
				t.Errorf("%q 附件标记 %v，期望 %v", c.subject, m.HasAttachment, c.attach)
			}
			var g struct {
				message
				Attachments []struct {
					Name string `json:"name"`
					Size int64  `json:"size"`
				} `json:"attachments"`
			}
			if isErr, text := h.call(t, "localmail_get", map[string]any{"source": olkSource, "id": m.ID}, &g); isErr {
				t.Fatalf("get %q: %s", c.subject, text)
			}
			if g.Body == "" || (len(g.Attachments) > 0) != c.attach {
				t.Errorf("%q get：正文 %d 字节，附件 %d 个", c.subject, len(g.Body), len(g.Attachments))
			}
			t.Logf("outlook_new_win %q：search 命中 %d 封，接收 %s，附件 %v，get 正文 %d 字节、附件 %d 个、incomplete %v",
				c.subject, len(p.Messages), m.Date.Local().Format(time.DateTime), m.HasAttachment, len(g.Body), len(g.Attachments), m.Incomplete)
		}
		if !found {
			t.Errorf("outlook_new_win 没搜到 %s 收到的 %q（命中 %d 封）", c.hhmm, c.subject, len(p.Messages))
		}
	}

	// 经典版：OST 样本能 search 和 get
	before := fileDigest(t, ost)
	var p page
	if isErr, text := h.call(t, "localmail_search", map[string]any{"source": winSource, "limit": 5}, &p); isErr || len(p.Messages) == 0 {
		t.Fatalf("outlook_classic_win search：%d 封 %s", len(p.Messages), text)
	}
	var g message
	if isErr, text := h.call(t, "localmail_get", map[string]any{"source": winSource, "id": p.Messages[0].ID}, &g); isErr {
		t.Fatalf("outlook_classic_win get: %s", text)
	}
	t.Logf("outlook_classic_win：search %d 封（total %d），get 正文 %d 字节", len(p.Messages), p.Total, len(g.Body))
	if fileDigest(t, ost) != before {
		t.Error("OST 样本在读取后变了")
	}
	h.alive(t)
}
