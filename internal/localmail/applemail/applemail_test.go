package applemail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// 以下全部是合成的假数据，账户、地址、内容均为虚构。
const (
	acct   = "1A2B3C4D-0000-4000-8000-000000000001"
	acct2  = "9F8E7D6C-0000-4000-8000-000000000002"
	storeU = "AAAA0000-1111-2222-3333-444455556666"
)

func day(d int) int64 { return time.Date(2025, 3, d, 9, 0, 0, 0, time.UTC).Unix() }

func gb(s string) []byte {
	b, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(s))
	if err != nil {
		panic(err)
	}
	return b
}

func qp(b []byte) string {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.String()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

const plistTail = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>flags</key>
	<integer>8590195713</integer>
</dict>
</plist>
`

func writeEmlx(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("%d\n%s%s", len(raw), raw, plistTail)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	root, base, db string
}

// buildFixture 造一个最小的 ~/Library/Mail：V2（空）、V9（有索引，诱饵）、V10（真正使用）。
func buildFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"V2", "V9/MailData"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "V9", "MailData", "Envelope Index"), nil, 0o644)
	base := filepath.Join(root, "V10")
	os.MkdirAll(filepath.Join(base, "MailData"), 0o755)
	dbPath := filepath.Join(base, "MailData", "Envelope Index")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT)`,
		`CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT)`,
		`CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT, total_count INTEGER, unread_count INTEGER)`,
		`CREATE TABLE summaries (ROWID INTEGER PRIMARY KEY, summary TEXT)`,
		`CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, message_id INTEGER, global_message_id INTEGER,
			sender INTEGER, subject_prefix TEXT, subject INTEGER, summary INTEGER, date_sent INTEGER,
			date_received INTEGER, mailbox INTEGER, remote_mailbox INTEGER, flags INTEGER, read INTEGER,
			flagged INTEGER, deleted INTEGER, size INTEGER, conversation_id INTEGER)`,
		`CREATE TABLE recipients (ROWID INTEGER PRIMARY KEY, message INTEGER, address INTEGER, type INTEGER, position INTEGER)`,
		`CREATE TABLE attachments (ROWID INTEGER PRIMARY KEY, message INTEGER, attachment_id TEXT, name TEXT)`,
		`INSERT INTO addresses VALUES (1,'alice@example.com','Alice 测试'),(2,'bob@example.org','Bob'),
			(3,'zhangsan@example.cn','张三'),(4,'me@example.net','')`,
		fmt.Sprintf(`INSERT INTO mailboxes VALUES (1,'imap://%[1]s/INBOX',5,2),(2,'imap://%[1]s/Archive/2024',1,0),
			(3,'ews://%[2]s/%%E6%%94%%B6%%E4%%BB%%B6%%E7%%AE%%B1',1,1),(4,'imap://%[1]s/..%%2F..%%2Fetc',1,0),
			(5,'local:///Drafts',1,0)`, acct, acct2),
		`INSERT INTO subjects VALUES (1,'周会纪要'),(2,'HTML 通讯'),(3,'季度报告'),(4,'合同附件'),
			(5,'大附件'),(6,'只有头'),(7,'草稿'),(8,'已删除'),(9,'穿越'),(10,'100% off')`,
		`INSERT INTO summaries VALUES (1,'本周进度……')`,
	}
	type row struct {
		id, sender, subject, mailbox int64
		date                         int64
		read, deleted                int
	}
	rows := []row{
		{1, 1, 1, 1, day(1), 1, 0},
		{12345, 2, 2, 1, day(2), 0, 0},
		{3, 3, 3, 2, day(3), 1, 0},
		{4, 1, 4, 3, day(4), 0, 0},
		{5, 3, 5, 1, day(5), 1, 0},
		{6, 2, 6, 1, day(6), 0, 0},
		{7, 4, 7, 5, day(7), 1, 0},
		{8, 1, 8, 1, day(8), 1, 1},
		{9, 2, 9, 4, day(9), 1, 0},
		{10, 2, 10, 1, day(10), 1, 0},
	}
	for _, r := range rows {
		summary := "NULL"
		if r.id == 1 {
			summary = "1"
		}
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO messages (ROWID,sender,subject,summary,date_sent,date_received,
			mailbox,read,flagged,deleted,size) VALUES (%d,%d,%d,%s,%d,%d,%d,%d,0,%d,1000)`,
			r.id, r.sender, r.subject, summary, r.date-60, r.date, r.mailbox, r.read, r.deleted))
	}
	stmts = append(stmts,
		`INSERT INTO recipients VALUES (1,6,4,0,0),(2,6,1,1,0)`,
		`INSERT INTO attachments VALUES (1,6,'2','预算.xlsx')`,
	)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	db.Close()

	inbox := filepath.Join(base, acct, "INBOX.mbox", storeU, "Data")

	// 1：普通 text/plain，UTF-8 quoted-printable 中文。
	writeEmlx(t, filepath.Join(inbox, "Messages", "1.emlx"), "From: =?UTF-8?B?"+b64([]byte("Alice 测试"))+"?= <alice@example.com>\n"+
		"To: me@example.net\nCc: Bob <bob@example.org>\nSubject: =?UTF-8?Q?"+strings.ReplaceAll(strings.TrimSpace(qp([]byte("周会纪要"))), " ", "_")+"?=\n"+
		"Message-ID: <m1@example.com>\nDate: Sat, 01 Mar 2025 09:00:00 +0000\nMIME-Version: 1.0\n"+
		"Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\n"+
		qp([]byte("本周进度：完成登录模块。\n下周计划：联调。"))+"\n")

	// 12345：只有 HTML，UTF-8 base64；ROWID ≥1000 走分桶目录 Data/2/1/Messages。
	html := `<html><head><style>p{color:red}</style><title>t</title></head><body><p>你好，<b>世界</b>！</p>` +
		`<ul><li>第一项</li><li>第二项 &amp; 更多</li></ul><script>alert(1)</script></body></html>`
	writeEmlx(t, filepath.Join(inbox, "2", "1", "Messages", "12345.emlx"),
		"From: Bob <bob@example.org>\nTo: me@example.net\nSubject: HTML\nMIME-Version: 1.0\n"+
			"Content-Type: text/html; charset=UTF-8\nContent-Transfer-Encoding: base64\n\n"+b64([]byte(html))+"\n")

	// 3：嵌套邮箱 Archive/2024，GB18030 主题（RFC 2047 B）+ GB18030 QP 正文。
	writeEmlx(t, filepath.Join(base, acct, "Archive.mbox", "2024.mbox", storeU, "Data", "Messages", "3.emlx"),
		"From: =?GB18030?B?"+b64(gb("张三"))+"?= <zhangsan@example.cn>\nTo: me@example.net\n"+
			"Subject: =?GB18030?B?"+b64(gb("季度报告"))+"?=\nMIME-Version: 1.0\n"+
			"Content-Type: text/plain; charset=GB18030\nContent-Transfer-Encoding: quoted-printable\n\n"+
			qp(gb("第三季度营收增长，详见附表。"))+"\n")

	// 4：Exchange 中文文件夹，multipart/mixed 带 PDF 附件（文件名 RFC 2231 编码）。
	pdf := []byte("%PDF-1.4 fake pdf for test\n")
	writeEmlx(t, filepath.Join(base, acct2, "收件箱.mbox", storeU, "Data", "Messages", "4.emlx"),
		"From: alice@example.com\nTo: me@example.net\nSubject: contract\nMIME-Version: 1.0\n"+
			"Content-Type: multipart/mixed; boundary=\"BOUND\"\n\n--BOUND\n"+
			"Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\n"+b64([]byte("请查收合同。"))+"\n"+
			"--BOUND\nContent-Type: application/pdf\nContent-Transfer-Encoding: base64\n"+
			"Content-Disposition: attachment; filename*=UTF-8''%E5%90%88%E5%90%8C.pdf\n\n"+b64(pdf)+"\n--BOUND--\n")

	// 5：.partial.emlx，附件分段被剥离，本体在 Attachments/5/2/。
	writeEmlx(t, filepath.Join(inbox, "Messages", "5.partial.emlx"),
		"From: zhangsan@example.cn\nTo: me@example.net\nSubject: big\nMIME-Version: 1.0\n"+
			"Content-Type: multipart/mixed; boundary=\"B2\"\n\n--B2\n"+
			"Content-Type: text/plain; charset=utf-8\n\n附件太大，见下。\n"+
			"--B2\nContent-Type: application/vnd.ms-excel; name=report.xlsx\n"+
			"Content-Disposition: attachment; filename=report.xlsx\nX-Apple-Content-Length: 99\n\n\n--B2--\n")
	att := filepath.Join(inbox, "Attachments", "5", "2", "report.xlsx")
	os.MkdirAll(filepath.Dir(att), 0o755)
	os.WriteFile(att, bytes.Repeat([]byte{'x'}, 1234), 0o644)

	// 7：「我的 Mac 上」本地邮箱，旧式 <mbox>/Messages 布局。
	writeEmlx(t, filepath.Join(base, "Mailboxes", "Drafts.mbox", "Messages", "7.emlx"),
		"From: me@example.net\nSubject: draft\n\n草稿内容\n")

	// 9：放一份在 V10 之外，验证 URL 里的 ../ 不会被跟随。
	writeEmlx(t, filepath.Join(root, "etc.mbox", "Messages", "9.emlx"), "Subject: leak\n\nSHOULD NOT READ\n")

	return fixture{root: root, base: base, db: dbPath}
}

func fileHash(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func openFixture(t *testing.T) (fixture, *Store) {
	f := buildFixture(t)
	s, err := Open(f.root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return f, s
}

func TestDetectVersionPicksHighestNumeric(t *testing.T) {
	f := buildFixture(t)
	v, err := DetectVersion(f.root)
	if err != nil || v != "V10" {
		t.Fatalf("got %q, %v; want V10", v, err)
	}
	if _, err := DetectVersion(t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空目录应返回 ErrNotFound，得到 %v", err)
	}
}

func TestListMailboxes(t *testing.T) {
	_, s := openFixture(t)
	mbs, err := s.ListMailboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Mailbox{}
	for _, m := range mbs {
		got[m.Path] = m
	}
	if m, ok := got["Archive/2024"]; !ok || m.Account != acct || m.Scheme != "imap" {
		t.Errorf("嵌套邮箱解析错误: %+v", got)
	}
	if m, ok := got["收件箱"]; !ok || m.Scheme != "ews" || m.Unread != 1 {
		t.Errorf("Exchange 中文文件夹解析错误: %+v", m)
	}
	if m := got["INBOX"]; m.Total != 5 || m.Unread != 2 {
		t.Errorf("INBOX 计数错误: %+v", m)
	}
}

func ids(p *Page) []int64 {
	var out []int64
	for _, m := range p.Messages {
		out = append(out, m.ID)
	}
	return out
}

func TestListMessagesFiltersAndPaging(t *testing.T) {
	_, s := openFixture(t)
	ctx := context.Background()
	all, err := s.ListMessages(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(all)) != "[10 9 7 6 5 4 3 12345 1]" {
		t.Fatalf("默认应按接收时间倒序且排除已删除: %v", ids(all))
	}
	if m := all.Messages[len(all.Messages)-1]; m.Subject != "周会纪要" || m.From != "alice@example.com" ||
		m.FromName != "Alice 测试" || !m.Read || m.Snippet != "本周进度……" || !m.DateReceived.Equal(time.Unix(day(1), 0)) {
		t.Errorf("摘要字段错误: %+v", m)
	}

	p1, _ := s.ListMessages(ctx, Query{Limit: 4})
	p2, _ := s.ListMessages(ctx, Query{Limit: 4, Offset: p1.NextOffset})
	p3, _ := s.ListMessages(ctx, Query{Limit: 4, Offset: p2.NextOffset})
	if !p1.HasMore || p1.NextOffset != 4 || !p2.HasMore || p3.HasMore || len(p3.Messages) != 1 {
		t.Errorf("分页错误: %v %v %v", ids(p1), ids(p2), ids(p3))
	}

	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"发件人地址不区分大小写", Query{From: "ALICE@"}, "[4 1]"},
		{"发件人显示名中文", Query{From: "张三"}, "[5 3]"},
		{"主题关键词", Query{Subject: "季度"}, "[3]"},
		{"LIKE 通配符被转义", Query{Subject: "%"}, "[10]"},
		{"时间范围 [3日,6日)", Query{Since: time.Unix(day(3), 0), Until: time.Unix(day(6), 0)}, "[5 4 3]"},
		{"按邮箱", Query{MailboxID: 1, From: "bob"}, "[10 6 12345]"},
		{"包含已删除", Query{Subject: "已删除", IncludeDeleted: true}, "[8]"},
	}
	for _, c := range cases {
		p, err := s.ListMessages(ctx, c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := fmt.Sprint(ids(p)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestGetMessage(t *testing.T) {
	_, s := openFixture(t)
	ctx := context.Background()
	get := func(id int64) *Message {
		t.Helper()
		m, err := s.GetMessage(ctx, id, 0)
		if err != nil {
			t.Fatalf("GetMessage(%d): %v", id, err)
		}
		return m
	}

	t.Run("普通邮件 UTF-8 QP", func(t *testing.T) {
		m := get(1)
		if !strings.Contains(m.Text, "本周进度：完成登录模块。") || m.FromHTML || m.BodyError != "" {
			t.Errorf("正文错误: %+v", m)
		}
		if fmt.Sprint(m.To) != "[me@example.net]" || fmt.Sprint(m.Cc) != "[Bob <bob@example.org>]" || m.MessageID != "m1@example.com" {
			t.Errorf("头信息错误: to=%v cc=%v id=%q", m.To, m.Cc, m.MessageID)
		}
		if len(m.Attachments) != 0 {
			t.Errorf("不该有附件: %+v", m.Attachments)
		}
	})
	t.Run("只有 HTML 且在分桶目录", func(t *testing.T) {
		m := get(12345)
		if !m.FromHTML || !strings.Contains(m.Text, "你好，世界！") || !strings.Contains(m.Text, "- 第二项 & 更多") {
			t.Errorf("HTML 转文本错误: %q", m.Text)
		}
		if strings.Contains(m.Text, "alert") || strings.Contains(m.Text, "color") || strings.Contains(m.Text, "<") {
			t.Errorf("没有去掉脚本/样式/标签: %q", m.Text)
		}
	})
	t.Run("GB18030 嵌套邮箱", func(t *testing.T) {
		m := get(3)
		if m.Text != "第三季度营收增长，详见附表。" {
			t.Errorf("GB18030 正文解码错误: %q", m.Text)
		}
	})
	t.Run("带附件 Exchange 中文文件夹", func(t *testing.T) {
		m := get(4)
		if m.Text != "请查收合同。" || len(m.Attachments) != 1 {
			t.Fatalf("got %+v", m)
		}
		a := m.Attachments[0]
		if a.Name != "合同.pdf" || a.Size != 27 || a.ContentType != "application/pdf" || a.External {
			t.Errorf("附件信息错误: %+v", a)
		}
	})
	t.Run("partial 外置附件", func(t *testing.T) {
		m := get(5)
		if !m.Partial || m.Text != "附件太大，见下。" || len(m.Attachments) != 1 {
			t.Fatalf("got %+v", m)
		}
		if a := m.Attachments[0]; a.Name != "report.xlsx" || a.Size != 1234 || !a.External || a.ContentType != "application/vnd.ms-excel" {
			t.Errorf("附件信息错误: %+v", a)
		}
	})
	t.Run("本机没有正文文件", func(t *testing.T) {
		m := get(6)
		if m.BodyError == "" || m.Text != "" {
			t.Errorf("应只给 BodyError: %+v", m)
		}
		if fmt.Sprint(m.To) != "[me@example.net]" || fmt.Sprint(m.Cc) != "[Alice 测试 <alice@example.com>]" {
			t.Errorf("应退回索引里的收件人: %v %v", m.To, m.Cc)
		}
		if len(m.Attachments) != 1 || m.Attachments[0].Name != "预算.xlsx" || m.Attachments[0].Size != -1 {
			t.Errorf("应退回索引里的附件名: %+v", m.Attachments)
		}
	})
	t.Run("本地邮箱旧布局", func(t *testing.T) {
		if m := get(7); m.Text != "草稿内容" {
			t.Errorf("got %q", m.Text)
		}
	})
	t.Run("URL 目录穿越被拒绝", func(t *testing.T) {
		m := get(9)
		if m.BodyError == "" || strings.Contains(m.Text, "SHOULD NOT READ") {
			t.Errorf("不应读到 V10 之外的文件: %+v", m)
		}
	})
	t.Run("不存在的 id", func(t *testing.T) {
		if _, err := s.GetMessage(ctx, 424242, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})
	t.Run("正文截断不切断汉字", func(t *testing.T) {
		m, err := s.GetMessage(ctx, 3, 10)
		if err != nil || !m.Truncated || m.Text != "第三季" {
			t.Errorf("got %q truncated=%v err=%v", m.Text, m.Truncated, err)
		}
	})
}

func TestReadOnly(t *testing.T) {
	f := buildFixture(t)
	before := fileHash(t, f.db)
	s, err := Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Immutable() {
		t.Log("注意：fixture 走了 immutable 后备")
	}
	if _, err := s.db.Exec(`INSERT INTO subjects VALUES (99,'x')`); err == nil {
		t.Fatal("只读连接竟然能写入")
	}
	if _, err := s.db.Exec(`CREATE TABLE evil(x)`); err == nil {
		t.Fatal("只读连接竟然能建表")
	}
	ctx := context.Background()
	s.ListMailboxes(ctx)
	s.ListMessages(ctx, Query{})
	s.GetMessage(ctx, 1, 0)
	s.Close()
	if fileHash(t, f.db) != before {
		t.Fatal("Envelope Index 内容被改动")
	}

	// immutable 后备同样不可写。
	db, err := openRO(f.db, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO subjects VALUES (98,'x')`); err == nil {
		t.Fatal("immutable 连接竟然能写入")
	}
}

func TestPermissionDeniedIsExplained(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受文件权限限制")
	}
	f := buildFixture(t)
	md := filepath.Join(f.base, "MailData")
	if err := os.Chmod(md, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(md, 0o755) })
	_, err := Open(f.root)
	if !errors.Is(err, ErrFullDiskAccess) || !strings.Contains(err.Error(), "完全磁盘访问权限") {
		t.Fatalf("want ErrFullDiskAccess with guidance, got %v", err)
	}
	os.Chmod(md, 0o755)

	os.Chmod(f.root, 0)
	t.Cleanup(func() { os.Chmod(f.root, 0o755) })
	if _, err := Open(f.root); !errors.Is(err, ErrFullDiskAccess) {
		t.Fatalf("根目录被拒应返回 ErrFullDiskAccess, got %v", err)
	}
	os.Chmod(f.root, 0o755)

	// 正文目录被拒：GetMessage 必须报错而不是静默返回空正文。
	s, err := Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acctDir := filepath.Join(f.base, acct)
	os.Chmod(acctDir, 0)
	t.Cleanup(func() { os.Chmod(acctDir, 0o755) })
	if _, err := s.GetMessage(context.Background(), 1, 0); !errors.Is(err, ErrFullDiskAccess) {
		t.Fatalf("want ErrFullDiskAccess, got %v", err)
	}
}

func TestEmlxPayload(t *testing.T) {
	raw := "Subject: a\n\nbody\n"
	if got := string(emlxPayload([]byte(fmt.Sprintf("%d\n%s%s", len(raw), raw, plistTail)))); got != raw {
		t.Errorf("没去掉 plist: %q", got)
	}
	if got := string(emlxPayload([]byte(raw))); got != raw {
		t.Errorf("非 emlx 应原样返回: %q", got)
	}
	if got := string(emlxPayload([]byte("999\n" + raw))); got != raw {
		t.Errorf("长度超出时取到文件末尾: %q", got)
	}
}

func TestInferEpoch(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()
	if e := inferEpoch(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), now); e != 0 {
		t.Errorf("Unix 秒误判为 CoreData: %d", e)
	}
	if e := inferEpoch(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()-coreDataEpoch, now); e != coreDataEpoch {
		t.Errorf("CoreData 秒未识别: %d", e)
	}
}

func TestBucketSegments(t *testing.T) {
	for in, want := range map[string]string{"194184": "[4 9 1]", "79665": "[9 7]", "999": "[]", "1234567": "[4 3 2 1]"} {
		if got := fmt.Sprint(bucketSegments(in)); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestMailboxSegmentsRejectsTraversal(t *testing.T) {
	for _, u := range []string{"imap://A/..", "imap://A/..%2Fx", "imap://../INBOX", "imap://A/a%5Cb", "nourl"} {
		if _, _, _, ok := mailboxSegments(u); ok {
			t.Errorf("%s 应被拒绝", u)
		}
	}
	segs, _, _, ok := mailboxSegments("imap://A/%5BGmail%5D/All%20Mail")
	if !ok || fmt.Sprint(segs) != "[[Gmail] All Mail]" {
		t.Errorf("got %v %v", segs, ok)
	}
}
