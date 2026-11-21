package olkwin

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf16"
)

// Chromium IndexedDB 在 LevelDB 上的键编码，只实现读取需要的部分。
// 依据：Chromium content/browser/indexed_db/indexed_db_leveldb_coding.{h,cc}（BSD-3）。
//
// 每个键以 KeyPrefix 开头：第一个字节的高 3 位是 database_id 的字节数减 1，中间 3 位是 object_store_id 的
// 字节数减 1，低 2 位是 index_id 的字节数减 1，后面依次是这三个小端整数。
//   - database_id == 0：全局元数据（库名 → 库 ID 在类型 201 下）
//   - object_store_id == 0：库元数据（对象仓库名在类型 50 下）
//   - index_id == 1：对象仓库的数据记录，后面跟 IDB 主键，值是 varint 版本号 + Blink 序列化值
//   - index_id == 3：外部 blob 记录
//   - index_id >= 30：索引

type keyPrefix struct {
	db, store, index int64
}

func decodePrefix(k []byte) (keyPrefix, []byte, bool) {
	if len(k) < 1 {
		return keyPrefix{}, nil, false
	}
	b := k[0]
	nd, ns, ni := int(b>>5)+1, int(b>>2&7)+1, int(b&3)+1
	if len(k) < 1+nd+ns+ni || nd > 8 || ns > 8 {
		return keyPrefix{}, nil, false
	}
	le := func(p []byte) int64 {
		var v uint64
		for i := len(p) - 1; i >= 0; i-- {
			v = v<<8 | uint64(p[i])
		}
		return int64(v)
	}
	p := k[1:]
	kp := keyPrefix{db: le(p[:nd]), store: le(p[nd : nd+ns]), index: le(p[nd+ns : nd+ns+ni])}
	return kp, p[nd+ns+ni:], true
}

// encodePrefix 是 decodePrefix 的逆，夹具和查找都用。
func encodePrefix(kp keyPrefix) []byte {
	n := func(v int64) int {
		c := 1
		for u := uint64(v) >> 8; u != 0; u >>= 8 {
			c++
		}
		return c
	}
	nd, ns, ni := n(kp.db), n(kp.store), n(kp.index)
	if ni > 4 {
		ni = 4
	}
	out := []byte{byte((nd-1)<<5 | (ns-1)<<2 | (ni - 1))}
	put := func(v int64, c int) {
		for i := 0; i < c; i++ {
			out = append(out, byte(uint64(v)>>(8*i)))
		}
	}
	put(kp.db, nd)
	put(kp.store, ns)
	put(kp.index, ni)
	return out
}

// idbVarint 是 IndexedDB 编码里的 varint（和 LevelDB 一样是 LEB128）。
func idbVarint(p []byte) (int64, []byte, bool) {
	v, n := binary.Uvarint(p)
	if n <= 0 {
		return 0, nil, false
	}
	return int64(v), p[n:], true
}

// idbInt 是 Chromium 的 EncodeInt：小端、去掉高位零字节，长度就是值的长度。
func idbInt(p []byte) (int64, bool) {
	if len(p) == 0 || len(p) > 8 {
		return 0, false
	}
	var v uint64
	for i := len(p) - 1; i >= 0; i-- {
		v = v<<8 | uint64(p[i])
	}
	return int64(v), true
}

// idbStringWithLength：varint 字符数 + UTF-16BE。
func idbStringWithLength(p []byte) (string, []byte, bool) {
	n, p, ok := idbVarint(p)
	if !ok || n < 0 || n*2 > int64(len(p)) {
		return "", nil, false
	}
	return utf16be(p[:n*2]), p[n*2:], true
}

func utf16be(p []byte) string {
	u := make([]uint16, len(p)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(p[2*i:])
	}
	return string(utf16.Decode(u))
}

// idbKey 解出一个 IndexedDB 主键，返回它的可读字符串形式（用于 ID 和诊断）。
// 类型：0 null、1 字符串、2 日期、3 数字、4 数组、6 二进制。
func idbKey(p []byte, depth int) (string, []byte, bool) {
	if len(p) < 1 || depth > 8 {
		return "", nil, false
	}
	t := p[0]
	p = p[1:]
	switch t {
	case 0:
		return "null", p, true
	case 1:
		return idbStringWithLength(p)
	case 2, 3:
		if len(p) < 8 {
			return "", nil, false
		}
		f := math.Float64frombits(binary.LittleEndian.Uint64(p))
		return fmt.Sprintf("%v", f), p[8:], true
	case 4:
		n, rest, ok := idbVarint(p)
		if !ok || n > 1<<16 {
			return "", nil, false
		}
		p = rest
		s := "["
		for i := int64(0); i < n; i++ {
			var e string
			e, p, ok = idbKey(p, depth+1)
			if !ok {
				return "", nil, false
			}
			if i > 0 {
				s += ","
			}
			s += e
		}
		return s + "]", p, true
	case 6:
		n, rest, ok := idbVarint(p)
		if !ok || n < 0 || n > int64(len(rest)) {
			return "", nil, false
		}
		return fmt.Sprintf("%x", rest[:n]), rest[n:], true
	}
	return "", nil, false
}

// idbSchema 是从元数据键里读出的库和对象仓库。
type idbSchema struct {
	dbs    map[int64]string           // 库 ID → 库名
	stores map[int64]map[int64]string // 库 ID → 仓库 ID → 仓库名
}

func readSchema(d *ldbDB) idbSchema {
	s := idbSchema{dbs: map[int64]string{}, stores: map[int64]map[int64]string{}}
	for k, e := range d.entries {
		if e.del {
			continue
		}
		kp, rest, ok := decodePrefix([]byte(k))
		if !ok || kp.store != 0 || kp.index != 0 || len(rest) == 0 {
			continue
		}
		if kp.db == 0 && rest[0] == 201 { // DatabaseName：origin + 库名 → 库 ID
			_, r, ok := idbStringWithLength(rest[1:])
			if !ok {
				continue
			}
			name, _, ok := idbStringWithLength(r)
			if !ok {
				continue
			}
			if id, ok := idbInt(e.val); ok {
				s.dbs[id] = name
			}
			continue
		}
		if kp.db > 0 && rest[0] == 50 { // 对象仓库元数据：仓库 ID + 元数据类型；类型 0 是仓库名
			id, r, ok := idbVarint(rest[1:])
			if !ok || len(r) != 1 || r[0] != 0 {
				continue
			}
			if s.stores[kp.db] == nil {
				s.stores[kp.db] = map[int64]string{}
			}
			s.stores[kp.db][id] = utf16be(e.val)
		}
	}
	return s
}

// keepKey 决定合并时保留哪些键：元数据、对象仓库数据（index 1）和 blob 记录（index 3）。索引条目丢掉以省内存。
// shortcut: 全部对象仓库记录（含正文）都留在内存里，9.5 MB 的库没问题；离线缓存设到 180 天、库到几百 MB 时，
// 改成只保留 messages/folders 和 messageBodies 的文件位置，Get 时再按需读。
func keepKey(k []byte) bool {
	kp, _, ok := decodePrefix(k)
	if !ok {
		return false
	}
	return (kp.store == 0 && kp.index == 0) || kp.index == 1 || kp.index == 3
}
