package applog

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 三个平台的分支都在这里验，而不是只验本机那一条。
//
// 这条用例的由来：日志轮转那条用例只在 Windows 上红，而原因是测试用 HOME
// 做隔离，可 Windows 那条分支读的是 %LOCALAPPDATA% —— 于是所有用例写进同一个
// 真实目录，前一个用例的文件句柄还开着，Windows 下占用中的文件改不了名。
// 在 macOS 上开发时，走不到的那条分支等于没有被任何东西看着。
func TestDirForEachPlatform(t *testing.T) {
	t.Setenv("HOME", "/h")
	t.Setenv("LOCALAPPDATA", `C:\Users\me\AppData\Local`)
	t.Setenv("XDG_STATE_HOME", "/h/.local/state")

	cases := []struct{ goos, want string }{
		// 隔离靠的就是这一条：Windows 不看 HOME。
		{"windows", filepath.Join(`C:\Users\me\AppData\Local`, "mcp-hub", "logs")},
		{"darwin", filepath.Join("/h", "Library", "Logs", "mcp-hub")},
		{"linux", filepath.Join("/h/.local/state", "mcp-hub")},
	}
	for _, c := range cases {
		got, err := dirFor(c.goos, "mcp-hub")
		if err != nil {
			t.Errorf("%s: %v", c.goos, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: %q，想要 %q", c.goos, got, c.want)
		}
	}

	// HOME 变了而 Windows 的结果不该跟着变 —— 把那次红的根因钉死。
	t.Setenv("HOME", "/another")
	got, _ := dirFor("windows", "mcp-hub")
	if !strings.HasPrefix(got, `C:\Users\me\AppData\Local`) {
		t.Errorf("Windows 分支跟着 HOME 变了：%q", got)
	}
}

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

// isolateLogDir 把日志目录指到一个新的临时位置。
//
// 三个变量都要设，因为 applog.Dir 按系统走不同分支 —— 而 Windows 那条用
// os.UserCacheDir()，它读的是 %LocalAppData%，**不是** HOME。只设 HOME 在
// macOS / Linux 上看着管用，在 Windows 上却让所有用例写进同一个真实目录；
// 前一个用例还开着文件句柄，而 Windows 下占用中的文件改不了名，于是轮转
// 那条用例失败。这正是 CI 里 Windows 红、另两个平台绿的原因。
func isolateLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pointLogDirAt(t, dir)
	// 断言隔离真的生效。不加这一条的话，漏设某个变量只会在**那个平台**上
	// 以别的症状暴露（Windows 上是一句看不懂的 rename 失败），而其余平台
	// 照常绿 —— 这次就是这么红的。现在任何平台漏了都会直接报在这里。
	got, err := Dir("mcp-hub-test")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("日志目录没有被隔离：%s 不在 %s 下。"+
			"applog.Dir 按系统走不同分支，三个环境变量都要设（见 pointLogDirAt）", got, dir)
	}
	return dir
}

// pointLogDirAt 把日志目录指到一个指定路径（用来构造「建不出目录」的情形）。
func pointLogDirAt(t *testing.T, base string) {
	t.Helper()
	t.Setenv("HOME", base)                                   // macOS: ~/Library/Logs
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state")) // Linux
	t.Setenv("LOCALAPPDATA", base)                           // Windows: os.UserCacheDir()
}

// Open 必须真的能写进去，而且同时还能往 stderr 走。
func TestOpenWritesToFile(t *testing.T) {
	isolateLogDir(t)
	lgf, err := Open("mcp-hub-test")
	// 必须关掉：Windows 上删不掉被占用的文件，不关的话 t.TempDir() 的清理
	// 会失败（而且只在 Windows 上失败）。
	t.Cleanup(func() { lgf.Close() })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if lgf.Path == "" {
		t.Fatal("路径是空的")
	}
	path := lgf.Path
	lg := log.New(lgf.Writer, "", 0)
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
	pointLogDirAt(t, bad)
	lgf, err := Open("mcp-hub-test")
	t.Cleanup(func() { lgf.Close() })
	if err == nil {
		t.Skip("这个平台下仍然能建目录，跳过")
	}
	if lgf == nil || lgf.Writer == nil {
		t.Fatal("退回的 Writer 是 nil —— 调用方会往 nil 上写")
	}
	if lgf.Path != "" {
		t.Errorf("失败时不该回一个路径，否则页面会显示一个点不开的日志入口: %q", lgf.Path)
	}
}

// 超过上限时把旧的挪成 .old，而不是无限增长——用户机器上没人会去清它。
func TestOpenRotatesOversizeLog(t *testing.T) {
	isolateLogDir(t)
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
	lgf, err := Open("mcp-hub-test")
	t.Cleanup(func() { lgf.Close() })
	if err != nil {
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

// Close 必须真的释放文件句柄，而不是只把字段置空。
//
// 这条用例的由来：原来 Open 把文件句柄藏在 MultiWriter 里，谁都关不掉。
// 在 macOS / Linux 上看不出问题（能删掉打开着的文件），只有 Windows 上
// t.TempDir() 的清理会失败。所以这里不验「目录能不能删」，而验「关掉之后
// 还能不能写进去」—— 那个事实在三个平台上都一样。
func TestCloseReleasesTheFile(t *testing.T) {
	isolateLogDir(t)
	lgf, err := Open("mcp-hub-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := lgf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lgf.Close(); err != nil {
		t.Errorf("重复 Close 应当是空操作，得到 %v", err)
	}
	if _, err := lgf.Writer.Write([]byte("x")); err == nil {
		t.Error("Close 之后还能写进文件，说明句柄没被释放")
	}
}
