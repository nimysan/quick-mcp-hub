package outlooklegacy

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func dirSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range es {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func keys(m map[string][]byte) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// liveWAL 把 db 转成 WAL 库，在副本上开写连接写入一行留在 WAL 里，
// 趁写连接开着取主库、-wal、-shm 的字节（Outlook 运行中或刚崩溃的现场）。
func liveWAL(t *testing.T, db string) (main, wal, shm []byte) {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w.db")
	b, _ := os.ReadFile(db)
	os.WriteFile(w, b, 0o600)
	wdb, err := sql.Open("sqlite", "file:"+w+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer wdb.Close()
	wdb.SetMaxOpenConns(1)
	if _, err := wdb.Exec(`INSERT INTO Folders VALUES (99,'',1,10,0,0,'只在 WAL 里')`); err != nil {
		t.Fatal(err)
	}
	read := func(p string) []byte {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			t.Fatalf("准备夹具失败（%s）: %v", p, err)
		}
		return b
	}
	return read(w), read(w + "-wal"), read(w + "-shm")
}

// M1：Data 目录里绝不能新建文件。-wal 为空或缺、或 -shm 缺时用 immutable=1，
// 读完之后文件清单和字节都不变。
func TestOpenNeverCreatesSidecars(t *testing.T) {
	cases := []struct {
		name          string
		wal, shm      bool
		wantImmutable bool
	}{
		{"两个都不在", false, false, true},
		{"只有 -wal（有未合并数据）", true, false, true},
		{"两个都在", true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := buildFixture(t)
			db := filepath.Join(dir, "Outlook.sqlite")
			main, wal, shm := liveWAL(t, db) // 真实的 Outlook.sqlite 是 WAL 库
			os.WriteFile(db, main, 0o600)
			if c.wal {
				os.WriteFile(db+"-wal", wal, 0o600)
			}
			if c.shm {
				os.WriteFile(db+"-shm", shm, 0o600)
			}
			before := dirSnapshot(t, dir)

			s, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if s.Immutable() != c.wantImmutable {
				t.Errorf("Immutable() = %v，想要 %v", s.Immutable(), c.wantImmutable)
			}
			ctx := context.Background()
			if _, err := s.Folders(ctx); err != nil {
				t.Fatal(err)
			}
			s.List(ctx, Query{})
			s.Get(ctx, 1)
			if s.Stale() {
				t.Error("刚打开不应过期")
			}
			s.Close()

			after := dirSnapshot(t, dir)
			if !reflect.DeepEqual(keys(before), keys(after)) {
				t.Fatalf("Data 目录文件清单变了：读之前 %v，读之后 %v", keys(before), keys(after))
			}
			for _, n := range []string{"Outlook.sqlite", "Outlook.sqlite-wal"} {
				if b, ok := before[n]; ok && !bytes.Equal(b, after[n]) {
					t.Errorf("%s 的字节被改动了", n)
				}
			}
		})
	}
}

func TestStaleAfterOutlookWrites(t *testing.T) {
	dir := buildFixture(t)
	db := filepath.Join(dir, "Outlook.sqlite")
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Immutable() || s.Stale() {
		t.Fatal("没有 -wal 时应 immutable 且不过期")
	}
	os.WriteFile(db+"-wal", []byte("x"), 0o600)
	if s.Stale() {
		t.Error("只有 -wal 时还不能换 mode=ro（会新建 -shm）")
	}
	os.WriteFile(db+"-shm", nil, 0o600)
	if !s.Stale() {
		t.Error("-wal 有数据且 -shm 在时应报告过期")
	}
	imm := true
	forced, _ := Open(dir, Options{Immutable: &imm})
	defer forced.Close()
	if forced.Stale() {
		t.Error("调用方指定的 immutable 不应报告过期")
	}
}
