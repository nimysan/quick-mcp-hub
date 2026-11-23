package olkwin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 解析器的 fuzz 测试：损坏或恶意的 olk 缓存文件只能让读取返回错误，不能 panic、死循环或爆内存。
// 跑法：go test -run '^$' -fuzz FuzzTable -fuzztime 2m ./internal/localmail/olkwin/

func fixtureFiles(f *testing.F) (table, log, manifest []byte) {
	root := writeFixture(f, f.TempDir(), fixtureOpt{})
	dir := filepath.Join(root, "https_outlook.office.com_0.indexeddb.leveldb")
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	return read("000005.ldb"), read("000007.log"), read("MANIFEST-000002")
}

func keepAll([]byte) bool { return true }

func FuzzSnappy(f *testing.F) {
	f.Add(snappyLiteral([]byte("hello hello hello")))
	f.Add([]byte{0x40, 0x00, 'a', 0xfe, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		out, err := snappyDecode(b)
		if err == nil && len(out) > maxSnappyLen {
			t.Fatalf("解压结果 %d 字节超过上限", len(out))
		}
	})
}

// FuzzValue 覆盖 Blink 包装、V8 反序列化，以及从解出的对象取字段（summaryOf）。
func FuzzValue(f *testing.F) {
	alice := mbx("A", "a@example.com")
	f.Add(ssv(itemRow("M1", "F", "s", alice, fxRecv1, true, false, "p")))
	f.Add(ssv(bodyRec("M1", "F", "s", alice, fxRecv1, "<p>x</p>", []any{o("Name", "n", "Size", 1.0)})))
	f.Add(append([]byte{0xFF, 0x11, 0x02}, snappyLiteral(ssv(o("a", []any{1.0, "x"})))...))
	f.Add([]byte{0xFF, 0x11, 0x01, 0x80, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		v8, idx, err := unwrapBlink(b)
		if err != nil {
			blobNumber(b, idx)
			return
		}
		v, err := decodeV8(v8)
		if err != nil {
			return
		}
		if obj, ok := v.(*jsObject); ok {
			summaryOf(obj)
		}
	})
}

// FuzzKey 覆盖 IndexedDB 键前缀、主键、带长度字符串和 blob 记录。
func FuzzKey(f *testing.F) {
	f.Add(dataKey(3, 7, 1, "AAMkAD-id"))
	f.Add(dbNameKey("owa-offline-data-x"))
	f.Add(storeNameKey(3, 7))
	f.Fuzz(func(t *testing.T, b []byte) {
		keepKey(b)
		if _, rest, ok := decodePrefix(b); ok {
			idbKey(rest, 0)
		}
		idbKey(b, 0)
		idbStringWithLength(b)
		for i := 0; i < 3; i++ {
			blobNumber(b, i)
		}
	})
}

// FuzzLog 覆盖日志分片、写批次和 MANIFEST 版本编辑。
func FuzzLog(f *testing.F) {
	_, lg, mf := fixtureFiles(f)
	f.Add(lg)
	f.Add(mf)
	f.Fuzz(func(t *testing.T, b []byte) {
		d := &ldbDB{entries: map[string]ldbEntry{}}
		readLog(d, b, keepAll)
		recs, _ := logRecords(b)
		m := &manifest{tables: map[uint64]bool{}}
		for _, r := range recs {
			m.apply(r)
		}
	})
}

// FuzzTable 覆盖 SSTable（块、CRC、Snappy、前缀压缩），再把读到的键值当成整库走一遍 build/List/Get。
func FuzzTable(f *testing.F) {
	tb, _, _ := fixtureFiles(f)
	f.Add(tb)
	none := filepath.Join(f.TempDir(), "no-blobs") // blob 目录不存在：blob 值走错误路径
	f.Fuzz(func(t *testing.T, b []byte) {
		d := &ldbDB{entries: map[string]ldbEntry{}}
		if readTable(d, b, keepKey) != nil {
			return
		}
		s := &Store{maxBody: 1 << 10, byID: map[int64]*Folder{}, byMsg: map[int64]msgRef{}}
		s.dbs = []*idbStore{{dir: none, blobDir: none, ldb: d, schema: readSchema(d)}}
		if s.build() != nil {
			return
		}
		ctx := context.Background()
		p, _ := s.List(ctx, Query{Limit: 200})
		for _, m := range p.Items {
			s.Get(ctx, m.ID)
		}
	})
}

// TestLengthOverflow：长度字段接近 2^64 时，边界检查里的加法 / 乘法不能溢出（以前会 panic 退出整个服务）。
func TestLengthOverflow(t *testing.T) {
	data := make([]byte, 64)
	for _, size := range []uint64{^uint64(0), ^uint64(0) - 2, ^uint64(0) - 4} {
		if _, err := tableBlock(data, 10, size); err == nil {
			t.Errorf("块大小 %#x 应报损坏", size)
		}
	}
	if _, err := tableBlock(data, 10, 50); err == nil { // 正好没有 5 字节尾部
		t.Error("缺块尾应报损坏")
	}
	for _, n := range []uint64{1 << 62, 1<<63 - 1, 1<<62 + 3} {
		p := append(binaryUvarint(n), 'a', 0, 'b', 0)
		if _, _, ok := idbStringWithLength(p); ok {
			t.Errorf("字符数 %#x 应报损坏", n)
		}
	}
}

func binaryUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}
