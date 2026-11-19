// OAuth 2.0 设备授权流（RFC 8628）。
//
// 为什么是设备流而不是浏览器回调：桌面程序存不住 client_secret，而 GitHub 的
// web 流换 token 必须带 secret。设备流只要 client_id —— 它本来就是给「没有
// 浏览器的设备」设计的，正好适合一个发给客户的 exe。
//
// 代价是用户要手动输一个 8 位码（exe 可以顺手把浏览器打开到那一页）。
package oauthdev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenMarker 标识钥匙串里存的是一套 OAuth 令牌，而不是用户手填的 PAT。
//
// 同一个凭据槽要装两种东西（手填的长期令牌 / 连接拿到的令牌组），靠这个
// 标记区分：没有它就只能靠猜字符串长相，那是会错的。
const TokenMarker = "v1"

// TokenSet 是一次授权的产物。存进密码管理器时序列化成 JSON。
type TokenSet struct {
	Marker       string `json:"mcphubOAuth"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	TokenType    string `json:"tokenType,omitempty"`
	Scope        string `json:"scope,omitempty"`
	// ExpiresAt 为零值表示「不过期」—— GitHub 的 OAuth App 默认就是这样。
	ExpiresAt        time.Time `json:"expiresAt,omitempty"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt,omitempty"`
	ObtainedAt       time.Time `json:"obtainedAt"`
	// ClientSecret 只有「换令牌和刷新都必须带应用密钥」的提供方才有（飞书）。
	// 那个密钥属于用户自己企业的应用、由用户提供，不是随程序发出去的，所以
	// 和令牌放在同一个密码管理器条目里：刷新时要用，而且它和这份授权同生
	// 同灭 —— 删连接时一起删，重新授权时一起换。
	ClientSecret string `json:"clientSecret,omitempty"`
	// ClientID 是签发这份令牌的应用。和 ClientSecret 一起存，续期时以它为准：
	// 配置页上的 App ID 是用户能改的，填错一个（两个应用的 ID 前缀几乎一样）
	// 就会让续期报 invalid_grant。
	ClientID string `json:"clientId,omitempty"`
}

// Marshal 序列化成要存进密码管理器的字符串。
func (t TokenSet) Marshal() (string, error) {
	t.Marker = TokenMarker
	b, err := json.Marshal(t)
	return string(b), err
}

// Parse 判断一份已存的凭据是不是 OAuth 令牌组。
// 第二个返回值为 false 表示「这是用户手填的长期令牌」，照原样当 bearer 用。
func Parse(stored string) (TokenSet, bool) {
	s := strings.TrimSpace(stored)
	if !strings.HasPrefix(s, "{") {
		return TokenSet{}, false
	}
	var t TokenSet
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return TokenSet{}, false
	}
	if t.Marker != TokenMarker || t.AccessToken == "" {
		return TokenSet{}, false
	}
	return t, true
}

// expirySkew 提前多久就认为该换了：卡着到期时刻去用，请求在途中就会失效。
const expirySkew = 60 * time.Second

// Stale 报告这套令牌是否该刷新了。不过期的令牌永远返回 false。
func (t TokenSet) Stale(now time.Time) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return now.After(t.ExpiresAt.Add(-expirySkew))
}

// CanRefresh 报告是否还有可用的刷新令牌。
func (t TokenSet) CanRefresh(now time.Time) bool {
	if t.RefreshToken == "" {
		return false
	}
	return t.RefreshExpiresAt.IsZero() || now.Before(t.RefreshExpiresAt)
}

// 授权服务的线上格式。RFC 8628 规定了字段名，但不是每家都照着写。
const (
	// DialectRFC8628 是标准形态：form 编码请求、snake_case 响应、
	// 顶层 error 码。GitHub 走这条。空字符串等同于它。
	DialectRFC8628 = "rfc8628"
	// DialectYunzhijia 是云之家：JSON 请求（参数名 appId/deviceCode）、
	// camelCase 响应、而且整个响应裹在 {data,error,errorCode,success}
	// 信封里，真正的授权状态在 data 里面。错误码本身仍用 RFC 8628 的
	// 标准串（authorization_pending / expired_token），所以 describe
	// 能原样复用。
	DialectYunzhijia = "yunzhijia"
	// DialectFeishu 是飞书：线上格式基本照 RFC 8628，但它是机密客户端 ——
	// 出码要用 Basic(App ID:App Secret)，换令牌和刷新要带 client_secret，
	// 刷新走 JSON 请求体。见 feishu.go。
	DialectFeishu = "feishu"
)

// Config 是一个渠道的设备流参数。
//
// 公共客户端（GitHub、云之家）没有 client_secret —— 随程序发出去的密钥不是
// 秘密。ClientSecret 只给飞书方言用：那里的密钥是用户自己企业应用的，由
// 用户提供或一键建应用时拿到，存在本机密码管理器里。
type Config struct {
	ClientID      string
	ClientSecret  string
	DeviceCodeURL string
	TokenURL      string
	Scopes        []string

	// Dialect 选择线上格式，空＝RFC 8628。
	Dialect string

	// VerificationURI 是用户去输码的那一页。RFC 8628 要求授权服务在
	// 出码响应里返回它，但云之家不返回 —— 只能由声明给出，而声明里
	// 的这个地址按和 baseUrl 同样严格的规则校验：一份被篡改的目录
	// 若能改掉它，用户就会把自己真实账号的密码输给攻击者。
	VerificationURI string

	// RefreshURL 是刷新令牌的地址。留空表示和 TokenURL 是同一个
	// （RFC 8628 就是这样）；云之家的刷新在另一个端点上。
	RefreshURL string
}

// refreshEndpoint 返回实际要打的刷新地址。
func (c Config) refreshEndpoint() string {
	if strings.TrimSpace(c.RefreshURL) != "" {
		return c.RefreshURL
	}
	return c.TokenURL
}

// Pending 是第一步的产物：给用户看的码，和用来轮询的设备码。
type Pending struct {
	DeviceCode string `json:"-"` // 不回传给前端：它等价于一次授权的凭据
	UserCode   string `json:"userCode"`
	// VerificationURI 是用户要去输码的地址。
	VerificationURI string `json:"verificationUri"`
	// VerificationURIComplete 是带上码的一键地址。RFC 8628 可选，
	// GitHub 不返回它；有的提供方返回，能省掉用户手输。
	VerificationURIComplete string        `json:"verificationUriComplete,omitempty"`
	Interval                time.Duration `json:"-"`
	ExpiresAt               time.Time     `json:"expiresAt"`
}

// 轮询过程中的几种状态。调用方要区分它们，因为处置完全不同。
var (
	// ErrPending 用户还没授权，继续等。
	ErrPending = errors.New("等待用户授权")
	// ErrSlowDown 轮询太快，间隔要加 5 秒（RFC 8628 的规定）。
	ErrSlowDown = errors.New("轮询过快")
	// ErrExpired 码过期了，要重新开始。
	ErrExpired = errors.New("验证码已过期，请重新点连接")
	// ErrDenied 用户拒绝了。
	ErrDenied = errors.New("用户拒绝了授权")
)

// Start 走第一步：要一个设备码和给用户看的验证码。
func (c Config) Start(ctx context.Context, hc *http.Client) (Pending, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Pending{}, fmt.Errorf("缺少 client id")
	}
	switch c.Dialect {
	case "", DialectRFC8628:
	case DialectYunzhijia:
		return c.startYunzhijia(ctx, hc)
	case DialectFeishu:
		return c.startFeishu(ctx, hc)
	default:
		// 认不出的方言一律拒绝，不要退回默认实现：按错误的线上格式去打
		// 一个真实的授权服务，错法是「一直等待授权」这种看不出原因的样子。
		return Pending{}, fmt.Errorf("不支持的授权方言 %q", c.Dialect)
	}
	form := url.Values{"client_id": {c.ClientID}}
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}
	var body struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := post(ctx, hc, c.DeviceCodeURL, form, &body); err != nil {
		return Pending{}, err
	}
	if body.Error != "" {
		return Pending{}, describe(body.Error, body.ErrorDescription)
	}
	if body.DeviceCode == "" || body.UserCode == "" {
		return Pending{}, fmt.Errorf("授权服务没有返回设备码")
	}
	interval := time.Duration(body.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second // RFC 8628 的默认值
	}
	expires := time.Duration(body.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	uri := body.VerificationURI
	if uri == "" {
		return Pending{}, fmt.Errorf("授权服务没有返回验证地址")
	}
	return Pending{
		DeviceCode:              body.DeviceCode,
		UserCode:                body.UserCode,
		VerificationURI:         uri,
		VerificationURIComplete: body.VerificationURIComplete,
		Interval:                interval,
		ExpiresAt:               time.Now().Add(expires),
	}, nil
}

// Poll 查一次授权状态。调用方按 Interval 节流，收到 ErrSlowDown 要加间隔。
func (c Config) Poll(ctx context.Context, hc *http.Client, deviceCode string) (TokenSet, error) {
	switch c.Dialect {
	case "", DialectRFC8628:
	case DialectYunzhijia:
		return c.pollYunzhijia(ctx, hc, deviceCode)
	case DialectFeishu:
		return c.pollFeishu(ctx, hc, deviceCode)
	default:
		return TokenSet{}, fmt.Errorf("不支持的授权方言 %q", c.Dialect)
	}
	form := url.Values{
		"client_id":   {c.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	return c.exchange(ctx, hc, form)
}

// Refresh 用刷新令牌换一套新的。
//
// 注意：GitHub 的 OAuth App 刷新接口要求 client_secret，而公共客户端没有它，
// 所以 GitHub 这条路实际走不通 —— 对应做法是不要开「过期令牌」、也不要申请
// offline_access，让令牌长期有效，被吊销时重新点连接。这个函数留着是因为
// 别的提供方（支持公共客户端刷新的那些）用得上。
func (c Config) Refresh(ctx context.Context, hc *http.Client, refreshToken string) (TokenSet, error) {
	switch c.Dialect {
	case "", DialectRFC8628:
	case DialectYunzhijia:
		return c.refreshYunzhijia(ctx, hc, refreshToken)
	case DialectFeishu:
		return c.refreshFeishu(ctx, hc, refreshToken)
	default:
		return TokenSet{}, fmt.Errorf("不支持的授权方言 %q", c.Dialect)
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return c.exchange(ctx, hc, form)
}

// tokenResponse 是 RFC 6749 / 8628 的令牌响应。
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	TokenType             string `json:"token_type"`
	Scope                 string `json:"scope"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
}

func (c Config) exchange(ctx context.Context, hc *http.Client, form url.Values) (TokenSet, error) {
	var body tokenResponse
	if err := post(ctx, hc, c.TokenURL, form, &body); err != nil {
		return TokenSet{}, err
	}
	return body.tokenSet()
}

// tokenSet 把令牌响应变成要存的令牌组；响应里带错误码时返回对应的错误。
func (body tokenResponse) tokenSet() (TokenSet, error) {
	if body.Error != "" {
		return TokenSet{}, describe(body.Error, body.ErrorDescription)
	}
	if body.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("授权服务没有返回访问令牌")
	}
	now := time.Now()
	t := TokenSet{
		Marker:       TokenMarker,
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		TokenType:    body.TokenType,
		Scope:        body.Scope,
		ObtainedAt:   now,
	}
	if body.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	if body.RefreshTokenExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second)
	}
	return t, nil
}

// Await 轮询到拿到令牌为止，自己处理 interval 和 slow_down。
func (c Config) Await(ctx context.Context, hc *http.Client, p Pending) (TokenSet, error) {
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		case <-time.After(interval):
		}
		if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
			return TokenSet{}, ErrExpired
		}
		t, err := c.Poll(ctx, hc, p.DeviceCode)
		switch {
		case err == nil:
			return t, nil
		case errors.Is(err, ErrPending):
			continue
		case errors.Is(err, ErrSlowDown):
			// RFC 8628：收到 slow_down 就把间隔加 5 秒，不是重试一次了事。
			interval += 5 * time.Second
		default:
			return TokenSet{}, err
		}
	}
}

// describe 把授权服务的错误码翻成能据以行动的话。
//
// 这几条都是真会遇到的：device_flow_disabled 是 OAuth App 建好后忘了勾那个
// 复选框，incorrect_client_credentials 是 client id 填错 —— 两者的报错如果
// 都写成「授权失败」，用户无从下手。
func describe(code, desc string) error {
	switch code {
	case "authorization_pending":
		return ErrPending
	case "slow_down":
		return ErrSlowDown
	case "expired_token":
		return ErrExpired
	case "access_denied":
		return ErrDenied
	case "device_flow_disabled":
		return fmt.Errorf("这个 OAuth App 没有开启设备流：到 App 设置里勾上 Enable Device Flow")
	case "incorrect_client_credentials":
		return fmt.Errorf("client id 不对（设备流不需要 client secret，只要 id）")
	case "incorrect_device_code":
		return fmt.Errorf("设备码无效，请重新点连接")
	case "unsupported_grant_type":
		return fmt.Errorf("授权服务不支持设备流")
	}
	if desc != "" {
		return fmt.Errorf("授权失败（%s）: %s", code, desc)
	}
	return fmt.Errorf("授权失败: %s", code)
}

func post(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) error {
	return postRaw(ctx, hc, endpoint, "application/x-www-form-urlencoded", form.Encode(), nil, out)
}

// postRaw 是 post 的通用形：指定请求体类型和额外的请求头。
func postRaw(ctx context.Context, hc *http.Client, endpoint, contentType, body string, header http.Header, out any) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("缺少授权服务地址")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(body))
	if err != nil {
		return err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", contentType)
	// 不指定的话 GitHub 回的是 form 编码，解析起来还要分两套代码。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mcp-hub")

	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接授权服务失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取授权响应失败: %w", err)
	}
	// 401/400 的响应体里也带 error 码，所以先尝试解析再判状态码。
	if jsonErr := json.Unmarshal(raw, out); jsonErr != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("授权服务返回 %s: %s", resp.Status, excerpt(raw))
		}
		return fmt.Errorf("授权响应不是 JSON: %s", excerpt(raw))
	}
	return nil
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
