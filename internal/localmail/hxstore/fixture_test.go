package hxstore

// 合成 HxStore.hxd fixture。全部是假数据，按 block.go / record.go 里记录的格式手工拼出来。

import (
	"encoding/binary"
	"hash/crc32"
	"time"
	"unicode/utf16"
)

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

// lz4Literal 把 src 编成只有一个字面量序列的合法 LZ4 block。
func lz4Literal(src []byte) []byte {
	l := len(src)
	var out []byte
	if l < 15 {
		out = append(out, byte(l<<4))
	} else {
		out = append(out, 0xF0)
		r := l - 15
		for r >= 255 {
			out = append(out, 255)
			r -= 255
		}
		out = append(out, byte(r))
	}
	return append(out, src...)
}

func objHdr(id uint64, typ uint16, size int) []byte {
	h := make([]byte, 18)
	copy(h, []byte{0x06, 0x00, 0x70, 0x05})
	binary.LittleEndian.PutUint64(h[4:], id)
	binary.LittleEndian.PutUint16(h[12:], typ)
	binary.LittleEndian.PutUint32(h[14:], uint32(size))
	return h
}

func toTicks(t time.Time) uint64 {
	const unixEpochTicks = 621355968000000000 // .NET DateTime(1970,1,1).Ticks
	return uint64(unixEpochTicks + t.UnixNano()/100)
}

type fakeMail struct {
	id, folder, bodyObj          uint64
	class, msgID, subject, topic string
	fromName, fromAddr, preview  string
	recv, sent, modified         time.Time
	to, cc                       []string
	noRecipObj                   bool
}

// mailObject 拼一个邮件对象（含收件人子对象）。
func mailObject(m fakeMail) []byte {
	const slotsAt = 1300 // j 相对对象头
	const heapAt = slotsAt + 256
	obj := make([]byte, heapAt)
	copy(obj, objHdr(m.id, typeMail, 0))
	var heap []byte
	putStr := func(slot int, s string) {
		if s == "" {
			return
		}
		b := append(utf16le(s), 0, 0)
		j := slotsAt + slot
		binary.LittleEndian.PutUint32(obj[j:], uint32(len(heap)))
		binary.LittleEndian.PutUint32(obj[j+4:], uint32(len(b))|0x80000000)
		heap = append(heap, b...)
	}
	put64 := func(slot int, v uint64) { binary.LittleEndian.PutUint64(obj[slotsAt+slot:], v) }
	putStr(0, m.class)
	putStr(slotMsgID, m.msgID)
	putStr(slotTopic, m.topic)
	putStr(slotPreview, m.preview)
	putStr(slotFromName, m.fromName)
	putStr(slotFromAddr, m.fromAddr)
	putStr(slotSubject, m.subject)
	if !m.recv.IsZero() {
		put64(slotRecv, toTicks(m.recv))
	}
	if !m.sent.IsZero() {
		put64(slotSent, toTicks(m.sent))
	}
	if !m.modified.IsZero() {
		put64(slotModified, toTicks(m.modified))
	}
	put64(slotFolder, m.folder)
	put64(slotBodyObj, m.bodyObj)
	obj = append(obj, heap...)
	binary.LittleEndian.PutUint32(obj[14:], uint32(len(obj)))
	if !m.noRecipObj {
		obj = append(obj, recipObj(m.id, typeTo, m.to)...)
		obj = append(obj, recipObj(m.id, typeCc, m.cc)...)
	}
	return obj
}

func recipObj(id uint64, typ uint16, addrs []string) []byte {
	var body []byte
	for _, a := range addrs {
		body = append(body, 0, 0, 0, 0) // 分隔
		body = append(body, utf16le(a)...)
		body = append(body, 0, 0)
	}
	body = append(body, 0, 0, 0, 0)
	return append(objHdr(id, typ, 18+len(body)-2), body...)
}

// folderNavObject 拼一个 0x1E4 导航对象：每个条目 fid、+20 再一次 fid、+46 处是名称。
func folderNavObject(names map[uint64]string, order []uint64) []byte {
	obj := objHdr(0x777, typeFolderNav, 0)
	for _, fid := range order {
		e := make([]byte, navNameOff)
		binary.LittleEndian.PutUint64(e[0:], fid)
		binary.LittleEndian.PutUint64(e[pairGap:], fid)
		e = append(e, utf16le(names[fid])...)
		e = append(e, 0, 0, 0xAA, 0xBB, 0xCC, 0xDD)
		obj = append(obj, e...)
	}
	binary.LittleEndian.PutUint32(obj[14:], uint32(len(obj)))
	return obj
}

// fileBuilder 组装整个文件。
type fileBuilder struct {
	buf []byte
}

func newFile(version byte) *fileBuilder {
	b := make([]byte, 4096)
	copy(b, fileMagic)
	b[len(fileMagic)] = version
	for i := 512; i+4 <= len(b); i += 4 {
		binary.LittleEndian.PutUint32(b[i:], 0xDEADBEEF)
	}
	return &fileBuilder{buf: b}
}

func (f *fileBuilder) align() {
	for len(f.buf)%blockAlign != 0 {
		f.buf = append(f.buf, 0)
	}
}

// rawBlock 按真实格式拼一个块（可传自定义负载做坏块）。返回块起点偏移。
func (f *fileBuilder) rawBlock(id uint64, payload []byte, rawLen int, codec uint32) int {
	f.align()
	off := len(f.buf)
	h := make([]byte, blockHdrLen+8)
	copy(h[8:], blockMagic)
	binary.LittleEndian.PutUint32(h[16:], 8)
	binary.LittleEndian.PutUint32(h[20:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[24:], uint32(rawLen))
	binary.LittleEndian.PutUint32(h[28:], codec)
	binary.LittleEndian.PutUint64(h[32:], id)
	blk := append(h, payload...)
	binary.LittleEndian.PutUint32(blk[4:], crc32.ChecksumIEEE(blk[8:]))
	binary.LittleEndian.PutUint32(blk[0:], 0x12345678)
	f.buf = append(f.buf, blk...)
	return off
}

func (f *fileBuilder) block(id uint64, raw []byte) int {
	return f.rawBlock(id, lz4Literal(raw), len(raw), codecLZ4)
}

func (f *fileBuilder) bytes() []byte {
	f.align()
	return f.buf
}

func htmlBody(text string) []byte {
	return []byte("\x00\x01junk<html><head><style>p{color:red}</style></head><body><p>" + text +
		"</p><br>第二行 &amp; more</body></html>\x00tail")
}
