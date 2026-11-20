package applemail

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// mode=ro 的读连接是进程里最后一个连接时，关闭时也不能做检查点：主库和
// 未合并的 -wal 都必须原样不动（Mail.app 没在跑、WAL 里还有数据的情况）。
//
// 注意这里直接用 mode=ro 打开、**不**断言 -shm：SQLite 读 WAL 库必须有 -shm，
// 没有时会新建一个。所以 Open 只在 -wal、-shm 都存在时才用 mode=ro，
// 见 sidecar_test.go 的 TestOpenNeverCreatesSidecars。
func TestReadOnlyOpenLeavesDBAndWALUntouched(t *testing.T) {
	dir := t.TempDir()
	w := filepath.Join(dir, "writer.db")
	wdb, err := sql.Open("sqlite", "file:"+w+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer wdb.Close()
	wdb.SetMaxOpenConns(1)
	if _, err := wdb.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := wdb.Exec("INSERT INTO t VALUES(?)", i); err != nil {
			t.Fatal(err)
		}
	}
	// 写连接还开着时复制主库和 -wal：副本就是「WAL 里有未合并数据、没有任何连接」。
	p := filepath.Join(dir, "Envelope Index")
	want := map[string][]byte{}
	for _, suf := range []string{"", "-wal"} {
		b, err := os.ReadFile(w + suf)
		if err != nil || (suf == "-wal" && len(b) == 0) {
			t.Fatalf("准备夹具失败（%s）: %v", suf, err)
		}
		if err := os.WriteFile(p+suf, b, 0o600); err != nil {
			t.Fatal(err)
		}
		want[suf] = b
	}

	db, err := openRO(p, false)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 50 {
		t.Fatalf("应读到 WAL 里的 50 行，实际 %d（%v）", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for suf, b := range want {
		got, err := os.ReadFile(p + suf)
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("只读连接关闭后 %q 被改动了（%v）", "Envelope Index"+suf, err)
		}
	}
}
