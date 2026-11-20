package hxstore

// HxStore.hxd（"Nostromo" 容器）的块层。格式无官方文档，以下是对 macOS 新版 Outlook 16.115
// 的实测结论（见 reports/hxstore-prototype.md）：
//
//	文件头：ASCII "Nostromo" + 版本字节（macOS 为 'i'，Windows Mail 为 'h'）
//	数据块从 512 字节边界开始，块头 32 字节：
//	  +0  u32 crcA（用途未确认，不校验）
//	  +4  u32 crcB = CRC-32/IEEE(bytes[8 : 32+extLen+compLen])
//	  +8  8B  魔数 05 6a 70 3b 64 45 02 5d
//	  +16 u32 extLen（8 = 对象 id，16 = GUID）
//	  +20 u32 compLen
//	  +24 u32 rawLen
//	  +28 u32 codec（4 = LZ4 block，无 frame 头）
//	  +32 extLen 字节扩展头，之后是 compLen 字节 LZ4 负载
//
// 读取策略：顺序扫描、逐块解压、解压缓冲复用，不把整文件或全部解压结果留在内存里。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/pierrec/lz4/v4"
)

const (
	blockAlign   = 512
	blockHdrLen  = 32
	codecLZ4     = 4
	maxExtLen    = 64
	maxCompLen   = 16 << 20 // 单块压缩负载上限；实测最大解压后约 770 KB
	maxRawLen    = 16 << 20 // 单块解压长度上限，防恶意头部撑爆内存
	scanChunk    = 4 << 20  // 顺序扫描窗口
	fileMagic    = "Nostromo"
	knownVersion = 'i'
)

var blockMagic = []byte{0x05, 0x6a, 0x70, 0x3b, 0x64, 0x45, 0x02, 0x5d}

var (
	errBlockHeader = errors.New("块头非法")
	errBlockCRC    = errors.New("块 CRC 不符")
	errBlockCodec  = errors.New("未知压缩方式")
	errBlockDecode = errors.New("LZ4 解压失败")
)

// blockHdr 是解析过、做过边界检查的块头。
type blockHdr struct {
	extLen, compLen, rawLen, codec uint32
}

func (h blockHdr) total() int { return blockHdrLen + int(h.extLen) + int(h.compLen) }

func parseHdr(b []byte) (blockHdr, bool) {
	if len(b) < blockHdrLen || !bytes.Equal(b[8:16], blockMagic) {
		return blockHdr{}, false
	}
	h := blockHdr{
		extLen:  binary.LittleEndian.Uint32(b[16:]),
		compLen: binary.LittleEndian.Uint32(b[20:]),
		rawLen:  binary.LittleEndian.Uint32(b[24:]),
		codec:   binary.LittleEndian.Uint32(b[28:]),
	}
	if h.extLen > maxExtLen || h.compLen == 0 || h.compLen > maxCompLen || h.rawLen == 0 || h.rawLen > maxRawLen {
		return h, false
	}
	return h, true
}

// blockID 返回 8 字节扩展头里的对象 id；GUID 扩展头返回 0。
func blockID(ext []byte) uint64 {
	if len(ext) != 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(ext)
}

// decodeBlock 校验 CRC 并解压。whole 是完整的块（头+扩展头+负载），dst 是可复用缓冲。
// 返回的切片指向 dst 的底层数组（可能扩容），下次调用前有效。
func decodeBlock(whole []byte, h blockHdr, dst []byte) (raw []byte, err error) {
	end := h.total()
	if end > len(whole) {
		return nil, errBlockHeader
	}
	if crc32.ChecksumIEEE(whole[8:end]) != binary.LittleEndian.Uint32(whole[4:]) {
		return nil, errBlockCRC
	}
	if h.codec != codecLZ4 {
		return nil, errBlockCodec
	}
	if cap(dst) < int(h.rawLen) {
		dst = make([]byte, h.rawLen)
	}
	dst = dst[:h.rawLen]
	// 第三方解码器对恶意输入理应返回错误；再兜一层 recover，保证本包不会因坏块 panic。
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, errBlockDecode
		}
	}()
	n, err := lz4.UncompressBlock(whole[blockHdrLen+int(h.extLen):end], dst)
	if err != nil || n != int(h.rawLen) {
		return nil, errBlockDecode
	}
	return dst[:n], nil
}

// scanStats 是一次扫描的块级统计。
type scanStats struct {
	Candidates int // 魔数命中
	OK         int // CRC 通过且解压成功
	BadHeader  int
	BadCRC     int
	BadDecode  int // 含未知 codec
	RawBytes   int64
}

// scanBlocks 顺序扫描 r 的 [0,size)，对每个有效块回调 fn(off, id, raw)。
// raw 只在回调期间有效。回调返回错误则中止扫描。
func scanBlocks(r io.ReaderAt, size int64, fn func(off int64, id uint64, raw []byte) error) (scanStats, error) {
	var st scanStats
	win := make([]byte, scanChunk)
	var w []byte   // 当前窗口内容，对应文件 [base, base+len(w))
	var base int64 // 窗口起点
	var big, dst []byte
	for pos := int64(0); pos+blockHdrLen <= size; {
		if pos < base || pos+blockHdrLen > base+int64(len(w)) {
			n, err := r.ReadAt(win, pos)
			if n < blockHdrLen {
				if err != nil && err != io.EOF {
					return st, err
				}
				break
			}
			base, w = pos, win[:n]
		}
		o := int(pos - base)
		h, ok := parseHdr(w[o:])
		if !ok {
			if bytes.Equal(w[o+8:o+16], blockMagic) {
				st.Candidates++
				st.BadHeader++
			}
			pos += blockAlign
			continue
		}
		st.Candidates++
		total := h.total()
		if pos+int64(total) > size {
			st.BadHeader++ // 截断：声明长度超过文件末尾（半写块）
			pos += blockAlign
			continue
		}
		var whole []byte
		if o+total <= len(w) {
			whole = w[o : o+total]
		} else {
			if cap(big) < total {
				big = make([]byte, total)
			}
			big = big[:total]
			if _, err := r.ReadAt(big, pos); err != nil {
				st.BadHeader++
				pos += blockAlign
				continue
			}
			whole = big
		}
		raw, err := decodeBlock(whole, h, dst)
		if err != nil {
			switch err {
			case errBlockCRC:
				st.BadCRC++
			case errBlockHeader:
				st.BadHeader++
			default:
				st.BadDecode++
			}
			pos += blockAlign
			continue
		}
		dst = raw[:0] // 复用（可能已扩容）
		st.OK++
		st.RawBytes += int64(len(raw))
		if err := fn(pos, blockID(whole[blockHdrLen:blockHdrLen+int(h.extLen)]), raw); err != nil {
			return st, err
		}
		// 有效块内部不会再有块头，直接跳到块尾后的对齐位置。
		pos = (pos + int64(total) + blockAlign - 1) / blockAlign * blockAlign
	}
	return st, nil
}

// readBlock 读取 off 处的单个块，并确认它仍是对象 id 为 want 的有效块。
func readBlock(r io.ReaderAt, size, off int64, want uint64) ([]byte, error) {
	if off < 0 || off+blockHdrLen > size {
		return nil, errBlockHeader
	}
	hb := make([]byte, blockHdrLen)
	if _, err := r.ReadAt(hb, off); err != nil {
		return nil, err
	}
	h, ok := parseHdr(hb)
	if !ok || off+int64(h.total()) > size {
		return nil, errBlockHeader
	}
	whole := make([]byte, h.total())
	if _, err := r.ReadAt(whole, off); err != nil {
		return nil, err
	}
	if id := blockID(whole[blockHdrLen : blockHdrLen+int(h.extLen)]); id != want {
		return nil, fmt.Errorf("块已被改写（id %x != %x）", id, want)
	}
	return decodeBlock(whole, h, nil)
}
