package outlookwin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var variants = []struct {
	name  string
	lay   layout
	crypt byte
}{
	{"unicode-none", layoutUnicode, cryptNone},
	{"unicode-permute", layoutUnicode, cryptPermute},
	{"unicode-cyclic", layoutUnicode, cryptCyclic},
	{"ansi-permute", layoutANSI, cryptPermute},
	{"ansi-cyclic", layoutANSI, cryptCyclic},
	{"ost-4k-zlib", layoutUnicode4K, cryptNone},
	{"ost-4k-zlib-permute", layoutUnicode4K, cryptPermute},
}

func openFixture(t *testing.T, lay layout, crypt byte, opt Options) *Store {
	t.Helper()
	if opt.Codepage == 0 {
		opt.Codepage = 936 // ANSI 夹具的文件夹名是 GB18030；Unicode 夹具不受影响
	}
	s, err := Open(buildFixture(t, lay, crypt), opt)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFixture(t *testing.T) {
	ctx := context.Background()
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			s := openFixture(t, v.lay, v.crypt, Options{})
			if lay, ost, c := s.Format(); lay != v.lay.String() || ost != (v.lay == layoutUnicode4K) || c != int(v.crypt) {
				t.Fatalf("Format = %s %v %d", lay, ost, c)
			}
			fs, err := s.Folders(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range fs {
				got = append(got, f.Path+"|"+f.Special+"|"+map[bool]string{true: "mail", false: "other"}[f.Mail])
			}
			want := []string{"收件箱|inbox|mail", "已发送邮件|sent|mail", "日历||other"}
			// 收件箱靠英文名认不出（中文名），这里只有 EntryID 认得的「已发送」
			want[0] = "收件箱||mail"
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Folders = %q, want %q", got, want)
			}

			p, err := s.List(ctx, Query{})
			if err != nil {
				t.Fatal(err)
			}
			if p.Total != 3 || len(p.Items) != 3 || p.NextOffset != -1 {
				t.Fatalf("List 全部 = total %d items %d next %d（日历里的约会不应列出）", p.Total, len(p.Items), p.NextOffset)
			}
			m1, m2, m3 := p.Items[0], p.Items[1], p.Items[2]
			if m1.ID != nidMsg1 || m2.ID != nidMsg2 || m3.ID != nidMsg3 {
				t.Fatalf("顺序不对: %d %d %d", m1.ID, m2.ID, m3.ID)
			}
			if m1.Subject != "RE: 季度报告 😀" || m1.From != "张三" || m1.FromAddress != "zhangsan@example.com" ||
				!m1.Read || !m1.HasAttachment || !m1.Received.Equal(fxT1) || m1.FolderID != nidInbox {
				t.Errorf("m1 = %+v", m1)
			}
			if !strings.HasPrefix(m1.Preview, "第三季度报告摘要") {
				t.Errorf("m1 摘要 = %q", m1.Preview)
			}
			if m2.Read || m2.FromAddress != "alice@example.com" || m2.Preview != "你好， 世界" && m2.Preview != "你好，世界" {
				t.Errorf("m2 = %+v", m2)
			}
			if m3.From != "Bob" || m3.FromAddress != "bob@example.com" {
				t.Errorf("m3 发件人回退 = %q %q", m3.From, m3.FromAddress)
			}

			check := func(q Query, ids ...int64) {
				t.Helper()
				p, err := s.List(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				var got []int64
				for _, m := range p.Items {
					got = append(got, m.ID)
				}
				if !reflect.DeepEqual(got, ids) {
					t.Errorf("List(%+v) = %v, want %v", q, got, ids)
				}
			}
			check(Query{Subject: "季度"}, nidMsg1)
			check(Query{Subject: "rtf"}, nidMsg3)
			check(Query{From: "ALICE"}, nidMsg2)
			check(Query{Since: fxT2}, nidMsg1, nidMsg2)
			check(Query{Until: fxT2}, nidMsg3)
			check(Query{FolderID: nidSent})
			check(Query{FolderID: nidCal}, nidAppt) // 指明文件夹时不按类型过滤
			check(Query{Limit: 2, Offset: 2}, nidMsg3)
			if p, _ := s.List(ctx, Query{Limit: 2}); p.NextOffset != 2 || p.Total != 3 {
				t.Errorf("分页 next=%d total=%d", p.NextOffset, p.Total)
			}
			if _, err := s.List(ctx, Query{FolderID: 999}); !errors.Is(err, ErrNotFound) {
				t.Errorf("不存在的文件夹: %v", err)
			}

			g, err := s.Get(ctx, nidMsg1)
			if err != nil {
				t.Fatal(err)
			}
			if g.Body != fxLongBody || g.BodySource != BodyText || g.BodyTruncated {
				t.Errorf("m1 正文 %d 字节 来源 %s（XBLOCK 两块拼接）", len(g.Body), g.BodySource)
			}
			if g.MessageID != "<m1@example.com>" || g.FolderID != nidInbox {
				t.Errorf("m1 = %q folder %d", g.MessageID, g.FolderID)
			}
			if !reflect.DeepEqual(g.To, []string{"李四 <lisi@example.com>"}) || !reflect.DeepEqual(g.Cc, []string{"王五 <wangwu@example.com>"}) {
				t.Errorf("收件人 to=%q cc=%q", g.To, g.Cc)
			}
			if want := []Attachment{{Name: "报告.pdf", ContentType: "application/pdf", Size: 1234}}; !reflect.DeepEqual(g.Attachments, want) {
				t.Errorf("附件 = %+v", g.Attachments)
			}
			g2, err := s.Get(ctx, nidMsg2)
			if err != nil || g2.Body != "你好，世界" || g2.BodySource != BodyHTML {
				t.Errorf("m2 HTML 正文 = %q %s %v", g2.Body, g2.BodySource, err)
			}
			g3, err := s.Get(ctx, nidMsg3)
			if err != nil || g3.Body != "hello world" || g3.BodySource != BodyRTF {
				t.Errorf("m3 RTF 正文 = %q %v", g3.Body, err)
			}
			for _, id := range []int64{0, nidInbox, 0x7FFFFFE4, 1 << 40} {
				if _, err := s.Get(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Errorf("Get(%#x) = %v, want ErrNotFound", id, err)
				}
			}
			if s.Stale() {
				t.Error("刚打开就 Stale")
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	s := openFixture(t, layoutUnicode, cryptPermute, Options{MaxBodyBytes: 100})
	g, err := s.Get(context.Background(), nidMsg1)
	if err != nil {
		t.Fatal(err)
	}
	if !g.BodyTruncated || len(g.Body) > 100 || !strings.HasPrefix(fxLongBody, g.Body) {
		t.Errorf("截断 = %v %d", g.BodyTruncated, len(g.Body))
	}
}

func TestStale(t *testing.T) {
	path := buildFixture(t, layoutUnicode, cryptNone)
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(make([]byte, 512))
	f.Close()
	if !s.Stale() {
		t.Error("文件变了 Stale 应为真")
	}
}

func TestBadFiles(t *testing.T) {
	dir := t.TempDir()
	good, _ := os.ReadFile(buildFixture(t, layoutUnicode, cryptNone))
	edp := append([]byte(nil), good...)
	edp[0x201] = cryptEDP
	ver := append([]byte(nil), good...)
	ver[10] = 99
	badRoot := append([]byte(nil), good...)
	le.PutUint64(badRoot[0xE0:], 1<<40)
	cases := map[string][]byte{
		"empty":    {},
		"text":     bytes.Repeat([]byte("hello"), 200),
		"edp":      edp,
		"version":  ver,
		"bad-root": badRoot,
	}
	for name, b := range cases {
		p := filepath.Join(dir, name+".pst")
		os.WriteFile(p, b, 0o600)
		if _, err := Open(p, Options{}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
	if _, err := Open(filepath.Join(dir, "missing.pst"), Options{}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// 结构损坏但文件头正常：Folders 返回 ErrUnsupported，不 panic。
	broken := append([]byte(nil), good...)
	for i := fxDataStart; i < len(broken)-2048; i++ {
		broken[i] ^= 0x5A
	}
	p := filepath.Join(dir, "broken.pst")
	os.WriteFile(p, broken, 0o600)
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Folders(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("broken Folders: %v", err)
	}
}

func TestCrypt(t *testing.T) {
	orig := []byte("Hello, 世界 \x00\x01\xff")
	b := append([]byte(nil), orig...)
	encodePermute(b)
	if bytes.Equal(b, orig) {
		t.Fatal("置换没有生效")
	}
	decodePermute(b)
	if !bytes.Equal(b, orig) {
		t.Fatal("置换往返不一致")
	}
	cryptCyclicBuf(b, 0x12345678)
	if bytes.Equal(b, orig) {
		t.Fatal("循环编码没有生效")
	}
	cryptCyclicBuf(b, 0x12345678)
	if !bytes.Equal(b, orig) {
		t.Fatal("循环编码不对称")
	}
}

func TestRTF(t *testing.T) {
	if len(rtfPrebuf) != 207 {
		t.Fatalf("字典初始内容 %d 字节，规范是 207", len(rtfPrebuf))
	}
	raw, err := decompressRTF(rtfSample)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\\rtf1\\ansi\\ansicpg1252\\pard hello world}\r\n"; string(raw) != want {
		t.Fatalf("解压 = %q", raw)
	}
	cases := map[string]string{
		`{\rtf1\ansi{\fonttbl{\f0 Arial;}}\pard Hello\par World}`:         "Hello\nWorld",
		`{\rtf1\ansi\ansicpg936 \'c4\'e3\'ba\'c3}`:                        "你好",
		`{\rtf1\uc1\u20320?\u22909?!}`:                                    "你好!",
		`{\rtf1{\*\htmltag19 <p>}\htmlrtf {\htmlrtf0 Hi}\htmlrtf0 there}`: "Hithere",
	}
	for in, want := range cases {
		if got := rtfToText([]byte(in), 936); got != want {
			t.Errorf("rtfToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoverHelpers(t *testing.T) {
	dir := t.TempDir()
	la := filepath.Join(dir, "AppData", "Local")
	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(la, "Microsoft", "Outlook"), 0o700)
	os.MkdirAll(filepath.Join(home, "Documents", "Outlook Files"), 0o700)
	os.WriteFile(filepath.Join(la, "Microsoft", "Outlook", "me@example.com.ost"), nil, 0o600)
	os.WriteFile(filepath.Join(la, "Microsoft", "Outlook", "notes.txt"), nil, 0o600)
	os.WriteFile(filepath.Join(home, "Documents", "Outlook Files", "Archive.PST"), nil, 0o600)
	got := mergeFiles(
		[]DataFile{{Path: filepath.Join(la, "Microsoft", "Outlook", "me@example.com.ost"), Kind: "ost", Profile: "Outlook", From: "registry"}},
		scanDirs(defaultDirs(home, la)),
	)
	if len(got) != 2 || got[0].Kind != "ost" || got[0].From != "registry" || got[1].Kind != "pst" || got[1].From != "default_dir" {
		t.Fatalf("发现 = %+v", got)
	}
}
