package outlookwin

// 自造夹具：按 [MS-PST] 在内存里拼一个最小的 PST，覆盖 ANSI / Unicode、三种编码方式、
// 中文与 emoji、XBLOCK 数据树、子节点树、内容表、收件人表、附件表、HTML 与 RTF 正文。
// 公开样本不进 git，这些夹具是 CI 里唯一跑得到的测试数据。

import (
	"bytes"
	"compress/zlib"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type fxProp struct {
	id, typ uint16
	v       []byte
	hnid    uint32 // 非 0 时值放在这个子节点里（PC 里直接写 HNID）
}

type fxCol struct{ id, typ uint16 }

type fxBBT struct {
	bid, ib  uint64
	cb, cbIn int // 磁盘上的长度、解压后的长度
}

type fxNBT struct {
	nid       uint32
	data, sub uint64
	parent    uint32
}

type fixture struct {
	lay   layout
	crypt byte
	data  []byte
	bbt   []fxBBT
	nbt   []fxNBT
	next  uint64
}

const fxDataStart = 0x1000

func (f *fixture) n() int {
	if f.lay == layoutANSI {
		return 4
	}
	return 8
}

func (f *fixture) putID(b []byte, v uint64) []byte {
	if f.lay == layoutANSI {
		return le.AppendUint32(b, uint32(v))
	}
	return le.AppendUint64(b, v)
}

// block 写一个块，返回 BID。外部块按文件的编码方式编码。
func (f *fixture) block(b []byte, internal bool) uint64 {
	f.next += 4
	bid := f.next
	if internal {
		bid |= 2
	}
	enc := append([]byte(nil), b...)
	if !internal {
		switch f.crypt {
		case cryptPermute:
			encodePermute(enc)
		case cryptCyclic:
			cryptCyclicBuf(enc, uint32(bid))
		}
	}
	if f.lay == layoutUnicode4K && len(enc) > 64 {
		// 4K 布局：块用 zlib 压缩（压缩后更短才压），BBT 里记两种长度。
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		w.Write(enc)
		w.Close()
		if z.Len() < len(enc) {
			enc = z.Bytes()
		}
	}
	ib := uint64(fxDataStart + len(f.data))
	f.data = append(f.data, enc...)
	for len(f.data)%64 != 0 {
		f.data = append(f.data, 0)
	}
	f.bbt = append(f.bbt, fxBBT{bid, ib, len(enc), len(b)})
	return bid
}

func (f *fixture) xblock(parts ...[]byte) uint64 {
	b := []byte{0x01, 0x01, byte(len(parts)), 0}
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	b = le.AppendUint32(b, uint32(total))
	for _, p := range parts {
		b = f.putID(b, f.block(p, false))
	}
	return f.block(b, true)
}

type fxSub struct {
	nid       uint32
	data, sub uint64
}

func (f *fixture) slblock(ents ...fxSub) uint64 {
	sort.Slice(ents, func(i, j int) bool { return ents[i].nid < ents[j].nid })
	b := []byte{0x02, 0x00, byte(len(ents)), 0}
	if f.lay != layoutANSI {
		b = append(b, 0, 0, 0, 0)
	}
	for _, e := range ents {
		b = f.putID(b, uint64(e.nid))
		b = f.putID(b, e.data)
		b = f.putID(b, e.sub)
	}
	return f.block(b, true)
}

// siblock 写一层 SIBLOCK。SIENTRY 的 nid 键读取器不用（它遍历全部子块），这里填 0。
func (f *fixture) siblock(children ...uint64) uint64 {
	b := []byte{0x02, 0x01, byte(len(children)), 0}
	if f.lay != layoutANSI {
		b = append(b, 0, 0, 0, 0)
	}
	for _, c := range children {
		b = f.putID(b, 0)
		b = f.putID(b, c)
	}
	return f.block(b, true)
}

func (f *fixture) node(nid, parent uint32, data, sub uint64) {
	f.nbt = append(f.nbt, fxNBT{nid, data, sub, parent})
}

// str 按文件类型编码字符串：Unicode 用 UTF-16LE，ANSI 用 GB18030（代码页 936）。
func (f *fixture) str(id uint16, s string) fxProp {
	if f.lay == layoutANSI {
		b, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(s))
		if err != nil {
			panic(err)
		}
		return fxProp{id: id, typ: ptString8, v: b}
	}
	return fxProp{id: id, typ: ptUnicode, v: u16(s)}
}

func (f *fixture) strType() uint16 {
	if f.lay == layoutANSI {
		return ptString8
	}
	return ptUnicode
}

func u16(s string) []byte {
	var b []byte
	for _, c := range utf16.Encode([]rune(s)) {
		b = le.AppendUint16(b, c)
	}
	return b
}

func i32(id uint16, v int32) fxProp {
	return fxProp{id: id, typ: ptInt32, v: le.AppendUint32(nil, uint32(v))}
}

func ftime(id uint16, t time.Time) fxProp {
	return fxProp{id: id, typ: ptTime, v: le.AppendUint64(nil, uint64(t.UnixNano()/100)+filetimeEpochDiff)}
}

func bin(id uint16, b []byte) fxProp { return fxProp{id: id, typ: ptBinary, v: b} }

func entryID(nid uint32) []byte {
	b := make([]byte, 20)
	return le.AppendUint32(b, nid)
}

// hnBlock 拼一个单块 HN，allocs[i] 的 HID 是 hid(i)，allocs[0] 是 userRoot。
func hnBlock(sig byte, allocs [][]byte) []byte {
	b := make([]byte, 12)
	b[2], b[3] = hnSig, sig
	le.PutUint32(b[4:], hid(0))
	var offs []uint16
	for _, a := range allocs {
		offs = append(offs, uint16(len(b)))
		b = append(b, a...)
	}
	offs = append(offs, uint16(len(b)))
	if len(b)%2 == 1 {
		b = append(b, 0)
	}
	le.PutUint16(b, uint16(len(b)))
	b = le.AppendUint16(b, uint16(len(allocs)))
	b = le.AppendUint16(b, 0)
	for _, o := range offs {
		b = le.AppendUint16(b, o)
	}
	return b
}

func hid(i int) uint32 { return uint32(i+1) << 5 }

func pcBlock(props []fxProp) []byte {
	sort.Slice(props, func(i, j int) bool { return props[i].id < props[j].id })
	allocs := [][]byte{nil, nil}
	var recs []byte
	for _, p := range props {
		recs = le.AppendUint16(recs, p.id)
		recs = le.AppendUint16(recs, p.typ)
		switch sz := fixedSize(p.typ); {
		case p.hnid != 0:
			recs = le.AppendUint32(recs, p.hnid)
		case sz > 0 && sz <= 4:
			v := make([]byte, 4)
			copy(v, p.v)
			recs = append(recs, v...)
		default:
			allocs = append(allocs, p.v)
			recs = le.AppendUint32(recs, hid(len(allocs)-1))
		}
	}
	allocs[0] = le.AppendUint32([]byte{clientBTH, 2, 6, 0}, hid(1))
	allocs[1] = recs
	return hnBlock(clientPC, allocs)
}

// tcBlock 拼一个 TC，行矩阵放在 HN 里。
func tcBlock(cols []fxCol, rows []map[uint16][]byte) []byte {
	hn, _ := tcBuild(cols, rows, 0)
	return hn
}

// fxRowsNID 是行矩阵放进子节点时用的 NID（LTP 类型）。
const fxRowsNID = 0x3F

// tcBuild 拼一个 TC。perBlock>0 时行矩阵放进子节点 fxRowsNID，每块 perBlock 行，
// 返回各块内容，由调用方写成数据树（大文件夹的内容表就是这样存的）。
func tcBuild(cols []fxCol, rows []map[uint16][]byte, perBlock int) ([]byte, [][]byte) {
	type place struct {
		c      fxCol
		ib, cb int
		bit    int
	}
	size := func(t uint16) int {
		if sz := fixedSize(t); sz > 0 && sz <= 8 {
			return sz
		}
		return 4
	}
	var ps []place
	off := 0
	for _, want := range []int{8, 4, 2, 1} {
		for i, c := range cols {
			if size(c.typ) == want {
				ps = append(ps, place{c: c, ib: off, cb: want, bit: i})
				off += want
			}
		}
	}
	end4, end2, end1 := 0, 0, 0
	for _, p := range ps {
		switch {
		case p.cb >= 4:
			end4 = p.ib + p.cb
		case p.cb == 2:
			end2 = p.ib + p.cb
		default:
			end1 = p.ib + p.cb
		}
	}
	if end2 < end4 {
		end2 = end4
	}
	if end1 < end2 {
		end1 = end2
	}
	rowSize := end1 + (len(cols)+7)/8

	allocs := [][]byte{nil, nil}
	var matrix []byte
	for _, r := range rows {
		row := make([]byte, rowSize)
		for _, p := range ps {
			v, ok := r[p.c.id]
			if !ok {
				continue
			}
			row[end1+p.bit/8] |= 1 << (7 - uint(p.bit%8))
			if sz := fixedSize(p.c.typ); sz > 0 && sz <= 8 {
				copy(row[p.ib:p.ib+p.cb], v)
				continue
			}
			allocs = append(allocs, v)
			le.PutUint32(row[p.ib:], hid(len(allocs)-1))
		}
		matrix = append(matrix, row...)
	}
	info := []byte{clientTC, byte(len(cols))}
	info = le.AppendUint16(info, uint16(end4))
	info = le.AppendUint16(info, uint16(end2))
	info = le.AppendUint16(info, uint16(end1))
	info = le.AppendUint16(info, uint16(rowSize))
	info = le.AppendUint32(info, 0) // hidRowIndex：读取器不用
	switch {
	case len(rows) == 0:
		info = le.AppendUint32(info, 0)
	case perBlock > 0:
		info = le.AppendUint32(info, fxRowsNID)
	default:
		info = le.AppendUint32(info, hid(1))
	}
	info = le.AppendUint32(info, 0)
	for _, p := range ps {
		info = le.AppendUint32(info, uint32(p.c.id)<<16|uint32(p.c.typ))
		info = le.AppendUint16(info, uint16(p.ib))
		info = append(info, byte(p.cb), byte(p.bit))
	}
	var parts [][]byte
	if perBlock > 0 {
		for i := 0; i < len(matrix); i += perBlock * rowSize {
			parts = append(parts, matrix[i:min(i+perBlock*rowSize, len(matrix))])
		}
		matrix = nil
	}
	allocs[0], allocs[1] = info, matrix
	return hnBlock(clientTC, allocs), parts
}

// rowOf 把 fxProp 转成 TC 的一行（只取列里有的）。
func rowOf(props ...fxProp) map[uint16][]byte {
	m := map[uint16][]byte{}
	for _, p := range props {
		m[p.id] = p.v
	}
	return m
}

// bytes 拼出整个文件：文件头、数据区、两棵两层的 B 树（根是中间页，下面一页叶子）。
func (f *fixture) bytes() []byte {
	ps := 512
	if f.lay == layoutUnicode4K {
		ps = 4096
	}
	for len(f.data)%ps != 0 {
		f.data = append(f.data, 0)
	}
	out := make([]byte, fxDataStart)
	out = append(out, f.data...)
	n := f.n()
	page := func(ptype byte, level int, ents [][]byte) uint64 {
		p := make([]byte, ps)
		cb := len(ents[0])
		for i, e := range ents {
			copy(p[i*cb:], e)
		}
		switch f.lay {
		case layoutANSI:
			p[496], p[497], p[498], p[499], p[500], p[501] = byte(len(ents)), byte(496/cb), byte(cb), byte(level), ptype, ptype
		case layoutUnicode4K:
			le.PutUint16(p[4056:], uint16(len(ents)))
			le.PutUint16(p[4058:], uint16(4056/cb))
			p[4060], p[4061], p[4072], p[4073] = byte(cb), byte(level), ptype, ptype
		default:
			p[488], p[489], p[490], p[491], p[496], p[497] = byte(len(ents)), byte(488/cb), byte(cb), byte(level), ptype, ptype
		}
		ib := uint64(len(out))
		out = append(out, p...)
		return ib
	}
	sort.Slice(f.bbt, func(i, j int) bool { return f.bbt[i].bid < f.bbt[j].bid })
	sort.Slice(f.nbt, func(i, j int) bool { return f.nbt[i].nid < f.nbt[j].nid })
	var bents, nents [][]byte
	for _, e := range f.bbt {
		b := f.putID(nil, e.bid)
		b = f.putID(b, e.ib)
		b = le.AppendUint16(b, uint16(e.cb))
		switch f.lay {
		case layoutANSI:
			b = le.AppendUint16(b, 2)
		case layoutUnicode4K:
			b = le.AppendUint16(b, uint16(e.cbIn))
			b = append(b, 2, 0, 0, 0)
		default:
			b = append(b, 2, 0, 0, 0, 0, 0)
		}
		bents = append(bents, b)
	}
	for _, e := range f.nbt {
		b := f.putID(nil, uint64(e.nid))
		b = f.putID(b, e.data)
		b = f.putID(b, e.sub)
		b = le.AppendUint32(b, e.parent)
		if f.lay != layoutANSI {
			b = append(b, 0, 0, 0, 0)
		}
		nents = append(nents, b)
	}
	tree := func(ptype byte, ents [][]byte, keys []uint64) uint64 {
		capacity := 488 / len(ents[0])
		if f.lay == layoutANSI {
			capacity = 496 / len(ents[0])
		}
		capacity = min(capacity, 8) // 故意让叶子分成多页，覆盖中间页的查找
		var inner [][]byte
		for i := 0; i < len(ents); i += capacity {
			leaf := page(ptype, 0, ents[i:min(i+capacity, len(ents))])
			e := f.putID(nil, keys[i])
			e = f.putID(e, 0x1000) // BREF.bid：读取器不用
			e = f.putID(e, leaf)
			inner = append(inner, e)
		}
		return page(ptype, 1, inner)
	}
	var bkeys, nkeys []uint64
	for _, e := range f.bbt {
		bkeys = append(bkeys, e.bid)
	}
	for _, e := range f.nbt {
		nkeys = append(nkeys, uint64(e.nid))
	}
	bbtRoot := tree(ptypeBBT, bents, bkeys)
	nbtRoot := tree(ptypeNBT, nents, nkeys)

	copy(out, "!BDN")
	copy(out[8:], "SM")
	if f.lay == layoutANSI {
		le.PutUint16(out[10:], 14)
		out[461] = f.crypt
		le.PutUint32(out[0xBC:], uint32(nbtRoot))
		le.PutUint32(out[0xC4:], uint32(bbtRoot))
	} else {
		le.PutUint16(out[10:], 23)
		if f.lay == layoutUnicode4K {
			copy(out[8:], "SO")
			le.PutUint16(out[10:], 36)
		}
		out[0x201] = f.crypt
		le.PutUint64(out[0xB8:], uint64(len(out)))
		le.PutUint64(out[0xE0:], nbtRoot)
		le.PutUint64(out[0xF0:], bbtRoot)
	}
	_ = n
	return out
}

// rtfSample 是 [MS-OXRTFCP] 4.1 节的压缩 RTF 示例，解压后是
// {\rtf1\ansi\ansicpg1252\pard hello world}\r\n
var rtfSample = []byte{
	0x2d, 0x00, 0x00, 0x00, 0x2b, 0x00, 0x00, 0x00, 0x4c, 0x5a, 0x46, 0x75, 0xf1, 0xc5, 0xc7, 0xa7,
	0x03, 0x00, 0x0a, 0x00, 0x72, 0x63, 0x70, 0x67, 0x31, 0x32, 0x35, 0x42, 0x32, 0x0a, 0xf3, 0x20,
	0x68, 0x65, 0x6c, 0x09, 0x00, 0x20, 0x62, 0x77, 0x05, 0xb0, 0x6c, 0x64, 0x7d, 0x0a, 0x80, 0x0f,
	0xa0,
}

// 夹具里的固定内容。
var (
	fxT1       = time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	fxT2       = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	fxT3       = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fxLongBody = func() string {
		s := ""
		for i := 0; i < 300; i++ {
			s += "第三季度报告摘要，数据见附件。"
		}
		return s + "😀结束"
	}()
)

const (
	nidTop    = 0x8022
	nidInbox  = 0x8042
	nidSent   = 0x8062
	nidCal    = 0x8082
	nidMsg1   = 0x200024
	nidMsg2   = 0x200044
	nidMsg3   = 0x200064
	nidAppt   = 0x200084
	nidBodySN = 0x041F // LTP 类型的子节点
	nidAttach = 0x8025 // 附件对象子节点
)

// buildFixture 生成一个夹具文件，返回路径。
func buildFixture(t *testing.T, lay layout, crypt byte) string {
	t.Helper()
	f := &fixture{lay: lay, crypt: crypt}
	st := f.strType()
	cpProp := []fxProp{}
	if lay == layoutANSI {
		cpProp = append(cpProp, i32(pidMessageCodepage, 936))
	}
	folder := func(nid, parent uint32, name, class string, count int32) {
		props := []fxProp{f.str(pidDisplayName, name), i32(pidContentCount, count), i32(pidContentUnreadCount, 0)}
		if class != "" {
			props = append(props, f.str(pidContainerClass, class))
		}
		f.node(nid, parent, f.block(pcBlock(props), false), 0)
	}
	hier := func(nid uint32, children ...uint32) {
		var rows []map[uint16][]byte
		for _, c := range children {
			rows = append(rows, rowOf(i32(pidLtpRowID, int32(c))))
		}
		f.node(nid&^0x1F|nidTypeHierarchy, 0, f.block(tcBlock([]fxCol{{pidLtpRowID, ptInt32}}, rows), false), 0)
	}
	contentCols := []fxCol{{pidLtpRowID, ptInt32}, {pidSubject, st}, {pidSentRepresentingName, st}, {pidDeliveryTime, ptTime}, {pidMessageFlags, ptInt32}, {pidDisplayTo, st}}
	contents := func(nid uint32, rows ...map[uint16][]byte) {
		f.node(nid&^0x1F|nidTypeContents, 0, f.block(tcBlock(contentCols, rows), false), 0)
	}
	// 收件箱的内容表把行矩阵放进子节点，每块 2 行、共两块（XBLOCK），走大文件夹的读法。
	contentsSub := func(nid uint32, rows ...map[uint16][]byte) {
		hn, parts := tcBuild(contentCols, rows, 2)
		f.node(nid&^0x1F|nidTypeContents, 0, f.block(hn, false), f.slblock(fxSub{fxRowsNID, f.xblock(parts...), 0}))
	}

	f.node(nidMessageStore, 0, f.block(pcBlock([]fxProp{
		f.str(pidDisplayName, "测试数据文件"),
		bin(pidIpmSubtreeEntryID, entryID(nidTop)),
		bin(pidIpmSentMailEntryID, entryID(nidSent)),
	}), false), 0)
	folder(nidRootFolder, nidRootFolder, "", "", 0)
	hier(nidRootFolder, nidTop)
	folder(nidTop, nidRootFolder, "个人文件夹的顶部", "", 0)
	hier(nidTop, nidInbox, nidSent, nidCal)
	folder(nidInbox, nidTop, "收件箱", "IPF.Note", 3)
	folder(nidSent, nidTop, "已发送邮件", "IPF.Note", 0)
	folder(nidCal, nidTop, "日历", "IPF.Appointment", 1)

	subj1 := "\x01\x04RE: 季度报告 😀"
	m1 := []fxProp{
		f.str(pidSubject, subj1), f.str(pidSentRepresentingName, "张三"), f.str(pidSenderName, "张三"),
		f.str(pidSenderSmtpAddress, "zhangsan@example.com"), ftime(pidDeliveryTime, fxT1), ftime(pidClientSubmitTime, fxT1.Add(-time.Minute)),
		i32(pidMessageFlags, msgFlagRead|msgFlagHasAttach), f.str(pidDisplayTo, "李四"), f.str(pidDisplayCc, "王五"),
		f.str(pidInternetMessageID, "<m1@example.com>"),
	}
	m1 = append(m1, cpProp...)
	body := f.str(pidBody, fxLongBody)
	half := len(body.v) / 2 &^ 1
	bodyBid := f.xblock(body.v[:half], body.v[half:])
	m1 = append(m1, fxProp{id: pidBody, typ: body.typ, hnid: nidBodySN})
	recip := f.block(tcBlock([]fxCol{{pidRecipientType, ptInt32}, {pidDisplayName, st}, {pidSmtpAddress, st}}, []map[uint16][]byte{
		rowOf(i32(pidRecipientType, 1), f.str(pidDisplayName, "李四"), f.str(pidSmtpAddress, "lisi@example.com")),
		rowOf(i32(pidRecipientType, 2), f.str(pidDisplayName, "王五"), f.str(pidSmtpAddress, "wangwu@example.com")),
	}), false)
	attTable := f.block(tcBlock([]fxCol{{pidLtpRowID, ptInt32}, {pidAttachSize, ptInt32}, {pidAttachLongFilename, st}}, []map[uint16][]byte{
		rowOf(i32(pidLtpRowID, nidAttach), i32(pidAttachSize, 1234), f.str(pidAttachLongFilename, "报告.pdf")),
	}), false)
	attObj := f.block(pcBlock([]fxProp{f.str(pidAttachMimeTag, "application/pdf"), i32(pidAttachMethod, 1)}), false)
	// 子节点树用两层：SIBLOCK 下挂两个 SLBLOCK。
	sub1 := f.siblock(
		f.slblock(fxSub{nidBodySN, bodyBid, 0}, fxSub{nidAttachTable, attTable, 0}),
		f.slblock(fxSub{nidRecipientTable, recip, 0}, fxSub{nidAttach, attObj, 0}),
	)
	f.node(nidMsg1, nidInbox, f.block(pcBlock(m1), false), sub1)

	m2 := []fxProp{
		f.str(pidSubject, "你好 HTML"), f.str(pidSentRepresentingName, "Alice"), f.str(pidSenderSmtpAddress, "alice@example.com"),
		ftime(pidDeliveryTime, fxT2), i32(pidMessageFlags, 0), i32(pidInternetCodepage, 65001),
		bin(pidHTML, []byte("<html><head><style>p{}</style></head><body><p>你好，<b>世界</b></p></body></html>")),
	}
	m2 = append(m2, cpProp...)
	f.node(nidMsg2, nidInbox, f.block(pcBlock(m2), false), 0)

	m3 := []fxProp{
		f.str(pidSubject, "RTF only"), f.str(pidSenderName, "Bob"), f.str(pidSenderAddrType, "SMTP"), f.str(pidSenderEmail, "bob@example.com"),
		ftime(pidDeliveryTime, fxT3), i32(pidMessageFlags, msgFlagRead), bin(pidRtfCompressed, rtfSample),
	}
	m3 = append(m3, cpProp...)
	f.node(nidMsg3, nidInbox, f.block(pcBlock(m3), false), 0)
	f.node(nidAppt, nidCal, f.block(pcBlock([]fxProp{f.str(pidSubject, "会议"), ftime(pidDeliveryTime, fxT1)}), false), 0)

	// 内容表里故意不放发件人地址：List 要从邮件 PC 补上。
	contentsSub(nidInbox,
		rowOf(i32(pidLtpRowID, nidMsg1), f.str(pidSubject, subj1), f.str(pidSentRepresentingName, "张三"), ftime(pidDeliveryTime, fxT1), i32(pidMessageFlags, msgFlagRead|msgFlagHasAttach), f.str(pidDisplayTo, "李四")),
		rowOf(i32(pidLtpRowID, nidMsg2), f.str(pidSubject, "你好 HTML"), f.str(pidSentRepresentingName, "Alice"), ftime(pidDeliveryTime, fxT2), i32(pidMessageFlags, 0)),
		rowOf(i32(pidLtpRowID, nidMsg3), f.str(pidSubject, "RTF only"), ftime(pidDeliveryTime, fxT3), i32(pidMessageFlags, msgFlagRead)),
	)
	contents(nidSent)
	contents(nidCal, rowOf(i32(pidLtpRowID, nidAppt), f.str(pidSubject, "会议"), ftime(pidDeliveryTime, fxT1)))

	path := filepath.Join(t.TempDir(), "fixture-"+lay.String()+".pst")
	if err := os.WriteFile(path, f.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
