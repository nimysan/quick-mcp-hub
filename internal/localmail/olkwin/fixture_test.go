package olkwin

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
	"unicode/utf16"
)

// 这里用测试代码按格式拼出一个最小的「新版 Outlook IndexedDB」：
// LevelDB（CURRENT、MANIFEST、一个 Snappy 压缩的 .ldb、一个 .log），IndexedDB 元数据键，
// Blink/V8 序列化的值（原样、Snappy 压缩、外部 blob 三种包装）。字段形状照真机（olk 1.2026.1103.200）上看到的。

// ---------- V8 / Blink 写入 ----------

type kv struct {
	k string
	v any
}

// tObj 是 JS 对象；同一个指针写第二次时输出对象引用 '^'。
type tObj struct{ kv []kv }

func o(pairs ...any) *tObj {
	t := &tObj{}
	for i := 0; i+1 < len(pairs); i += 2 {
		t.kv = append(t.kv, kv{pairs[i].(string), pairs[i+1]})
	}
	return t
}

type undef struct{}

type v8w struct {
	b   []byte
	ids map[any]int
	n   int
}

func (w *v8w) varint(v uint64) { w.b = binary.AppendUvarint(w.b, v) }

func (w *v8w) str(s string) {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
		}
	}
	if ascii {
		w.b = append(w.b, '"')
		w.varint(uint64(len(s)))
		w.b = append(w.b, s...)
		return
	}
	u := utf16.Encode([]rune(s))
	// 双字节串前面按 V8 的做法可能有一个对齐填充字节
	if (len(w.b)+1+len(binary.AppendUvarint(nil, uint64(len(u)*2))))%2 == 1 {
		w.b = append(w.b, 0)
	}
	w.b = append(w.b, 'c')
	w.varint(uint64(len(u) * 2))
	for _, c := range u {
		w.b = binary.LittleEndian.AppendUint16(w.b, c)
	}
}

func (w *v8w) val(v any) {
	switch x := v.(type) {
	case nil:
		w.b = append(w.b, '0')
	case undef:
		w.b = append(w.b, '_')
	case bool:
		if x {
			w.b = append(w.b, 'T')
		} else {
			w.b = append(w.b, 'F')
		}
	case int:
		w.b = append(w.b, 'I')
		w.varint(uint64(uint32(int32(x)<<1) ^ uint32(int32(x)>>31)))
	case float64:
		w.b = append(w.b, 'N')
		w.b = binary.LittleEndian.AppendUint64(w.b, math.Float64bits(x))
	case string:
		w.str(x)
	case time.Time:
		w.b = append(w.b, 'D')
		w.b = binary.LittleEndian.AppendUint64(w.b, math.Float64bits(float64(x.UnixMilli())))
		w.n++
	case *tObj:
		if x == nil {
			w.b = append(w.b, '_')
			return
		}
		if id, ok := w.ids[x]; ok {
			w.b = append(w.b, '^')
			w.varint(uint64(id))
			return
		}
		w.ids[x] = w.n
		w.n++
		w.b = append(w.b, 'o')
		for _, p := range x.kv {
			w.str(p.k)
			w.val(p.v)
		}
		w.b = append(w.b, '{')
		w.varint(uint64(len(x.kv)))
	case []any:
		w.n++
		w.b = append(w.b, 'A')
		w.varint(uint64(len(x)))
		for _, e := range x {
			w.val(e)
		}
		w.b = append(w.b, '$')
		w.varint(0)
		w.varint(uint64(len(x)))
	case []byte:
		w.n += 2
		w.b = append(w.b, 'B')
		w.varint(uint64(len(x)))
		w.b = append(w.b, x...)
		w.b = append(w.b, 'V', 'B', 0)
		w.varint(uint64(len(x)))
		w.varint(0)
	default:
		panic(fmt.Sprintf("fixture: %T", v))
	}
}

// ssv 写出 Blink SSV：Blink v21 头 + 尾部偏移占位 + V8 v16 头 + 值。
func ssv(v any) []byte {
	w := &v8w{ids: map[any]int{}}
	w.b = append(w.b, 0xFF, 21, 0xFE)
	w.b = append(w.b, make([]byte, 12)...)
	w.b = append(w.b, 0xFF, 16)
	w.val(v)
	return w.b
}

// snappyLiteral 只用字面量元素做 Snappy 编码（解码器的回引路径另有单测）。
func snappyLiteral(p []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(len(p)))
	for len(p) > 0 {
		n := min(len(p), 65536)
		if n <= 60 {
			out = append(out, byte(n-1)<<2)
		} else {
			out = append(out, 61<<2, byte(n-1), byte((n-1)>>8))
		}
		out = append(out, p[:n]...)
		p = p[n:]
	}
	return out
}

// ---------- IndexedDB 键 ----------

func sWithLen(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := binary.AppendUvarint(nil, uint64(len(u)))
	for _, c := range u {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return b
}

func utf16beBytes(s string) []byte {
	return sWithLen(s)[len(binary.AppendUvarint(nil, uint64(len(utf16.Encode([]rune(s)))))):]
}

func dbNameKey(name string) []byte {
	k := append(encodePrefix(keyPrefix{}), 201)
	k = append(k, sWithLen("https_outlook.office.com_0@1")...)
	return append(k, sWithLen(name)...)
}

func storeNameKey(db, store int64) []byte {
	k := append(encodePrefix(keyPrefix{db: db}), 50)
	k = binary.AppendUvarint(k, uint64(store))
	return append(k, 0)
}

func dataKey(db, store, index int64, pk string) []byte {
	k := encodePrefix(keyPrefix{db: db, store: store, index: index})
	k = append(k, 1)
	return append(k, sWithLen(pk)...)
}

func recordVal(wrapped []byte) []byte { return append([]byte{1}, wrapped...) } // varint 版本 1

// ---------- LevelDB 写入 ----------

type ikv struct {
	k   []byte // 用户键
	seq uint64
	del bool
	v   []byte
}

func maskCRC(c uint32) uint32 { return (c>>15 | c<<17) + 0xa282ead8 }

func blockBytes(ents []ikv, internal bool) []byte {
	var b, last []byte
	var restarts []uint32
	for i, e := range ents {
		k := e.k
		if internal {
			t := e.seq << 8
			if !e.del {
				t |= 1
			}
			k = binary.LittleEndian.AppendUint64(append([]byte(nil), e.k...), t)
		}
		shared := 0
		if i%16 == 0 {
			restarts = append(restarts, uint32(len(b)))
		} else {
			for shared < len(k) && shared < len(last) && k[shared] == last[shared] {
				shared++
			}
		}
		b = binary.AppendUvarint(b, uint64(shared))
		b = binary.AppendUvarint(b, uint64(len(k)-shared))
		b = binary.AppendUvarint(b, uint64(len(e.v)))
		b = append(b, k[shared:]...)
		b = append(b, e.v...)
		last = k
	}
	if len(restarts) == 0 {
		restarts = []uint32{0}
	}
	for _, r := range restarts {
		b = binary.LittleEndian.AppendUint32(b, r)
	}
	return binary.LittleEndian.AppendUint32(b, uint32(len(restarts)))
}

func appendBlock(f []byte, raw []byte, snappy bool) ([]byte, uint64, uint64) {
	data, typ := raw, byte(0)
	if snappy {
		data, typ = snappyLiteral(raw), 1
	}
	off := uint64(len(f))
	f = append(f, data...)
	f = append(f, typ)
	c := crc32.Update(crc32.Checksum(data, crcTable), crcTable, []byte{typ})
	f = binary.LittleEndian.AppendUint32(f, maskCRC(c))
	return f, off, uint64(len(data))
}

// tableFile 写一个表文件：每 perBlock 条一个数据块。
func tableFile(ents []ikv, perBlock int) []byte {
	sort.Slice(ents, func(i, j int) bool { return string(ents[i].k) < string(ents[j].k) })
	var f []byte
	var index []ikv
	for i := 0; i < len(ents); i += perBlock {
		chunk := ents[i:min(i+perBlock, len(ents))]
		var off, size uint64
		f, off, size = appendBlock(f, blockBytes(chunk, true), i%2 == 0)
		last := chunk[len(chunk)-1]
		h := binary.AppendUvarint(binary.AppendUvarint(nil, off), size)
		index = append(index, ikv{k: binary.LittleEndian.AppendUint64(append([]byte(nil), last.k...), last.seq<<8|1), v: h})
	}
	var mo, ms, io, is uint64
	f, mo, ms = appendBlock(f, blockBytes(nil, false), false)
	f, io, is = appendBlock(f, blockBytes(index, false), false)
	foot := binary.AppendUvarint(nil, mo)
	foot = binary.AppendUvarint(foot, ms)
	foot = binary.AppendUvarint(foot, io)
	foot = binary.AppendUvarint(foot, is)
	foot = append(foot, make([]byte, 40-len(foot))...)
	foot = binary.LittleEndian.AppendUint64(foot, 0xdb4775248b80fb57)
	return append(f, foot...)
}

// logFile 按 32 KB 块把记录切成 FULL/FIRST/MIDDLE/LAST 片段。
func logFile(recs [][]byte) []byte {
	const block = 32768
	var f []byte
	for _, r := range recs {
		first := true
		for {
			left := block - len(f)%block
			if left < 7 {
				f = append(f, make([]byte, left)...)
				continue
			}
			n := min(len(r), left-7)
			typ := byte(1)
			switch {
			case first && n == len(r):
				typ = 1
			case first:
				typ = 2
			case n == len(r):
				typ = 4
			default:
				typ = 3
			}
			h := binary.LittleEndian.AppendUint32(nil, maskCRC(crc32.Update(crc32.Checksum([]byte{typ}, crcTable), crcTable, r[:n])))
			h = binary.LittleEndian.AppendUint16(h, uint16(n))
			f = append(append(f, h...), typ)
			f = append(f, r[:n]...)
			r = r[n:]
			first = false
			if len(r) == 0 {
				break
			}
		}
	}
	return f
}

func writeBatch(seq uint64, ents []ikv) []byte {
	b := binary.LittleEndian.AppendUint64(nil, seq)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ents)))
	for _, e := range ents {
		if e.del {
			b = append(b, 0)
			b = binary.AppendUvarint(b, uint64(len(e.k)))
			b = append(b, e.k...)
			continue
		}
		b = append(b, 1)
		b = binary.AppendUvarint(b, uint64(len(e.k)))
		b = append(b, e.k...)
		b = binary.AppendUvarint(b, uint64(len(e.v)))
		b = append(b, e.v...)
	}
	return b
}

func manifestEdit(comparator string, logNum uint64, tables []uint64) []byte {
	var b []byte
	b = binary.AppendUvarint(b, 1)
	b = binary.AppendUvarint(b, uint64(len(comparator)))
	b = append(b, comparator...)
	b = binary.AppendUvarint(b, 2)
	b = binary.AppendUvarint(b, logNum)
	b = binary.AppendUvarint(b, 3)
	b = binary.AppendUvarint(b, 100)
	b = binary.AppendUvarint(b, 4)
	b = binary.AppendUvarint(b, 1000)
	for _, t := range tables {
		b = binary.AppendUvarint(b, 7)
		b = binary.AppendUvarint(b, 0) // level
		b = binary.AppendUvarint(b, t)
		b = binary.AppendUvarint(b, 1)
		b = binary.AppendUvarint(b, 1)
		b = append(b, 'a')
		b = binary.AppendUvarint(b, 1)
		b = append(b, 'z')
	}
	return b
}

// ---------- 夹具内容 ----------

const (
	acctDB    = 3
	stFolders = 1
	stMsgs    = 9
	stBodies  = 11
)

var (
	fxRecv1 = time.Date(2026, 11, 21, 21, 54, 7, 0, time.UTC)
	fxRecv2 = time.Date(2026, 11, 21, 21, 53, 1, 0, time.UTC)
	fxRecv3 = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
)

func folderRec(id, parent, name, special, class string, total int) *tObj {
	return o("id", id, "__typename", "MailFolder", "parentFolderId", parent, "displayName", name,
		"distinguishedFolderType", special, "FolderClass", class, "UnreadCount", 0.0, "totalMessageCount", float64(total),
		"mailboxInfo", o("mailboxSmtpAddress", "user@example.com", "type", "UserMailbox"))
}

func mbx(name, addr string) *tObj {
	return o("Mailbox", o("Name", name, "EmailAddress", addr, "RoutingType", "SMTP"))
}

func itemRow(id, folder, subject string, from *tObj, recv time.Time, attach, read bool, preview string) *tObj {
	t := o("__typename", "ItemRow", "ItemId", o("Id", id, "ChangeKey", "ck"), "Categories", undef{},
		"DateTimeSent", recv.Add(-time.Minute).Format(time.RFC3339), "DisplayTo", "张三; Li Si",
		"HasAttachments", attach, "ItemClass", "IPM.Note", "ParentFolderId", o("__typename", "FolderId", "Id", folder),
		"Preview", preview, "Subject", subject, "id", id, "From", from, "IsRead", read,
		"MetaData", o("sortTime", recv.Format(time.RFC3339), "isDirty", 0.0))
	if !recv.IsZero() {
		t.kv = append(t.kv, kv{"DateTimeReceived", recv.Format(time.RFC3339)})
	}
	return t
}

func bodyRec(id, folder, subject string, from *tObj, recv time.Time, html string, atts any) *tObj {
	return o("__typename", "Message", "Sender", from, "ToRecipients", []any{o("Name", "张三", "EmailAddress", "zhang@example.com")},
		"CcRecipients", []any{o("Name", "王五", "EmailAddress", "wang@example.com")},
		"ItemId", o("Id", id), "ParentFolderId", o("Id", folder), "Subject", subject, "Attachments", atts, "From", from,
		"DateTimeReceived", recv.Format(time.RFC3339), "DateTimeSent", recv.Format(time.RFC3339), "InternetMessageId", "<"+id+"@example.com>",
		"IsRead", true, "HasAttachments", atts != nil,
		"NormalizedBody", o("__typename", "BodyContent", "BodyType", "HTML", "Value", html, "IsTruncated", false),
		"MetaData", o("keywords", []any{"a", "b"}, "created", recv))
}

type fixtureOpt struct {
	comparator string
	v8Version  byte
}

// writeFixture 在 dir 下建 IndexedDB 目录结构，返回 IndexedDB 根目录。
func writeFixture(t testing.TB, dir string, opt fixtureOpt) string {
	t.Helper()
	if opt.comparator == "" {
		opt.comparator = "idb_cmp1"
	}
	root := filepath.Join(dir, "IndexedDB")
	ldb := filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb")
	blob := filepath.Join(root, "https_outlook.office.com_0.indexeddb.blob")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(ldb, 0o755))
	enc := func(v any) []byte {
		b := ssv(v)
		if opt.v8Version != 0 {
			b[16] = opt.v8Version
		}
		return b
	}
	var tbl []ikv
	seq := uint64(1)
	put := func(k, v []byte) {
		tbl = append(tbl, ikv{k: k, seq: seq, v: v})
		seq++
	}
	// 元数据
	put(dbNameKey("owa-offline-data"), []byte{2})
	put(dbNameKey("owa-offline-data-bcec907f-0000-0000-0000-000000000001"), []byte{acctDB})
	for id, n := range map[int64]string{stFolders: "folders", 2: "settings", stMsgs: "messages", stBodies: "messageBodies"} {
		put(storeNameKey(acctDB, id), utf16beBytes(n))
	}
	put(storeNameKey(2, 1), utf16beBytes("folders")) // 非账户库，忽略
	// 文件夹
	folders := []*tObj{
		folderRec("F-ROOT", "F-NONE", "Top of Information Store", "msgfolderroot", "IPF.Note", 0),
		folderRec("F-INBOX", "F-ROOT", "Inbox", "inbox", "IPF.Note", 3035),
		folderRec("F-SENT", "F-ROOT", "Sent Items", "sentitems", "IPF.Note", 10),
		folderRec("F-SUB", "F-INBOX", "项目 2026", "", "IPF.Note", 1),
		folderRec("F-NOTES", "F-ROOT", "Notes", "notes", "IPF.StickyNote", 1),
	}
	for _, f := range folders {
		put(dataKey(acctDB, stFolders, 1, f.kv[0].v.(string)), recordVal(enc(f)))
	}
	alice := mbx("Yiming Liang", "yiming@example.com")
	bob := mbx("Bob", "bob@example.com")
	// M1：中文主题带附件，列表行和完整邮件都在表里；完整邮件用 Snappy 包装
	put(dataKey(acctDB, stMsgs, 1, "M1"), recordVal(enc(itemRow("M1", "F-INBOX", "mcp-hub-test 111 :最好有中文主题、带附件", alice, fxRecv1, true, false, "你好 😀"))))
	att := []any{o("__type", "FileAttachment:#Exchange", "AttachmentId", o("Id", "A1"), "Name", "报告.pdf", "ContentType", "application/pdf", "Size", 1339765.0, "IsInline", false)}
	b1 := enc(bodyRec("M1", "F-INBOX", "mcp-hub-test 111 :最好有中文主题、带附件", alice, fxRecv1, "<html><head><style>p{}</style></head><body><p>你好&nbsp;世界</p><div>第二行</div></body></html>", att))
	put(dataKey(acctDB, stBodies, 1, "M1"), recordVal(append([]byte{0xFF, 0x11, 0x02}, snappyLiteral(b1)...)))
	// M2：HTML，无附件；表里是旧版本（已读=false），日志里有新版本（已读=true）；完整邮件在外部 blob 里
	put(dataKey(acctDB, stMsgs, 1, "M2"), recordVal(enc(itemRow("M2", "F-INBOX", "旧主题", alice, fxRecv2, false, false, ""))))
	put(dataKey(acctDB, stBodies, 1, "M2"), recordVal([]byte{0xFF, 0x11, 0x01, 0x80, 0x01, 0x00}))
	bi := []byte{0}
	bi = binary.AppendUvarint(bi, 0x105) // blob 号 0x105 → 3/01/105
	bi = append(bi, sWithLen("")...)
	bi = binary.AppendUvarint(bi, 200)
	put(dataKey(acctDB, stBodies, 3, "M2"), bi)
	must(os.MkdirAll(filepath.Join(blob, "3", "01"), 0o755))
	big := ""
	for i := 0; i < 3000; i++ {
		big += "长正文"
	}
	must(os.WriteFile(filepath.Join(blob, "3", "01", "105"), enc(bodyRec("M2", "F-INBOX", "mcp-hub-test 222:HTML", alice, fxRecv2, "<p>"+big+"</p>", nil)), 0o644))
	// M3：子文件夹里，没有完整邮件；发件人缺失、接收时间缺失
	put(dataKey(acctDB, stMsgs, 1, "M3"), recordVal(enc(itemRow("M3", "F-SUB", "no sender", nil, time.Time{}, false, true, "only preview"))))
	// M4：只有完整邮件没有列表行（已发送）
	put(dataKey(acctDB, stBodies, 1, "M4"), recordVal(enc(bodyRec("M4", "F-SENT", "Re: 周报", bob, fxRecv3, "<p>hi</p>", nil))))
	// M5：日志里删除
	put(dataKey(acctDB, stMsgs, 1, "M5"), recordVal(enc(itemRow("M5", "F-INBOX", "deleted later", bob, fxRecv3, false, false, ""))))
	// 解不开的记录：未知 V8 标签，计入 Skipped
	put(dataKey(acctDB, stMsgs, 1, "M6"), recordVal([]byte{0xFF, 21, 0xFF, 16, 'q'}))
	// 一条无关的索引条目（index 30），合并时应丢掉
	put(append(encodePrefix(keyPrefix{db: acctDB, store: stMsgs, index: 30}), 1, 2, 3), []byte{1})

	must(os.WriteFile(filepath.Join(ldb, "000005.ldb"), tableFile(tbl, 4), 0o644))
	// 日志：M2 新版本 + M5 删除；再加一条超过 32 KB 的记录测试分片
	m2new := enc(itemRow("M2", "F-INBOX", "mcp-hub-test 222:最好有中文主题、带附件的、HTML 格式的", alice, fxRecv2, false, true, "预览"))
	pad := make([]byte, 70000)
	logRecs := [][]byte{
		writeBatch(seq, []ikv{{k: dataKey(acctDB, stMsgs, 1, "M2"), v: recordVal(m2new)}, {k: dataKey(acctDB, stMsgs, 1, "M5"), del: true}}),
		writeBatch(seq+2, []ikv{{k: dataKey(acctDB, 2, 1, "big"), v: pad}}),
	}
	lf := logFile(logRecs)
	lf = append(lf, 0x12, 0x34, 0x56) // 末尾半条记录（正在写），应忽略
	must(os.WriteFile(filepath.Join(ldb, "000007.log"), lf, 0o644))
	must(os.WriteFile(filepath.Join(ldb, "000003.log"), logFile([][]byte{writeBatch(1, []ikv{{k: dataKey(acctDB, stMsgs, 1, "OLD"), v: []byte{1}}})}), 0o644)) // 旧日志，日志号之前，不读
	must(os.WriteFile(filepath.Join(ldb, "MANIFEST-000002"), logFile([][]byte{manifestEdit(opt.comparator, 7, []uint64{5})}), 0o644))
	must(os.WriteFile(filepath.Join(ldb, "CURRENT"), []byte("MANIFEST-000002\n"), 0o644))
	must(os.WriteFile(filepath.Join(ldb, "LOCK"), nil, 0o644))
	return root
}
