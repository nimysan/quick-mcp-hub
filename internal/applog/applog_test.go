package applog

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 日志目录按各系统的惯例走。只验「路径里带 namespace 且是绝对路径」——
// 具体位置是平台约定，钉死字面值只会让这个测试在换平台时假失败。
func TestDirIsAbsoluteAndNamespaced(t *testing.T) {
	dir, err := Dir("mcp-hub")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("不是绝对路径: %s", dir)
	}
	if !strings.Contains(dir, "mcp-hub") {
		t.Errorf("路径里没有 namespace: %s", dir)
	}
}

// Open 必须真的能写进去，而且同时还能往 stderr 走。
func TestOpenWritesToFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	w, path, err := Open("mcp-hub-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if path == "" {
		t.Fatal("路径是空的")
	}
	lg := log.New(w, "", 0)
	lg.Println("写进日志的一行")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("日志文件读不到: %v", err)
	}
	if !strings.Contains(string(b), "写进日志的一行") {
		t.Errorf("文件内容: %q", b)
	}
}

// 日志写不进去**不能**是致命的：没有人会因为日志目录不可写就接受程序起不来。
func TestOpenFallsBackToStderr(t *testing.T) {
	// 把 HOME 指到一个文件上，于是 MkdirAll 必然失败。
	bad := filepath.Join(t.TempDir(), "不是目录")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", bad)
	t.Setenv("XDG_STATE_HOME", filepath.Join(bad, "state"))
	w, path, err := Open("mcp-hub-test")
	if err == nil {
		t.Skip("这个平台下仍然能建目录，跳过")
	}
	if w == nil {
		t.Fatal("退回的 writer 是 nil —— 调用方会往 nil 上写")
	}
	if path != "" {
		t.Errorf("失败时不该回一个路径，否则页面会显示一个点不开的日志入口: %q", path)
	}
}

// 超过上限时把旧的挪成 .old，而不是无限增长——用户机器上没人会去清它。
func TestOpenRotatesOversizeLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	path, err := Path("mcp-hub-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open("mcp-hub-test"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Errorf("新日志应当是空的，size=%v err=%v", fi, err)
	}
	if _, err := os.Stat(path + ".old"); err != nil {
		t.Errorf("旧日志应当被挪成 .old: %v", err)
	}
}

// Tail 只读尾部：这个文件可能有兆级，而排查时要看的总是最后那段。
func TestTailReturnsEndAndSaysSoWhenTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	body := strings.Repeat("A", 1000) + "最后一行"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Tail(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "最后一行") {
		t.Error("没有读到结尾")
	}
	// 截断了就要说出来，否则用户会以为自己看到的是全部日志。
	if !strings.Contains(out, "只显示最后") {
		t.Error("截断了却没有提示")
	}
	if strings.Count(out, "A") > 200 {
		t.Error("读得太多了，没有按尾部截")
	}
}
