package olkwin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf16"
)

// V8 ValueSerializer（结构化克隆）的反序列化，只实现 IndexedDB 里会出现的普通数据：
// 对象、数组、字符串、数字、布尔、null/undefined、日期、Map/Set、ArrayBuffer 及视图、对象引用。
// 依据：V8 src/objects/value-serializer.cc（BSD-3）和 Blink 的
// third_party/blink/renderer/bindings/core/v8/serialization/serialization_tag.h。
// 不认识的标签返回 errV8，调用方把这条记录标为解不开，不猜。

var errV8 = errors.New("V8 序列化值不认识")

// jsObject 是 JS 对象，保留键的顺序便于诊断输出。
type jsObject struct {
	keys []string
	vals map[string]any
}

func (o *jsObject) get(k string) any {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

// jsUndefined 区分 undefined 和 null（null 用 Go 的 nil）。
type jsUndefined struct{}

type v8Reader struct {
	p     []byte
	ver   uint64
	objs  []any // 按出现顺序登记的对象，供 '^' 引用
	depth int
}

const maxV8Depth = 64

func (r *v8Reader) byte() (byte, error) {
	if len(r.p) == 0 {
		return 0, errV8
	}
	b := r.p[0]
	r.p = r.p[1:]
	return b, nil
}

func (r *v8Reader) varint() (uint64, error) {
	v, n := binary.Uvarint(r.p)
	if n <= 0 {
		return 0, errV8
	}
	r.p = r.p[n:]
	return v, nil
}

func (r *v8Reader) bytes(n uint64) ([]byte, error) {
	if n > uint64(len(r.p)) {
		return nil, errV8
	}
	b := r.p[:n]
	r.p = r.p[n:]
	return b, nil
}

func (r *v8Reader) double() (float64, error) {
	b, err := r.bytes(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

// peek 跳过填充字节后看下一个标签。
func (r *v8Reader) peek() (byte, bool) {
	for len(r.p) > 0 && r.p[0] == 0 {
		r.p = r.p[1:]
	}
	if len(r.p) == 0 {
		return 0, false
	}
	return r.p[0], true
}

func (r *v8Reader) register(v any) int {
	r.objs = append(r.objs, v)
	return len(r.objs) - 1
}

func latin1(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func utf16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// readString 读字符串类标签（对象的键也走这里）。
func (r *v8Reader) readString(tag byte) (string, error) {
	n, err := r.varint()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(n)
	if err != nil {
		return "", err
	}
	switch tag {
	case '"':
		return latin1(b), nil
	case 'c':
		if n%2 != 0 {
			return "", errV8
		}
		return utf16le(b), nil
	case 'S':
		return string(b), nil
	}
	return "", errV8
}

// value 读一个值。
func (r *v8Reader) value() (any, error) {
	r.depth++
	defer func() { r.depth-- }()
	if r.depth > maxV8Depth {
		return nil, errV8
	}
	if _, ok := r.peek(); !ok {
		return nil, errV8
	}
	tag, _ := r.byte()
	switch tag {
	case '_':
		return jsUndefined{}, nil
	case '0':
		return nil, nil
	case 'T':
		return true, nil
	case 'F':
		return false, nil
	case 'I':
		v, err := r.varint()
		return float64(int32(uint32(v>>1) ^ -uint32(v&1))), err
	case 'U':
		v, err := r.varint()
		return float64(uint32(v)), err
	case 'N':
		return r.double()
	case 'Z': // BigInt：位域 varint + 字节，只给出一个占位字符串
		bf, err := r.varint()
		if err != nil {
			return nil, err
		}
		_, err = r.bytes(bf >> 1 & 0x3fffffff)
		return "bigint", err
	case '"', 'c', 'S':
		return r.readString(tag)
	case '^':
		id, err := r.varint()
		if err != nil || id >= uint64(len(r.objs)) {
			return nil, errV8
		}
		return r.objs[id], nil
	case 'o':
		o := &jsObject{vals: map[string]any{}}
		r.register(o)
		if err := r.properties(o, '{'); err != nil {
			return nil, err
		}
		return o, nil
	case 'A': // 稠密数组
		n, err := r.varint()
		if err != nil || n > 1<<24 {
			return nil, errV8
		}
		arr := make([]any, 0, min(n, 1024))
		idx := r.register(nil)
		for i := uint64(0); i < n; i++ {
			if t, ok := r.peek(); ok && t == '-' { // 空位
				r.byte()
				arr = append(arr, jsUndefined{})
				continue
			}
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		r.objs[idx] = arr
		extra := &jsObject{vals: map[string]any{}}
		if err := r.properties(extra, '$'); err != nil {
			return nil, err
		}
		if _, err := r.varint(); err != nil { // 长度
			return nil, err
		}
		return arr, nil
	case 'a': // 稀疏数组：长度，然后是键值对
		if _, err := r.varint(); err != nil {
			return nil, err
		}
		o := &jsObject{vals: map[string]any{}}
		r.register(o)
		if err := r.properties(o, '@'); err != nil {
			return nil, err
		}
		if _, err := r.varint(); err != nil {
			return nil, err
		}
		var arr []any
		for _, k := range o.keys {
			arr = append(arr, o.vals[k])
		}
		return arr, nil
	case 'D':
		ms, err := r.double()
		if err != nil {
			return nil, err
		}
		t := msTime(ms)
		r.register(t)
		return t, nil
	case 'y', 'x':
		r.register(tag == 'y')
		return tag == 'y', nil
	case 'n':
		f, err := r.double()
		r.register(f)
		return f, err
	case 's':
		t, ok := r.peek()
		if !ok {
			return nil, errV8
		}
		r.byte()
		s, err := r.readString(t)
		r.register(s)
		return s, err
	case 'R':
		t, ok := r.peek()
		if !ok {
			return nil, errV8
		}
		r.byte()
		s, err := r.readString(t)
		if err != nil {
			return nil, err
		}
		if _, err := r.varint(); err != nil {
			return nil, err
		}
		r.register(s)
		return s, nil
	case ';': // Map
		o := &jsObject{vals: map[string]any{}}
		r.register(o)
		for {
			if t, ok := r.peek(); ok && t == ':' {
				r.byte()
				_, err := r.varint()
				return o, err
			}
			k, err := r.value()
			if err != nil {
				return nil, err
			}
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			ks := fmt.Sprint(k)
			o.keys = append(o.keys, ks)
			o.vals[ks] = v
		}
	case '\'': // Set
		idx := r.register(nil)
		var arr []any
		for {
			if t, ok := r.peek(); ok && t == ',' {
				r.byte()
				_, err := r.varint()
				r.objs[idx] = arr
				return arr, err
			}
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	case 'B':
		n, err := r.varint()
		if err != nil {
			return nil, err
		}
		b, err := r.bytes(n)
		if err != nil {
			return nil, err
		}
		buf := append([]byte(nil), b...)
		r.register(buf)
		return r.maybeView(buf)
	case '~': // 可变长 ArrayBuffer：字节长度 + 最大长度
		n, err := r.varint()
		if err != nil {
			return nil, err
		}
		if _, err := r.varint(); err != nil {
			return nil, err
		}
		b, err := r.bytes(n)
		if err != nil {
			return nil, err
		}
		buf := append([]byte(nil), b...)
		r.register(buf)
		return r.maybeView(buf)
	}
	return nil, fmt.Errorf("%w: 标签 0x%02x", errV8, tag)
}

// maybeView 处理紧跟在 ArrayBuffer 后面的视图（Uint8Array 等），直接返回视图覆盖的字节。
func (r *v8Reader) maybeView(buf []byte) (any, error) {
	t, ok := r.peek()
	if !ok || t != 'V' {
		return buf, nil
	}
	r.byte()
	if _, err := r.byte(); err != nil { // 子类型
		return nil, err
	}
	off, err := r.varint()
	if err != nil {
		return nil, err
	}
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if r.ver >= 14 {
		if _, err := r.varint(); err != nil { // flags
			return nil, err
		}
	}
	if off > uint64(len(buf)) || n > uint64(len(buf))-off {
		return nil, errV8
	}
	v := buf[off : off+n]
	r.register(v)
	return v, nil
}

// properties 读对象的键值对，直到结束标签 end，再读属性个数。
func (r *v8Reader) properties(o *jsObject, end byte) error {
	for {
		t, ok := r.peek()
		if !ok {
			return errV8
		}
		if t == end {
			r.byte()
			if end == '{' {
				_, err := r.varint()
				return err
			}
			_, err := r.varint() // 属性个数；数组还要读长度，由调用方读
			return err
		}
		k, err := r.value()
		if err != nil {
			return err
		}
		v, err := r.value()
		if err != nil {
			return err
		}
		var ks string
		switch kk := k.(type) {
		case string:
			ks = kk
		case float64:
			ks = fmt.Sprintf("%v", kk)
		default:
			return errV8
		}
		if _, dup := o.vals[ks]; !dup {
			o.keys = append(o.keys, ks)
		}
		o.vals[ks] = v
	}
}

func msTime(ms float64) time.Time {
	if math.IsNaN(ms) || math.IsInf(ms, 0) {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// decodeV8 解 V8 头（0xFF 版本号）之后的一个值。
func decodeV8(p []byte) (any, error) {
	r := &v8Reader{p: p}
	t, ok := r.peek()
	if !ok || t != 0xFF {
		return nil, fmt.Errorf("%w: 缺少 V8 版本头", errV8)
	}
	r.byte()
	v, err := r.varint()
	if err != nil {
		return nil, err
	}
	if v < 13 || v > maxV8Version {
		return nil, fmt.Errorf("%w: V8 序列化版本 %d", errV8, v)
	}
	r.ver = v
	return r.value()
}

// maxV8Version 是验证过的最高 V8 序列化版本（olk 1.2026.1103.200 / WebView2 157 写的是 16）。
// 更高的版本可能加了新标签，按不支持处理。
const maxV8Version = 16
