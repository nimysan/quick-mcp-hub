package outlookwin

// NDB 层（[MS-PST] 2.2）：文件头、两棵页级 B 树（NBT 按 NID 找节点，BBT 按 BID 找块）、
// 数据块（含 XBLOCK/XXBLOCK 数据树）和子节点树（SLBLOCK/SIBLOCK）。
//
// 支持三种布局：
//   - ANSI（wVer 14/15）：4 字节 BID/IB，512 字节页；
//   - Unicode（wVer 23）：8 字节 BID/IB，512 字节页；
//   - Unicode 4K（wVer 36，Outlook 2013 起的 OST）：4096 字节页，BBT 叶子多一个解压后长度，
//     块可能是 zlib 压缩的。这种布局不在 [MS-PST] 里，按公开样本实测（见报告）。

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

type layout int

const (
	layoutANSI layout = iota
	layoutUnicode
	layoutUnicode4K
)

func (l layout) String() string {
	switch l {
	case layoutANSI:
		return "ansi"
	case layoutUnicode:
		return "unicode"
	default:
		return "unicode-4k"
	}
}

// 加密方式（HEADER.bCryptMethod）。
const (
	cryptNone    = 0x00
	cryptPermute = 0x01
	cryptCyclic  = 0x02
	cryptEDP     = 0x10 // Windows 信息保护，本包读不了
)

const (
	ptypeBBT = 0x80
	ptypeNBT = 0x81

	maxTreeDepth = 16       // B 树 / 数据树的层数上限，防止坏文件造成死循环
	maxBlockSize = 64 << 10 // 单块上限（4K 布局解压后也不超过它）
)

var le = binary.LittleEndian

// errFormat 表示文件内容和规范对不上（坏文件或不认识的变体）。
var errFormat = errors.New("PST 结构损坏或不认识")

func formatErr(f string, a ...any) error {
	return fmt.Errorf("%w: %s", errFormat, fmt.Sprintf(f, a...))
}

// pstFile 是一个已打开的 PST/OST 文件。只用 ReadAt 读，并发安全。
type pstFile struct {
	f       *os.File
	r       io.ReaderAt
	size    int64
	layout  layout
	ver     uint16
	isOST   bool
	crypt   byte
	nbtRoot uint64 // NBT 根页的文件偏移
	bbtRoot uint64
}

func (p *pstFile) pageSize() int {
	if p.layout == layoutUnicode4K {
		return 4096
	}
	return 512
}

// idSize 是 BID / IB 的字节数。
func (p *pstFile) idSize() int {
	if p.layout == layoutANSI {
		return 4
	}
	return 8
}

func (p *pstFile) readID(b []byte) uint64 {
	if p.layout == layoutANSI {
		return uint64(le.Uint32(b))
	}
	return le.Uint64(b)
}

// parseHeader 解析文件头。r 至少要能读到前 4 KiB 里的头部。
func parseHeader(r io.ReaderAt, size int64) (*pstFile, error) {
	h := make([]byte, 564)
	if _, err := r.ReadAt(h, 0); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, formatErr("文件太短，不是 PST/OST")
		}
		return nil, err // I/O 错误（例如 Outlook 加了字节范围锁）原样返回，不当作格式问题
	}
	if string(h[0:4]) != "!BDN" {
		return nil, formatErr("不是 PST/OST 文件（魔数不对）")
	}
	p := &pstFile{r: r, size: size}
	switch string(h[8:10]) {
	case "SM":
	case "SO":
		p.isOST = true
	case "SA": // PAB 通讯录
		return nil, formatErr("这是个人通讯簿（.pab），不是邮件文件")
	default:
		return nil, formatErr("wMagicClient=%x", h[8:10])
	}
	p.ver = le.Uint16(h[10:])
	switch {
	case p.ver == 14 || p.ver == 15:
		p.layout = layoutANSI
		p.crypt = h[461]
		p.nbtRoot = uint64(le.Uint32(h[0xBC:]))
		p.bbtRoot = uint64(le.Uint32(h[0xC4:]))
	case p.ver >= 21 && p.ver <= 23:
		p.layout = layoutUnicode
	case p.ver == 36 || p.ver == 37:
		p.layout = layoutUnicode4K
	default:
		return nil, formatErr("wVer=%d", p.ver)
	}
	if p.layout != layoutANSI {
		p.crypt = h[0x201]
		p.nbtRoot = le.Uint64(h[0xE0:])
		p.bbtRoot = le.Uint64(h[0xF0:])
	}
	switch p.crypt {
	case cryptNone, cryptPermute, cryptCyclic:
	case cryptEDP:
		return nil, formatErr("文件被 Windows 信息保护（EDP）加密，只有 Outlook 本身能解开")
	default:
		return nil, formatErr("bCryptMethod=%#x", p.crypt)
	}
	ps := uint64(p.pageSize())
	if p.nbtRoot == 0 || p.bbtRoot == 0 || p.nbtRoot+ps > uint64(size) || p.bbtRoot+ps > uint64(size) {
		return nil, formatErr("B 树根页越界")
	}
	return p, nil
}

// btPage 是一页 B 树。
type btPage struct {
	ptype byte
	level int
	cEnt  int
	cbEnt int
	data  []byte
}

func (p *pstFile) readPage(ib uint64, want byte) (*btPage, error) {
	ps := p.pageSize()
	if ib+uint64(ps) > uint64(p.size) {
		return nil, formatErr("页偏移越界 %#x", ib)
	}
	b := make([]byte, ps)
	if _, err := p.r.ReadAt(b, int64(ib)); err != nil {
		return nil, err
	}
	pg := &btPage{data: b}
	var entBytes int
	switch p.layout {
	case layoutANSI:
		pg.ptype = b[500]
		pg.cEnt, pg.cbEnt, pg.level = int(b[496]), int(b[498]), int(b[499])
		entBytes = 496
	case layoutUnicode:
		pg.ptype = b[496]
		pg.cEnt, pg.cbEnt, pg.level = int(b[488]), int(b[490]), int(b[491])
		entBytes = 488
	default:
		pg.ptype = b[4072]
		pg.cEnt, pg.cbEnt, pg.level = int(le.Uint16(b[4056:])), int(b[4060]), int(b[4061])
		entBytes = 4056
	}
	if pg.ptype != want {
		return nil, formatErr("页类型 %#x，期望 %#x（偏移 %#x）", pg.ptype, want, ib)
	}
	if pg.cbEnt == 0 || pg.cEnt*pg.cbEnt > entBytes || pg.level > maxTreeDepth {
		return nil, formatErr("B 树页头不合理（偏移 %#x）", ib)
	}
	return pg, nil
}

func (pg *btPage) entry(i int) []byte { return pg.data[i*pg.cbEnt : (i+1)*pg.cbEnt] }

// search 在一棵页级 B 树里找 key，返回叶子条目（拷贝前的切片）。找不到返回 nil。
func (p *pstFile) search(root uint64, ptype byte, key uint64, keyOf func([]byte) uint64) ([]byte, error) {
	ib := root
	for depth := 0; depth <= maxTreeDepth; depth++ {
		pg, err := p.readPage(ib, ptype)
		if err != nil {
			return nil, err
		}
		if pg.level == 0 {
			for i := 0; i < pg.cEnt; i++ {
				e := pg.entry(i)
				if keyOf(e) == key {
					return e, nil
				}
			}
			return nil, nil
		}
		// 中间页：BTENTRY = btkey + BREF(bid, ib)；取最后一个 btkey <= key 的。
		n := p.idSize()
		next := uint64(0)
		for i := 0; i < pg.cEnt; i++ {
			e := pg.entry(i)
			if p.readID(e) > key {
				break
			}
			next = p.readID(e[2*n:])
		}
		if next == 0 {
			return nil, nil
		}
		ib = next
	}
	return nil, formatErr("B 树层数过多")
}

// walk 遍历一棵页级 B 树的全部叶子条目。
func (p *pstFile) walk(ib uint64, ptype byte, depth int, fn func([]byte) error) error {
	if depth > maxTreeDepth {
		return formatErr("B 树层数过多")
	}
	pg, err := p.readPage(ib, ptype)
	if err != nil {
		return err
	}
	n := p.idSize()
	for i := 0; i < pg.cEnt; i++ {
		e := pg.entry(i)
		if pg.level == 0 {
			if err := fn(e); err != nil {
				return err
			}
			continue
		}
		if err := p.walk(p.readID(e[2*n:]), ptype, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// nbtEntry 是 NBT 的一个叶子条目：一个节点。
type nbtEntry struct {
	nid       uint32
	bidData   uint64
	bidSub    uint64
	nidParent uint32
}

func (p *pstFile) parseNBT(e []byte) nbtEntry {
	n := p.idSize()
	return nbtEntry{
		nid:       le.Uint32(e),
		bidData:   p.readID(e[n:]),
		bidSub:    p.readID(e[2*n:]),
		nidParent: le.Uint32(e[3*n:]),
	}
}

// node 按 NID 在 NBT 里找节点。找不到返回 ok=false。
func (p *pstFile) node(nid uint32) (nbtEntry, bool, error) {
	e, err := p.search(p.nbtRoot, ptypeNBT, uint64(nid), func(b []byte) uint64 { return uint64(le.Uint32(b)) })
	if err != nil || e == nil {
		return nbtEntry{}, false, err
	}
	return p.parseNBT(e), true, nil
}

// walkNodes 遍历 NBT 的所有节点。
func (p *pstFile) walkNodes(fn func(nbtEntry) error) error {
	return p.walk(p.nbtRoot, ptypeNBT, 0, func(e []byte) error { return fn(p.parseNBT(e)) })
}

// blockRef 是 BBT 叶子条目：块在文件里的位置和长度。
type blockRef struct {
	bid        uint64
	ib         uint64
	cb         int // 磁盘上的长度
	cbInflated int // 解压后的长度（只有 4K 布局可能不同）
}

func (p *pstFile) blockRef(bid uint64) (blockRef, error) {
	key := bid &^ 1 // 最低位是保留位，查表时忽略
	n := p.idSize()
	e, err := p.search(p.bbtRoot, ptypeBBT, key, func(b []byte) uint64 { return p.readID(b) &^ 1 })
	if err != nil {
		return blockRef{}, err
	}
	if e == nil {
		return blockRef{}, formatErr("BBT 里找不到块 %#x", bid)
	}
	r := blockRef{bid: p.readID(e), ib: p.readID(e[n:]), cb: int(le.Uint16(e[2*n:]))}
	r.cbInflated = r.cb
	if p.layout == layoutUnicode4K {
		r.cbInflated = int(le.Uint16(e[2*n+2:]))
	}
	return r, nil
}

// isInternal 报告 BID 指向的是内部块（XBLOCK / SLBLOCK 等），内部块从不加密。
func isInternal(bid uint64) bool { return bid&2 != 0 }

// readBlock 读一个块并还原成明文（解压、解密）。
func (p *pstFile) readBlock(bid uint64) ([]byte, error) {
	r, err := p.blockRef(bid)
	if err != nil {
		return nil, err
	}
	if r.cb > maxBlockSize || r.cbInflated > maxBlockSize || r.ib+uint64(r.cb) > uint64(p.size) {
		return nil, formatErr("块 %#x 越界", bid)
	}
	b := make([]byte, r.cb)
	if _, err := p.r.ReadAt(b, int64(r.ib)); err != nil {
		return nil, err
	}
	if r.cbInflated != r.cb {
		// 4K 布局的压缩块是 zlib。规范没写压缩和加密的先后，样本里 OST 都不加密；
		// 先直接解压，解不开再解密后解压。
		out, err := inflate(b, r.cbInflated)
		if err != nil && p.crypt != cryptNone && !isInternal(bid) {
			c := append([]byte(nil), b...)
			p.decrypt(c, bid)
			if out2, err2 := inflate(c, r.cbInflated); err2 == nil {
				return out2, nil
			}
		}
		if err != nil {
			return nil, formatErr("块 %#x 解压失败: %v", bid, err)
		}
		b = out
		if !isInternal(bid) {
			p.decrypt(b, bid)
		}
		return b, nil
	}
	if !isInternal(bid) {
		p.decrypt(b, bid)
	}
	return b, nil
}

func inflate(b []byte, n int) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, n)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *pstFile) decrypt(b []byte, bid uint64) {
	switch p.crypt {
	case cryptPermute:
		decodePermute(b)
	case cryptCyclic:
		cryptCyclicBuf(b, uint32(bid))
	}
}

// readData 读一个节点的数据：单个数据块，或者 XBLOCK / XXBLOCK 数据树展开后的块列表。
// 块列表保持原样不拼接：HN 的 HID 按块序号寻址。
func (p *pstFile) readData(bid uint64) ([][]byte, error) {
	if bid == 0 {
		return nil, nil
	}
	var out [][]byte
	total := 0
	var rec func(bid uint64, depth int) error
	rec = func(bid uint64, depth int) error {
		if depth > 2 {
			return formatErr("数据树层数过多")
		}
		b, err := p.readBlock(bid)
		if err != nil {
			return err
		}
		if !isInternal(bid) {
			total += len(b)
			if total > maxNodeBytes {
				return formatErr("节点数据超过 %d 字节上限", maxNodeBytes)
			}
			out = append(out, b)
			return nil
		}
		if len(b) < 8 || b[0] != 0x01 {
			return formatErr("块 %#x 不是 XBLOCK", bid)
		}
		cEnt := int(le.Uint16(b[2:]))
		n := p.idSize()
		if 8+cEnt*n > len(b) {
			return formatErr("XBLOCK 条目越界")
		}
		for i := 0; i < cEnt; i++ {
			if err := rec(p.readID(b[8+i*n:]), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := rec(bid, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// maxNodeBytes 是单个节点（例如一封信的正文、一个附件）最多读多少字节。
// shortcut: 超过就报错而不是流式读，附件内容本包不读，正文和表一般远小于它。
const maxNodeBytes = 64 << 20

// subEntry 是子节点树的叶子条目。
type subEntry struct {
	bidData uint64
	bidSub  uint64
}

// readSubnodes 读一棵子节点树（SLBLOCK / SIBLOCK）。
func (p *pstFile) readSubnodes(bid uint64) (map[uint32]subEntry, error) {
	out := map[uint32]subEntry{}
	if bid == 0 {
		return out, nil
	}
	n := p.idSize()
	hdr := 8
	if p.layout == layoutANSI {
		hdr = 4
	}
	var rec func(bid uint64, depth int) error
	rec = func(bid uint64, depth int) error {
		if depth > maxTreeDepth {
			return formatErr("子节点树层数过多")
		}
		b, err := p.readBlock(bid)
		if err != nil {
			return err
		}
		if len(b) < hdr || b[0] != 0x02 {
			return formatErr("块 %#x 不是 SLBLOCK/SIBLOCK", bid)
		}
		level, cEnt := b[1], int(le.Uint16(b[2:]))
		if level == 0 {
			if hdr+cEnt*3*n > len(b) {
				return formatErr("SLBLOCK 条目越界")
			}
			for i := 0; i < cEnt; i++ {
				e := b[hdr+i*3*n:]
				out[le.Uint32(e)] = subEntry{bidData: p.readID(e[n:]), bidSub: p.readID(e[2*n:])}
			}
			return nil
		}
		if hdr+cEnt*2*n > len(b) {
			return formatErr("SIBLOCK 条目越界")
		}
		for i := 0; i < cEnt; i++ {
			if err := rec(p.readID(b[hdr+i*2*n+n:]), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := rec(bid, 0); err != nil {
		return nil, err
	}
	return out, nil
}
