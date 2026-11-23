package olkwin

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
)

// idbStore 是一次快照里的 IndexedDB 数据（一个 origin 一个 LevelDB）。
type idbStore struct {
	dir, blobDir string
	ldb          *ldbDB
	schema       idbSchema
}

func openIDB(dir, blobDir string) (*idbStore, error) {
	d, err := openLevelDB(dir, keepKey)
	if err != nil {
		return nil, err
	}
	return &idbStore{dir: dir, blobDir: blobDir, ldb: d, schema: readSchema(d)}, nil
}

// idbRecord 是对象仓库里的一条记录。
type idbRecord struct {
	key    string // 主键的可读形式
	rawKey []byte // 完整 LevelDB 键
	val    []byte // 原始值（含 varint 版本号）
}

// records 按键序返回某个对象仓库的全部记录。
func (s *idbStore) records(db, store int64) []idbRecord {
	pre := encodePrefix(keyPrefix{db: db, store: store, index: 1})
	var out []idbRecord
	for k, e := range s.ldb.entries {
		if e.del || !bytes.HasPrefix([]byte(k), pre) {
			continue
		}
		kp, rest, ok := decodePrefix([]byte(k))
		if !ok || kp.db != db || kp.store != store || kp.index != 1 {
			continue
		}
		pk, _, ok := idbKey(rest, 0)
		if !ok {
			continue
		}
		out = append(out, idbRecord{key: pk, rawKey: []byte(k), val: e.val})
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].rawKey, out[j].rawKey) < 0 })
	return out
}

// decode 把一条记录的值解成 Go 值。
func (s *idbStore) decode(db, store int64, r idbRecord) (any, error) {
	_, p, ok := idbVarint(r.val) // 版本号
	if !ok {
		return nil, fmt.Errorf("%w: 记录值没有版本号", ErrUnsupportedFormat)
	}
	v8, idx, err := unwrapBlink(p)
	if errors.Is(err, errNeedBlob) {
		v8, err = s.blobValue(db, store, r, idx)
	}
	if err != nil {
		return nil, err
	}
	return decodeV8(v8)
}

// blobValue 读外部 blob 文件里的值。
func (s *idbStore) blobValue(db, store int64, r idbRecord, idx int) ([]byte, error) {
	_, rest, _ := decodePrefix(r.rawKey)
	bk := append(encodePrefix(keyPrefix{db: db, store: store, index: 3}), rest...)
	bv, ok := s.ldb.get(bk)
	if !ok {
		return nil, fmt.Errorf("%w: 找不到 blob 记录", ErrUnsupportedFormat)
	}
	num, ok := blobNumber(bv, idx)
	if !ok {
		return nil, fmt.Errorf("%w: blob 记录不认识", ErrUnsupportedFormat)
	}
	data, err := readWhole(blobPath(s.blobDir, db, num))
	if err != nil {
		return nil, err
	}
	v8, _, err := unwrapBlink(data)
	return v8, err
}
