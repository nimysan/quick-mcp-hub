// 云之家方言的设备流。
//
// 和 RFC 8628 的差别全在线上格式，不在流程：
//
//   - 请求是 JSON，参数名叫 appId / deviceCode（不是 client_id / device_code），
//     而且没有 grant_type。
//   - 响应裹着一层 {data, error, errorCode, success} 的信封，真正的授权状态
//     在 data 里面 —— 外层 success 为 true 只表示「这次调用本身没出错」，
//     用户授权了没有要看 data.success。两层都要判，少判一层的后果是
//     「一直等待授权」或者「把失败当成拿到了令牌」。
//   - 出码响应不带验证地址（RFC 8628 要求带），所以那一页的地址只能由
//     声明给出。
//
// 不同的地方到此为止：data.errorCode 用的是 RFC 8628 的标准串
// （实测到 authorization_pending、expired_token），所以错误翻译直接复用
// describe，没有第二套。
//
// 这些形状是对 www.yunzhijia.com 实测出来的，不是照文档写的 —— 云之家的
// 开放平台文档是个 SPA，抓不到接口清单。所以它们随时可能变。
package oauthdev

import (
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

// yzjEnvelope 是云之家所有接口的外层信封。
type yzjEnvelope struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	ErrorCode any             `json:"errorCode"`
	Success   bool            `json:"success"`
}

// yzjDeviceCode 是出码响应的 data。
type yzjDeviceCode struct {
	ClientID   string `json:"clientId"`
	DeviceCode string `json:"deviceCode"`
	UserCode   string `json:"userCode"`
	ExpiresIn  int    `json:"expiresIn"`
	Interval   int    `json:"interval"`
}

// yzjToken 是轮询和刷新响应的 data。
//
// 两个端点的 data 形状不一样，这件事是实测出来的，而两处差异各自都足以
// 让刷新彻底失效：
//
//	轮询 /cli/pollTokenByDeviceCode
//	  {success, errorCode, errorDescription, accessToken, refreshToken,
//	   expiresIn, scope, suggestedInterval}
//	刷新 /oauth2/token/refreshToken
//	  {accessToken, refreshToken, expireIn}
type yzjToken struct {
	// Success 只有轮询响应才有 —— 它必须表达「用户还没授权」这种
	// 「调用成功但授权没完成」的状态。刷新没有这个中间态，所以刷新
	// 响应里没有这个字段，外层信封的 success 就是全部结论。
	//
	// 所以它必须是指针：用 bool 的话「字段不存在」会被读成 false，
	// 于是每一次刷新都被判成失败，渠道在令牌过期后直接不可用。
	Success          *bool  `json:"success"`
	ErrorCode        string `json:"errorCode"`
	ErrorDescription string `json:"errorDescription"`
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	Scope            string `json:"scope"`
	ExpiresIn        int    `json:"expiresIn"`
	// ExpireIn 是刷新响应里的拼法 —— 没有那个 s。只认 expiresIn 的
	// 后果不是报错而是静默：刷新到的令牌没有到期时间，Stale 永远为
	// false，于是它再也不会被刷新，两小时后每次调用都失败。
	ExpireIn int `json:"expireIn"`
	// RefreshExpiresIn 两个端点都没返回过，留着是因为令牌响应里出现
	// 它是常见的。为空意味着刷新令牌不记到期时间，CanRefresh 永远为真。
	RefreshExpiresIn int `json:"refreshExpiresIn"`
}

// expiry 返回访问令牌的有效秒数，兼容两个端点的两种拼法。
func (t yzjToken) expiry() int {
	if t.ExpiresIn > 0 {
		return t.ExpiresIn
	}
	return t.ExpireIn
}

// failed 报告这份 data 本身是否表示失败。
//
// 只有 success 字段真的存在、而且为 false 才算失败。字段不存在时不作
// 判断 —— 那是刷新响应的正常形状，这时结论已经由外层信封给过了。
func (t yzjToken) failed() bool {
	return t.Success != nil && !*t.Success
}

func (c Config) startYunzhijia(ctx context.Context, hc *http.Client) (Pending, error) {
	verify := strings.TrimSpace(c.VerificationURI)
	if verify == "" {
		// 没有这一页，用户无处输码。不能猜一个地址出来：猜错的后果是
		// 把用户送到一个陌生页面去登录自己的账号。
		return Pending{}, fmt.Errorf("声明里缺少 verificationUri（云之家出码响应不返回它）")
	}

	body := map[string]any{"appId": c.ClientID}
	if len(c.Scopes) > 0 {
		body["scope"] = strings.Join(c.Scopes, " ")
	}
	var got yzjDeviceCode
	if err := c.postYunzhijia(ctx, hc, c.DeviceCodeURL, body, &got); err != nil {
		return Pending{}, err
	}
	if got.DeviceCode == "" || got.UserCode == "" {
		return Pending{}, fmt.Errorf("授权服务没有返回设备码")
	}

	interval := time.Duration(got.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expires := time.Duration(got.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}

	complete, err := withCodeQuery(verify, got.UserCode)
	if err != nil {
		return Pending{}, err
	}
	return Pending{
		DeviceCode:              got.DeviceCode,
		UserCode:                got.UserCode,
		VerificationURI:         verify,
		VerificationURIComplete: complete,
		Interval:                interval,
		ExpiresAt:               time.Now().Add(expires),
	}, nil
}

func (c Config) pollYunzhijia(ctx context.Context, hc *http.Client, deviceCode string) (TokenSet, error) {
	body := map[string]any{"appId": c.ClientID, "deviceCode": deviceCode}
	return c.tokenYunzhijia(ctx, hc, c.TokenURL, body)
}

func (c Config) refreshYunzhijia(ctx context.Context, hc *http.Client, refreshToken string) (TokenSet, error) {
	// timestamp 是必填的（不给就是「timestamp不能为空」）。但刷新不要
	// secret —— 实测带上 appId/refreshToken/timestamp 就过了参数校验，
	// 只因为令牌本身是假的才被拒。这一点很要紧：公共客户端能自己刷新，
	// 否则令牌（约 100 分钟）一过期就得让用户重新扫码。
	body := map[string]any{
		"appId":        c.ClientID,
		"refreshToken": refreshToken,
		"timestamp":    strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	return c.tokenYunzhijia(ctx, hc, c.refreshEndpoint(), body)
}

func (c Config) tokenYunzhijia(ctx context.Context, hc *http.Client, endpoint string, body map[string]any) (TokenSet, error) {
	var got yzjToken
	if err := c.postYunzhijia(ctx, hc, endpoint, body, &got); err != nil {
		return TokenSet{}, err
	}
	if got.failed() {
		if got.ErrorCode != "" {
			return TokenSet{}, describe(got.ErrorCode, got.ErrorDescription)
		}
		if got.ErrorDescription != "" {
			return TokenSet{}, fmt.Errorf("授权失败: %s", got.ErrorDescription)
		}
		return TokenSet{}, fmt.Errorf("授权失败（授权服务没有说明原因）")
	}
	if got.AccessToken == "" {
		return TokenSet{}, fmt.Errorf("授权服务没有返回访问令牌")
	}
	now := time.Now()
	t := TokenSet{
		Marker:       TokenMarker,
		AccessToken:  got.AccessToken,
		RefreshToken: got.RefreshToken,
		Scope:        got.Scope,
		ObtainedAt:   now,
	}
	if secs := got.expiry(); secs > 0 {
		t.ExpiresAt = now.Add(time.Duration(secs) * time.Second)
	} else {
		// 没有到期时间的令牌永远不会被判为该刷新了，所以它一过期渠道就
		// 死在原地。宁可报错也不要存一份注定会这样坏掉的凭据。
		return TokenSet{}, fmt.Errorf("授权服务没有返回访问令牌的有效期")
	}
	if got.RefreshExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(got.RefreshExpiresIn) * time.Second)
	}
	return t, nil
}

// postYunzhijia 发一次 JSON 请求，拆掉外层信封，把 data 解进 out。
func (c Config) postYunzhijia(ctx context.Context, hc *http.Client, endpoint string, body map[string]any, out any) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("缺少授权服务地址")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

	var env yzjEnvelope
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("授权服务返回 %s: %s", resp.Status, excerpt(raw))
		}
		return fmt.Errorf("授权响应不是 JSON: %s", excerpt(raw))
	}
	// 云之家一律回 HTTP 200，失败藏在信封里 —— 只看状态码等于看不见错误。
	if !env.Success {
		if env.Error != "" {
			return fmt.Errorf("授权服务返回失败: %s", env.Error)
		}
		return fmt.Errorf("授权服务返回失败: %s", excerpt(raw))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("授权服务没有返回内容: %s", excerpt(raw))
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("授权响应解析失败: %s", excerpt(raw))
	}
	return nil
}

// withCodeQuery 把给用户看的码挂到验证地址上，省掉手输。
// 用 url 解析而不是字符串拼接：地址本身可能已经带了查询参数。
func withCodeQuery(base, code string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("verificationUri 不是合法地址: %w", err)
	}
	q := u.Query()
	q.Set("code", code)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
