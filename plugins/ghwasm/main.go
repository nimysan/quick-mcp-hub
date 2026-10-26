//go:build wasip1

// 这是一个 connector 插件，站在「第三方贡献者」的位置写的：
// 它编译成 wasm，由 mcp-hub 的 exe 加载执行，不是 hub 本体的一部分。
//
// 它故意不 import mcp-hub 的任何包 —— 贡献者拿到的只有 ABI 约定（这个文件
// 顶部的那几个 //go:wasmimport / //go:wasmexport），不需要读 hub 的代码。
//
// 构建：
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugins/dist/github.wasm ./plugins/github
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unsafe"
)

// ───────────────────────── ABI v1 ─────────────────────────
//
// 宿主给插件的，只有这三个函数。没有文件、没有 socket、没有钥匙串 ——
// wasm 里本来就没有系统调用，所以这不是「被限制」，是「只有这些」。

// hostFetch 发一次 HTTPS 请求。宿主会校验目标主机在 manifest 声明的范围内。
// 入参是一段 JSON 的指针和长度，返回值是响应 JSON 的长度（响应暂存在宿主侧）。
//
//go:wasmimport mcphub fetch
func hostFetch(ptr, length uint32) uint32

// hostRead 把上一次 fetch 的响应拷进插件内存的 ptr 处。
// 分两步是为了避免宿主回调插件的 alloc —— 重入会把 Go 运行时搅乱。
//
//go:wasmimport mcphub read
func hostRead(ptr uint32)

// hostLog 写一行诊断日志。宿主决定要不要显示。
//
//go:wasmimport mcphub log
func hostLog(ptr, length uint32)

// retained / inbox 挡住 GC：这两块内存的裸指针被交给了宿主，
// 在宿主用完之前不能被回收。
var (
	retained []byte
	inbox    []byte
)

// pluginABI 让宿主在执行任何逻辑前就能判断版本是否兼容。
//
//go:wasmexport plugin_abi
func pluginABI() uint32 { return 1 }

// alloc 让宿主在插件内存里开一块地方放请求。
// 不需要 free：插件里是 Go，交给 GC。
//
//go:wasmexport alloc
func alloc(n uint32) uint32 {
	if n == 0 {
		n = 1
	}
	inbox = make([]byte, n)
	return uint32(uintptr(unsafe.Pointer(&inbox[0])))
}

// call 是唯一的入口。入参 / 返回都是 JSON，形状见 request / response。
//
//go:wasmexport call
func call(ptr, length uint32) uint64 {
	raw := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return ret(response{Error: "插件收到的请求不是 JSON: " + err.Error()})
	}
	result, meta, err := dispatch(req)
	if err != nil {
		return ret(response{Error: err.Error()})
	}
	return ret(response{OK: true, Result: result, Meta: meta})
}

func main() {} // c-shared 模式下不会被调用，但 Go 要求它存在

// ───────────────────────── envelope ─────────────────────────

type request struct {
	Op       string            `json:"op"` // "call" | "test"
	Tool     string            `json:"tool"`
	Args     map[string]any    `json:"args"`
	Account  string            `json:"account"`
	BaseURL  string            `json:"baseUrl"`
	Settings map[string]string `json:"settings"`
	// 注意：这里没有 secret 字段。凭据由宿主在 fetch 时注入请求头，
	// 插件从头到尾拿不到 token —— 它只能「使用」这个身份，不能带走。
}

type response struct {
	OK     bool           `json:"ok"`
	Result any            `json:"result,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func ret(r response) uint64 {
	b, err := json.Marshal(r)
	if err != nil {
		b = []byte(`{"ok":false,"error":"插件序列化结果失败"}`)
	}
	retained = b
	return uint64(uintptr(unsafe.Pointer(&retained[0])))<<32 | uint64(len(retained))
}

func logf(format string, a ...any) {
	msg := []byte(fmt.Sprintf(format, a...))
	if len(msg) == 0 {
		return
	}
	hostLog(uint32(uintptr(unsafe.Pointer(&msg[0]))), uint32(len(msg)))
}

// ───────────────────────── 业务逻辑 ─────────────────────────
//
// 下面这些就是「声明式配方表达不了、只能写代码」的那一层在插件里的样子。
// 这个例子本身是能声明的（GitHub 就是普通 REST），选它是为了能和已有的
// 声明式实现逐字段对比 —— 行为一致才说明这条路真的跑通了。

func dispatch(req request) (any, map[string]any, error) {
	if req.Op == "test" {
		out, _, err := get(req, "/user", nil, []string{"login", "name", "html_url"}, 0)
		return out, nil, err
	}
	switch req.Tool {
	case "ghwasm_whoami":
		return get(req, "/user", nil,
			[]string{"login", "name", "html_url", "public_repos"}, 0)

	case "ghwasm_list_my_repos":
		q := map[string]string{
			"per_page":    str(req.Args["per_page"], "20"),
			"sort":        str(req.Args["sort"], "pushed"),
			"affiliation": str(req.Args["affiliation"], "owner,collaborator"),
		}
		return get(req, "/user/repos", q, []string{
			"full_name", "private", "description", "language",
			"pushed_at", "html_url", "open_issues_count",
		}, 50)

	case "ghwasm_search_repos":
		qv := str(req.Args["q"], "")
		if qv == "" {
			return nil, nil, errors.New("参数 q 必填")
		}
		q := map[string]string{"q": qv, "per_page": str(req.Args["per_page"], "10")}
		if s := str(req.Args["sort"], ""); s != "" {
			q["sort"] = s
		}
		body, err := fetchJSON(req, "GET", "/search/repositories", q, nil)
		if err != nil {
			return nil, nil, err
		}
		meta := map[string]any{}
		for _, k := range []string{"total_count", "incomplete_results"} {
			if v, ok := pick(body, k); ok {
				meta[k] = v
			}
		}
		items, _ := pick(body, "items")
		out, trunc := shapeList(items, []string{
			"full_name", "description", "language", "stargazers_count",
			"pushed_at", "html_url", "license.spdx_id",
		}, 30)
		if trunc > 0 {
			meta["truncatedTo"] = trunc
		}
		return out, meta, nil

	case "ghwasm_list_issues":
		owner, repo := str(req.Args["owner"], ""), str(req.Args["repo"], "")
		if owner == "" || repo == "" {
			return nil, nil, errors.New("参数 owner 和 repo 必填")
		}
		q := map[string]string{
			"state":    str(req.Args["state"], "open"),
			"per_page": str(req.Args["per_page"], "20"),
		}
		if l := str(req.Args["labels"], ""); l != "" {
			q["labels"] = l
		}
		return get(req, "/repos/"+esc(owner)+"/"+esc(repo)+"/issues", q, []string{
			"number", "title", "state", "user.login",
			"comments", "created_at", "updated_at", "html_url",
		}, 50)

	case "ghwasm_get_issue":
		owner, repo := str(req.Args["owner"], ""), str(req.Args["repo"], "")
		num := str(req.Args["issue_number"], "")
		if owner == "" || repo == "" || num == "" {
			return nil, nil, errors.New("参数 owner、repo、issue_number 必填")
		}
		return get(req, "/repos/"+esc(owner)+"/"+esc(repo)+"/issues/"+esc(num), nil, []string{
			"number", "title", "state", "user.login", "body",
			"comments", "created_at", "updated_at", "html_url",
		}, 0)

	case "ghwasm_create_issue":
		owner, repo := str(req.Args["owner"], ""), str(req.Args["repo"], "")
		title := str(req.Args["title"], "")
		if owner == "" || repo == "" || title == "" {
			return nil, nil, errors.New("参数 owner、repo、title 必填")
		}
		payload := map[string]any{"title": title}
		if b := str(req.Args["body"], ""); b != "" {
			payload["body"] = b
		}
		body, err := fetchJSON(req, "POST", "/repos/"+esc(owner)+"/"+esc(repo)+"/issues", nil, payload)
		if err != nil {
			return nil, nil, err
		}
		return project(body, []string{"number", "title", "state", "html_url"}), nil, nil
	}
	return nil, nil, fmt.Errorf("插件没有工具 %s", req.Tool)
}

func get(req request, path string, query map[string]string, fields []string, limit int) (any, map[string]any, error) {
	body, err := fetchJSON(req, "GET", path, query, nil)
	if err != nil {
		return nil, nil, err
	}
	if _, isList := body.([]any); isList {
		out, trunc := shapeList(body, fields, limit)
		if trunc > 0 {
			return out, map[string]any{"truncatedTo": trunc}, nil
		}
		return out, nil, nil
	}
	return project(body, fields), nil, nil
}

// ───────────────────────── 走宿主的 HTTP ─────────────────────────

type fetchReq struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type fetchResp struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
	Error  string `json:"error"`
}

func fetchJSON(req request, method, path string, query map[string]string, payload any) (any, error) {
	url := strings.TrimRight(req.BaseURL, "/") + path
	if len(query) > 0 {
		parts := make([]string, 0, len(query))
		for _, k := range sortedKeys(query) {
			parts = append(parts, k+"="+escQuery(query[k]))
		}
		url += "?" + strings.Join(parts, "&")
	}

	fr := fetchReq{
		Method: method,
		URL:    url,
		Headers: map[string]string{
			// 这里没有 Authorization —— 宿主会按 manifest 声明的认证方式注入。
			// 插件看不到 token，所以也没法把它发到别处。
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		},
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		fr.Body = string(b)
		fr.Headers["Content-Type"] = "application/json"
	}

	raw, err := json.Marshal(fr)
	if err != nil {
		return nil, err
	}
	n := hostFetch(uint32(uintptr(unsafe.Pointer(&raw[0]))), uint32(len(raw)))
	if n == 0 {
		return nil, errors.New("宿主没有返回响应")
	}
	buf := make([]byte, n)
	hostRead(uint32(uintptr(unsafe.Pointer(&buf[0]))))

	var resp fetchResp
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("宿主响应无法解析: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		logf("%s %s -> %d", method, path, resp.Status)
		return nil, fmt.Errorf("%s %s 返回 %d: %s", method, path, resp.Status, excerpt(resp.Body))
	}
	if strings.TrimSpace(resp.Body) == "" {
		return map[string]any{"ok": true, "status": resp.Status}, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(resp.Body), &decoded); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %s", excerpt(resp.Body))
	}
	return decoded, nil
}

// ───────────────────────── 小工具 ─────────────────────────

func shapeList(v any, fields []string, limit int) (any, int) {
	arr, ok := v.([]any)
	if !ok {
		return project(v, fields), 0
	}
	trunc := 0
	if limit > 0 && len(arr) > limit {
		arr, trunc = arr[:limit], limit
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		out = append(out, project(item, fields))
	}
	return out, trunc
}

func project(v any, fields []string) any {
	if len(fields) == 0 {
		return v
	}
	if _, ok := v.(map[string]any); !ok {
		return v
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if got, ok := pick(v, f); ok {
			out[f] = got
		}
	}
	return out
}

func pick(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func str(v any, def string) string {
	switch t := v.(type) {
	case nil:
		return def
	case string:
		if strings.TrimSpace(t) == "" {
			return def
		}
		return strings.TrimSpace(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return def
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

const hexDigits = "0123456789ABCDEF"

// esc / escQuery 自己实现，不用 net/url：wasip1 下能用，但多带一堆代码进模块。
func esc(s string) string { return pctEncode(s, false) }

func escQuery(s string) string { return pctEncode(s, true) }

func pctEncode(s string, query bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case query && c == ' ':
			b.WriteByte('+')
		case !query && (c == '$' || c == '&' || c == '+' || c == ',' || c == ':' ||
			c == ';' || c == '=' || c == '@'):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
