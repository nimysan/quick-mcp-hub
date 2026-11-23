package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 经典版 Outlook for Windows（source=outlook_classic_win）的用例，只在 Windows 上跑。
// 和 Mac 的用例一样，只打印数量和字段名，不打印邮件内容。

const winSource = "outlook_classic_win"

func winOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("经典版 Outlook for Windows 只在 Windows 上注册")
	}
}

// Windows 上只有 Windows 来源：Mac 来源即使写在配置里也不注册、不出现在工具说明和结果里。
func TestLocalMailWindowsHidesMacSources(t *testing.T) {
	winOnly(t)
	bin := binPath(t)
	h := mustStart(t, bin, []inst{localInst(true, map[string]string{
		"apple_mail": "true", "outlook_legacy": "true", "outlook": "true", winSource: "true",
		"apple_mail_dir": `C:\nonexistent-e2e\a`, winSource + "_dir": `C:\nonexistent-e2e\x.pst`,
	})})
	ts := h.tools(t)
	if n := localNames(ts); len(n) != 3 {
		t.Fatalf("本机邮件工具 = %v", n)
	}
	for _, x := range ts {
		if !strings.HasPrefix(x.Name, "localmail_") {
			continue
		}
		if !strings.Contains(x.Description, winSource) {
			t.Errorf("%s 的说明里没有 %s", x.Name, winSource)
		}
		for _, bad := range []string{"apple_mail", "outlook_legacy", "Apple Mail", "for Mac", "outlook（"} {
			if strings.Contains(x.Description, bad) {
				t.Errorf("%s 的说明里出现了 Mac 来源 %q", x.Name, bad)
			}
		}
	}
	var fo foldersOut
	if isErr, text := h.call(t, "localmail_list_folders", nil, &fo); isErr {
		// 只启用了一个来源时，它的错误就是整个调用的错误。
		if !strings.Contains(text, "不存在") {
			t.Errorf("路径不存在时说明不清楚（%d 字）", len([]rune(text)))
		}
	} else if len(fo.Sources) != 1 || fo.Sources[0].Source != winSource {
		t.Errorf("来源分组 = %d 个", len(fo.Sources))
	}
	if isErr, text := h.call(t, "localmail_search", map[string]any{"source": "apple_mail"}, nil); !isErr || !strings.Contains(text, "未启用") {
		t.Error("Windows 上 apple_mail 应是未启用")
	}
	h.alive(t)
}

func sampleFiles(t *testing.T) []string {
	t.Helper()
	winOnly(t)
	dir := os.Getenv("MCPHUB_E2E_PST_DIR")
	if dir == "" {
		t.Skip("设 MCPHUB_E2E_PST_DIR=<放 .pst/.ost 样本的目录> 才跑")
	}
	var out []string
	for _, pat := range []string{"*.pst", "*.ost"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		out = append(out, m...)
	}
	if len(out) == 0 {
		t.Fatalf("%s 里没有 .pst / .ost", dir)
	}
	return out
}

func fileDigest(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	fi, _ := f.Stat()
	return fmt.Sprintf("%s|%s|%d", hex.EncodeToString(h.Sum(nil)), fi.ModTime().UTC(), fi.Size())
}

// 用样本读一遍：每个文件单独一次，再整个目录一次（多文件聚合）。读前后文件不变。
func TestLocalMailWindowsSamples(t *testing.T) {
	files := sampleFiles(t)
	bin := binPath(t)
	before := map[string]string{}
	for _, f := range files {
		before[f] = fileDigest(t, f)
	}
	runs := append(append([]string{}, files...), filepath.Dir(files[0]))
	for _, target := range runs {
		name := filepath.Base(target)
		if target == filepath.Dir(files[0]) {
			name = "整个目录"
		}
		t.Run(name, func(t *testing.T) {
			h := mustStart(t, bin, []inst{localInst(true, map[string]string{winSource: "true", winSource + "_dir": target})})
			var fo foldersOut
			if isErr, text := h.call(t, "localmail_list_folders", nil, &fo); isErr {
				t.Fatalf("list_folders 失败（%d 字）: %s", len([]rune(text)), text)
			}
			folders, total := 0, int64(0)
			for _, g := range fo.Sources {
				folders += len(g.Folders)
				for _, f := range g.Folders {
					total += f.Total
				}
			}
			seen, got := 0, 0
			fields := map[string]int{}
			for off := 0; ; {
				var p page
				if isErr, text := h.call(t, "localmail_search", map[string]any{"limit": 50, "offset": off}, &p); isErr {
					t.Fatalf("search 失败: %s", text)
				}
				for _, m := range p.Messages {
					seen++
					for k, ok := range map[string]bool{"subject": m.Subject != "", "from": m.From != "", "from_address": m.FromAddress != "", "date": !m.Date.IsZero()} {
						if ok {
							fields[k]++
						}
					}
					var msg map[string]any
					if isErr, text := h.call(t, "localmail_get", map[string]any{"id": m.ID}, &msg); isErr {
						t.Errorf("get 失败: %s", text)
						continue
					}
					got++
					for _, k := range []string{"body", "message_id", "to", "cc", "attachments", "body_note"} {
						if v, ok := msg[k]; ok && v != "" && v != nil {
							fields["get."+k]++
						}
					}
					if b, _ := msg["body"].(string); !utf8Valid(b) {
						t.Error("正文不是合法 UTF-8")
					}
				}
				if !p.HasMore {
					break
				}
				off = p.NextOffset
			}
			t.Logf("文件夹 %d 个（邮件数合计 %d），search 列出 %d 封，get 成功 %d 封，字段计数 %v", folders, total, seen, got, fields)
			if folders == 0 {
				t.Error("一个邮件文件夹都没有")
			}
			if int64(seen) != total {
				t.Errorf("search 列出 %d 封，和文件夹合计 %d 对不上", seen, total)
			}
			h.alive(t)
		})
	}
	for _, f := range files {
		if fileDigest(t, f) != before[f] {
			t.Errorf("%s 在读取后变了", filepath.Base(f))
		}
	}
}

// psLock 用 PowerShell 独占打开文件（FileShare.None，模拟经典版 Outlook 运行时的锁），
// 返回释放函数。opened 为假表示独占打开本身失败了（有别的程序开着这个文件）。
func psLock(t *testing.T, path string) (opened bool, release func()) {
	t.Helper()
	script := "try { $f=[IO.File]::Open('" + strings.ReplaceAll(path, "'", "''") + "','Open','Read','None') } catch { [Console]::Out.WriteLine('FAIL'); exit 1 }; " +
		"[Console]::Out.WriteLine('LOCKED'); [Console]::Out.Flush(); [void][Console]::In.ReadLine(); $f.Close()"
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 PowerShell: %v", err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	release = func() {
		stdin.Close()
		cmd.Wait()
	}
	if strings.TrimSpace(line) != "LOCKED" {
		release()
		return false, func() {}
	}
	return true, release
}

// 文件被独占时：工具返回「退出 Outlook 再试」的提示，服务不崩、来源不停用；解锁后照常读。
// 再看反方向：mcp-hub 读过之后句柄会在空闲 30 秒后关闭，不会一直挡住别的程序独占打开。
func TestLocalMailWindowsLocked(t *testing.T) {
	files := sampleFiles(t)
	bin := binPath(t)
	target := files[0]
	ok, release := psLock(t, target)
	if !ok {
		t.Fatal("PowerShell 没能独占打开样本")
	}
	h := mustStart(t, bin, []inst{localInst(true, map[string]string{winSource: "true", winSource + "_dir": target})})
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"localmail_list_folders", nil},
		{"localmail_search", map[string]any{"limit": 5}},
		{"localmail_get", map[string]any{"id": 0x200024}},
		{"localmail_list_folders", nil},
	} {
		isErr, text := h.call(t, c.tool, c.args, nil)
		if !isErr || strings.HasPrefix(text, "PROTOCOL: ") {
			t.Errorf("%s：文件被锁时应返回工具错误", c.tool)
		}
		if !strings.Contains(text, "退出") || !strings.Contains(text, "OUTLOOK.EXE") || strings.Contains(text, "已停用") {
			t.Errorf("%s：提示不对：%s", c.tool, text)
		}
		h.alive(t)
	}
	t.Logf("锁住时的提示：%s", func() string { _, s := h.call(t, "localmail_list_folders", nil, nil); return s }())
	release()

	var fo foldersOut
	if isErr, text := h.call(t, "localmail_list_folders", nil, &fo); isErr || len(fo.Sources) != 1 || len(fo.Sources[0].Folders) == 0 {
		t.Fatalf("解锁后应能读（来源没有被停用）：%s", text)
	}

	// mcp-hub 刚读过，句柄还开着：别的程序要独占打开会失败。空闲 30 秒后句柄关闭，独占打开就能成功。
	busy, rel := psLock(t, target)
	rel()
	t.Logf("mcp-hub 刚读完时独占打开：%v", map[bool]string{true: "成功", false: "失败（句柄还开着）"}[busy])
	time.Sleep(35 * time.Second)
	idle, rel2 := psLock(t, target)
	rel2()
	if !idle {
		t.Error("空闲 35 秒后仍不能独占打开：mcp-hub 没有释放句柄")
	}
	if isErr, text := h.call(t, "localmail_search", map[string]any{"limit": 5}, nil); isErr {
		t.Errorf("句柄关闭后再查应自动重开：%s", text)
	}
	h.alive(t)
}
