package olkwin

import (
	"encoding/binary"
	"errors"
)

// errSnappy 表示 Snappy 数据损坏。
var errSnappy = errors.New("snappy 数据损坏")

// maxSnappyLen 限制单块解压后的大小，防止被损坏的长度字段撑爆内存。
const maxSnappyLen = 64 << 20

// snappyDecode 解压 Snappy 块格式（不是帧格式）。LevelDB 的数据块和 Blink 压缩的 IndexedDB 值都用这种格式。
// 格式：varint 解压后长度，然后是一串字面量和回引元素。
func snappyDecode(src []byte) ([]byte, error) {
	n, k := binary.Uvarint(src)
	if k <= 0 || n > maxSnappyLen {
		return nil, errSnappy
	}
	src = src[k:]
	dst := make([]byte, 0, n)
	for len(src) > 0 {
		tag := src[0]
		var length, offset int
		switch tag & 3 {
		case 0: // 字面量
			l := int(tag >> 2)
			src = src[1:]
			if l >= 60 {
				nb := l - 59
				if len(src) < nb {
					return nil, errSnappy
				}
				l = 0
				for i := 0; i < nb; i++ {
					l |= int(src[i]) << (8 * i)
				}
				src = src[nb:]
			}
			l++
			if l > len(src) || len(dst)+l > int(n) {
				return nil, errSnappy
			}
			dst = append(dst, src[:l]...)
			src = src[l:]
			continue
		case 1:
			if len(src) < 2 {
				return nil, errSnappy
			}
			length = 4 + int(tag>>2)&7
			offset = int(tag&0xe0)<<3 | int(src[1])
			src = src[2:]
		case 2:
			if len(src) < 3 {
				return nil, errSnappy
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint16(src[1:]))
			src = src[3:]
		case 3:
			if len(src) < 5 {
				return nil, errSnappy
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint32(src[1:]))
			src = src[5:]
		}
		if offset <= 0 || offset > len(dst) || len(dst)+length > int(n) {
			return nil, errSnappy
		}
		// 回引可以和自身重叠，逐字节拷贝。
		start := len(dst) - offset
		for i := 0; i < length; i++ {
			dst = append(dst, dst[start+i])
		}
	}
	if len(dst) != int(n) {
		return nil, errSnappy
	}
	return dst, nil
}
