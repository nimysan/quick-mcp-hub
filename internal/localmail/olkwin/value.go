package olkwin

import (
	"errors"
	"fmt"
	"path/filepath"
)

// Blink 在 V8 序列化值外面还有一层包装（IDBValueWrapper 和 SerializedScriptValue）：
//   - FF 11 01 varint(大小) varint(序号)：值太大，放进了外部 blob 文件，序号指向 blob 记录里的第几个对象
//   - FF 11 02 + Snappy 块：值被压缩过
//   - 否则是 Blink 的 SSV：FF <blink 版本>，版本 ≥ 16 时跟一个 FE + 12 字节的尾部偏移，然后是 V8 的 FF <版本>
// 依据：Blink modules/indexeddb/idb_value_wrapping.cc 和 bindings/.../serialized_script_value.cc（BSD-3）。

var errNeedBlob = errors.New("值在外部 blob 里")

const (
	ssvPseudoVersion = 0x11
	wrapBlob         = 0x01
	wrapSnappy       = 0x02
)

// unwrapBlink 剥掉 Blink 包装，返回 V8 部分；值在外部 blob 时返回 errNeedBlob 和 blob 序号。
func unwrapBlink(p []byte) ([]byte, int, error) {
	for round := 0; round < 3; round++ {
		if len(p) >= 3 && p[0] == 0xFF && p[1] == ssvPseudoVersion {
			switch p[2] {
			case wrapBlob:
				q := p[3:]
				if _, q, ok := idbVarint(q); ok {
					if idx, _, ok := idbVarint(q); ok {
						return nil, int(idx), errNeedBlob
					}
				}
				return nil, 0, fmt.Errorf("%w: blob 包装头损坏", ErrUnsupportedFormat)
			case wrapSnappy:
				d, err := snappyDecode(p[3:])
				if err != nil {
					return nil, 0, err
				}
				p = d
				continue
			default:
				return nil, 0, fmt.Errorf("%w: Blink 包装类型 %d", ErrUnsupportedFormat, p[2])
			}
		}
		break
	}
	// Blink SSV 头
	if len(p) < 2 || p[0] != 0xFF {
		return nil, 0, fmt.Errorf("%w: 值没有 Blink 版本头", ErrUnsupportedFormat)
	}
	ver, q, ok := idbVarint(p[1:])
	if !ok {
		return nil, 0, fmt.Errorf("%w: Blink 版本头损坏", ErrUnsupportedFormat)
	}
	if ver < 13 || ver > 0x7f {
		return nil, 0, fmt.Errorf("%w: Blink 序列化版本 %d", ErrUnsupportedFormat, ver)
	}
	if ver >= 16 && len(q) >= 13 && q[0] == 0xFE {
		q = q[13:]
	}
	return q, 0, nil
}

// blobNumber 从 blob 记录（index 3 的值）里取第 idx 个外部对象的 blob 号。
// 编码：每个对象一个类型字节（0 blob、1 文件），varint blob 号，字符串 MIME 类型，文件还有文件名和修改时间，最后是 varint 大小。
func blobNumber(v []byte, idx int) (int64, bool) {
	for i := 0; len(v) > 0; i++ {
		typ := v[0]
		if typ > 1 {
			return 0, false
		}
		n, rest, ok := idbVarint(v[1:])
		if !ok {
			return 0, false
		}
		if i == idx {
			return n, true
		}
		_, rest, ok = idbStringWithLength(rest)
		if !ok {
			return 0, false
		}
		if typ == 1 {
			if _, rest, ok = idbStringWithLength(rest); !ok {
				return 0, false
			}
			if _, rest, ok = idbVarint(rest); !ok {
				return 0, false
			}
		}
		if _, rest, ok = idbVarint(rest); !ok {
			return 0, false
		}
		v = rest
	}
	return 0, false
}

// blobPath 是 Chromium 外部 blob 的路径：<blob 目录>/<库 ID 十六进制>/<blob 号的第二个字节，两位十六进制>/<blob 号十六进制>。
func blobPath(blobDir string, db, num int64) string {
	return filepath.Join(blobDir, fmt.Sprintf("%x", db), fmt.Sprintf("%02x", (num>>8)&0xff), fmt.Sprintf("%x", num))
}
