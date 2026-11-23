package outlooklegacy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// 全部是合成假数据。

func olkHeader() []byte {
	h := make([]byte, olkHeaderLen)
	copy(h, olkMagic)
	binary.LittleEndian.PutUint32(h[4:], 1)
	binary.LittleEndian.PutUint32(h[8:], 2)
	binary.LittleEndian.PutUint32(h[12:], 3)
	return h
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

const schema = `
CREATE TABLE Folders (Record_RecordID INTEGER PRIMARY KEY, PathToDataFile TEXT, Record_AccountUID INTEGER,
  Folder_ParentID INTEGER, Folder_FolderClass INTEGER, Folder_SpecialFolderType INTEGER, Folder_Name TEXT);
CREATE TABLE Mail (Record_RecordID INTEGER PRIMARY KEY, PathToDataFile TEXT NOT NULL, Record_FolderID INTEGER NOT NULL,
  Message_HasAttachment BOOLEAN DEFAULT 0, Message_MessageID TEXT COLLATE NOCASE,
  Message_NormalizedSubject TEXT COLLATE NOCASE, Message_PartiallyDownloaded BOOLEAN DEFAULT 0,
  Message_ReadFlag BOOLEAN DEFAULT 0, Message_DisplayTo TEXT, Message_Preview TEXT, Message_SenderList TEXT COLLATE NOCASE,
  Message_TimeReceived DATETIME, Message_TimeSent DATETIME, Message_ToRecipientAddressList TEXT,
  Message_CCRecipientAddressList TEXT, Message_SenderAddressList TEXT COLLATE NOCASE);
CREATE TABLE Blocks (BlockID BLOB, BlockTag INTEGER, PathToDataFile TEXT);
CREATE TABLE Mail_OwnedBlocks (Record_RecordID INTEGER, BlockID BLOB, BlockTag INTEGER);
`

var t0 = time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC)

// buildFixture 造一个最小 Data 目录，返回路径。
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "Outlook.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(schema)
	// 文件夹：根(-2) > 收件箱 > 项目A；另有已发送、一个占位、一个日历（非邮件类）。
	exec(`INSERT INTO Folders VALUES (10,'',1,-2,0,99,''),(11,'',1,10,0,1,'收件箱'),(12,'',1,11,0,0,'项目A'),
		(13,'',1,10,0,8,'已发送邮件'),(14,'',1,10,0,1,'Placeholder_Inbox_Placeholder'),(15,'',1,10,2,4,'Calendar')`)

	ins := `INSERT INTO Mail (Record_RecordID, PathToDataFile, Record_FolderID, Message_HasAttachment, Message_MessageID,
		Message_NormalizedSubject, Message_PartiallyDownloaded, Message_ReadFlag, Message_DisplayTo, Message_Preview,
		Message_SenderList, Message_TimeReceived, Message_TimeSent, Message_ToRecipientAddressList,
		Message_CCRecipientAddressList, Message_SenderAddressList) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	day := int64(86400)
	base := t0.Unix()
	// 1: 有完整 MIME（multipart + 附件）
	exec(ins, 1, "Messages/1/AAA.olk15Message", 11, 1, "<m1@example.test>", "季度报告", 0, 1, "张三", "预览一",
		"李四", base, base-60, "zhang@example.test; ", "", "li@example.test")
	// 2: 只有缓存 HTML，外加一个附件块
	exec(ins, 2, "Messages/2/BBB.olk15Message", 12, 1, "<m2@example.test>", "Build 100% done", 0, 0, "", "预览二",
		"Wang, Wu", base+day, base+day, "a@example.test; b@example.test; ", "c@example.test", "wang@example.test")
	// 3: 数据文件缺失，只剩预览
	exec(ins, 3, "Messages/3/CCC.olk15Message", 11, 0, "", "午餐", 0, 0, "", "只有预览",
		"Zhao", base+2*day, base+2*day, "", nil, "zhao@example.test")
	// 4: 半下载 + 未闭合 HTML
	exec(ins, 4, "Messages/4/DDD.olk15Message", 13, 0, "", "partial", 1, 1, "", "",
		"Me", base+3*day, cocoaZero, "x@example.test", "", "me@example.test")
	// 5: 指向越界路径的源块，应被忽略
	exec(ins, 5, "../outside.olk15Message", 11, 0, "", "evil", 0, 0, "", "p5",
		"Evil", base+4*day, base+4*day, "", "", "evil@example.test")

	src := "Received: from mx.example.test\r\nFrom: =?UTF-8?B?5p2O5Zub?= <li@example.test>\r\nTo: zhang@example.test\r\n" +
		"Subject: =?UTF-8?B?UkU6IOWto+W6puaKpeWRig==?=\r\nDate: Sat, 01 Mar 2025 08:00:00 +0000\r\n" +
		"Message-ID: <m1@example.test>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n这是假的正文。\r\n第二行\r\n" +
		"--XX\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ=\r\n--XX--\r\n"
	// 真实文件是老 Mac 的单 CR 换行
	writeFile(t, dir, "Message Sources/7/SRC1.olk15MsgSource", append(olkHeader(), strings.ReplaceAll(src, "\r\n", "\r")...))
	writeFile(t, dir, "Messages/1/AAA.olk15Message", append(olkHeader(), utf16le("<html><body>cache1</body></html>")...))

	cache2 := append(olkHeader(), []byte{1, 2, 3, 0, 9, 9}...)
	cache2 = append(cache2, utf16le("IPM.Note")...)
	cache2 = append(cache2, utf16le(`<HTML><head><style>p{color:red}</style></head><body><p>Hello &amp; 你好</p><br>Line2</body></HTML>`)...)
	cache2 = append(cache2, 0, 0, 3, 0, 0, 0, 0, 0)
	writeFile(t, dir, "Messages/2/BBB.olk15Message", cache2)

	att := "Content-type: application/vnd.ms-excel; name=\"=?UTF-8?B?5pWw5o2uLnhsc3g=?=\"\r\n" +
		"Content-disposition: attachment; filename=\"=?UTF-8?B?5pWw5o2uLnhsc3g=?=\"\r\nContent-transfer-encoding: base64\r\n\r\n" +
		strings.Repeat(strings.Repeat("A", 76)+"\r\n", 10)
	writeFile(t, dir, "Message Attachments/3/ATT2.olk15MsgAttachment", append(olkHeader(), strings.ReplaceAll(att, "\r\n", "\r")...))
	writeFile(t, dir, "Messages/4/DDD.olk15Message", append(olkHeader(), utf16le("<html><body>half")...))
	writeFile(t, filepath.Dir(dir), "outside.olk15Message", append(olkHeader(), utf16le("<html>leak</html>")...))

	exec(`INSERT INTO Blocks VALUES (x'01',?,?),(x'02',?,?),(x'03',?,?)`,
		tagMsgSource, "Message%20Sources/7/SRC1.olk15MsgSource",
		tagAttachment, "Message%20Attachments/3/ATT2.olk15MsgAttachment",
		tagMsgSource, "../../etc/passwd")
	exec(`INSERT INTO Mail_OwnedBlocks VALUES (1,x'01',?),(2,x'02',?),(5,x'03',?)`, tagMsgSource, tagAttachment, tagMsgSource)
	return dir
}

func hashFile(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func openFixture(t *testing.T) (*Store, string) {
	dir := buildFixture(t)
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestFolders(t *testing.T) {
	s, _ := openFixture(t)
	fs, err := s.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Folder{}
	for _, f := range fs {
		got[f.Path] = f
	}
	if len(fs) != 5 {
		t.Fatalf("want 5 mail folders, got %d: %+v", len(fs), fs)
	}
	if f, ok := got["#10/收件箱/项目A"]; !ok || f.MessageCount != 1 || f.ParentID != 11 {
		t.Errorf("subfolder path wrong: %+v", got)
	}
	if got["#10/收件箱"].Special != "inbox" || got["#10/收件箱"].MessageCount != 3 {
		t.Errorf("inbox: %+v", got["#10/收件箱"])
	}
	if !got["#10/Placeholder_Inbox_Placeholder"].Placeholder {
		t.Error("placeholder not flagged")
	}
	if _, ok := got["#10/Calendar"]; ok {
		t.Error("non-mail folder listed")
	}
}

func TestList(t *testing.T) {
	s, _ := openFixture(t)
	ctx := context.Background()
	p, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 5 || p.Items[0].ID != 5 || p.NextOffset != -1 {
		t.Fatalf("default order/total wrong: total=%d first=%d next=%d", p.Total, p.Items[0].ID, p.NextOffset)
	}
	p, _ = s.List(ctx, Query{Limit: 2, Offset: 2})
	if len(p.Items) != 2 || p.Items[0].ID != 3 || p.NextOffset != 4 {
		t.Fatalf("paging wrong: %+v", p)
	}
	cases := []struct {
		name string
		q    Query
		want []int64
	}{
		{"folder", Query{FolderID: 11}, []int64{5, 3, 1}},
		{"since/until", Query{Since: t0.Add(24 * time.Hour), Until: t0.Add(72 * time.Hour)}, []int64{3, 2}},
		{"from name", Query{From: "wang,"}, []int64{2}},
		{"from addr case", Query{From: "LI@EXAMPLE"}, []int64{1}},
		{"subject", Query{Subject: "报告"}, []int64{1}},
		{"subject literal %", Query{Subject: "100%"}, []int64{2}},
		{"subject % not wildcard", Query{Subject: "%d"}, nil},
	}
	for _, c := range cases {
		p, err := s.List(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, it := range p.Items {
			ids = append(ids, it.ID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v want %v", c.name, ids, c.want)
		}
	}
	p, _ = s.List(ctx, Query{FolderID: 12})
	it := p.Items[0]
	if it.From != "Wang, Wu" || it.FromAddress != "wang@example.test" || len(it.To) != 2 || len(it.Cc) != 1 || !it.Received.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("summary fields: %+v", it)
	}
	p, _ = s.List(ctx, Query{FolderID: 13})
	if !p.Items[0].Sent.IsZero() {
		t.Error("cocoa zero date should map to zero time")
	}
}

func TestGet(t *testing.T) {
	s, _ := openFixture(t)
	ctx := context.Background()

	m, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.BodySource != BodyMIME || !m.BodyComplete || m.Body != "这是假的正文。\r\n第二行" && m.Body != "这是假的正文。\n第二行" {
		t.Errorf("mime body: src=%s complete=%v body=%q", m.BodySource, m.BodyComplete, m.Body)
	}
	if m.OriginalSubject != "RE: 季度报告" || m.MessageID != "<m1@example.test>" || m.Date.IsZero() {
		t.Errorf("mime headers: %q %q %v", m.OriginalSubject, m.MessageID, m.Date)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "a.pdf" || m.Attachments[0].Size != 11 || m.Attachments[0].SizeEstimated {
		t.Errorf("mime attachments: %+v", m.Attachments)
	}

	m, _ = s.Get(ctx, 2)
	if m.BodySource != BodyCache || !m.BodyComplete || m.Body != "Hello & 你好\n\nLine2" {
		t.Errorf("cache body: src=%s complete=%v body=%q", m.BodySource, m.BodyComplete, m.Body)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "数据.xlsx" || !m.Attachments[0].SizeEstimated ||
		m.Attachments[0].Size < 500 || m.Attachments[0].Size > 600 {
		t.Errorf("block attachments: %+v", m.Attachments)
	}

	m, _ = s.Get(ctx, 3)
	if m.BodySource != BodyPreview || m.BodyComplete || m.Body != "只有预览" {
		t.Errorf("preview fallback: %+v", m)
	}

	m, _ = s.Get(ctx, 4)
	if m.BodySource != BodyCache || m.BodyComplete || m.Body != "half" {
		t.Errorf("partial: src=%s complete=%v body=%q", m.BodySource, m.BodyComplete, m.Body)
	}

	m, _ = s.Get(ctx, 5)
	if m.BodySource != BodyPreview || strings.Contains(m.Body, "leak") {
		t.Errorf("path traversal not blocked: %+v", m)
	}

	if _, err := s.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestTruncate(t *testing.T) {
	dir := buildFixture(t)
	s, err := Open(dir, Options{MaxBodyBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, _ := s.Get(context.Background(), 1)
	if !m.BodyTruncated || m.BodyComplete || len(m.Body) > 8 {
		t.Errorf("truncate: %+v", m)
	}
}

// 只读：两种打开方式跑完所有 API 后，库文件字节不变；immutable 下不生成 -wal/-shm。
func TestReadOnly(t *testing.T) {
	dir := buildFixture(t)
	dbPath := filepath.Join(dir, "Outlook.sqlite")
	before := hashFile(t, dbPath)
	for _, imm := range []bool{true, false} {
		s, err := Open(dir, Options{Immutable: &imm})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		s.Folders(ctx)
		s.List(ctx, Query{Subject: "x"})
		for id := int64(1); id <= 5; id++ {
			s.Get(ctx, id)
		}
		if _, err := s.db.Exec("CREATE TABLE x(a)"); err == nil {
			t.Errorf("immutable=%v: write succeeded", imm)
		}
		s.Close()
		if imm {
			for _, suf := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(dbPath + suf); err == nil {
					t.Errorf("immutable open created %s", suf)
				}
			}
		}
	}
	if hashFile(t, dbPath) != before {
		t.Fatal("database file changed")
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>t</title><style>x{}</style></head><body><!-- c --><div>A&nbsp;B</div><p>C</p>` +
		`<script>bad()</script><table><tr><td>1</td><td>2</td></tr></table></body></html>`
	if got := htmlToText(in); got != "A B\nC\n12" {
		t.Errorf("got %q", got)
	}
}

func TestCRLFReader(t *testing.T) {
	for in, want := range map[string]string{
		"a\rb\r\rc\r": "a\r\nb\r\n\r\nc\r\n",
		"a\nb\r\nc":   "a\r\nb\r\nc",
		"":            "",
	} {
		b, _ := io.ReadAll(&crlfReader{r: bufio.NewReader(strings.NewReader(in))})
		if string(b) != want {
			t.Errorf("%q: got %q want %q", in, b, want)
		}
	}
}
