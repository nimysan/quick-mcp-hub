package outlookwin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSamples 读公开样本做统计。样本不进 git，用 OUTLOOKWIN_SAMPLES 指向放样本的目录：
//
//	OUTLOOKWIN_SAMPLES=/path/to/pst-samples go test -run TestSamples -v ./internal/localmail/outlookwin/
//
// 目录里的每个 .pst / .ost 都会被打开、列全部文件夹、翻完全部邮件、逐封 Get，
// 并输出各字段的抽取率。测试前后对比文件的大小、修改时间和 SHA-256，确认没被改动。
func TestSamples(t *testing.T) {
	dir := os.Getenv("OUTLOOKWIN_SAMPLES")
	if dir == "" {
		t.Skip("设置 OUTLOOKWIN_SAMPLES=<样本目录> 才跑公开样本")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.[po]st"))
	if len(files) == 0 {
		t.Skip("样本目录里没有 .pst / .ost")
	}
	sort.Strings(files)
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) { checkSample(t, path) })
	}
}

func fileDigest(t *testing.T, path string) string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	h := sha256.New()
	io.Copy(h, f)
	return fmt.Sprintf("%d %s %x", fi.Size(), fi.ModTime().Format(time.RFC3339Nano), h.Sum(nil))
}

func checkSample(t *testing.T, path string) {
	before := fileDigest(t, path)
	defer func() {
		if after := fileDigest(t, path); after != before {
			t.Errorf("样本被改动了: %s -> %s", before, after)
		}
	}()
	ctx := context.Background()
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	lay, ost, crypt := s.Format()
	t.Logf("布局=%s ost=%v 加密=%d", lay, ost, crypt)

	folders, err := s.Folders(ctx)
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	mailFolders, declared := 0, 0
	for _, f := range folders {
		if f.Mail {
			mailFolders++
			declared += f.MessageCount
		}
		t.Logf("  文件夹 %-40q class=%-14s special=%-6s 邮件=%d 未读=%d", f.Path, f.Class, f.Special, f.MessageCount, f.UnreadCount)
	}
	t.Logf("文件夹 %d 个，其中邮件文件夹 %d 个，PidTagContentCount 合计 %d", len(folders), mailFolders, declared)

	var all []Summary
	for off := 0; off >= 0; {
		p, err := s.List(ctx, Query{Limit: 200, Offset: off})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		all = append(all, p.Items...)
		off = p.NextOffset
	}
	t.Logf("List 共 %d 封", len(all))
	if declared > 0 && len(all) == 0 {
		t.Errorf("文件夹声明有 %d 封，List 一封也没列出", declared)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Received.After(all[i-1].Received) {
			t.Fatalf("List 没按接收时间倒序: %d", i)
		}
	}

	stat := map[string]int{}
	bodySrc := map[string]int{}
	getFail := 0
	for _, m := range all {
		g, err := s.Get(ctx, m.ID)
		if err != nil {
			getFail++
			t.Logf("Get %d: %v", m.ID, err)
			continue
		}
		if g.FolderID != m.FolderID {
			t.Errorf("Get %d 的文件夹 %d 和 List 的 %d 不一致", m.ID, g.FolderID, m.FolderID)
		}
		count := func(k string, ok bool) {
			if ok {
				stat[k]++
			}
		}
		count("subject", g.Subject != "")
		count("from", g.From != "")
		count("from_address", g.FromAddress != "")
		count("to", len(g.To) > 0)
		count("received", !g.Received.IsZero())
		count("sent", !g.Sent.IsZero())
		count("message_id", g.MessageID != "")
		count("body", g.Body != "")
		count("recipients", len(g.Recipients) > 0)
		count("has_attachment", g.HasAttachment)
		count("attachments_named", len(g.Attachments) > 0 && g.Attachments[0].Name != "")
		count("list_subject_matches_get", m.Subject == g.Subject)
		count("list_from_address", m.FromAddress != "")
		count("list_preview", m.Preview != "")
		bodySrc[g.BodySource]++
		if !utf8ok(g.Subject) || !utf8ok(g.Body) {
			t.Errorf("邮件 %d 有非法 UTF-8", m.ID)
		}
	}
	n := len(all) - getFail
	t.Logf("Get 成功 %d/%d", n, len(all))
	keys := make([]string, 0, len(stat))
	for k := range stat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-26s %4d/%d (%.0f%%)\n", k, stat[k], n, 100*float64(stat[k])/float64(max(n, 1)))
	}
	t.Logf("字段抽取率:\n%s正文来源: %v", b.String(), bodySrc)
	if getFail > 0 {
		t.Errorf("%d 封 Get 失败", getFail)
	}
	// 非邮件文件夹（日历、联系人、通讯组）里的条目：指明文件夹时也要能列、能取，不报错。
	for _, f := range folders {
		if f.Mail || f.MessageCount == 0 {
			continue
		}
		p, err := s.List(ctx, Query{FolderID: f.ID, Limit: 200})
		if err != nil {
			t.Errorf("List(%q): %v", f.Path, err)
			continue
		}
		for _, m := range p.Items {
			if _, err := s.Get(ctx, m.ID); err != nil {
				t.Errorf("Get %d（%s）: %v", m.ID, f.Path, err)
			}
		}
		t.Logf("非邮件文件夹 %q: 声明 %d 条，列出 %d 条", f.Path, f.MessageCount, len(p.Items))
	}
}

func utf8ok(s string) bool { return strings.ToValidUTF8(s, "") == s }
