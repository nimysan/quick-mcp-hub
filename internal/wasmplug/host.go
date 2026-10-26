// 宿主侧：在 wasm 沙箱里跑第三方 connector。
//
// 沙箱的性质不是「我们拦住了什么」，而是「wasm 里本来就没有系统调用」——
// 模块想碰外界，只能调宿主显式导出的那几个函数。这里一共导出三个：
// fetch（受限 HTTP）、read（取回响应）、log。没有文件、没有 socket、
// 没有钥匙串、没有起进程。
//
// 凭据由宿主在 fetch 时注入，插件从头到尾看不到 token。
package wasmplug

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	// callTimeout 单次调用的硬上限。超时会连带关掉模块实例 —— 一个跑飞的
	// 第三方模块就该被拆掉，而不是留在进程里继续占资源。
	callTimeout = 30 * time.Second
	// maxBody 单次响应体上限。
	maxBody = 8 << 20
	// memoryLimitPages 插件内存上限（一页 64KiB）。1024 页 = 64MiB。
	memoryLimitPages = 1024
	// hostModule 是插件 import 的宿主模块名，ABI 的一部分。
	hostModule = "mcphub"
)

// Instance 是一个渠道实例的运行态。和声明式渠道一致：
// Secret 是函数，凭据惰性取用、不长期驻留明文。
type Instance struct {
	Account  string
	Settings map[string]string
	Secret   func() (string, error)
}

// AllowWrite 报告这个实例是否打开了写开关。
func (i Instance) AllowWrite() bool {
	switch strings.ToLower(strings.TrimSpace(i.Settings["allowWrite"])) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Plugin 是一个已加载的插件渠道。
//
// 一个 wasm 实例不能并发调用（里面是一份 Go 运行时），所以 Call 是串行的。
// 真要并发就得起实例池 —— 原型先不做，但这个限制要说清楚。
type Plugin struct {
	man Manifest

	// HTTP 可替换，测试里指向 httptest 的 TLS client。
	HTTP *http.Client

	// Log 非空时接收插件写的日志。
	Log func(string)

	rt     wazero.Runtime
	mod    api.Module
	callFn api.Function
	allocF api.Function

	mu sync.Mutex
	// pending 是上一次 fetch 的响应，等插件调 read 取走。
	pending []byte
	// stats 供诊断和测试断言。
	fetches int
	denied  []string
	secret  func() (string, error)
	baseURL string
}

// Open 加载一个插件：校验 manifest、编译模块、注入宿主函数。
func Open(ctx context.Context, man Manifest, module []byte) (*Plugin, error) {
	if err := man.Validate(); err != nil {
		return nil, err
	}
	p := &Plugin{man: man}

	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(memoryLimitPages)
	p.rt = wazero.NewRuntimeWithConfig(ctx, cfg)

	// Go 编译出的 wasip1 模块需要 WASI preview1。它只拿到宿主给的那份
	// 空壳：没有预开目录、没有环境变量、没有标准输入。
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, p.rt); err != nil {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("准备 WASI 失败: %w", err)
	}

	if _, err := p.rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().WithFunc(p.hostFetch).Export("fetch").
		NewFunctionBuilder().WithFunc(p.hostRead).Export("read").
		NewFunctionBuilder().WithFunc(p.hostLog).Export("log").
		Instantiate(ctx); err != nil {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("注入宿主函数失败: %w", err)
	}

	mod, err := p.rt.InstantiateWithConfig(ctx, module,
		wazero.NewModuleConfig().
			// c-shared 模式的模块入口是 _initialize，不是 _start。
			WithStartFunctions("_initialize").
			WithName(""))
	if err != nil {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("加载插件 %s 失败: %w", man.ID, err)
	}
	p.mod = mod

	// ABI 握手：在跑任何业务逻辑之前确认版本。
	abiFn := mod.ExportedFunction("plugin_abi")
	if abiFn == nil {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("插件 %s 没有导出 plugin_abi，不是一个 mcp-hub 插件", man.ID)
	}
	got, err := abiFn.Call(ctx)
	if err != nil || len(got) != 1 {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("插件 %s 的 plugin_abi 调用失败: %v", man.ID, err)
	}
	if uint32(got[0]) != uint32(man.ABI) {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("插件 %s 的模块报 ABI v%d，manifest 写的是 v%d —— 两者不一致，拒绝加载",
			man.ID, got[0], man.ABI)
	}

	p.callFn = mod.ExportedFunction("call")
	p.allocF = mod.ExportedFunction("alloc")
	if p.callFn == nil || p.allocF == nil {
		p.rt.Close(ctx)
		return nil, fmt.Errorf("插件 %s 缺少导出函数 call / alloc", man.ID)
	}
	return p, nil
}

// Manifest 返回这个插件的 manifest。
func (p *Plugin) Manifest() Manifest { return p.man }

// Fetches 返回已放行的请求数，Denied 返回被沙箱拒绝的请求。诊断与测试用。
func (p *Plugin) Fetches() int { return p.fetches }
func (p *Plugin) Denied() []string {
	out := make([]string, len(p.denied))
	copy(out, p.denied)
	return out
}

// Close 卸载插件。
func (p *Plugin) Close(ctx context.Context) error { return p.rt.Close(ctx) }

// Test 验证凭据可用。
func (p *Plugin) Test(ctx context.Context, inst Instance) (any, error) {
	out, _, err := p.invoke(ctx, inst, envelope{Op: "test"})
	return out, err
}

// Call 执行一个工具。
func (p *Plugin) Call(ctx context.Context, inst Instance, tool string, args map[string]any) (any, map[string]any, error) {
	t, ok := p.man.Tool(tool)
	if !ok {
		return nil, nil, fmt.Errorf("渠道 %s 没有工具 %s", p.man.ID, tool)
	}
	// 写开关在插件跑起来之前就判定：不让第三方代码有机会「自己决定」要不要写。
	if !t.ReadOnly && !inst.AllowWrite() {
		return nil, nil, fmt.Errorf("%s 会改动 %s 的数据，该渠道的写开关未打开", tool, p.man.Name)
	}
	return p.invoke(ctx, inst, envelope{Op: "call", Tool: tool, Args: args})
}

type envelope struct {
	Op       string            `json:"op"`
	Tool     string            `json:"tool,omitempty"`
	Args     map[string]any    `json:"args,omitempty"`
	Account  string            `json:"account"`
	BaseURL  string            `json:"baseUrl"`
	Settings map[string]string `json:"settings,omitempty"`
}

type pluginResult struct {
	OK     bool           `json:"ok"`
	Result any            `json:"result"`
	Meta   map[string]any `json:"meta"`
	Error  string         `json:"error"`
}

func (p *Plugin) invoke(ctx context.Context, inst Instance, env envelope) (any, map[string]any, error) {
	base, err := p.baseFor(inst)
	if err != nil {
		return nil, nil, err
	}
	env.Account, env.BaseURL, env.Settings = inst.Account, base, inst.Settings

	raw, err := json.Marshal(env)
	if err != nil {
		return nil, nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = nil
	p.secret, p.baseURL = inst.Secret, base

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// 把请求放进插件内存：向它要一块地方，再写进去。
	ptrs, err := p.allocF.Call(ctx, uint64(len(raw)))
	if err != nil || len(ptrs) != 1 {
		return nil, nil, fmt.Errorf("插件 alloc 失败: %v", err)
	}
	ptr := uint32(ptrs[0])
	if !p.mod.Memory().Write(ptr, raw) {
		return nil, nil, fmt.Errorf("写入插件内存失败（越界）")
	}

	res, err := p.callFn.Call(ctx, uint64(ptr), uint64(len(raw)))
	if err != nil {
		return nil, nil, fmt.Errorf("插件 %s 执行失败: %w", p.man.ID, err)
	}
	if len(res) != 1 {
		return nil, nil, fmt.Errorf("插件 %s 的 call 没有返回值", p.man.ID)
	}
	outPtr, outLen := uint32(res[0]>>32), uint32(res[0]&0xffffffff)
	if outLen == 0 {
		return nil, nil, fmt.Errorf("插件 %s 返回了空结果", p.man.ID)
	}
	body, ok := p.mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, nil, fmt.Errorf("读取插件结果失败（越界）")
	}

	var pr pluginResult
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, nil, fmt.Errorf("插件 %s 的结果不是 JSON: %w", p.man.ID, err)
	}
	if !pr.OK {
		if pr.Error == "" {
			pr.Error = "插件没有说明失败原因"
		}
		return nil, nil, fmt.Errorf("%s", pr.Error)
	}
	return pr.Result, pr.Meta, nil
}

// baseFor 解析这个实例实际要打的接入点。
func (p *Plugin) baseFor(inst Instance) (string, error) {
	raw := p.man.BaseURL
	if p.man.BaseURLField != "" {
		raw = strings.TrimSpace(inst.Settings[p.man.BaseURLField])
		if raw == "" {
			return "", fmt.Errorf("缺少站点地址（字段 %s）", p.man.BaseURLField)
		}
		// 配置文件是可手改的，不能信任落盘时校过。
		if err := checkHTTPSURL(raw); err != nil {
			return "", fmt.Errorf("站点地址 %w", err)
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

// ───────────────────────── 宿主函数 ─────────────────────────

type fetchRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type fetchResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// 这几个头不许插件自己设：Authorization 由宿主注入，Host 决定请求实际去哪。
var reservedHeaders = map[string]bool{
	"authorization": true, "host": true, "content-length": true,
}

// hostFetch 是插件唯一的对外通道。返回响应 JSON 的长度，响应暂存在宿主侧，
// 由插件再调 read 取走 —— 分两步是为了不从宿主回调插件（重入会把 Go
// 运行时搅乱）。
func (p *Plugin) hostFetch(ctx context.Context, m api.Module, ptr, length uint32) uint32 {
	raw, ok := m.Memory().Read(ptr, length)
	if !ok {
		return p.reply(fetchResponse{Error: "插件传来的请求越界"})
	}
	var fr fetchRequest
	if err := json.Unmarshal(raw, &fr); err != nil {
		return p.reply(fetchResponse{Error: "插件传来的请求不是 JSON: " + err.Error()})
	}

	resp, err := p.perform(ctx, fr)
	if err != nil {
		return p.reply(fetchResponse{Error: err.Error()})
	}
	return p.reply(resp)
}

// perform 做真正的请求，并在这里执行全部沙箱校验。
func (p *Plugin) perform(ctx context.Context, fr fetchRequest) (fetchResponse, error) {
	method := strings.ToUpper(strings.TrimSpace(fr.Method))
	if !allowedMethods[method] {
		return fetchResponse{}, p.deny("方法 %s 不允许", method)
	}
	u, err := url.Parse(fr.URL)
	if err != nil {
		return fetchResponse{}, p.deny("地址无法解析: %s", fr.URL)
	}
	if u.Scheme != "https" {
		// 明文不行：插件发出去的东西带着用户的身份。
		return fetchResponse{}, p.deny("只允许 https，收到 %s", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if !p.hostAllowed(host) {
		// 这是沙箱最要紧的一条：模块只能打 manifest 声明、用户看过的主机。
		return fetchResponse{}, p.deny("主机 %s 不在该插件声明的 allowedHosts 内（%s）",
			host, strings.Join(p.man.AllowedHosts, ", "))
	}

	var body io.Reader
	if fr.Body != "" {
		body = strings.NewReader(fr.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fetchResponse{}, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "mcp-hub")
	for k, v := range fr.Headers {
		if reservedHeaders[strings.ToLower(strings.TrimSpace(k))] {
			// 静默丢掉比报错好：插件可能只是照抄了文档里的示例头。
			continue
		}
		req.Header.Set(k, v)
	}
	if err := p.injectAuth(req); err != nil {
		return fetchResponse{}, err
	}

	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: callTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fetchResponse{}, fmt.Errorf("请求 %s %s 失败: %v", method, u.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fetchResponse{}, fmt.Errorf("读取响应失败: %w", err)
	}
	p.fetches++
	return fetchResponse{Status: resp.StatusCode, Body: string(data)}, nil
}

// injectAuth 由宿主带上凭据。插件看不到它，所以也带不走。
func (p *Plugin) injectAuth(req *http.Request) error {
	kind := p.man.Auth.Kind
	if kind == "" || kind == "none" {
		return nil
	}
	if p.secret == nil {
		return fmt.Errorf("没有可用的凭据")
	}
	secret, err := p.secret()
	if err != nil {
		return err
	}
	switch kind {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+secret)
	case "header":
		req.Header.Set(p.man.Auth.Name, secret)
	case "basic":
		req.SetBasicAuth(req.URL.User.Username(), secret)
	}
	return nil
}

func (p *Plugin) hostAllowed(host string) bool {
	for _, h := range p.man.AllowedHosts {
		if host == h {
			return true
		}
	}
	return false
}

func (p *Plugin) deny(format string, a ...any) error {
	msg := fmt.Sprintf(format, a...)
	p.denied = append(p.denied, msg)
	return fmt.Errorf("沙箱拒绝: %s", msg)
}

func (p *Plugin) reply(r fetchResponse) uint32 {
	b, err := json.Marshal(r)
	if err != nil {
		b = []byte(`{"error":"宿主序列化响应失败"}`)
	}
	p.pending = b
	return uint32(len(b))
}

// hostRead 把暂存的响应拷进插件内存。
func (p *Plugin) hostRead(_ context.Context, m api.Module, ptr uint32) {
	if len(p.pending) == 0 {
		return
	}
	m.Memory().Write(ptr, p.pending)
	p.pending = nil
}

// hostLog 收插件的诊断输出。
func (p *Plugin) hostLog(_ context.Context, m api.Module, ptr, length uint32) {
	if p.Log == nil || length == 0 {
		return
	}
	if b, ok := m.Memory().Read(ptr, length); ok {
		p.Log(string(b))
	}
}
