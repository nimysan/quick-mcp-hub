package outlookwin

// LTP 层（[MS-PST] 2.3）：节点数据之上的堆（HN）、堆里的 B 树（BTH）、
// 属性上下文（PC，一个对象的属性表）和表上下文（TC，一张行列表）。

import (
	"fmt"
	"sort"
)

const (
	hnSig      = 0xEC
	clientPC   = 0xBC
	clientTC   = 0x7C
	clientBTH  = 0xB5
	maxBTHRecs = 1 << 20
)

// nodeData 是一个节点：数据块列表 + 惰性读取的子节点树。
type nodeData struct {
	p      *pstFile
	blocks [][]byte
	bidSub uint64
	subs   map[uint32]subEntry
}

func (p *pstFile) openNode(nid uint32) (*nodeData, uint32, error) {
	e, ok, err := p.node(nid)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, errNoNode
	}
	n, err := p.openNodeAt(e.bidData, e.bidSub)
	return n, e.nidParent, err
}

func (p *pstFile) openNodeAt(bidData, bidSub uint64) (*nodeData, error) {
	blocks, err := p.readData(bidData)
	if err != nil {
		return nil, err
	}
	return &nodeData{p: p, blocks: blocks, bidSub: bidSub}, nil
}

// sub 打开这个节点的一个子节点。
func (n *nodeData) sub(nid uint32) (*nodeData, error) {
	if n.subs == nil {
		m, err := n.p.readSubnodes(n.bidSub)
		if err != nil {
			return nil, err
		}
		n.subs = m
	}
	e, ok := n.subs[nid]
	if !ok {
		return nil, errNoNode
	}
	return n.p.openNodeAt(e.bidData, e.bidSub)
}

func (n *nodeData) bytes() []byte {
	if len(n.blocks) == 1 {
		return n.blocks[0]
	}
	var out []byte
	for _, b := range n.blocks {
		out = append(out, b...)
	}
	return out
}

// heap 是节点数据上的 HN。
type heap struct {
	n         *nodeData
	clientSig byte
	userRoot  uint32
}

func newHeap(n *nodeData) (*heap, error) {
	if len(n.blocks) == 0 || len(n.blocks[0]) < 12 {
		return nil, formatErr("HN 太短")
	}
	b := n.blocks[0]
	if b[2] != hnSig {
		return nil, formatErr("HN 签名 %#x", b[2])
	}
	return &heap{n: n, clientSig: b[3], userRoot: le.Uint32(b[4:])}, nil
}

// get 取一个 HID 指向的分配。hid=0 返回 nil。
func (h *heap) get(hid uint32) ([]byte, error) {
	if hid == 0 {
		return nil, nil
	}
	if hid&0x1F != 0 {
		return nil, formatErr("不是 HID: %#x", hid)
	}
	idx := int(hid>>5) & 0x7FF
	blk := int(hid >> 16)
	if blk >= len(h.n.blocks) || idx == 0 {
		return nil, formatErr("HID %#x 越界", hid)
	}
	b := h.n.blocks[blk]
	if len(b) < 2 {
		return nil, formatErr("HN 块太短")
	}
	pm := int(le.Uint16(b))
	if pm+4 > len(b) {
		return nil, formatErr("HNPAGEMAP 越界")
	}
	cAlloc := int(le.Uint16(b[pm:]))
	if idx > cAlloc || pm+4+2*(cAlloc+1) > len(b) {
		return nil, formatErr("HID %#x 超出分配表", hid)
	}
	start := int(le.Uint16(b[pm+4+2*(idx-1):]))
	end := int(le.Uint16(b[pm+4+2*idx:]))
	if start > end || end > len(b) {
		return nil, formatErr("HID %#x 分配越界", hid)
	}
	return b[start:end], nil
}

// hnid 取 HNID 指向的数据：低 5 位为 0 是堆里的 HID，否则是子节点 NID。
func (h *heap) hnid(v uint32) ([]byte, error) {
	if v == 0 {
		return nil, nil
	}
	if v&0x1F == 0 {
		return h.get(v)
	}
	s, err := h.n.sub(v)
	if err != nil {
		return nil, err
	}
	return s.bytes(), nil
}

type bthRec struct{ key, val []byte }

// bth 读一棵 BTH 的全部叶子记录。
func (h *heap) bth(hid uint32) (cbKey, cbEnt int, recs []bthRec, err error) {
	hdr, err := h.get(hid)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(hdr) < 8 || hdr[0] != clientBTH {
		return 0, 0, nil, formatErr("BTH 头不对")
	}
	cbKey, cbEnt = int(hdr[1]), int(hdr[2])
	levels, root := int(hdr[3]), le.Uint32(hdr[4:])
	if cbKey == 0 || levels > maxTreeDepth {
		return 0, 0, nil, formatErr("BTH 头不合理")
	}
	var rec func(hid uint32, level int) error
	rec = func(hid uint32, level int) error {
		b, err := h.get(hid)
		if err != nil {
			return err
		}
		sz := cbKey + cbEnt
		if level > 0 {
			sz = cbKey + 4
		}
		for off := 0; off+sz <= len(b); off += sz {
			if level > 0 {
				if err := rec(le.Uint32(b[off+cbKey:]), level-1); err != nil {
					return err
				}
				continue
			}
			if len(recs) >= maxBTHRecs {
				return formatErr("BTH 记录过多")
			}
			recs = append(recs, bthRec{key: b[off : off+cbKey], val: b[off+cbKey : off+sz]})
		}
		return nil
	}
	if root != 0 {
		err = rec(root, levels)
	}
	return cbKey, cbEnt, recs, err
}

// 属性类型（[MS-OXCDATA] 2.11.1）。
const (
	ptInt16    = 0x0002
	ptInt32    = 0x0003
	ptFloat32  = 0x0004
	ptFloat64  = 0x0005
	ptCurrency = 0x0006
	ptAppTime  = 0x0007
	ptError    = 0x000A
	ptBool     = 0x000B
	ptObject   = 0x000D
	ptInt64    = 0x0014
	ptString8  = 0x001E
	ptUnicode  = 0x001F
	ptTime     = 0x0040
	ptGUID     = 0x0048
	ptBinary   = 0x0102
)

// fixedSize 返回定长类型的字节数，变长类型返回 0。
func fixedSize(t uint16) int {
	switch t {
	case ptBool:
		return 1
	case ptInt16:
		return 2
	case ptInt32, ptFloat32, ptError:
		return 4
	case ptFloat64, ptCurrency, ptAppTime, ptInt64, ptTime:
		return 8
	case ptGUID:
		return 16
	}
	return 0
}

// prop 是一个属性的原始值：定长类型是小端字节，变长类型是内容本身。
type prop struct {
	typ uint16
	raw []byte
}

// propBag 是一个对象的属性，键是属性 ID（标签高 16 位）。
type propBag map[uint16]prop

// readPC 读一个 PC 节点的全部属性。变长属性的内容在这里一次取出。
func readPC(n *nodeData) (propBag, error) {
	h, err := newHeap(n)
	if err != nil {
		return nil, err
	}
	if h.clientSig != clientPC {
		return nil, formatErr("不是 PC（bClientSig=%#x）", h.clientSig)
	}
	cbKey, cbEnt, recs, err := h.bth(h.userRoot)
	if err != nil {
		return nil, err
	}
	if cbKey != 2 || cbEnt != 6 {
		return nil, formatErr("PC 的 BTH 键/值长度 %d/%d", cbKey, cbEnt)
	}
	out := make(propBag, len(recs))
	for _, r := range recs {
		id, typ, v := le.Uint16(r.key), le.Uint16(r.val), le.Uint32(r.val[2:])
		var raw []byte
		switch sz := fixedSize(typ); {
		case sz > 0 && sz <= 4:
			raw = append([]byte(nil), r.val[2:2+sz]...)
		default:
			b, err := h.hnid(v)
			if err != nil {
				continue // 单个属性坏了不影响其他属性
			}
			raw = b
		}
		out[id] = prop{typ: typ, raw: raw}
	}
	return out, nil
}

// column 是 TC 的一列。
type column struct {
	tag  uint32
	ib   int
	cb   int
	iBit int
}

// table 是读好的 TC：行按行矩阵里的顺序。
type table struct {
	h    *heap
	cols map[uint16]column // 键是属性 ID
	ceb  int               // 单元格存在位图在行里的偏移
	rows [][]byte
}

func readTC(n *nodeData) (*table, error) {
	h, err := newHeap(n)
	if err != nil {
		return nil, err
	}
	if h.clientSig != clientTC {
		return nil, formatErr("不是 TC（bClientSig=%#x）", h.clientSig)
	}
	info, err := h.get(h.userRoot)
	if err != nil {
		return nil, err
	}
	if len(info) < 22 || info[0] != clientTC {
		return nil, formatErr("TCINFO 不对")
	}
	nCols := int(info[1])
	if len(info) < 22+8*nCols {
		return nil, formatErr("TCINFO 列描述越界")
	}
	t := &table{h: h, cols: make(map[uint16]column, nCols)}
	t.ceb = int(le.Uint16(info[6:]))
	rowSize := int(le.Uint16(info[8:]))
	hnidRows := le.Uint32(info[14:])
	for i := 0; i < nCols; i++ {
		d := info[22+8*i:]
		c := column{tag: le.Uint32(d), ib: int(le.Uint16(d[4:])), cb: int(d[6]), iBit: int(d[7])}
		if c.ib+c.cb > rowSize {
			return nil, formatErr("列 %#x 越界", c.tag)
		}
		t.cols[uint16(c.tag>>16)] = c
	}
	if hnidRows == 0 || rowSize == 0 {
		return t, nil
	}
	var blocks [][]byte
	if hnidRows&0x1F == 0 {
		b, err := h.get(hnidRows)
		if err != nil {
			return nil, err
		}
		blocks = [][]byte{b}
	} else {
		s, err := n.sub(hnidRows)
		if err != nil {
			return nil, err
		}
		blocks = s.blocks
	}
	// 行不跨块：每块装 floor(块长/行长) 行。
	for _, b := range blocks {
		for off := 0; off+rowSize <= len(b); off += rowSize {
			t.rows = append(t.rows, b[off:off+rowSize])
		}
	}
	return t, nil
}

// cell 取一行里一个属性的值。ok=false 表示没有这一列或单元格不存在。
func (t *table) cell(row []byte, id uint16) (prop, bool) {
	c, ok := t.cols[id]
	if !ok {
		return prop{}, false
	}
	if t.ceb+c.iBit/8 >= len(row) || row[t.ceb+c.iBit/8]&(1<<(7-uint(c.iBit%8))) == 0 {
		return prop{}, false
	}
	typ := uint16(c.tag)
	v := row[c.ib : c.ib+c.cb]
	if sz := fixedSize(typ); sz > 0 && sz <= 8 && c.cb >= sz {
		return prop{typ: typ, raw: v[:sz]}, true
	}
	if c.cb != 4 {
		return prop{}, false
	}
	b, err := t.h.hnid(le.Uint32(v))
	if err != nil {
		return prop{}, false
	}
	return prop{typ: typ, raw: b}, true
}

// rowBag 把一行转成属性表。
func (t *table) rowBag(row []byte) propBag {
	out := make(propBag, len(t.cols))
	for id := range t.cols {
		if p, ok := t.cell(row, id); ok {
			out[id] = p
		}
	}
	return out
}

func (t *table) bags() []propBag {
	out := make([]propBag, 0, len(t.rows))
	for _, r := range t.rows {
		out = append(out, t.rowBag(r))
	}
	return out
}

// columnIDs 返回列的属性 ID，排好序，给诊断用。
func (t *table) columnIDs() []string {
	var ids []int
	for id := range t.cols {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = fmt.Sprintf("%04X", id)
	}
	return out
}
