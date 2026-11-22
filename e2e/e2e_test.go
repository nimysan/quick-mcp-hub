// Package e2e 用构建好的 mcp-hub 二进制，按 Amazon Quick 的方式（`mcp-hub -serve`，
// MCP over stdio）跑端到端测试。
//
// 环境变量：
//
//	MCPHUB_E2E_BIN            被测二进制，默认 ../dist/mcp-hub；不存在时跳过全部用例
//	MCPHUB_E2E_REAL=1         打开读真实本机邮件的用例（只读；断言只看数量、字段名、类型、长度）
//	MCPHUB_E2E_APPLE_DIR      Apple Mail 目录，默认 ~/Library/Mail
//	MCPHUB_E2E_LEGACY_DIR     旧版 Outlook 的 Data 目录，默认 Main Profile/Data
//	MCPHUB_E2E_OUTLOOK_DIR    新版 Outlook 的配置文件目录，默认 Main Profile
//	MCPHUB_E2E_BASELINE_BIN   旧版（main 分支）二进制；设置后跑 7 个渠道的 tools/list 回归对比
//	MCPHUB_E2E_PST_DIR        （Windows）放 .pst / .ost 样本的目录；设置后用样本跑经典版 Outlook for Windows 的用例
//
// Mac 来源的用例只在 darwin 上跑，经典版 Outlook for Windows 的用例只在 windows 上跑：
// 服务按平台注册来源，别的平台上的来源本来就不存在。
//
// 每个被测进程用独立的临时 HOME 和 TMPDIR，不读写用户真实的 mcp-hub 配置。
// 没有任何用例往钥匙串写东西：假渠道只写 channels.json，凭据本来就是惰性读取的，
// 注册工具不需要它。
//
// 用例本身绝不打印邮件内容（主题、人名、地址、正文）：比较都在内存里做，
// 失败信息只带数量和字段名。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- 进程与配置 ----------

func binPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("MCPHUB_E2E_BIN")
	if p == "" {
		p = filepath.Join("..", "dist", "mcp-hub")
		if runtime.GOOS == "windows" {
			p += ".exe"
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("没有被测二进制 %s（先 make build，或设 MCPHUB_E2E_BIN）", abs)
	}
	return abs
}

// inst 是 channels.json 里的一条渠道。
type inst struct {
	Type     string            `json:"type"`
	Account  string            `json:"account"`
	Settings map[string]string `json:"settings,omitempty"`
	Enabled  bool              `json:"enabled"`
}

// newHome 建一个临时 HOME，写好 channels.json。
func newHome(t *testing.T, chans []inst) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(configBase(home), "mcp-hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"channels": chans})
	if err := os.WriteFile(filepath.Join(dir, "channels.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// configBase 是被测进程的 os.UserConfigDir()（由 procEnv 里的变量决定）。
func configBase(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support")
	case "windows":
		return filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(home, ".config")
}

// procEnv 是被测进程的最小环境：不继承 MCPHUB_MAIL_* 之类会额外注册渠道的变量。
func procEnv(home string) []string {
	env := []string{
		"HOME=" + home,
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"PATH=" + os.Getenv("PATH"),
		"USER=" + os.Getenv("USER"),
		"LANG=en_US.UTF-8",
	}
	if runtime.GOOS == "windows" {
		env = append(env,
			"USERPROFILE="+home,
			"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
			"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
			"TEMP="+filepath.Join(home, "tmp"),
			"TMP="+filepath.Join(home, "tmp"),
			"SystemRoot="+os.Getenv("SystemRoot"),
			"USERNAME="+os.Getenv("USERNAME"),
		)
	} else {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	}
	return env
}

// macOnly 跳过 Mac 来源的用例。
func macOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Mac 来源只在 macOS 上注册")
	}
}

// platformOn 是当前平台上全部来源都打开、目录都无效的设置。
func platformOn() map[string]string {
	if runtime.GOOS == "windows" {
		return map[string]string{"outlook_classic_win": "true", "outlook_classic_win_dir": `C:\nonexistent-e2e\x.pst`}
	}
	return map[string]string{"apple_mail": "true", "outlook_legacy": "true", "outlook": "true",
		"apple_mail_dir": "/nonexistent-e2e/a", "outlook_legacy_dir": "/nonexistent-e2e/b", "outlook_dir": "/nonexistent-e2e/c"}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type hub struct {
	cs     *mcp.ClientSession
	cmd    *exec.Cmd
	stderr *syncBuf
	home   string
}

// start 按 Quick 的方式拉起 `mcp-hub -serve` 并完成 MCP 握手。
func start(t *testing.T, bin, home string) (*hub, error) {
	t.Helper()
	cmd := exec.Command(bin, "-serve")
	cmd.Env = procEnv(home)
	errBuf := &syncBuf{}
	cmd.Stderr = errBuf
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "mcp-hub-e2e", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
	if err != nil {
		return &hub{cmd: cmd, stderr: errBuf, home: home}, err
	}
	h := &hub{cs: cs, cmd: cmd, stderr: errBuf, home: home}
	t.Cleanup(func() { _ = cs.Close() })
	return h, nil
}

func mustStart(t *testing.T, bin string, chans []inst) *hub {
	t.Helper()
	h, err := start(t, bin, newHome(t, chans))
	if err != nil {
		t.Fatalf("启动/握手失败: %v（stderr 尾部 %d 字节）", err, len(h.stderr.String()))
	}
	return h
}

func (h *hub) tools(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := h.cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	return r.Tools
}

// call 调一个工具。protoErr 是协议层错误；isErr 是工具层错误（isError=true），
// text 是错误时的说明文字（只用于 strings.Contains 断言，不打印）。
func (h *hub) call(t *testing.T, name string, args map[string]any, out any) (isErr bool, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := h.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// 协议错误也算「返回了清楚的错误」，但要让调用方能区分。
		return true, "PROTOCOL: " + err.Error()
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if r.IsError {
		return true, text
	}
	if out != nil {
		raw, _ := json.Marshal(r.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s 结果解不开: %v", name, err)
		}
	}
	return false, text
}

// alive 确认服务进程还在、还能应答。
func (h *hub) alive(t *testing.T) {
	t.Helper()
	if h.cmd.ProcessState != nil {
		t.Fatalf("服务进程已退出: %v", h.cmd.ProcessState)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.cs.Ping(ctx, nil); err != nil {
		t.Fatalf("服务不再应答: %v", err)
	}
}

func toolNames(ts []*mcp.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, x := range ts {
		out = append(out, x.Name)
	}
	sort.Strings(out)
	return out
}

func localNames(ts []*mcp.Tool) []string {
	var out []string
	for _, n := range toolNames(ts) {
		if strings.HasPrefix(n, "localmail_") {
			out = append(out, n)
		}
	}
	return out
}

// fakeGitHub 是一条假 GitHub 渠道：没有凭据，只为让服务有别的渠道可起。
var fakeGitHub = inst{Type: "github", Account: "e2e-fake-github", Enabled: true}

func localInst(enabled bool, s map[string]string) inst {
	return inst{Type: "localmail", Account: "本机", Enabled: enabled, Settings: s}
}

// ---------- 注册 ----------

func TestLocalMailRegistration(t *testing.T) {
	bin := binPath(t)
	allOn := platformOn()

	off := []struct {
		name  string
		chans []inst
	}{
		{"没有本机邮件渠道", []inst{fakeGitHub}},
		{"渠道已停用", []inst{fakeGitHub, localInst(false, allOn)}},
		{"渠道开着但一个来源都没勾", []inst{fakeGitHub, localInst(true, map[string]string{"apple_mail": "false"})}},
	}
	for _, c := range off {
		t.Run(c.name, func(t *testing.T) {
			h := mustStart(t, bin, c.chans)
			ts := h.tools(t)
			if n := localNames(ts); len(n) != 0 {
				t.Fatalf("不该注册本机邮件工具，实际 %v", n)
			}
			if len(ts) == 0 {
				t.Fatal("假 GitHub 渠道的工具也没注册")
			}
		})
	}

	t.Run("只有本机邮件且没勾来源时服务按设计退出", func(t *testing.T) {
		h, err := start(t, bin, newHome(t, []inst{localInst(true, nil)}))
		if err == nil {
			if _, lerr := h.cs.ListTools(context.Background(), nil); lerr == nil {
				t.Fatal("没有任何可用渠道，服务却正常应答")
			}
		}
		time.Sleep(300 * time.Millisecond)
		if !strings.Contains(h.stderr.String(), "没有任何可用渠道") {
			t.Fatalf("退出时没有说明原因（stderr %d 字节）", len(h.stderr.String()))
		}
	})

	t.Run("开启后正好 3 个只读工具", func(t *testing.T) {
		h := mustStart(t, bin, []inst{fakeGitHub, localInst(true, allOn)})
		ts := h.tools(t)
		want := []string{"localmail_get", "localmail_list_folders", "localmail_search"}
		if got := localNames(ts); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("本机邮件工具 = %v，想要 %v", got, want)
		}
		for _, x := range ts {
			if !strings.HasPrefix(x.Name, "localmail_") {
				continue
			}
			a := x.Annotations
			if a == nil || !a.ReadOnlyHint {
				t.Errorf("%s 没有 readOnlyHint", x.Name)
				continue
			}
			if a.DestructiveHint == nil || *a.DestructiveHint {
				t.Errorf("%s destructiveHint 应为 false", x.Name)
			}
			if a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Errorf("%s openWorldHint 应为 false", x.Name)
			}
		}
		if !strings.Contains(h.stderr.String(), "注册 3 个工具") {
			t.Error("服务日志里没有本机邮件注册记录")
		}
	})
}

// ---------- 异常路径（不需要真实数据） ----------

type folder struct {
	ID             int64  `json:"id"`
	Path           string `json:"path"`
	Total          int64  `json:"total"`
	NameIncomplete bool   `json:"name_incomplete"`
}

type folderGroup struct {
	Source  string   `json:"source"`
	Error   string   `json:"error"`
	Folders []folder `json:"folders"`
}

type foldersOut struct {
	Sources []folderGroup `json:"sources"`
}

type summary struct {
	ID          int64     `json:"id"`
	FolderID    int64     `json:"folder_id"`
	Subject     string    `json:"subject"`
	From        string    `json:"from"`
	FromAddress string    `json:"from_address"`
	Date        time.Time `json:"date"`
	Preview     string    `json:"preview"`
	Incomplete  []string  `json:"incomplete"`
}

type page struct {
	Source     string    `json:"source"`
	Count      int       `json:"count"`
	Messages   []summary `json:"messages"`
	HasMore    bool      `json:"has_more"`
	NextOffset int       `json:"next_offset"`
	Total      int       `json:"total"`
}

type message struct {
	summary
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
	BodyNote      string `json:"body_note"`
}

// noPermDir 建一个 chmod 000 的目录，模拟「数据目录无权限」。
func noPermDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "noperm")
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o700) }) // 让 TempDir 清理得掉
	return d
}

func TestLocalMailErrorPaths(t *testing.T) {
	macOnly(t)
	bin := binPath(t)
	junk := filepath.Join(t.TempDir(), "HxStore.hxd")
	if err := os.WriteFile(junk, bytes.Repeat([]byte("not-a-hxstore-file "), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	h := mustStart(t, bin, []inst{localInst(true, map[string]string{
		"apple_mail": "true", "outlook_legacy": "true", "outlook": "true",
		"apple_mail_dir":     noPermDir(t),
		"outlook_legacy_dir": filepath.Join(t.TempDir(), "does-not-exist"),
		"outlook_dir":        junk,
	})})

	t.Run("不指定来源列文件夹：每个来源各自报错，整体不失败", func(t *testing.T) {
		var out foldersOut
		if isErr, _ := h.call(t, "localmail_list_folders", nil, &out); isErr {
			t.Fatal("多来源时一个来源出错不该让整个调用失败")
		}
		if len(out.Sources) != 3 {
			t.Fatalf("应有 3 个来源分组，实际 %d", len(out.Sources))
		}
		for _, g := range out.Sources {
			if g.Error == "" {
				t.Errorf("%s：数据目录无效却没有 error", g.Source)
			}
			if g.Folders == nil {
				t.Errorf("%s：folders 应为空数组而不是 null", g.Source)
			}
		}
		h.alive(t)
	})

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string // 错误说明里应有的关键词
	}{
		{"无权限目录（普通文件权限，不是隐私保护）", "localmail_list_folders", map[string]any{"source": "apple_mail"}, "不是 macOS 隐私保护"},
		{"目录不存在", "localmail_list_folders", map[string]any{"source": "outlook_legacy"}, "未找到"},
		{"格式不认识", "localmail_search", map[string]any{"source": "outlook"}, ""},
		{"格式不认识第二次", "localmail_search", map[string]any{"source": "outlook"}, ""},
		{"非法 source", "localmail_search", map[string]any{"source": "thunderbird"}, "未启用"},
		{"多来源不填 source", "localmail_search", map[string]any{}, "source"},
		{"日期格式不对", "localmail_search", map[string]any{"source": "apple_mail", "since": "2024/01/01"}, "YYYY-MM-DD"},
		{"id 非正", "localmail_get", map[string]any{"source": "apple_mail", "id": -5}, "正整数"},
		{"get 非法 source", "localmail_get", map[string]any{"source": "../../etc", "id": 1}, "未启用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isErr, text := h.call(t, c.tool, c.args, nil)
			if !isErr {
				t.Fatal("应返回错误")
			}
			t.Logf("错误说明：%s", text) // 这里全是测试夹具，没有邮件内容
			if strings.HasPrefix(text, "PROTOCOL: ") {
				t.Errorf("应是工具错误（isError），实际是协议错误")
			}
			if c.want != "" && !strings.Contains(text, c.want) {
				t.Errorf("错误说明里没有 %q（说明长 %d 字）", c.want, len([]rune(text)))
			}
			h.alive(t)
		})
	}
}

// ---------- 真实数据 ----------

func realDirs(t *testing.T) map[string]string {
	t.Helper()
	macOnly(t)
	if os.Getenv("MCPHUB_E2E_REAL") != "1" {
		t.Skip("设 MCPHUB_E2E_REAL=1 才读真实本机邮件")
	}
	home, _ := os.UserHomeDir()
	prof := filepath.Join(home, "Library", "Group Containers", "UBF8T346G9.Office", "Outlook", "Outlook 15 Profiles", "Main Profile")
	pick := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return def
	}
	return map[string]string{
		"apple_mail":     pick("MCPHUB_E2E_APPLE_DIR", filepath.Join(home, "Library", "Mail")),
		"outlook_legacy": pick("MCPHUB_E2E_LEGACY_DIR", filepath.Join(prof, "Data")),
		"outlook":        pick("MCPHUB_E2E_OUTLOOK_DIR", prof),
	}
}

// watched 是只读核验要看的文件：主库和它的 -wal / -shm。
func watched(dirs map[string]string) map[string]string {
	apple := ""
	if m, _ := filepath.Glob(filepath.Join(dirs["apple_mail"], "V*", "MailData", "Envelope Index")); len(m) > 0 {
		apple = m[len(m)-1]
	}
	return map[string]string{
		"apple_mail":     apple,
		"outlook_legacy": filepath.Join(dirs["outlook_legacy"], "Outlook.sqlite"),
		"outlook":        filepath.Join(dirs["outlook"], "HxStore.hxd"),
	}
}

type fileStamp struct {
	Size  int64
	MTime time.Time
}

func stamp(paths map[string]string) map[string]fileStamp {
	out := map[string]fileStamp{}
	for _, p := range paths {
		for _, q := range []string{p, p + "-wal", p + "-shm"} {
			if fi, err := os.Stat(q); err == nil {
				out[q] = fileStamp{fi.Size(), fi.ModTime()}
			}
		}
	}
	return out
}

// openModes 用 lsof 列出进程打开的被监视文件及其访问方式（r / w / u）。
func openModes(t *testing.T, pid int, paths map[string]string) map[string]string {
	t.Helper()
	out, err := exec.Command("lsof", "-nP", "-p", fmt.Sprint(pid), "-F", "an").Output()
	if err != nil && len(out) == 0 {
		t.Logf("lsof 不可用: %v", err)
		return nil
	}
	modes := map[string]string{}
	mode := ""
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			mode = ""
		case 'a':
			mode = strings.TrimSpace(line[1:])
		case 'n':
			name := line[1:]
			for _, p := range paths {
				if p != "" && strings.HasPrefix(name, p) {
					modes[name] += mode + ";"
				}
			}
		}
	}
	return modes
}

// leakSet 收集结果里的邮件文本，最后核对服务日志里没有它们。只在内存里。
type leakSet map[string]struct{}

func (l leakSet) add(ss ...string) {
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if len(s) >= 8 {
			l[s] = struct{}{}
		}
	}
}

func (l leakSet) count(text string) int {
	n := 0
	for s := range l {
		if strings.Contains(text, s) {
			n++
		}
	}
	return n
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// walk 用 limit 翻页取完 [since, before) 的邮件（最多 maxItems 封），检查翻页本身的一致性。
func walk(t *testing.T, h *hub, src string, args map[string]any, limit, maxItems int) (ids []int64, total int, complete bool) {
	t.Helper()
	off := 0
	for len(ids) < maxItems {
		a := map[string]any{"source": src, "limit": limit, "offset": off}
		for k, v := range args {
			a[k] = v
		}
		var p page
		if isErr, _ := h.call(t, "localmail_search", a, &p); isErr {
			t.Fatalf("翻页 offset=%d 出错", off)
		}
		if p.Count != len(p.Messages) || p.Count > limit {
			t.Fatalf("count=%d 和 messages=%d / limit=%d 对不上", p.Count, len(p.Messages), limit)
		}
		for _, m := range p.Messages {
			ids = append(ids, m.ID)
		}
		total = p.Total
		if !p.HasMore {
			return ids, total, true
		}
		if p.NextOffset != off+p.Count {
			t.Fatalf("next_offset=%d，应为 %d", p.NextOffset, off+p.Count)
		}
		off = p.NextOffset
	}
	return ids, total, false
}

func dayOf(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

// walkFull 取完 [since, before) 的全部邮件摘要（最多 2000 封）。
func walkFull(t *testing.T, h *hub, src string, since, before time.Time) (out []summary, total int, done bool) {
	t.Helper()
	off := 0
	for len(out) < 2000 {
		var p page
		if isErr, _ := h.call(t, "localmail_search", map[string]any{"source": src, "limit": realMaxResults, "offset": off,
			"since": since.Format("2006-01-02"), "before": before.Format("2006-01-02")}, &p); isErr {
			t.Fatalf("翻页出错")
		}
		out = append(out, p.Messages...)
		if !p.HasMore {
			return out, p.Total, true
		}
		off = p.NextOffset
	}
	return out, 0, false
}

func dupCount(ids []int64) int {
	seen := map[int64]bool{}
	d := 0
	for _, id := range ids {
		if seen[id] {
			d++
		}
		seen[id] = true
	}
	return d
}

const realMaxBody = 2048
const realMaxResults = 40

func TestLocalMailRealSources(t *testing.T) {
	bin := binPath(t)
	dirs := realDirs(t)
	files := watched(dirs)
	before := stamp(files)

	settings := map[string]string{"maxBodyBytes": fmt.Sprint(realMaxBody), "maxResults": fmt.Sprint(realMaxResults)}
	for id, d := range dirs {
		settings[id], settings[id+"_dir"] = "true", d
	}
	h := mustStart(t, bin, []inst{localInst(true, settings)})
	leaks := leakSet{}
	allModes := map[string]string{}
	// 整个用例期间持续用 lsof 采样：HxStore.hxd 只在调用期间短暂打开。
	var modesMu sync.Mutex
	stopSample := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stopSample:
				return
			default:
			}
			for k, v := range openModes(t, h.cmd.Process.Pid, files) {
				modesMu.Lock()
				if !strings.Contains(allModes[k], v) {
					allModes[k] += v
				}
				modesMu.Unlock()
			}
		}
	}()

	now := time.Now()
	today := dayOf(now)

	for _, src := range []string{"apple_mail", "outlook_legacy", "outlook"} {
		t.Run(src, func(t *testing.T) {
			// list_folders
			var fo foldersOut
			if isErr, _ := h.call(t, "localmail_list_folders", map[string]any{"source": src}, &fo); isErr {
				t.Fatal("list_folders 出错")
			}
			if len(fo.Sources) != 1 || len(fo.Sources[0].Folders) == 0 {
				t.Fatalf("没有文件夹")
			}
			fs := fo.Sources[0].Folders
			var big folder
			nameInc := 0
			for _, f := range fs {
				if f.Total > big.Total {
					big = f
				}
				if f.NameIncomplete {
					nameInc++
				}
			}
			t.Logf("文件夹 %d 个（name_incomplete %d 个）", len(fs), nameInc)

			// 超大 limit：不报错，按每页上限截断。
			var huge page
			if isErr, _ := h.call(t, "localmail_search", map[string]any{"source": src, "limit": 100000}, &huge); isErr {
				t.Fatal("超大 limit 出错")
			}
			if huge.Count == 0 || huge.Count > realMaxResults {
				t.Fatalf("超大 limit 返回 %d 封，应在 1..%d", huge.Count, realMaxResults)
			}
			// 时间窗：最新一封信所在日的次日（最晚到今天，不含）往前 60 天。
			// 不含今天，新到的信就不会让翻页结果漂移；旧版 Outlook 这类不再更新的
			// 来源，窗口跟着它最新的那封信走。
			beforeT := dayOf(huge.Messages[0].Date).AddDate(0, 0, 1)
			if beforeT.After(today) {
				beforeT = today
			}
			sinceT := beforeT.AddDate(0, 0, -60)
			window := map[string]any{"since": sinceT.Format("2006-01-02"), "before": beforeT.Format("2006-01-02")}

			// 字段名和类型直接看服务返回的 JSON。
			var raw struct {
				Messages []map[string]any `json:"messages"`
			}
			h.call(t, "localmail_search", map[string]any{"source": src, "limit": 1}, &raw)
			if len(raw.Messages) == 1 {
				m := raw.Messages[0]
				for k, want := range map[string]string{"id": "float64", "subject": "string", "from": "string",
					"date": "string", "read": "bool", "folder_id": "float64"} {
					if got := fmt.Sprintf("%T", m[k]); got != want {
						t.Errorf("字段 %s 类型 %s，应为 %s", k, got, want)
					}
				}
				if _, err := time.Parse(time.RFC3339, fmt.Sprint(m["date"])); err != nil {
					t.Errorf("date 不是 RFC3339")
				}
			}

			// 翻页：两种页大小取到的序列必须一样，且无重复。
			const maxItems = 400
			a, totA, doneA := walk(t, h, src, window, 13, maxItems)
			bIDs, _, doneB := walk(t, h, src, window, realMaxResults, maxItems)
			n := min(len(a), len(bIDs))
			if n == 0 {
				t.Skip("时间窗里没有邮件")
			}
			for i := 0; i < n; i++ {
				if a[i] != bIDs[i] {
					t.Fatalf("两种页大小在第 %d 位取到不同的邮件", i)
				}
			}
			if d := dupCount(a); d > 0 {
				t.Fatalf("翻页有 %d 个重复 id", d)
			}
			if doneA && doneB && len(a) != len(bIDs) {
				t.Fatalf("取完后两种页大小条数不同：%d vs %d", len(a), len(bIDs))
			}
			if doneA && totA > 0 && totA != len(a) {
				t.Errorf("total=%d，翻页实际取到 %d", totA, len(a))
			}
			t.Logf("时间窗翻页：limit13 取 %d 封（取完=%v），limit40 取 %d 封，total=%d，无重复", len(a), doneA, len(bIDs), totA)

			// 时间过滤：每封都在 [since, before) 内（按本地时区的日期）。
			var wp page
			wargs := map[string]any{"source": src, "limit": realMaxResults}
			for k, v := range window {
				wargs[k] = v
			}
			h.call(t, "localmail_search", wargs, &wp)
			bad := 0
			for _, m := range wp.Messages {
				leaks.add(m.Subject, m.From, m.FromAddress, m.Preview)
				if m.Date.Before(sinceT) || !m.Date.Before(beforeT) {
					bad++
				}
			}
			if bad > 0 {
				t.Errorf("时间过滤：%d/%d 封不在本地日期窗口内", bad, len(wp.Messages))
			}
			// 也看看最旧一页的边界（翻到最后一页）。
			if doneA && len(a) > realMaxResults {
				var lp page
				largs := map[string]any{"source": src, "limit": realMaxResults, "offset": len(a) - realMaxResults}
				for k, v := range window {
					largs[k] = v
				}
				h.call(t, "localmail_search", largs, &lp)
				for _, m := range lp.Messages {
					if m.Date.Before(sinceT) {
						bad++
					}
				}
				if bad > 0 {
					t.Errorf("时间过滤（最旧页）：有早于 since 的邮件")
				}
			}

			// 文件夹过滤。
			if big.Total > 0 {
				var fp page
				h.call(t, "localmail_search", map[string]any{"source": src, "folder_id": big.ID, "limit": realMaxResults}, &fp)
				wrong := 0
				for _, m := range fp.Messages {
					if m.FolderID != big.ID {
						wrong++
					}
				}
				if fp.Count == 0 || wrong > 0 {
					t.Errorf("文件夹过滤：返回 %d 封，其中 %d 封不属于该文件夹", fp.Count, wrong)
				}
			}

			// 发件人过滤：用结果里第一个有地址的发件人。
			pool := append(append([]summary{}, huge.Messages...), wp.Messages...)
			for _, m := range pool {
				if m.FromAddress == "" {
					continue
				}
				var sp page
				h.call(t, "localmail_search", map[string]any{"source": src, "from": strings.ToUpper(m.FromAddress), "limit": realMaxResults}, &sp)
				wrong := 0
				for _, x := range sp.Messages {
					if !containsFold(x.From, m.FromAddress) && !containsFold(x.FromAddress, m.FromAddress) {
						wrong++
					}
				}
				if sp.Count == 0 || wrong > 0 {
					t.Errorf("发件人过滤（大小写不敏感）：返回 %d 封，%d 封不匹配", sp.Count, wrong)
				}
				t.Logf("发件人过滤：返回 %d 封，全部匹配", sp.Count)
				break
			}

			// 主题过滤：取某个主题中间的 4 个字。
			for _, m := range pool {
				r := []rune(m.Subject)
				if len(r) < 12 || containsStr(m.Incomplete, "subject") {
					continue
				}
				sub := string(r[len(r)/2 : len(r)/2+4])
				if strings.TrimSpace(sub) != sub || strings.ContainsAny(sub, "%_\\") {
					continue
				}
				var sp page
				h.call(t, "localmail_search", map[string]any{"source": src, "subject": sub, "limit": realMaxResults}, &sp)
				wrong, hit := 0, false
				for _, x := range sp.Messages {
					if !containsFold(x.Subject, sub) {
						wrong++
					}
					if x.ID == m.ID {
						hit = true
					}
				}
				if sp.Count == 0 || wrong > 0 {
					t.Errorf("主题过滤：返回 %d 封，%d 封不匹配", sp.Count, wrong)
				}
				t.Logf("主题过滤：返回 %d 封，全部匹配，含源邮件=%v", sp.Count, hit)
				break
			}

			// get：正文长度上限、UTF-8 完整、incomplete 标记。
			truncated, shortCut, notes, incomplete := 0, 0, 0, 0
			got := 0
			for _, m := range pool {
				if got >= 10 {
					break
				}
				var msg message
				if isErr, _ := h.call(t, "localmail_get", map[string]any{"source": src, "id": m.ID}, &msg); isErr {
					t.Errorf("get 出错")
					continue
				}
				got++
				leaks.add(msg.Subject, msg.From, msg.FromAddress)
				for _, line := range strings.Split(msg.Body, "\n") {
					leaks.add(line)
				}
				if len(msg.Body) > realMaxBody {
					t.Errorf("正文 %d 字节，超过上限 %d", len(msg.Body), realMaxBody)
				}
				if !json.Valid(mustJSON(msg.Body)) || !utf8Valid(msg.Body) {
					t.Errorf("正文不是合法 UTF-8")
				}
				if msg.BodyTruncated {
					truncated++
					// HTML 正文先按 4 倍上限读、再转文本，转完可能远短于上限（见报告）。
					if len(msg.Body) < realMaxBody-4 {
						shortCut++
					}
				}
				if msg.BodyNote != "" {
					notes++
				}
				if len(msg.Incomplete) > 0 {
					incomplete++
				}
				if src == "outlook" {
					for _, f := range []string{"read", "has_attachment", "attachments"} {
						if !containsStr(msg.Incomplete, f) {
							t.Errorf("新版 Outlook 的邮件 incomplete 缺 %s", f)
						}
					}
				}
			}
			if src == "outlook" {
				for _, m := range pool {
					if !containsStr(m.Incomplete, "read") || !containsStr(m.Incomplete, "has_attachment") {
						t.Errorf("新版 Outlook 列表项 incomplete 缺 read/has_attachment")
						break
					}
				}
			}
			var fields []string
			if len(raw.Messages) == 1 {
				fields = keys(raw.Messages[0])
			}
			t.Logf("get %d 封：截断 %d（其中正文明显短于上限 %d），带 body_note %d，带 incomplete %d；列表项字段 %v",
				got, truncated, shortCut, notes, incomplete, fields)

			// 日期按本机时区：since=D、before=D+1 取到的，应正好是本地日期为 D 的那些信。
			d := dayOf(huge.Messages[0].Date).AddDate(0, 0, -1)
			wide, _, wideDone := walkFull(t, h, src, d.AddDate(0, 0, -1), d.AddDate(0, 0, 2))
			day, _, dayDone := walkFull(t, h, src, d, d.AddDate(0, 0, 1))
			if wideDone && dayDone {
				local := 0
				for _, m := range wide {
					if dayOf(m.Date).Equal(d) {
						local++
					}
				}
				if local != len(day) {
					t.Errorf("since/before 不按本机时区：本地日期为该日的信 %d 封，按日期过滤取到 %d 封", local, len(day))
				}
				t.Logf("单日过滤：本地日期为该日 %d 封，过滤取到 %d 封", local, len(day))
			}

			// 不存在的 id。
			isErr, text := h.call(t, "localmail_get", map[string]any{"source": src, "id": int64(1) << 40}, nil)
			if !isErr || !strings.Contains(text, "未找到") {
				t.Errorf("不存在的 id 应返回「未找到」错误（isErr=%v）", isErr)
			}
			h.alive(t)

		})
	}
	close(stopSample)
	<-sampled

	t.Run("只读核验", func(t *testing.T) {
		for name, modes := range allModes {
			base := filepath.Base(name)
			t.Logf("mcp-hub 打开 %s，方式 %s", base, modes)
			if strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-wal") {
				// SQLite 读 WAL 库时总以读写方式打开 -wal / -shm（读连接也要在 -shm 里
				// 登记读标记）。主库是只读句柄，拿不到检查点要的排他锁，所以不会改
				// -wal；applemail 的 TestReadOnlyOpenLeavesDBAndWALUntouched 验证这一点。
				continue
			}
			if strings.ContainsAny(modes, "wu") {
				t.Errorf("%s 被以写方式打开（%s）", base, modes)
			}
		}
		after := stamp(files)
		for p, b := range before {
			a := after[p]
			t.Logf("%s：size %d→%d，mtime %s→%s", filepath.Base(p), b.Size, a.Size,
				b.MTime.Format(time.RFC3339), a.MTime.Format(time.RFC3339))
		}
	})

	t.Run("日志里没有邮件内容", func(t *testing.T) {
		logText := h.stderr.String()
		if data, err := os.ReadFile(filepath.Join(h.home, "Library", "Logs", "mcp-hub", "mcp-hub.log")); err == nil {
			logText += string(data)
		}
		if n := leaks.count(logText); n > 0 {
			t.Errorf("服务日志里出现了 %d 段邮件文本", n)
		}
		t.Logf("核对了 %d 段邮件文本，日志 %d 字节", len(leaks), len(logText))
	})

	t.Run("关闭后不留快照", func(t *testing.T) {
		_ = h.cs.Close()
		time.Sleep(500 * time.Millisecond)
		left, _ := filepath.Glob(filepath.Join(h.home, "tmp", "hxstore-snap-*"))
		if len(left) > 0 {
			t.Errorf("临时目录里残留 %d 个 hxstore 快照", len(left))
		}
	})
}

// 一个来源坏了，其他来源照常。
func TestLocalMailRealIsolation(t *testing.T) {
	bin := binPath(t)
	dirs := realDirs(t)
	h := mustStart(t, bin, []inst{localInst(true, map[string]string{
		"apple_mail": "true", "apple_mail_dir": dirs["apple_mail"],
		"outlook_legacy": "true", "outlook_legacy_dir": noPermDir(t),
		"outlook": "true", "outlook_dir": dirs["outlook"],
	})})
	var fo foldersOut
	if isErr, _ := h.call(t, "localmail_list_folders", nil, &fo); isErr {
		t.Fatal("list_folders 整体失败")
	}
	for _, g := range fo.Sources {
		bad := g.Source == "outlook_legacy"
		if bad != (g.Error != "") {
			t.Errorf("%s：error 非空=%v，期望 %v", g.Source, g.Error != "", bad)
		}
		if !bad && len(g.Folders) == 0 {
			t.Errorf("%s：没有文件夹", g.Source)
		}
	}
	if isErr, text := h.call(t, "localmail_search", map[string]any{"source": "outlook_legacy"}, nil); !isErr || !strings.Contains(text, "文件权限") {
		t.Errorf("chmod 000 的来源应返回文件权限说明")
	}
	for _, src := range []string{"apple_mail", "outlook"} {
		var p page
		if isErr, _ := h.call(t, "localmail_search", map[string]any{"source": src, "limit": 3}, &p); isErr || p.Count == 0 {
			t.Errorf("%s 受到了坏来源的影响", src)
		}
	}
	h.alive(t)
}

// ---------- 回归：7 个现有渠道 ----------

// sevenChannels 是 7 个现有渠道的假配置。write 控制 allowSend / allowWrite。
func sevenChannels(write bool) []inst {
	w := fmt.Sprint(write)
	return []inst{
		{Type: "qqmail", Account: "e2e-fake@qq.com", Enabled: true, Settings: map[string]string{"allowSend": w}},
		{Type: "custommail", Account: "e2e-fake@example.invalid", Enabled: true, Settings: map[string]string{
			"allowSend": w, "imapHost": "imap.example.invalid", "smtpHost": "smtp.example.invalid"}},
		{Type: "feishu", Account: "e2e-fake-feishu", Enabled: true, Settings: map[string]string{"allowWrite": w, "appId": "cli_e2efake"}},
		{Type: "github", Account: "e2e-fake-github", Enabled: true, Settings: map[string]string{"allowWrite": w}},
		{Type: "jira", Account: "e2e-fake@example.invalid", Enabled: true, Settings: map[string]string{
			"allowWrite": w, "site": "https://e2e-fake.example.invalid"}},
		{Type: "napcat", Account: "10000", Enabled: true, Settings: map[string]string{"allowWrite": w, "endpoint": "http://127.0.0.1:1"}},
		{Type: "yunzhijia", Account: "e2e-fake-yzj", Enabled: true, Settings: map[string]string{"allowWrite": w}},
	}
}

// feishuWebInst 是飞书网页文档渠道的配置（没有凭据，列工具不需要扩展连着）。
var feishuWebInst = inst{Type: "feishu-web", Account: "本机浏览器", Enabled: true, Settings: map[string]string{}}

func toolJSON(ts []*mcp.Tool) map[string]string {
	out := map[string]string{}
	for _, x := range ts {
		b, _ := json.Marshal(x)
		out[x.Name] = string(b)
	}
	return out
}

func TestRegressionSevenChannels(t *testing.T) {
	bin := binPath(t)
	base := os.Getenv("MCPHUB_E2E_BASELINE_BIN")
	if base == "" {
		t.Skip("设 MCPHUB_E2E_BASELINE_BIN（main 分支构建）才跑回归对比")
	}
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%v", write), func(t *testing.T) {
			chans := sevenChannels(write)
			old := toolJSON(mustStart(t, base, chans).tools(t))
			cur := toolJSON(mustStart(t, bin, chans).tools(t))
			withLocal := toolJSON(mustStart(t, bin, append(chans, localInst(true, platformOn()))).tools(t))

			perType := map[string]int{}
			for n := range old {
				perType[strings.SplitN(n, "_", 2)[0]]++
			}
			t.Logf("main：%d 个工具，按前缀 %v", len(old), perType)
			diff := 0
			for n, j := range old {
				if cur[n] != j {
					diff++
					t.Errorf("工具 %s 和 main 不一致", n)
				}
			}
			for n := range cur {
				if _, ok := old[n]; !ok {
					diff++
					t.Errorf("新版多出工具 %s", n)
				}
			}
			t.Logf("新版：%d 个工具，与 main 不同 %d 个", len(cur), diff)
			extra := []string{}
			for n, j := range withLocal {
				if o, ok := old[n]; !ok {
					extra = append(extra, n)
				} else if o != j {
					t.Errorf("加了本机邮件后 %s 变了", n)
				}
			}
			sort.Strings(extra)
			if strings.Join(extra, ",") != "localmail_get,localmail_list_folders,localmail_search" || len(withLocal) != len(old)+3 {
				t.Errorf("加本机邮件后多出 %v，应正好是 3 个 localmail_*", extra)
			}

			// 再加上飞书网页文档：预期内的新增只有 3 个 feishu_web_*，其余工具一个不变。
			all := toolJSON(mustStart(t, bin, append(chans, localInst(true, platformOn()), feishuWebInst)).tools(t))
			extra = extra[:0]
			for n, j := range all {
				if o, ok := withLocal[n]; !ok {
					extra = append(extra, n)
				} else if o != j {
					t.Errorf("加了飞书网页文档后 %s 变了", n)
				}
			}
			sort.Strings(extra)
			if strings.Join(extra, ",") != "feishu_web_read_doc,feishu_web_recent,feishu_web_search" || len(all) != len(withLocal)+3 {
				t.Errorf("加飞书网页文档后多出 %v，应正好是 3 个 feishu_web_*", extra)
			}
			t.Logf("7 渠道 + 本机邮件 + 飞书网页文档：%d 个工具", len(all))
		})
	}
}

// ---------- 小工具 ----------

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustJSON(s string) []byte { b, _ := json.Marshal(s); return b }

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

// Quick 可能并发调用工具：三个来源同时搜索、读信，服务不能出错或崩溃。
// 用 -race 构建的二进制跑这个用例，能顺带抓服务端的数据竞争。
func TestLocalMailRealConcurrent(t *testing.T) {
	bin := binPath(t)
	dirs := realDirs(t)
	settings := map[string]string{}
	for id, d := range dirs {
		settings[id], settings[id+"_dir"] = "true", d
	}
	h := mustStart(t, bin, []inst{localInst(true, settings)})
	var wg sync.WaitGroup
	var mu sync.Mutex
	fails := 0
	for _, src := range []string{"apple_mail", "outlook_legacy", "outlook"} {
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(src string, i int) {
				defer wg.Done()
				var p page
				isErr, _ := h.call(t, "localmail_search", map[string]any{"source": src, "limit": 5, "offset": i * 5}, &p)
				if !isErr && len(p.Messages) > 0 {
					isErr, _ = h.call(t, "localmail_get", map[string]any{"source": src, "id": p.Messages[0].ID}, nil)
				}
				if i%2 == 0 {
					var fo foldersOut
					if e, _ := h.call(t, "localmail_list_folders", nil, &fo); e {
						isErr = true
					}
				}
				if isErr {
					mu.Lock()
					fails++
					mu.Unlock()
				}
			}(src, i)
		}
	}
	wg.Wait()
	if fails > 0 {
		t.Errorf("并发调用里 %d 次出错", fails)
	}
	h.alive(t)
	if strings.Contains(h.stderr.String(), "DATA RACE") || strings.Contains(h.stderr.String(), "panic") {
		t.Errorf("服务端出现数据竞争或 panic（stderr %d 字节）", len(h.stderr.String()))
	}
}
