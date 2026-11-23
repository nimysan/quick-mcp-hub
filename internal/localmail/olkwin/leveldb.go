package olkwin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// 这里是 LevelDB 的只读解析，只做 IndexedDB 需要的部分：
// CURRENT → MANIFEST（版本编辑日志）得到活跃的 .ldb 表文件和 .log 日志号，
// 逐个读表文件的数据块和日志里的写批次，按序号合并出每个键的最新值。
// 不打开 LOCK，不写任何文件，不做压实。每个文件读完立即关闭，并且以允许别人删除的共享模式打开，
// 所以 olk 运行时照常压实、删除旧文件，不会被我们挡住。
//
// 格式依据：LevelDB 源码里的 doc/table_format.md、doc/log_format.md 和 db/version_edit.cc（BSD-3）。

var errCorrupt = errors.New("leveldb 数据损坏")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// unmaskCRC 还原 LevelDB 存盘时做过掩码的 CRC32C。
func unmaskCRC(m uint32) uint32 {
	rot := m - 0xa282ead8
	return rot>>17 | rot<<15
}

// maxFileBytes 限制单个 LevelDB 文件的大小。Chromium 的表文件默认约 2 MB，日志 4 MB 左右。
const maxFileBytes = 512 << 20

// readWhole 只读、共享读写删地打开文件，读完全部内容后立即关闭。
func readWhole(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > maxFileBytes {
		return nil, fmt.Errorf("%w: %s 太大（%d 字节）", ErrUnsupportedFormat, filepath.Base(path), st.Size())
	}
	return io.ReadAll(io.LimitReader(f, maxFileBytes))
}

// ldbEntry 是合并后的一条记录。
type ldbEntry struct {
	seq uint64
	del bool
	val []byte
}

// ldbDB 是某一时刻读到的整库快照（只保留 keep 认为有用的键）。
type ldbDB struct {
	entries    map[string]ldbEntry
	comparator string
	files      []string // 读过的文件名，用于诊断
}

// get 返回键的最新值。
func (d *ldbDB) get(k []byte) ([]byte, bool) {
	e, ok := d.entries[string(k)]
	if !ok || e.del {
		return nil, false
	}
	return e.val, true
}

// put 按序号合并：序号大的胜出。
func (d *ldbDB) put(k []byte, seq uint64, del bool, v []byte) {
	if old, ok := d.entries[string(k)]; ok && old.seq >= seq {
		return
	}
	var cp []byte
	if !del {
		cp = append([]byte(nil), v...)
	}
	d.entries[string(k)] = ldbEntry{seq: seq, del: del, val: cp}
}

// manifest 是 MANIFEST 回放后的版本状态。
type manifest struct {
	comparator string
	logNumber  uint64
	prevLog    uint64
	tables     map[uint64]bool
}

// readManifest 读 CURRENT 指向的 MANIFEST。
func readManifest(dir string) (*manifest, error) {
	cur, err := readWhole(filepath.Join(dir, "CURRENT"))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(string(cur))
	if !strings.HasPrefix(name, "MANIFEST-") || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("%w: CURRENT 内容不认识", ErrUnsupportedFormat)
	}
	data, err := readWhole(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	m := &manifest{tables: map[uint64]bool{}}
	recs, _ := logRecords(data) // MANIFEST 可能正被追加，末尾半条记录忽略
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: MANIFEST 为空", ErrUnsupportedFormat)
	}
	for _, r := range recs {
		if err := m.apply(r); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// apply 回放一条 VersionEdit。
func (m *manifest) apply(r []byte) error {
	bad := fmt.Errorf("%w: MANIFEST 版本编辑不认识", ErrUnsupportedFormat)
	for len(r) > 0 {
		tag, n := binary.Uvarint(r)
		if n <= 0 {
			return bad
		}
		r = r[n:]
		uv := func() (uint64, bool) {
			v, n := binary.Uvarint(r)
			if n <= 0 {
				return 0, false
			}
			r = r[n:]
			return v, true
		}
		str := func() ([]byte, bool) {
			l, ok := uv()
			if !ok || l > uint64(len(r)) {
				return nil, false
			}
			s := r[:l]
			r = r[l:]
			return s, true
		}
		ok := true
		switch tag {
		case 1: // comparator
			var s []byte
			s, ok = str()
			m.comparator = string(s)
		case 2:
			m.logNumber, ok = uv()
		case 3, 4: // next file number, last sequence
			_, ok = uv()
		case 5: // compact pointer
			if _, ok = uv(); ok {
				_, ok = str()
			}
		case 6: // deleted file
			var num uint64
			if _, ok = uv(); ok {
				num, ok = uv()
				delete(m.tables, num)
			}
		case 7: // new file
			var num uint64
			if _, ok = uv(); ok {
				if num, ok = uv(); ok {
					if _, ok = uv(); ok {
						if _, ok = str(); ok {
							_, ok = str()
						}
					}
				}
			}
			m.tables[num] = true
		case 9:
			m.prevLog, ok = uv()
		default:
			return bad
		}
		if !ok {
			return bad
		}
	}
	return nil
}

// logRecords 解析 LevelDB 日志格式（.log 和 MANIFEST 共用），返回完整的逻辑记录。
// 遇到 CRC 不对或者半条记录就停：正在被写的日志末尾本来就可能不完整，前面的记录仍然有效。
// 第二个返回值表示是否因为损坏提前停止。
func logRecords(data []byte) ([][]byte, bool) {
	const block = 32768
	var out [][]byte
	var cur []byte
	inFrag := false
	for off := 0; off < len(data); {
		left := block - off%block
		if left < 7 {
			off += left
			continue
		}
		if off+7 > len(data) {
			return out, true
		}
		h := data[off : off+7]
		crc := binary.LittleEndian.Uint32(h)
		ln := int(binary.LittleEndian.Uint16(h[4:]))
		typ := h[6]
		if typ == 0 && ln == 0 { // 预分配的零区
			off += left
			continue
		}
		if off+7+ln > len(data) || 7+ln > left {
			return out, true
		}
		payload := data[off+7 : off+7+ln]
		c := crc32.Update(crc32.Checksum([]byte{typ}, crcTable), crcTable, payload)
		if c != unmaskCRC(crc) {
			return out, true
		}
		off += 7 + ln
		switch typ {
		case 1: // FULL
			out = append(out, payload)
			inFrag = false
		case 2: // FIRST
			cur = append([]byte(nil), payload...)
			inFrag = true
		case 3: // MIDDLE
			if inFrag {
				cur = append(cur, payload...)
			}
		case 4: // LAST
			if inFrag {
				out = append(out, append(cur, payload...))
			}
			inFrag = false
		default:
			return out, true
		}
	}
	return out, false
}

// readLog 把一个 .log 文件里的写批次合并进 d。
func readLog(d *ldbDB, data []byte, keep func([]byte) bool) error {
	recs, _ := logRecords(data)
	for _, r := range recs {
		if len(r) < 12 {
			return errCorrupt
		}
		seq := binary.LittleEndian.Uint64(r)
		cnt := binary.LittleEndian.Uint32(r[8:])
		p := r[12:]
		for i := uint32(0); i < cnt; i++ {
			if len(p) < 1 {
				return errCorrupt
			}
			t := p[0]
			p = p[1:]
			k, rest, ok := lenPrefixed(p)
			if !ok {
				return errCorrupt
			}
			p = rest
			switch t {
			case 1:
				v, rest, ok := lenPrefixed(p)
				if !ok {
					return errCorrupt
				}
				p = rest
				if keep(k) {
					d.put(k, seq+uint64(i), false, v)
				}
			case 0:
				if keep(k) {
					d.put(k, seq+uint64(i), true, nil)
				}
			default:
				return errCorrupt
			}
		}
	}
	return nil
}

func lenPrefixed(p []byte) ([]byte, []byte, bool) {
	l, n := binary.Uvarint(p)
	if n <= 0 || l > uint64(len(p)-n) {
		return nil, nil, false
	}
	return p[n : n+int(l)], p[n+int(l):], true
}

// readTable 把一个 .ldb 表文件合并进 d。
func readTable(d *ldbDB, data []byte, keep func([]byte) bool) error {
	if len(data) < 48 {
		return errCorrupt
	}
	foot := data[len(data)-48:]
	if binary.LittleEndian.Uint64(foot[40:]) != 0xdb4775248b80fb57 {
		return fmt.Errorf("%w: 表文件魔数不对", ErrUnsupportedFormat)
	}
	p := foot
	if _, _, ok := handle(&p); !ok { // metaindex
		return errCorrupt
	}
	ioff, isz, ok := handle(&p)
	if !ok {
		return errCorrupt
	}
	idx, err := tableBlock(data, ioff, isz)
	if err != nil {
		return err
	}
	return blockEntries(idx, func(_, v []byte) error {
		o, s, ok := handle(&v)
		if !ok {
			return errCorrupt
		}
		blk, err := tableBlock(data, o, s)
		if err != nil {
			return err
		}
		return blockEntries(blk, func(ik, v []byte) error {
			if len(ik) < 8 {
				return errCorrupt
			}
			uk := ik[:len(ik)-8]
			tag := binary.LittleEndian.Uint64(ik[len(ik)-8:])
			if keep(uk) {
				d.put(uk, tag>>8, tag&0xff == 0, v)
			}
			return nil
		})
	})
}

func handle(p *[]byte) (uint64, uint64, bool) {
	o, n := binary.Uvarint(*p)
	if n <= 0 {
		return 0, 0, false
	}
	*p = (*p)[n:]
	s, n := binary.Uvarint(*p)
	if n <= 0 {
		return 0, 0, false
	}
	*p = (*p)[n:]
	return o, s, true
}

// tableBlock 取出一个块并校验、解压。
func tableBlock(data []byte, off, size uint64) ([]byte, error) {
	if off > uint64(len(data)) || size > uint64(len(data))-off || uint64(len(data))-off-size < 5 { // 不能写 size+5：size 很大时加法溢出
		return nil, errCorrupt
	}
	b := data[off : off+size]
	typ := data[off+size]
	crc := binary.LittleEndian.Uint32(data[off+size+1:])
	c := crc32.Update(crc32.Checksum(b, crcTable), crcTable, []byte{typ})
	if c != unmaskCRC(crc) {
		return nil, errCorrupt
	}
	switch typ {
	case 0:
		return b, nil
	case 1:
		return snappyDecode(b)
	default:
		return nil, fmt.Errorf("%w: 表块压缩类型 %d", ErrUnsupportedFormat, typ)
	}
}

// blockEntries 遍历一个块里的全部键值（前缀压缩的键）。
func blockEntries(b []byte, fn func(k, v []byte) error) error {
	if len(b) < 4 {
		return errCorrupt
	}
	nr := binary.LittleEndian.Uint32(b[len(b)-4:])
	if uint64(nr)*4+4 > uint64(len(b)) {
		return errCorrupt
	}
	end := len(b) - 4 - int(nr)*4
	var key []byte
	for p := b[:end]; len(p) > 0; {
		shared, n1 := binary.Uvarint(p)
		if n1 <= 0 {
			return errCorrupt
		}
		p = p[n1:]
		ns, n2 := binary.Uvarint(p)
		if n2 <= 0 {
			return errCorrupt
		}
		p = p[n2:]
		vl, n3 := binary.Uvarint(p)
		if n3 <= 0 {
			return errCorrupt
		}
		p = p[n3:]
		if shared > uint64(len(key)) || ns > uint64(len(p)) || vl > uint64(len(p))-ns {
			return errCorrupt
		}
		key = append(key[:shared], p[:ns]...)
		v := p[ns : ns+vl]
		p = p[ns+vl:]
		if err := fn(key, v); err != nil {
			return err
		}
	}
	return nil
}

// fileNum 从 000123.ldb 这种文件名里取编号。
func fileNum(name, ext string) (uint64, bool) {
	if !strings.HasSuffix(name, ext) {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(name, ext), 10, 64)
	return n, err == nil
}

// openLevelDB 读出整库快照。olk 运行时可能正在压实：列出的文件读的时候已被删掉，就整体重来，最多 5 次。
func openLevelDB(dir string, keep func([]byte) bool) (*ldbDB, error) {
	var last error
	for try := 0; try < 5; try++ {
		d, err := openLevelDBOnce(dir, keep)
		if err == nil {
			return d, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

func openLevelDBOnce(dir string, keep func([]byte) bool) (*ldbDB, error) {
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	if m.comparator != "" && m.comparator != "idb_cmp1" {
		return nil, fmt.Errorf("%w: 比较器 %q 不是 IndexedDB 的 idb_cmp1", ErrUnsupportedFormat, m.comparator)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	d := &ldbDB{entries: map[string]ldbEntry{}, comparator: m.comparator}
	var tables, logs []uint64
	for num := range m.tables {
		tables = append(tables, num)
	}
	for _, e := range ents {
		if n, ok := fileNum(e.Name(), ".log"); ok && (n >= m.logNumber || (m.prevLog != 0 && n == m.prevLog)) {
			logs = append(logs, n)
		}
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i] < tables[j] })
	sort.Slice(logs, func(i, j int) bool { return logs[i] < logs[j] })
	for _, n := range tables {
		name := fmt.Sprintf("%06d.ldb", n)
		data, err := readWhole(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			name = fmt.Sprintf("%06d.sst", n)
			data, err = readWhole(filepath.Join(dir, name))
		}
		if err != nil {
			return nil, err
		}
		if err := readTable(d, data, keep); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		d.files = append(d.files, name)
	}
	for _, n := range logs {
		name := fmt.Sprintf("%06d.log", n)
		data, err := readWhole(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if err := readLog(d, data, keep); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		d.files = append(d.files, name)
	}
	return d, nil
}
