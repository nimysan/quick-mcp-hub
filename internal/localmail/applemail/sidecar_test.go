package applemail

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

// listDir 返回目录里的文件名和字节，用来断言读完之后目录没有变化。
func listDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func names(m map[string][]byte) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

// walSidecars 在 db 的副本上开一个写连接、写入一行留在 WAL 里，
// 趁写连接还开着取主库、-wal、-shm 的字节：等于「WAL 里有未合并数据、Mail 刚崩溃」的现场。
func walSidecars(t *testing.T, db string) (main, wal, shm []byte) {
	t.Helper()
	w := filepath.Join(t.TempDir(), "w.db")
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w, b, 0o600); err != nil {
		t.Fatal(err)
	}
	wdb, err := sql.Open("sqlite", "file:"+w+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer wdb.Close()
	wdb.SetMaxOpenConns(1)
	if _, err := wdb.Exec(`INSERT INTO subjects VALUES (777,'只在 WAL 里')`); err != nil {
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

// M1：Mail 的数据目录里绝不能新建文件。缺 -wal 或 -shm 时必须用 immutable=1，
// 两个都在才用 mode=ro；三种情况下读完之后 MailData 目录的文件清单和字节都不变。
func TestOpenNeverCreatesSidecars(t *testing.T) {
	cases := []struct {
		name          string
		wal, shm      bool
		wantImmutable bool
	}{
		{"两个都不在（Mail 正常退出）", false, false, true},
		{"只有 -wal", true, false, true},
		{"两个都在（Mail 在运行或刚崩溃）", true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := buildFixture(t)
			if c.wal {
				main, wal, shm := walSidecars(t, f.db)
				os.WriteFile(f.db, main, 0o600)
				os.WriteFile(f.db+"-wal", wal, 0o600)
				if c.shm {
					os.WriteFile(f.db+"-shm", shm, 0o600)
				}
			}
			dir := filepath.Dir(f.db)
			before := listDir(t, dir)

			s, err := Open(f.root)
			if err != nil {
				t.Fatal(err)
			}
			if s.Immutable() != c.wantImmutable {
				t.Errorf("Immutable() = %v，想要 %v", s.Immutable(), c.wantImmutable)
			}
			ctx := context.Background()
			if _, err := s.ListMailboxes(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ListMessages(ctx, Query{}); err != nil {
				t.Fatal(err)
			}
			s.GetMessage(ctx, 1, 0)
			if c.wal && c.shm { // mode=ro 应能看到 WAL 里的新数据
				var n int
				s.db.QueryRow(`SELECT count(*) FROM subjects WHERE ROWID=777`).Scan(&n)
				if n != 1 {
					t.Error("mode=ro 应读到 WAL 里未合并的行")
				}
			}
			if s.Stale() {
				t.Error("刚打开不应过期")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			after := listDir(t, dir)
			if !reflect.DeepEqual(names(before), names(after)) {
				t.Fatalf("MailData 文件清单变了：读之前 %v，读之后 %v", names(before), names(after))
			}
			for _, n := range []string{"Envelope Index", "Envelope Index-wal"} {
				if b, ok := before[n]; ok && !bytes.Equal(b, after[n]) {
					t.Errorf("%s 的字节被改动了", n)
				}
			}
		})
	}
}

// immutable 打开之后 Mail 启动（-wal、-shm 都出现了），Stale 要报告过期，
// Hub 据此重新打开，换成能看到最新数据的 mode=ro。
func TestStaleAfterMailStarts(t *testing.T) {
	f := buildFixture(t)
	s, err := Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Immutable() || s.Stale() {
		t.Fatalf("没有 -wal/-shm 时应 immutable 且不过期：immutable=%v stale=%v", s.Immutable(), s.Stale())
	}
	os.WriteFile(f.db+"-wal", nil, 0o600)
	if s.Stale() {
		t.Error("只出现 -wal 时还不能换 mode=ro（会新建 -shm）")
	}
	os.WriteFile(f.db+"-shm", nil, 0o600)
	if !s.Stale() {
		t.Error("-wal、-shm 都出现后应报告过期")
	}
}
