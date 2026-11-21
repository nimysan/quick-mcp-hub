package olkwin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFixture(t *testing.T, opt fixtureOpt) (*Store, string) {
	t.Helper()
	root := writeFixture(t, t.TempDir(), opt)
	s, err := Open(root, Options{MaxBodyBytes: 4000})
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestFixture(t *testing.T) {
	ctx := context.Background()
	s, _ := openFixture(t, fixtureOpt{})
	if s.Skipped() != 1 {
		t.Errorf("Skipped = %d，期望 1（M6）", s.Skipped())
	}
	fs, err := s.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Folder{}
	for _, f := range fs {
		byPath[f.Path] = f
	}
	inbox, ok := byPath["Inbox"]
	if !ok {
		t.Fatalf("没有 Inbox：%v", byPath)
	}
	if inbox.Special != "inbox" || !inbox.Mail || inbox.ParentID != 0 || inbox.ServerTotal != 3035 || inbox.MessageCount != 2 || inbox.UnreadCount != 1 {
		t.Errorf("Inbox = %+v", inbox)
	}
	sub, ok := byPath["Inbox/项目 2026"]
	if !ok || sub.ParentID != inbox.ID || sub.MessageCount != 1 {
		t.Errorf("子文件夹 = %+v（%v）", sub, ok)
	}
	if n := byPath["Notes"]; n.Mail {
		t.Errorf("Notes 不该是邮件文件夹")
	}

	// 全部邮件：M1、M2、M4（只有完整邮件）、M3（无接收时间，排最后）；M5 已删除，M6 解不开
	p, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	var subj []string
	for _, m := range p.Items {
		subj = append(subj, m.Subject)
	}
	if p.Total != 4 || p.NextOffset != -1 || !strings.HasPrefix(subj[0], "mcp-hub-test 111") || !strings.HasPrefix(subj[1], "mcp-hub-test 222") || subj[2] != "Re: 周报" || subj[3] != "no sender" {
		t.Fatalf("List = %d %q", p.Total, subj)
	}
	m1 := p.Items[0]
	if !m1.Received.Equal(fxRecv1) || !m1.HasAttachment || m1.Read || m1.From != "Yiming Liang" || m1.FromAddress != "yiming@example.com" ||
		len(m1.To) != 2 || m1.To[0] != "张三" || m1.Preview != "你好 😀" || m1.FolderID != inbox.ID || m1.ItemID != "M1" || len(m1.Incomplete) != 0 {
		t.Errorf("M1 = %+v", m1)
	}
	m2 := p.Items[1]
	if !m2.Read || m2.Subject != "mcp-hub-test 222:最好有中文主题、带附件的、HTML 格式的" {
		t.Errorf("M2 没有用日志里的新版本：%+v", m2)
	}
	m3 := p.Items[3]
	if !m3.Received.IsZero() || !contains(m3.Incomplete, "received") || !contains(m3.Incomplete, "from") {
		t.Errorf("M3 incomplete = %v", m3.Incomplete)
	}

	// 过滤和分页
	for _, c := range []struct {
		q    Query
		want int
	}{
		{Query{FolderID: inbox.ID}, 2},
		{Query{Subject: "MCP-HUB"}, 2},
		{Query{From: "bob"}, 1},
		{Query{From: "yiming@"}, 2},
		{Query{Since: fxRecv2}, 2},
		{Query{Since: fxRecv3, Until: fxRecv2}, 1},
		{Query{FolderID: sub.ID}, 1},
	} {
		got, err := s.List(ctx, c.q)
		if err != nil || got.Total != c.want {
			t.Errorf("List(%+v) = %d, %v；期望 %d", c.q, got.Total, err, c.want)
		}
	}
	pg, _ := s.List(ctx, Query{Limit: 3})
	if len(pg.Items) != 3 || pg.NextOffset != 3 {
		t.Errorf("分页 1：%d %d", len(pg.Items), pg.NextOffset)
	}
	pg, _ = s.List(ctx, Query{Limit: 3, Offset: 3})
	if len(pg.Items) != 1 || pg.NextOffset != -1 {
		t.Errorf("分页 2：%d %d", len(pg.Items), pg.NextOffset)
	}
	if _, err := s.List(ctx, Query{FolderID: 12345}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的文件夹：%v", err)
	}

	// Get：M1（Snappy 包装）
	g1, err := s.Get(ctx, m1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g1.Body != "你好 世界\n第二行" || g1.BodySource != "html" || g1.BodyTruncated || g1.MessageID != "<M1@example.com>" {
		t.Errorf("M1 正文 = %q %s %v %q", g1.Body, g1.BodySource, g1.BodyTruncated, g1.MessageID)
	}
	if len(g1.Attachments) != 1 || g1.Attachments[0] != (Attachment{Name: "报告.pdf", ContentType: "application/pdf", Size: 1339765}) {
		t.Errorf("M1 附件 = %+v", g1.Attachments)
	}
	if len(g1.Recipients) != 2 || g1.Recipients[1] != (Recipient{Type: "cc", Name: "王五", Address: "wang@example.com"}) || len(g1.Cc) != 1 {
		t.Errorf("M1 收件人 = %+v cc=%v", g1.Recipients, g1.Cc)
	}
	// M2：外部 blob，正文截断到 4000 字节且不切断 UTF-8
	g2, err := s.Get(ctx, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !g2.BodyTruncated || len(g2.Body) > 4000 || !strings.HasPrefix(g2.Body, "长正文") || len(g2.Attachments) != 0 || g2.HasAttachment {
		t.Errorf("M2 = len %d truncated %v atts %d", len(g2.Body), g2.BodyTruncated, len(g2.Attachments))
	}
	// M3：没有完整邮件，正文只有摘要
	g3, _ := s.Get(ctx, m3.ID)
	if g3.BodySource != "preview" || g3.Body != "only preview" || !contains(g3.Incomplete, "body") || !contains(g3.Incomplete, "recipients") {
		t.Errorf("M3 = %+v", g3)
	}
	// M4：只有完整邮件，发件人来自 Sender/From
	g4, _ := s.Get(ctx, p.Items[2].ID)
	if g4.From != "Bob" || g4.Body != "hi" || g4.FolderID != byPath["Sent Items"].ID {
		t.Errorf("M4 = %+v", g4)
	}
	if _, err := s.Get(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的邮件：%v", err)
	}
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

func TestStale(t *testing.T) {
	s, root := openFixture(t, fixtureOpt{})
	if s.Stale() {
		t.Fatal("刚打开就 Stale")
	}
	lf := filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb", "000007.log")
	f, err := os.OpenFile(lf, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0})
	f.Close()
	if !s.Stale() {
		t.Error("日志变了以后应该 Stale")
	}
}

func TestReadOnly(t *testing.T) {
	root := writeFixture(t, t.TempDir(), fixtureOpt{})
	snap := func() map[string]string {
		m := map[string]string{}
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				b, _ := os.ReadFile(p)
				m[p] = info.ModTime().String() + string(b)
			}
			return nil
		})
		return m
	}
	before := snap()
	s, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.List(context.Background(), Query{})
	for _, m := range p.Items {
		s.Get(context.Background(), m.ID)
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatalf("文件数变了：%d → %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s 被改动", k)
		}
	}
}

func TestBadFormats(t *testing.T) {
	// 不存在的目录、空目录、没有账户库
	if _, err := Open(filepath.Join(t.TempDir(), "nope"), Options{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的目录：%v", err)
	}
	if _, err := Open(t.TempDir(), Options{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("空目录：%v", err)
	}
	// 比较器不对
	root := writeFixture(t, t.TempDir(), fixtureOpt{comparator: "leveldb.BytewiseComparator"})
	if _, err := Open(root, Options{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("比较器：%v", err)
	}
	// V8 版本过新：所有值都解不开 → 自检失败
	root = writeFixture(t, t.TempDir(), fixtureOpt{v8Version: 99})
	if _, err := Open(root, Options{}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("V8 版本：%v", err)
	}
	// CURRENT、MANIFEST、表文件被破坏
	for _, c := range []struct {
		file string
		data []byte
	}{
		{"CURRENT", []byte("garbage")},
		{"MANIFEST-000002", []byte("not a log")},
		{"000005.ldb", bytes.Repeat([]byte{0x5a}, 4096)},
	} {
		root := writeFixture(t, t.TempDir(), fixtureOpt{})
		ldb := filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb")
		os.WriteFile(filepath.Join(ldb, c.file), c.data, 0o644)
		if _, err := Open(root, Options{}); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%s 损坏：%v", c.file, err)
		}
	}
	// 表文件被删（压实中途）：重试后仍然没有 → ErrNotFound
	root = writeFixture(t, t.TempDir(), fixtureOpt{})
	os.Remove(filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb", "000005.ldb"))
	if _, err := Open(root, Options{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("表文件缺失：%v", err)
	}
}

// TestCorruptNoPanic 随机翻转表文件和日志里的字节，只要求不 panic。
func TestCorruptNoPanic(t *testing.T) {
	root := writeFixture(t, t.TempDir(), fixtureOpt{})
	ldb := filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb")
	for _, name := range []string{"000005.ldb", "000007.log"} {
		orig, _ := os.ReadFile(filepath.Join(ldb, name))
		for i := 0; i < 200; i++ {
			b := append([]byte(nil), orig...)
			for j := 0; j < 8; j++ {
				b[(i*7919+j*104729)%len(b)] ^= byte(i + j + 1)
			}
			os.WriteFile(filepath.Join(ldb, name), b, 0o644)
			if s, err := Open(root, Options{}); err == nil {
				p, _ := s.List(context.Background(), Query{})
				for _, m := range p.Items {
					s.Get(context.Background(), m.ID)
				}
			}
		}
		os.WriteFile(filepath.Join(ldb, name), orig, 0o644)
	}
	// V8 解码器直接喂截断的值
	b := ssv(o("a", []any{"x", 1.5, time.Unix(0, 0)}, "b", o("c", "中文")))
	for i := 15; i <= len(b); i++ {
		decodeV8(b[15:i])
	}
}

func TestSnappy(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
	}{
		{[]byte{0x0c, 0x08, 'a', 'b', 'c', 0x15, 0x03}, "abcabcabcabc"},               // 1 字节偏移的回引（重叠）
		{[]byte{0x0c, 0x08, 'a', 'b', 'c', 0x22, 0x03, 0x00}, "abcabcabcabc"},         // 2 字节偏移
		{[]byte{0x0c, 0x08, 'a', 'b', 'c', 0x23, 0x03, 0, 0, 0}, "abcabcabcabc"},      // 4 字节偏移
		{snappyLiteral(bytes.Repeat([]byte("x"), 70000)), strings.Repeat("x", 70000)}, // 长字面量
	} {
		got, err := snappyDecode(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("snappy % x: %v", c.in[:min(8, len(c.in))], err)
		}
	}
	for _, bad := range [][]byte{{}, {0x05, 0x08, 'a'}, {0x03, 0x15, 0x03}, {0xff, 0xff, 0xff, 0xff, 0xff}} {
		if _, err := snappyDecode(bad); err == nil {
			t.Errorf("坏数据 % x 应报错", bad)
		}
	}
}

func TestKeyPrefix(t *testing.T) {
	for _, kp := range []keyPrefix{{}, {db: 3, store: 11, index: 1}, {db: 300, store: 70000, index: 30}} {
		got, rest, ok := decodePrefix(append(encodePrefix(kp), 9))
		if !ok || got != kp || len(rest) != 1 {
			t.Errorf("前缀 %+v → %+v %v", kp, got, ok)
		}
	}
	k := append([]byte{4}, binary2(2)...)
	k = append(k, 1)
	k = append(k, sWithLen("中")...)
	k = append(k, 3, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f)
	if s, rest, ok := idbKey(k, 0); !ok || s != "[中,1]" || len(rest) != 0 {
		t.Errorf("数组主键 = %q %v", s, ok)
	}
}

func binary2(n byte) []byte { return []byte{n} }
