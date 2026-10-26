package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/wasmplug"
)

// 这组测试走真的 MCP 协议：注册 -> tools/list -> tools/call，确认 wasm 插件
// 渠道在客户端眼里和别的渠道没有区别，而写开关的门禁在注册阶段就生效。

const (
	pluginWasm     = "../../plugins/dist/ghwasm/ghwasm.wasm"
	pluginManifest = "../../plugins/ghwasm/manifest.json"
)

// fakeGitHub 是一台仿真的 api.github.com。
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"login":"nimysan","name":"Sean Ye",
			"html_url":"https://github.com/nimysan","public_repos":7}`)
	})
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":9,"title":"从插件建的","state":"open",
				"html_url":"https://github.com/o/r/issues/9"}`)
			return
		}
		io.WriteString(w, `[{"number":1,"title":"一条 issue","state":"open",
			"user":{"login":"a"},"comments":0,"created_at":"x","updated_at":"y",
			"html_url":"https://github.com/o/r/issues/1"}]`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// loadPlugin 加载真实的 wasm 插件，并把它指向仿真服务。
func loadPlugin(t *testing.T, srv *httptest.Server) (*wasmplug.Plugin, wasmplug.Manifest) {
	t.Helper()
	raw, err := os.ReadFile(pluginManifest)
	if err != nil {
		t.Fatalf("读 manifest 失败: %v", err)
	}
	man, err := wasmplug.ParseManifest(raw)
	if err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	man.BaseURL = srv.URL
	man.AllowedHosts = []string{"127.0.0.1"}

	module, err := os.ReadFile(pluginWasm)
	if err != nil {
		t.Fatalf("没有插件模块，先跑 plugins/build.sh: %v", err)
	}
	p, err := wasmplug.Open(context.Background(), man, module)
	if err != nil {
		t.Fatalf("加载插件失败: %v", err)
	}
	p.HTTP = srv.Client()
	t.Cleanup(func() { p.Close(context.Background()) })
	return p, man
}

// staticLoader 把一个已加载的插件直接交出去，省掉磁盘上的插件目录。
type staticLoader struct{ p *wasmplug.Plugin }

func (l staticLoader) Get(context.Context, string) (*wasmplug.Plugin, error) { return l.p, nil }

func pluginInstance(account string, allowWrite bool) wasmplug.Instance {
	return wasmplug.Instance{
		Account:  account,
		Settings: map[string]string{"allowWrite": boolStr(allowWrite)},
		Secret:   func() (string, error) { return "t0ken", nil },
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// connect 把 server 和一个客户端用内存传输接起来。
func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server 连接失败: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client 连接失败: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func listNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list 失败: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func newServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "mcp-hub", Version: "test"}, nil)
}

// TestPluginToolsReachMCP：插件工具经由真的 MCP 协议可见、可调。
func TestPluginToolsReachMCP(t *testing.T) {
	srv := fakeGitHub(t)
	p, man := loadPlugin(t, srv)

	server := newServer()
	n := RegisterPlugin(server, staticLoader{p}, man, []wasmplug.Instance{pluginInstance("nimysan", false)})
	// 6 个工具里有 1 个写操作，写开关关着 -> 只注册 5 个。
	if n != 5 {
		t.Fatalf("注册了 %d 个工具，期望 5（写工具被开关挡住）", n)
	}

	cs := connect(t, server)
	names := listNames(t, cs)
	if len(names) != 5 {
		t.Fatalf("客户端看到 %d 个工具: %v", len(names), names)
	}
	for _, n := range names {
		if n == "ghwasm_create_issue" {
			t.Fatal("写开关关着，create_issue 不该出现在工具列表里")
		}
		if !strings.HasPrefix(n, "ghwasm_") {
			t.Fatalf("工具名没有渠道前缀: %s", n)
		}
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghwasm_whoami"})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("调用返回错误: %v", res.Content)
	}
	// structuredContent 必须是对象：现实中的客户端中间层普遍只接受 dict。
	if res.StructuredContent == nil {
		t.Fatal("没有 structuredContent")
	}
	got, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent 不是对象: %T", res.StructuredContent)
	}
	if got["login"] != "nimysan" {
		t.Fatalf("结果不对: %v", got)
	}
}

// TestWriteToolRegisteredWhenSwitchOn：开关打开后写工具才出现。
func TestWriteToolRegisteredWhenSwitchOn(t *testing.T) {
	srv := fakeGitHub(t)
	p, man := loadPlugin(t, srv)

	server := newServer()
	if n := RegisterPlugin(server, staticLoader{p}, man,
		[]wasmplug.Instance{pluginInstance("nimysan", true)}); n != 6 {
		t.Fatalf("注册了 %d 个工具，期望 6", n)
	}
	cs := connect(t, server)
	names := listNames(t, cs)
	var found bool
	for _, n := range names {
		if n == "ghwasm_create_issue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("写工具没注册: %v", names)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghwasm_create_issue",
		Arguments: map[string]any{"owner": "o", "repo": "r", "title": "从插件建的"},
	})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("建 issue 失败: %v", res.Content)
	}
}

// TestMissingRequiredParamRejectedByHost：必填项在宿主侧就被拦下，
// 不交给插件 —— 「参数够不够」不该由被审的那一方说。
func TestMissingRequiredParamRejectedByHost(t *testing.T) {
	srv := fakeGitHub(t)
	p, man := loadPlugin(t, srv)

	server := newServer()
	RegisterPlugin(server, staticLoader{p}, man, []wasmplug.Instance{pluginInstance("nimysan", false)})
	cs := connect(t, server)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghwasm_list_issues",
		Arguments: map[string]any{"owner": "o"}, // 缺 repo
	})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if !res.IsError {
		t.Fatal("缺必填参数竟然成功了")
	}
	if !strings.Contains(contentText(res), "repo") {
		t.Fatalf("报错没点出缺哪个参数: %v", contentText(res))
	}
}

// TestMultiAccountDispatch：多账号只注册一套工具，用 channel_account 选。
func TestMultiAccountDispatch(t *testing.T) {
	srv := fakeGitHub(t)
	p, man := loadPlugin(t, srv)

	server := newServer()
	n := RegisterPlugin(server, staticLoader{p}, man, []wasmplug.Instance{
		pluginInstance("alice", false),
		pluginInstance("bob", true),
	})
	// 工具数不随账号膨胀；bob 开了写开关，所以 6 个都注册。
	if n != 6 {
		t.Fatalf("注册了 %d 个工具，期望 6", n)
	}
	cs := connect(t, server)

	// 不指定账号 -> 拒绝并列出可选项。替模型猜一个，在写工具上就是选错对象。
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghwasm_whoami"})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "channel_account") {
		t.Fatalf("多账号没要求指定账号: %v", contentText(res))
	}

	// 指定存在的账号 -> 通。
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ghwasm_whoami", Arguments: map[string]any{"channel_account": "alice"},
	})
	if err != nil || res.IsError {
		t.Fatalf("指定账号后仍失败: err=%v content=%v", err, contentText(res))
	}

	// alice 没开写开关 -> 她的写操作在插件执行前被拒。
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ghwasm_create_issue",
		Arguments: map[string]any{
			"channel_account": "alice", "owner": "o", "repo": "r", "title": "不该建",
		},
	})
	if err != nil {
		t.Fatalf("tools/call 失败: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "写开关") {
		t.Fatalf("alice 的写操作没被拦: %v", contentText(res))
	}

	// 指定不存在的账号 -> 拒绝并列出可选项。
	res, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ghwasm_whoami", Arguments: map[string]any{"channel_account": "nobody"},
	})
	if !res.IsError || !strings.Contains(contentText(res), "alice") {
		t.Fatalf("未知账号的报错没列出可选项: %v", contentText(res))
	}
}

// TestInputSchemaFromManifest：schema 来自 manifest 的参数表。
func TestInputSchemaFromManifest(t *testing.T) {
	tool := wasmplug.Tool{
		Name: "x_do", Description: "d",
		Params: []wasmplug.Param{
			{Name: "owner", Required: true, Description: "谁"},
			{Name: "n", Type: "number"},
			{Name: "state", Enum: []string{"open", "closed"}},
		},
	}
	// 单账号时不该出现分派参数 —— 多一个必填参数等于白给模型添一道坎。
	s := pluginInputSchema(tool, []string{"only"})
	props := s["properties"].(map[string]any)
	if _, ok := props[wasmplug.ReservedAccountParam]; ok {
		t.Fatal("单账号却有 channel_account")
	}
	if req := s["required"].([]string); len(req) != 1 || req[0] != "owner" {
		t.Fatalf("required = %v", req)
	}
	if props["n"].(map[string]any)["type"] != "number" {
		t.Fatalf("类型没传过去: %v", props["n"])
	}
	if props["owner"].(map[string]any)["type"] != "string" {
		t.Fatalf("默认类型应当是 string: %v", props["owner"])
	}
	if len(props["state"].(map[string]any)["enum"].([]any)) != 2 {
		t.Fatalf("enum 没传过去: %v", props["state"])
	}

	// 多账号时它是必填的。
	s = pluginInputSchema(tool, []string{"a", "b"})
	req := s["required"].([]string)
	if len(req) != 2 || req[0] != wasmplug.ReservedAccountParam {
		t.Fatalf("多账号时 channel_account 应当必填: %v", req)
	}
}

func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 {
		raw, _ := json.Marshal(res.Content)
		return string(raw)
	}
	return b.String()
}
