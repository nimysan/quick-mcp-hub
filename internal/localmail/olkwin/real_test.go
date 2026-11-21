package olkwin

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestReal 在装了新版 Outlook 的 Windows 上读真实数据：OLKWIN_REAL=1，
// OLKWIN_DIR 指定 IndexedDB 目录（留空用当前用户的默认目录；SSM 以 SYSTEM 运行时必须指定）。
// 断言测试邮箱里的已知邮件（用户自造的测试数据），并打印字段抽取率。不打印正文和邮箱地址。
func TestReal(t *testing.T) {
	if os.Getenv("OLKWIN_REAL") == "" {
		t.Skip("设置 OLKWIN_REAL=1（可选 OLKWIN_DIR）才运行")
	}
	ctx := context.Background()
	t0 := time.Now()
	s, err := Open(os.Getenv("OLKWIN_DIR"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Logf("Open %v，跳过的记录 %d", time.Since(t0), s.Skipped())

	folders, err := s.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var inbox *Folder
	for i, f := range folders {
		t.Logf("文件夹 %-28q special=%-14s mail=%-5v 本地=%3d 未读=%2d 服务器=%3d", f.Path, f.Special, f.Mail, f.MessageCount, f.UnreadCount, f.ServerTotal)
		if f.Special == "inbox" {
			inbox = &folders[i]
		}
	}
	if inbox == nil {
		t.Fatal("没有找到收件箱")
	}

	want := []struct {
		subject string
		hhmm    string
		attach  bool
		html    bool
	}{
		{"mcp-hub-test 111", "21:54", true, false},
		{"mcp-hub-test 222", "21:53", false, true},
		{"Microsoft account security info was added", "21:46", false, false},
	}
	for _, w := range want {
		p, err := s.List(ctx, Query{FolderID: inbox.ID, Subject: w.subject})
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != 1 {
			t.Errorf("收件箱里主题含 %q 的邮件 %d 封，期望 1", w.subject, p.Total)
			continue
		}
		m := p.Items[0]
		got := m.Received.Local().Format("2006-01-02 15:04")
		if got != "2026-11-21 "+w.hhmm {
			t.Errorf("%q 接收时间 %s（本地时区 %s），期望 2026-11-21 %s", w.subject, got, time.Local, w.hhmm)
		}
		if m.HasAttachment != w.attach {
			t.Errorf("%q 列表附件标记 %v，期望 %v", w.subject, m.HasAttachment, w.attach)
		}
		full, err := s.Get(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if w.attach && (len(full.Attachments) == 0 || full.Attachments[0].Name == "" || full.Attachments[0].Size <= 0) {
			t.Errorf("%q 附件元数据不全：%d 个", w.subject, len(full.Attachments))
		}
		if !w.attach && len(full.Attachments) != 0 {
			t.Errorf("%q 不该有附件，得到 %d 个", w.subject, len(full.Attachments))
		}
		if w.html && full.BodySource != "html" {
			t.Errorf("%q 正文来源 %s，期望 html", w.subject, full.BodySource)
		}
		if strings.HasPrefix(w.subject, "mcp-hub-test") && full.From != "Yiming Liang" {
			t.Errorf("%q 发件人显示名 %q", w.subject, full.From)
		}
		if full.Body == "" {
			t.Errorf("%q 正文为空", w.subject)
		}
		t.Logf("OK %q 接收=%s 附件=%v(%d) 正文来源=%s 正文=%d 字节 收件人=%d incomplete=%v",
			w.subject, got, m.HasAttachment, len(full.Attachments), full.BodySource, len(full.Body), len(full.Recipients), full.Incomplete)
	}

	// 字段抽取率：全部邮件逐封 Get。
	all, err := s.List(ctx, Query{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	var items []Summary
	for off := 0; off >= 0; {
		p, err := s.List(ctx, Query{Limit: 200, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, p.Items...)
		off = p.NextOffset
	}
	if len(items) != all.Total {
		t.Errorf("翻页得到 %d 封，Total=%d", len(items), all.Total)
	}
	rate := map[string]int{}
	t1 := time.Now()
	for _, m := range items {
		full, err := s.Get(ctx, m.ID)
		if err != nil {
			t.Fatalf("Get %d: %v", m.ID, err)
		}
		cnt := func(k string, ok bool) {
			if ok {
				rate[k]++
			}
		}
		cnt("subject", full.Subject != "")
		cnt("from", full.From != "")
		cnt("from_address", full.FromAddress != "")
		cnt("to", len(full.To) > 0)
		cnt("received", !full.Received.IsZero())
		cnt("sent", !full.Sent.IsZero())
		cnt("folder", s.byID[full.FolderID] != nil)
		cnt("preview", full.Preview != "")
		cnt("body", full.BodySource == "html" || full.BodySource == "text")
		cnt("body_nonempty", full.Body != "")
		cnt("message_id", full.MessageID != "")
		cnt("recipients", len(full.Recipients) > 0)
		cnt("has_attachment", full.HasAttachment)
		cnt("attachments_meta", len(full.Attachments) > 0)
		cnt("attach_consistent", full.HasAttachment == (len(full.Attachments) > 0))
		cnt("utf8_ok", utf8.ValidString(full.Subject) && utf8.ValidString(full.Body))
		cnt("complete", len(full.Incomplete) == 0)
	}
	t.Logf("全部 %d 封逐封 Get 用时 %v", len(items), time.Since(t1))
	for _, k := range []string{"subject", "from", "from_address", "to", "received", "sent", "folder", "preview", "body", "body_nonempty",
		"message_id", "recipients", "has_attachment", "attachments_meta", "attach_consistent", "utf8_ok", "complete"} {
		t.Logf("抽取率 %-18s %d/%d", k, rate[k], len(items))
	}
	if rate["utf8_ok"] != len(items) {
		t.Errorf("有 %d 封的主题或正文不是合法 UTF-8", len(items)-rate["utf8_ok"])
	}
	inc := map[string]int{}
	for _, m := range items {
		full, _ := s.Get(ctx, m.ID)
		for _, k := range full.Incomplete {
			inc[k]++
		}
	}
	t.Logf("incomplete 统计 %v", fmt.Sprint(inc))
}
