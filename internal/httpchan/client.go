// 通用执行器：按 Spec 发一次 HTTPS 请求，再把响应裁成给模型看的结果。
// 这里没有任何一个系统的名字——金蝶、Jira、GitHub 的差别全在 Spec 数据里。
package httpchan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// callTimeout 单次调用超时。Quick 对一次 tool 调用的硬上限是 5 分钟，
	// 这里取远小于它的值，免得客户端那边先超时、用户看不到原因。
	callTimeout = 30 * time.Second
	// maxBody 响应体上限，防止一个超大列表把内存和模型上下文都撑爆。
	maxBody = 8 << 20
	// errBodyExcerpt 出错时回显多少字节的响应体，用于定位问题。
	errBodyExcerpt = 400
)

// Runtime 是一个渠道实例的运行态：从哪里发、带什么凭据。
//
// Secret 是函数而不是字符串：凭据惰性取用，只有真的要发请求时才去读
// 系统密码管理器，且不在结构体里长期驻留明文。
type Runtime struct {
	Spec     Spec
	Account  string
	Settings map[string]string
	Secret   func() (string, error)
}

// BaseURL 解析这个实例实际要打的接入点。
func (r Runtime) BaseURL() (string, error) {
	raw := r.Spec.BaseURL
	if r.Spec.BaseURLField != "" {
		raw = strings.TrimSpace(r.Settings[r.Spec.BaseURLField])
		if raw == "" {
			if f, ok := r.Spec.Field(r.Spec.BaseURLField); ok {
				raw = f.Default
			}
		}
		if raw == "" {
			return "", fmt.Errorf("缺少站点地址（字段 %s）", r.Spec.BaseURLField)
		}
		// 用户填的地址同样必须是 https，并且在这里再校一遍：
		// 配置文件是可手改的，不能信任落盘时校过。
		if err := checkBaseURL(raw); err != nil {
			return "", fmt.Errorf("站点地址 %w", err)
		}
	}
	return strings.TrimRight(raw, "/"), nil
}

// AllowWrite 报告这个实例是否打开了写开关。
func (r Runtime) AllowWrite() bool {
	switch strings.ToLower(strings.TrimSpace(r.Settings[AllowWriteField])) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Client 执行声明式调用。零值可用。
type Client struct {
	// HTTP 可替换，测试里指向 httptest。
	HTTP *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: callTimeout}
}

// Test 打一次 spec 声明的只读测试接口，确认凭据可用。
// 返回响应里裁剪后的身份信息，供页面显示「以谁的身份连上了」。
func (c *Client) Test(ctx context.Context, rt Runtime) (any, error) {
	return c.do(ctx, rt, Tool{
		Name:   rt.Spec.Type + "_test",
		Method: rt.Spec.Test.Method,
		Path:   rt.Spec.Test.Path,
		Result: rt.Spec.Test.Result,
	}, nil)
}

// Call 执行一个声明出来的工具。
func (c *Client) Call(ctx context.Context, rt Runtime, toolName string, args map[string]any) (any, error) {
	t, ok := rt.Spec.Tool(toolName)
	if !ok {
		return nil, fmt.Errorf("渠道 %s 没有工具 %s", rt.Spec.Type, toolName)
	}
	if !t.ReadOnly && !rt.AllowWrite() {
		return nil, fmt.Errorf("%s 会改动 %s 的数据，该渠道的写开关未打开", toolName, rt.Spec.Name)
	}
	return c.do(ctx, rt, t, args)
}

func (c *Client) do(ctx context.Context, rt Runtime, t Tool, args map[string]any) (any, error) {
	base, err := rt.BaseURL()
	if err != nil {
		return nil, err
	}

	path := t.Path
	query := url.Values{}
	body := map[string]any{}

	for _, p := range t.Params {
		raw, given := args[p.Name]
		if !given || raw == nil || raw == "" {
			if p.Default != "" {
				raw, given = p.Default, true
			}
		}
		if !given || raw == nil {
			if p.Required {
				return nil, fmt.Errorf("参数 %s 必填", p.Name)
			}
			continue
		}
		switch p.In {
		case InPath:
			s, err := asString(p, raw)
			if err != nil {
				return nil, err
			}
			if s == "" {
				return nil, fmt.Errorf("路径参数 %s 不能为空", p.Name)
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(s))
		case InQuery:
			s, err := asString(p, raw)
			if err != nil {
				return nil, err
			}
			if s != "" {
				query.Set(p.Name, s)
			}
		case InBody:
			v, err := asTyped(p, raw)
			if err != nil {
				return nil, err
			}
			body[p.Name] = v
		}
	}
	if i := strings.IndexByte(path, '{'); i >= 0 {
		return nil, fmt.Errorf("路径还有未填的占位符: %s", path[i:])
	}

	secret, err := rt.Secret()
	if err != nil {
		return nil, err
	}
	if rt.Spec.Auth.Kind == AuthQuery {
		query.Set(rt.Spec.Auth.Name, secret)
	}

	full := base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}

	var reader io.Reader
	if len(body) > 0 {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, t.Method, full, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mcp-hub")
	for k, v := range rt.Spec.Headers {
		req.Header.Set(k, v)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch rt.Spec.Auth.Kind {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
	case AuthHeader:
		req.Header.Set(rt.Spec.Auth.Name, secret)
	case AuthBasic:
		req.SetBasicAuth(rt.Account, secret)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// http.Client 的错误信息里会带 URL，而 query 方式下 URL 含凭据，所以不直接回显。
		return nil, fmt.Errorf("请求 %s %s 失败: %s", t.Method, t.Path, scrub(err.Error(), secret))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s 返回 %s: %s",
			t.Method, t.Path, resp.Status, scrub(excerpt(raw), secret))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"ok": true, "status": resp.StatusCode}, nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("响应不是 JSON（%s）: %s", resp.Header.Get("Content-Type"), excerpt(raw))
	}
	return shape(decoded, t.Result)
}

// shape 按 Result 声明裁剪响应。
func shape(v any, r Result) (any, error) {
	if r.Path != "" {
		got, ok := pick(v, r.Path)
		if !ok {
			return nil, fmt.Errorf("响应里没有 %s", r.Path)
		}
		v = got
	}
	switch t := v.(type) {
	case []any:
		if r.Limit > 0 && len(t) > r.Limit {
			t = t[:r.Limit]
		}
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, project(item, r.Fields))
		}
		return out, nil
	default:
		return project(v, r.Fields), nil
	}
}

// project 只保留声明的字段，键名用完整点号路径，避免 user.login 和 owner.login 撞名。
func project(v any, fields []string) any {
	if len(fields) == 0 {
		return v
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if got, ok := pick(m, f); ok {
			out[f] = got
		}
	}
	return out
}

// pick 按点号路径取值。
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

func asString(p Param, v any) (string, error) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), nil
		}
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	// 走 MCP 过来的参数是 JSON，数字一律是 float64；int 这几支是给
	// Go 侧直接调用（测试、将来的 CLI）留的，省掉调用方自己转换。
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case json.Number:
		return t.String(), nil
	default:
		return "", fmt.Errorf("参数 %s 的值类型不支持（%T）", p.Name, v)
	}
}

func asTyped(p Param, v any) (any, error) {
	switch p.Type {
	case "number":
		switch t := v.(type) {
		case float64:
			return t, nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err != nil {
				return nil, fmt.Errorf("参数 %s 要数字，收到 %q", p.Name, t)
			}
			return f, nil
		}
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(t))
			if err != nil {
				return nil, fmt.Errorf("参数 %s 要布尔值，收到 %q", p.Name, t)
			}
			return b, nil
		}
	}
	return v, nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > errBodyExcerpt {
		return s[:errBodyExcerpt] + "…"
	}
	return s
}

// scrub 把可能混进错误信息的凭据抹掉。
func scrub(s, secret string) string {
	if len(secret) < 6 {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
