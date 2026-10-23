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
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
//
// 对登录式渠道（auth.kind=login）它是有状态的：每个实例一份会话（cookie jar），
// 惰性登录、整进程复用，会话过期时自动重登并重试一次。会话按
// 「渠道类型/账号」隔离，两个账号不会共用一个登录态。
type Client struct {
	// HTTP 可替换，测试里指向 httptest。
	HTTP *http.Client

	mu       sync.Mutex
	sessions map[string]*session
}

// session 是一个实例的登录态。
type session struct {
	mu     sync.Mutex
	client *http.Client
	live   bool
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: callTimeout}
}

// session 取（或建）一个实例的会话。
func (c *Client) session(rt Runtime) (*session, error) {
	key := rt.Spec.ID + "/" + rt.Account
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions == nil {
		c.sessions = map[string]*session{}
	}
	if s, ok := c.sessions[key]; ok {
		return s, nil
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	base := c.httpClient()
	// 复制一份，只换 cookie jar：httptest 给的 client 带着测试用的 TLS 配置。
	hc := &http.Client{Transport: base.Transport, Timeout: base.Timeout, Jar: jar}
	if hc.Timeout == 0 {
		hc.Timeout = callTimeout
	}
	s := &session{client: hc}
	c.sessions[key] = s
	return s, nil
}

// login 跑一次登录流程，把会话 cookie 留在 jar 里。
func (c *Client) login(ctx context.Context, rt Runtime, s *session) error {
	l := rt.Spec.Auth.Login
	base, err := rt.BaseURL()
	if err != nil {
		return err
	}
	secret, err := rt.Secret()
	if err != nil {
		return err
	}
	body, err := renderLoginBody(l.Body, rt, secret)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+l.Path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mcp-hub")
	for k, v := range rt.Spec.Headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("登录 %s 失败: %s", rt.Spec.Name, scrub(err.Error(), secret))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("读取登录响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("登录 %s 返回 %s: %s", rt.Spec.Name, resp.Status, scrub(excerpt(data), secret))
	}
	// 很多企业接口登录失败也回 200，必须看响应内容。
	if l.SuccessPath != "" {
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("登录响应不是 JSON: %s", excerpt(data))
		}
		got, ok := pick(decoded, l.SuccessPath)
		if !ok || !sameJSON(got, l.SuccessEquals) {
			return fmt.Errorf("登录 %s 未通过（%s=%v，期望 %v）: %s",
				rt.Spec.Name, l.SuccessPath, got, l.SuccessEquals, scrub(excerpt(data), secret))
		}
	}
	s.live = true
	return nil
}

// ensureLogin 保证会话可用。已登录就直接返回，不会每次调用都登一遍。
func (c *Client) ensureLogin(ctx context.Context, rt Runtime, s *session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		return nil
	}
	return c.login(ctx, rt, s)
}

func (s *session) invalidate() {
	s.mu.Lock()
	s.live = false
	s.mu.Unlock()
}

// renderLoginBody 把登录体里的取值标记换成实际值。
// 只认 {{account}} / {{secret}} / {{field:名字}}，别的照字面发。
func renderLoginBody(tpl map[string]any, rt Runtime, secret string) (map[string]any, error) {
	out := make(map[string]any, len(tpl))
	for k, v := range tpl {
		str, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		var bad error
		out[k] = tokenRe.ReplaceAllStringFunc(str, func(m string) string {
			tok := strings.TrimSpace(tokenRe.FindStringSubmatch(m)[1])
			switch {
			case tok == "account":
				return rt.Account
			case tok == "secret":
				return secret
			case strings.HasPrefix(tok, "field:"):
				name := strings.TrimSpace(strings.TrimPrefix(tok, "field:"))
				val := strings.TrimSpace(rt.Settings[name])
				if val == "" {
					if f, ok := rt.Spec.Field(name); ok {
						val = f.Default
					}
				}
				if val == "" {
					bad = fmt.Errorf("登录需要的字段 %s 没有值", name)
				}
				return val
			default:
				bad = fmt.Errorf("登录体里有不认识的标记 %s", m)
				return ""
			}
		})
		if bad != nil {
			return nil, bad
		}
	}
	return out, nil
}

// Test 验证凭据可用。
//
// 登录式渠道的测试就是登录本身（这类 RPC 接口往往一个 GET 都没有，
// 而随便挑一个 POST 当测试要么会改数据、要么只能靠声明自称无害）。
func (c *Client) Test(ctx context.Context, rt Runtime) (any, error) {
	out, _, err := c.testOrCall(ctx, rt)
	return out, err
}

func (c *Client) testOrCall(ctx context.Context, rt Runtime) (any, map[string]any, error) {
	if rt.Spec.Auth.Kind == AuthLogin {
		s, err := c.session(rt)
		if err != nil {
			return nil, nil, err
		}
		s.invalidate() // 测试要真的登一次，不能复用旧会话
		if err := c.ensureLogin(ctx, rt, s); err != nil {
			return nil, nil, err
		}
		return map[string]any{"loggedIn": true, "account": rt.Account}, nil, nil
	}
	return c.do(ctx, rt, Tool{
		Name:   rt.Spec.ID + "_test",
		Method: rt.Spec.Test.Method,
		Path:   rt.Spec.Test.Path,
		Result: rt.Spec.Test.Result,
	}, nil)
}

// Call 执行一个声明出来的工具。第二个返回值是分页/总数等元信息。
func (c *Client) Call(ctx context.Context, rt Runtime, toolName string, args map[string]any) (any, map[string]any, error) {
	t, ok := rt.Spec.Tool(toolName)
	if !ok {
		return nil, nil, fmt.Errorf("渠道 %s 没有工具 %s", rt.Spec.ID, toolName)
	}
	if !t.ReadOnly && !rt.AllowWrite() {
		return nil, nil, fmt.Errorf("%s 会改动 %s 的数据，该渠道的写开关未打开", toolName, rt.Spec.Name)
	}
	return c.do(ctx, rt, t, args)
}

func (c *Client) do(ctx context.Context, rt Runtime, t Tool, args map[string]any) (any, map[string]any, error) {
	req, secret, err := c.buildRequest(ctx, rt, t, args)
	if err != nil {
		return nil, nil, err
	}

	hc := c.httpClient()
	var sess *session
	if rt.Spec.Auth.Kind == AuthLogin {
		sess, err = c.session(rt)
		if err != nil {
			return nil, nil, err
		}
		if err := c.ensureLogin(ctx, rt, sess); err != nil {
			return nil, nil, err
		}
		hc = sess.client
	}

	cols := resolveColumns(t, args)
	out, meta, retryable, err := c.send(hc, rt, t, req, secret, cols)
	if err == nil || sess == nil || !retryable {
		return out, meta, err
	}

	// 会话过期：重登一次再试。企业系统的会话常有几十分钟上限，
	// 而 MCP server 是常驻进程，不自动重登就会「用一会儿就全挂」。
	sess.invalidate()
	if lerr := c.ensureLogin(ctx, rt, sess); lerr != nil {
		return nil, nil, fmt.Errorf("%v；重新登录也失败: %w", err, lerr)
	}
	req2, _, berr := c.buildRequest(ctx, rt, t, args)
	if berr != nil {
		return nil, nil, berr
	}
	out, meta, _, err = c.send(sess.client, rt, t, req2, secret, cols)
	return out, meta, err
}

// buildRequest 按声明拼出请求。返回的 secret 只用于把它从错误信息里抹掉。
func (c *Client) buildRequest(ctx context.Context, rt Runtime, t Tool, args map[string]any) (*http.Request, string, error) {
	base, err := rt.BaseURL()
	if err != nil {
		return nil, "", err
	}

	path := t.Path
	query := url.Values{}
	body := deepCopyMap(t.BodyConst)

	for _, p := range t.Params {
		raw, given := args[p.Name]
		if !given || raw == nil || raw == "" {
			if p.Default != "" {
				raw, given = p.Default, true
			}
		}
		if !given || raw == nil {
			if p.Required {
				return nil, "", fmt.Errorf("参数 %s 必填", p.Name)
			}
			continue
		}
		switch p.In {
		case InPath:
			s, err := asString(p, raw)
			if err != nil {
				return nil, "", err
			}
			if s == "" {
				return nil, "", fmt.Errorf("路径参数 %s 不能为空", p.Name)
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(s))
		case InQuery:
			s, err := asString(p, raw)
			if err != nil {
				return nil, "", err
			}
			if s != "" {
				query.Set(p.Name, s)
			}
		case InBody:
			v, err := asTyped(p, raw)
			if err != nil {
				return nil, "", err
			}
			if body == nil {
				body = map[string]any{}
			}
			if err := setPath(body, bodyKey(p), v); err != nil {
				return nil, "", err
			}
		}
	}
	if i := strings.IndexByte(path, '{'); i >= 0 {
		return nil, "", fmt.Errorf("路径还有未填的占位符: %s", path[i:])
	}

	secret, err := rt.Secret()
	if err != nil {
		return nil, "", err
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
			return nil, "", err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, t.Method, full, reader)
	if err != nil {
		return nil, "", err
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
	case AuthLogin:
		// 凭据在会话 cookie 里，请求本身不带
	}
	return req, secret, nil
}

// send 发请求并裁剪结果。retryable 为真表示「重新登录后值得再试一次」。
func (c *Client) send(hc *http.Client, rt Runtime, t Tool, req *http.Request, secret string, cols []string) (out any, meta map[string]any, retryable bool, err error) {
	resp, err := hc.Do(req)
	if err != nil {
		// http.Client 的错误信息里会带 URL，而 query 方式下 URL 含凭据，所以不直接回显。
		return nil, nil, false, fmt.Errorf("请求 %s %s 失败: %s", t.Method, t.Path, scrub(err.Error(), secret))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, false, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retry := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			matchesRelogin(rt, raw)
		return nil, nil, retry, fmt.Errorf("%s %s 返回 %s: %s",
			t.Method, t.Path, resp.Status, scrub(excerpt(raw), secret))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{"ok": true, "status": resp.StatusCode}, nil, false, nil
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, nil, false, fmt.Errorf("响应不是 JSON（%s）: %s", resp.Header.Get("Content-Type"), excerpt(raw))
	}

	// HTTP 200 但业务失败：不判定就会把失败当成功，模型拿着空结果编结论。
	if fw := rt.Spec.FailWhen; fw != nil {
		if got, ok := pick(decoded, fw.Path); ok {
			failed := fw.Exists || sameJSON(got, fw.Equals)
			if failed {
				msg := excerpt(raw)
				if fw.MessagePath != "" {
					if m, ok := pick(decoded, fw.MessagePath); ok {
						msg = fmt.Sprintf("%v", m)
					}
				}
				return nil, nil, matchesRelogin(rt, raw), fmt.Errorf("%s 返回业务失败: %s", t.Path, scrub(msg, secret))
			}
		}
	}

	shaped, shapedMeta, err := shape(decoded, t.Result, cols)
	return shaped, shapedMeta, false, err
}

// matchesRelogin 判断响应内容是否命中「会话过期」的字样。
func matchesRelogin(rt Runtime, body []byte) bool {
	if rt.Spec.Auth.Login == nil {
		return false
	}
	s := string(body)
	for _, marker := range rt.Spec.Auth.Login.ReloginOn {
		if marker != "" && strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// shape 按 Result 声明裁剪响应。cols 是已解析出的列名（可能来自参数）。
// 第二个返回值是 Meta 声明的那些兄弟字段（total / nextPageToken 之类）。
func shape(v any, r Result, cols []string) (any, map[string]any, error) {
	var meta map[string]any
	for _, path := range r.Meta {
		if got, ok := pick(v, path); ok {
			if meta == nil {
				meta = map[string]any{}
			}
			meta[lastSeg(path)] = got
		}
	}
	if r.Path != "" {
		got, ok := pick(v, r.Path)
		if !ok {
			return nil, nil, fmt.Errorf("响应里没有 %s", r.Path)
		}
		v = got
	}
	switch t := v.(type) {
	case []any:
		if r.Limit > 0 && len(t) > r.Limit {
			t = t[:r.Limit]
			if meta == nil {
				meta = map[string]any{}
			}
			// 被本地上限截断过就说明还有更多，这件事必须说出来，
			// 否则模型会把「前 N 条」当成「全部」。
			meta["truncatedTo"] = r.Limit
		}
		out := make([]any, 0, len(t))
		for _, item := range t {
			// 二维数组结果（金蝶 ExecuteBillQuery）：按列名拼成对象，
			// 否则模型拿到的是一堆位置相关的裸值，错位了也看不出来。
			if row, ok := item.([]any); ok && len(cols) > 0 {
				out = append(out, zipRow(cols, row))
				continue
			}
			out = append(out, project(item, r.Fields))
		}
		return out, meta, nil
	default:
		return project(v, r.Fields), meta, nil
	}
}

// lastSeg 取点号路径的最后一段，作为 meta 里的键名。
func lastSeg(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// resolveColumns 解析列名：声明里写死的，或者由某个参数带来的。
func resolveColumns(t Tool, args map[string]any) []string {
	if len(t.Result.Columns) > 0 {
		return t.Result.Columns
	}
	from := t.Result.ColumnsFromParam
	if from == "" {
		return nil
	}
	raw, ok := args[from]
	if !ok {
		for _, p := range t.Params {
			if p.Name == from && p.Default != "" {
				raw = p.Default
			}
		}
	}
	s, _ := raw.(string)
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// zipRow 把一行裸值按列名拼成对象。列数不匹配时多出来的值按下标命名，
// 丢掉它们会让「查出来的字段比要的少」这种问题静默消失。
func zipRow(cols []string, row []any) map[string]any {
	out := make(map[string]any, len(row))
	for i, v := range row {
		if i < len(cols) {
			out[cols[i]] = v
		} else {
			out[fmt.Sprintf("col%d", i)] = v
		}
	}
	return out
}

// deepCopyMap 复制一份 bodyConst：声明是全进程共享的，直接往里写参数
// 会污染下一次调用。
func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(sub)
			continue
		}
		out[k] = v
	}
	return out
}

// bodyKey 返回一个 body 参数的落点路径。
func bodyKey(p Param) string {
	if p.BodyPath != "" {
		return p.BodyPath
	}
	return p.Name
}

// setPath 按点号路径往嵌套 map 里写值，中间层缺了就建。
func setPath(m map[string]any, path string, v any) error {
	segs := strings.Split(path, ".")
	cur := m
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg]
		if !ok {
			child := map[string]any{}
			cur[seg] = child
			cur = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("body 路径 %s 冲突：%s 已经是非对象值", path, seg)
		}
		cur = child
	}
	cur[segs[len(segs)-1]] = v
	return nil
}

// sameJSON 比较两个来自 JSON 的值。数字统一按 float64 比，
// 这样声明里写 1 和响应里的 1.0 是相等的。
func sameJSON(a, b any) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
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
//
// 会穿透「单元素数组包着一个对象」这一层：金蝶 ExecuteBillQuery 成功时回
// 二维数组、失败时回 [{"Result":{"ResponseStatus":...}}]，不穿透的话
// 业务失败判定就对这类接口失效。
func pick(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		if arr, ok := cur.([]any); ok && len(arr) == 1 {
			if _, isMap := arr[0].(map[string]any); isMap {
				cur = arr[0]
			}
		}
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
