package hxstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 全部是合成假数据。

func TestMain(m *testing.M) {
	retryDelay = time.Millisecond // fixture 里故意放了坏块，会触发「疑似半写」重读
	os.Exit(m.Run())
}

var (
	t0      = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	fInbox  = uint64(0x1000001)
	fOther  = uint64(0x1000002)
	fNoName = uint64(0x1000003)
)

func baseMails() []fakeMail {
	return []fakeMail{
		{id: 0x2000001, folder: fInbox, bodyObj: 0x3000001, class: "IPM.Note", msgID: "<a1@example.test>",
			subject: "RE: 季度计划 alpha", topic: "季度计划 alpha", fromName: "张三", fromAddr: "zhang@example.test",
			preview: "Hello alpha preview text 你好", recv: t0, sent: t0.Add(-time.Minute), modified: t0,
			to: []string{"me@example.test"}, cc: []string{"boss@example.test"}},
		{id: 0x2000002, folder: fInbox, bodyObj: 0x3000002, class: "IPM.Note", msgID: "<b2@example.test>",
			subject: "Weekly report beta", topic: "Weekly report beta", fromName: "Li Si", fromAddr: "li@example.test",
			preview: "Beta body starts here", recv: t0.Add(time.Hour), sent: t0.Add(time.Hour - time.Minute), modified: t0.Add(time.Hour),
			to: []string{"me@example.test", "team@example.test"}},
		{id: 0x2000003, folder: fOther, class: "IPM.Schedule.Meeting.Request", msgID: "<c3@example.test>",
			subject: "Meeting gamma", topic: "Meeting gamma", fromName: "Wang", fromAddr: "wang@example.test",
			preview: "Only a preview gamma", recv: t0.Add(2 * time.Hour), sent: t0.Add(2 * time.Hour), modified: t0.Add(2 * time.Hour),
			noRecipObj: true},
		{id: 0x2000004, folder: fNoName, bodyObj: 0x3000004, class: "IPM.Note", msgID: "<d4@example.test>",
			subject: "Delta notice", topic: "Delta notice", fromName: "Zhao", fromAddr: "zhao@example.test",
			preview: "Delta preview", recv: t0.Add(-24 * time.Hour), sent: t0.Add(-24 * time.Hour), modified: t0.Add(-24 * time.Hour),
			to: []string{"me@example.test"}},
	}
}

// buildFixture 生成标准 fixture：4 封信、2 个正文块、1 个文件夹导航对象，
// 外加同对象旧版本、同 Message-ID 另一个对象、各种坏块。
func buildFixture(t *testing.T, version byte) []byte {
	t.Helper()
	ms := baseMails()
	f := newFile(version)
	// 同对象 0x2000002 的旧版本（修改时间更早，主题不同），应被新版本替换
	old := ms[1]
	old.subject, old.modified = "Weekly report OLD", t0
	f.block(old.id, mailObject(old))
	// 多封信打包在一个块里
	f.block(ms[0].id, append(append(mailObject(ms[0]), 0, 0, 0, 0), mailObject(ms[2])...))
	f.block(ms[1].id, mailObject(ms[1]))
	// 同 Message-ID 的另一个对象：不带收件人子对象，修改时间相同，靠后的块 -> 保留它并沿用收件人
	dup := ms[3]
	dup.id, dup.noRecipObj = 0x2000099, true
	f.block(ms[3].id, mailObject(ms[3]))
	f.block(dup.id, mailObject(dup))
	// 正文块
	f.block(0x3000001, htmlBody("Hello alpha preview text 你好，正文"))
	f.block(0x3000002, htmlBody("Beta body starts here"))
	f.block(0x3000004, htmlBody("Delta preview and more"))
	// 文件夹名
	f.block(0x777, folderNavObject(map[uint64]string{fInbox: "收件箱", fOther: "项目 X"}, []uint64{fInbox, fOther}))

	// ---- 坏块 ----
	// CRC 不过：拼好后翻转负载一个字节
	off := f.block(0x2000050, mailObject(fakeMail{id: 0x2000050, folder: fInbox, class: "IPM.Note", msgID: "<bad@x>",
		subject: "should not appear", fromAddr: "x@example.test", recv: t0, sent: t0, modified: t0}))
	f.buf[off+blockHdrLen+8+20] ^= 0xFF
	// 解压长度超上限的恶意头
	f.rawBlock(0x2000051, []byte{0x10, 'A'}, maxRawLen+1, codecLZ4)
	// CRC 正确但 LZ4 负载非法（匹配偏移越界）
	f.rawBlock(0x2000052, []byte{0x1F, 'A', 0xFF, 0xFF, 0x00}, 64, codecLZ4)
	// 未知 codec
	f.rawBlock(0x2000053, lz4Literal([]byte("hello")), 5, 9)
	// rawLen 与实际解压长度不符
	f.rawBlock(0x2000054, lz4Literal([]byte("hello")), 6, codecLZ4)
	// 截断（半写）块：头声明的长度超出文件末尾
	tr := f.rawBlock(0x2000055, lz4Literal(bytes.Repeat([]byte("z"), 2000)), 2000, codecLZ4)
	return f.buf[:tr+600]
}

func writeStore(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := writeStore(t, buildFixture(t, knownVersion))
	snap := t.TempDir()
	s, err := Open(dir, Options{SnapshotDir: snap})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if es, _ := os.ReadDir(snap); len(es) != 0 {
		t.Fatalf("快照目录没清理干净: %d 个条目", len(es))
	}
	return s, dir
}

func TestOpenAndStats(t *testing.T) {
	s, _ := openFixture(t)
	st := s.Stats()
	if st.Messages != 4 {
		t.Fatalf("Messages = %d, want 4", st.Messages)
	}
	if st.DupByObject != 1 || st.DupByMsgID != 1 {
		t.Errorf("去重统计 obj=%d msgid=%d, want 1/1", st.DupByObject, st.DupByMsgID)
	}
	if st.BadCRC != 1 || st.BadDecode < 3 || st.BadHeader < 2 {
		t.Errorf("坏块统计 crc=%d decode=%d header=%d", st.BadCRC, st.BadDecode, st.BadHeader)
	}
	if st.Version != "i" {
		t.Errorf("Version = %q", st.Version)
	}
}

func TestFolders(t *testing.T) {
	s, _ := openFixture(t)
	fs, err := s.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("folders = %d, want 3", len(fs))
	}
	byID := map[int64]Folder{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	if f := byID[int64(fInbox)]; f.Name != "收件箱" || f.NameIncomplete || f.MessageCount != 2 {
		t.Errorf("inbox = %+v", f)
	}
	if f := byID[int64(fOther)]; f.Name != "项目 X" || f.MessageCount != 1 {
		t.Errorf("other = %+v", f)
	}
	if f := byID[int64(fNoName)]; !f.NameIncomplete || !strings.HasPrefix(f.Name, "folder-") || !strings.Contains(f.Name, "共 1 封") {
		t.Errorf("无名文件夹应生成标签并标 incomplete: %+v", f)
	}
}

func TestListFiltersAndPaging(t *testing.T) {
	s, _ := openFixture(t)
	ctx := context.Background()
	p, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 4 || len(p.Items) != 4 || p.NextOffset != -1 {
		t.Fatalf("total=%d items=%d next=%d", p.Total, len(p.Items), p.NextOffset)
	}
	for i := 1; i < len(p.Items); i++ {
		if p.Items[i].Received.After(p.Items[i-1].Received) {
			t.Fatal("没有按接收时间倒序")
		}
	}
	if p.Items[1].Subject != "Weekly report beta" {
		t.Errorf("同对象应取最新版本，得到 %q", p.Items[1].Subject)
	}
	cases := []struct {
		q    Query
		want int
	}{
		{Query{From: "ZHANG"}, 1},
		{Query{From: "li@example"}, 1},
		{Query{Subject: "季度"}, 1},
		{Query{Subject: "weekly"}, 1},
		{Query{Subject: "OLD"}, 0},
		{Query{Subject: "should not appear"}, 0},
		{Query{FolderID: int64(fInbox)}, 2},
		{Query{Since: t0}, 3},
		{Query{Since: t0, Until: t0.Add(time.Hour)}, 1},
	}
	for _, c := range cases {
		p, err := s.List(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != c.want {
			t.Errorf("%+v: total=%d want %d", c.q, p.Total, c.want)
		}
	}
	p1, _ := s.List(ctx, Query{Limit: 3})
	p2, _ := s.List(ctx, Query{Limit: 3, Offset: p1.NextOffset})
	if len(p1.Items) != 3 || p1.NextOffset != 3 || len(p2.Items) != 1 || p2.NextOffset != -1 {
		t.Errorf("分页: %d/%d %d/%d", len(p1.Items), p1.NextOffset, len(p2.Items), p2.NextOffset)
	}
}

func TestSummaryFields(t *testing.T) {
	s, _ := openFixture(t)
	p, _ := s.List(context.Background(), Query{Limit: 10})
	by := map[int64]Summary{}
	for _, m := range p.Items {
		by[m.ID] = m
	}
	a := by[0x2000001]
	if a.From != "张三" || a.FromAddress != "zhang@example.test" || len(a.To) != 1 || a.To[0] != "me@example.test" ||
		len(a.Cc) != 1 || !a.Received.Equal(t0) || !a.Sent.Equal(t0.Add(-time.Minute)) || len(a.Incomplete) != 0 {
		t.Errorf("a = %+v", a)
	}
	c := by[0x2000003]
	if !has(c.Incomplete, FieldRecipients) || len(c.To) != 0 {
		t.Errorf("没有收件人子对象应标 recipients incomplete: %+v", c.Incomplete)
	}
	// 同 Message-ID：保留靠后块里的 0x2000099，收件人从另一个对象沿用
	d, ok := by[0x2000099]
	if !ok || len(d.To) != 1 || has(d.Incomplete, FieldRecipients) {
		t.Errorf("Message-ID 去重/收件人沿用不对: ok=%v %+v", ok, d)
	}
	if !has(d.Incomplete, FieldFolderName) {
		t.Errorf("无名文件夹的邮件应标 folder_name: %v", d.Incomplete)
	}
}

func TestGet(t *testing.T) {
	s, _ := openFixture(t)
	ctx := context.Background()
	m, err := s.Get(ctx, 0x2000001)
	if err != nil {
		t.Fatal(err)
	}
	if m.BodySource != BodyHTML || !m.BodyComplete || !m.BodyMatchesTip || m.MessageID != "<a1@example.test>" ||
		m.FolderName != "收件箱" || m.Class != "IPM.Note" {
		t.Fatalf("m = %+v", m)
	}
	if !strings.Contains(m.Body, "正文") || !strings.Contains(m.Body, "第二行 & more") || strings.Contains(m.Body, "<") || strings.Contains(m.Body, "color") {
		t.Errorf("body = %q", m.Body)
	}
	// 没有正文对象 -> 预览
	g, err := s.Get(ctx, 0x2000003)
	if err != nil {
		t.Fatal(err)
	}
	if g.BodySource != BodyPreview || g.BodyComplete || !has(g.Incomplete, FieldBody) || g.Body != "Only a preview gamma" {
		t.Errorf("g = %+v", g)
	}
	// 被 Message-ID 去重掉的旧 id 仍能取到保留的版本，正文从备选对象找到
	d, err := s.Get(ctx, 0x2000004)
	if err != nil || d.ID != 0x2000099 || d.BodySource != BodyHTML {
		t.Errorf("alias get: %v %+v", err, d)
	}
	if _, err := s.Get(ctx, 0x2000050); !errors.Is(err, ErrNotFound) {
		t.Errorf("坏块里的信不该存在: %v", err)
	}
	if _, err := s.Get(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestBodyTruncate(t *testing.T) {
	dir := writeStore(t, buildFixture(t, knownVersion))
	s, err := Open(dir, Options{MaxBodyBytes: 10, NoSnapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(context.Background(), 0x2000001)
	if err != nil {
		t.Fatal(err)
	}
	if !m.BodyTruncated || m.BodyComplete || len(m.Body) > 10 || !strings.HasPrefix("Hello alpha", m.Body[:5]) {
		t.Errorf("truncate: %+v", m)
	}
}

func TestReadOnly(t *testing.T) {
	data := buildFixture(t, knownVersion)
	dir := writeStore(t, data)
	p := filepath.Join(dir, FileName)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Options{SnapshotDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.Folders(ctx)
	s.List(ctx, Query{})
	s.Get(ctx, 0x2000001)
	s.Close()
	got, _ := os.ReadFile(p)
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Error("文件内容被改了")
	}
	fi, _ := os.Stat(p)
	if !fi.ModTime().Equal(old) {
		t.Error("mtime 变了")
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("数据目录多出了文件: %d", len(es))
	}
}

func TestUnsupported(t *testing.T) {
	// 版本字节不符
	if _, err := Open(writeStore(t, buildFixture(t, 'h')), Options{NoSnapshot: true}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("version: %v", err)
	}
	// 不是 Nostromo
	if _, err := Open(writeStore(t, bytes.Repeat([]byte{0}, 8192)), Options{NoSnapshot: true}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("magic: %v", err)
	}
	// 有块但没有邮件
	f := newFile(knownVersion)
	f.block(1, []byte("nothing here at all"))
	if _, err := Open(writeStore(t, f.bytes()), Options{NoSnapshot: true}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("no mails: %v", err)
	}
	// 槽位错乱（模拟 Outlook 升级后字段漂移）：主题、发件人都抽不到
	f = newFile(knownVersion)
	for i := 0; i < 5; i++ {
		f.block(uint64(0x2000100+i), mailObject(fakeMail{id: uint64(0x2000100 + i), folder: fInbox, class: "IPM.Note",
			msgID: "<z@example.test>" + string(rune('a'+i)), recv: t0, sent: t0, modified: t0}))
	}
	if _, err := Open(writeStore(t, f.bytes()), Options{NoSnapshot: true}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("self-check: %v", err)
	}
	// 文件不存在
	if _, err := Open(filepath.Join(t.TempDir(), "nope"), Options{}); err == nil {
		t.Error("不存在的目录应报错")
	}
}

func TestStaleBodyBlock(t *testing.T) {
	data := buildFixture(t, knownVersion)
	dir := writeStore(t, data)
	s, err := Open(dir, Options{NoSnapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 Outlook 改写：把正文块的 CRC 弄坏（半写），Get 不能报错也不能 panic，退到预览
	bad := append([]byte(nil), data...)
	for _, o := range s.idx.blocks[0x3000001] {
		bad[o+40] ^= 0xFF
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(context.Background(), 0x2000001)
	if err != nil {
		t.Fatal(err)
	}
	if m.BodySource != BodyPreview || m.BodyComplete {
		t.Errorf("半写正文块应退到预览: %+v", m.BodySource)
	}
}

func TestRefresh(t *testing.T) {
	dir := writeStore(t, buildFixture(t, knownVersion))
	s, err := Open(dir, Options{NoSnapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	ms := baseMails()
	f := newFile(knownVersion)
	for _, m := range ms[:2] {
		f.block(m.id, mailObject(m))
	}
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, f.bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(p, later, later)
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	if n := s.Stats().Messages; n != 2 {
		t.Errorf("Refresh 后 %d 封, want 2", n)
	}
}

// 随机字节 + 随机放置的块魔数：扫描和抽取都不能 panic。
func TestFuzzNoPanic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 200; round++ {
		n := 512 * (1 + rng.Intn(64))
		b := make([]byte, n)
		rng.Read(b)
		copy(b, fileMagic)
		b[len(fileMagic)] = knownVersion
		for o := 512; o+blockHdrLen <= n; o += 512 {
			if rng.Intn(3) == 0 {
				copy(b[o+8:], blockMagic)
				binary.LittleEndian.PutUint32(b[o+16:], uint32(rng.Intn(80)))
				binary.LittleEndian.PutUint32(b[o+20:], uint32(rng.Intn(4096)))
				binary.LittleEndian.PutUint32(b[o+24:], uint32(rng.Intn(1<<16)))
				binary.LittleEndian.PutUint32(b[o+28:], codecLZ4)
			}
		}
		scanBlocks(bytes.NewReader(b), int64(n), func(off int64, id uint64, raw []byte) error {
			extractMails(raw, off)
			return nil
		})
		// 抽取层直接喂随机数据（含 IPM. 锚点和对象头）
		r := make([]byte, 4096)
		rng.Read(r)
		copy(r[rng.Intn(4000):], clsPat)
		copy(r[rng.Intn(4000):], objHdrPat)
		extractMails(r, 0)
		recipObjects(r, binary.LittleEndian.Uint64(r[8:]))
		scanFolderNames(r, func(uint64, folderName) {})
		u16Emails(r)
	}
	// 截断的真实 fixture：每个截断点都能打开或返回错误，不 panic
	data := buildFixture(t, knownVersion)
	for cut := 0; cut < len(data); cut += 997 {
		dir := writeStore(t, data[:cut])
		if s, err := Open(dir, Options{NoSnapshot: true}); err == nil {
			s.List(context.Background(), Query{})
		}
	}
}

func has(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func TestConcurrent(t *testing.T) {
	dir := writeStore(t, buildFixture(t, knownVersion))
	s, err := Open(dir, Options{NoSnapshot: true, RefreshInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 20; i++ {
				s.List(ctx, Query{Subject: "a"})
				s.Get(ctx, 0x2000001)
				s.Folders(ctx)
				if g == 0 {
					os.Chtimes(filepath.Join(dir, FileName), time.Now(), time.Now().Add(time.Duration(i)*time.Second))
				}
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}
