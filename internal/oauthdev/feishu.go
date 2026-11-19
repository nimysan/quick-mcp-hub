package oauthdev

// 飞书（国内版）的设备流和「一键创建应用」。
//
// 和 GitHub、云之家的根本区别：飞书的用户授权**必须带应用密钥**。出码要
// Basic(App ID:App Secret)，换令牌和刷新要 client_secret。所以它不能像那两家
// 一样把一个公共 client id 写进声明发给所有人 —— 每个客户企业要有自己的
// 应用，密钥由客户自己持有。
//
// 「一键创建应用」把这件事从「找 IT 去开放平台建应用、配权限、发版本，
// 再把 App ID 和 Secret 抄过来」变成扫一次码：飞书在用户所在的企业里建好
// 应用（预填好要申请的用户权限），把 App ID 和 Secret 直接返回给程序。它也是
// 一次设备授权（同样是 出码 → 用户确认 → 轮询），只是轮询的产物是应用凭据
// 而不是用户令牌。
//
// 线上行为以 2026-11 实测为准（飞书官方 Go SDK v3.13.0 的 scene/registration
// 和 lark-cli v1.0.100 是参照）：
//
//	建应用   POST accounts.feishu.cn/oauth/v1/app/registration   form, 无鉴权
//	出码     POST accounts.feishu.cn/oauth/v1/device_authorization form, Basic
//	换令牌   POST open.feishu.cn/open-apis/authen/v2/oauth/token  form, 带 secret
//	刷新     同上                                                  JSON, 带 secret
//
// 令牌：access_token 2 小时，refresh_token 7 天，**每次刷新都会轮换**
// refresh_token。两者各约 1.9KB，所以凭据存储必须能分段（见 credstore/chunk.go）。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// feishuOfflineScope 不申请它就拿不到 refresh_token，2 小时后就得重新扫码。
const feishuOfflineScope = "offline_access"

func (c Config) feishuReady() error {
	if strings.TrimSpace(c.ClientID) == "" || strings.TrimSpace(c.ClientSecret) == "" {
		return fmt.Errorf("飞书授权需要 App ID 和 App Secret")
	}
	return nil
}

func (c Config) startFeishu(ctx context.Context, hc *http.Client) (Pending, error) {
	if err := c.feishuReady(); err != nil {
		return Pending{}, err
	}
	scopes := append([]string(nil), c.Scopes...)
	hasOffline := false
	for _, s := range scopes {
		if s == feishuOfflineScope {
			hasOffline = true
		}
	}
	if !hasOffline {
		scopes = append(scopes, feishuOfflineScope)
	}
	form := url.Values{"client_id": {c.ClientID}, "scope": {strings.Join(scopes, " ")}}
	hdr := http.Header{}
	hdr.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.ClientID+":"+c.ClientSecret)))
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
	if err := postRaw(ctx, hc, c.DeviceCodeURL, "application/x-www-form-urlencoded", form.Encode(), hdr, &body); err != nil {
		return Pending{}, err
	}
	if body.Error != "" {
		return Pending{}, describeFeishu(body.Error, body.ErrorDescription)
	}
	if body.DeviceCode == "" || body.UserCode == "" {
		return Pending{}, fmt.Errorf("飞书没有返回设备码")
	}
	uri := body.VerificationURI
	if uri == "" {
		uri = body.VerificationURIComplete
	}
	if uri == "" {
		return Pending{}, fmt.Errorf("飞书没有返回授权地址")
	}
	return Pending{
		DeviceCode:              body.DeviceCode,
		UserCode:                body.UserCode,
		VerificationURI:         uri,
		VerificationURIComplete: body.VerificationURIComplete,
		Interval:                secondsOr(body.Interval, 5),
		ExpiresAt:               time.Now().Add(secondsOr(body.ExpiresIn, 240)),
	}, nil
}

func (c Config) pollFeishu(ctx context.Context, hc *http.Client, deviceCode string) (TokenSet, error) {
	if err := c.feishuReady(); err != nil {
		return TokenSet{}, err
	}
	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code":   {deviceCode},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	}
	var body tokenResponse
	if err := post(ctx, hc, c.TokenURL, form, &body); err != nil {
		return TokenSet{}, err
	}
	return c.feishuTokenSet(body)
}

func (c Config) refreshFeishu(ctx context.Context, hc *http.Client, refreshToken string) (TokenSet, error) {
	if err := c.feishuReady(); err != nil {
		return TokenSet{}, err
	}
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     c.ClientID,
		"client_secret": c.ClientSecret,
	})
	if err != nil {
		return TokenSet{}, err
	}
	var body tokenResponse
	if err := postRaw(ctx, hc, c.refreshEndpoint(), "application/json; charset=utf-8", string(payload), nil, &body); err != nil {
		return TokenSet{}, err
	}
	return c.feishuTokenSet(body)
}

// feishuTokenSet 转成令牌组并把应用密钥一起带上：之后的刷新要用它，
// 而它和这份授权放在同一个密码管理器条目里。
func (c Config) feishuTokenSet(body tokenResponse) (TokenSet, error) {
	if body.Error != "" {
		return TokenSet{}, describeFeishu(body.Error, body.ErrorDescription)
	}
	t, err := body.tokenSet()
	if err != nil {
		return TokenSet{}, err
	}
	t.ClientSecret = c.ClientSecret
	t.ClientID = c.ClientID
	return t, nil
}

// describeFeishu 在通用翻译之外，认出飞书最常见的那条：密钥不对。
func describeFeishu(code, desc string) error {
	switch code {
	case "invalid_client", "unauthorized_client":
		return fmt.Errorf("飞书拒绝了这个应用（%s）：App ID 或 App Secret 不对，或应用已停用", code)
	}
	return describe(code, desc)
}

func secondsOr(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

// ---------- 一键创建应用 ----------

// AppRegistration 是一键建应用的参数。
type AppRegistration struct {
	// URL 是建应用接口，accounts.feishu.cn/oauth/v1/app/registration。
	URL string
	// Name / Desc 预填在确认页上，{user} 会被换成扫码人的名字。用户仍可修改。
	Name string
	Desc string
	// UserScopes 是要预先勾上的用户身份权限。
	UserScopes []string
}

// AppCredentials 是一键建应用的产物。
type AppCredentials struct {
	ClientID     string
	ClientSecret string
	// OpenID 是扫码人的 open_id，只用来核对后面授权的是不是同一个人。
	OpenID string
}

// ErrLarkTenant 扫码的是 Lark 国际版租户。目前只支持飞书国内版：两边的接口
// 域名不同，国际版没有实测过。
var ErrLarkTenant = errors.New("扫码的是 Lark 国际版账号，目前只支持飞书国内版")

// Begin 走第一步：拿到让用户打开的建应用链接。
func (r AppRegistration) Begin(ctx context.Context, hc *http.Client) (Pending, error) {
	var body struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		ExpireIn                int    `json:"expire_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	form := url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	}
	if err := post(ctx, hc, r.URL, form, &body); err != nil {
		return Pending{}, err
	}
	if body.Error != "" {
		return Pending{}, describe(body.Error, body.ErrorDescription)
	}
	if body.DeviceCode == "" || body.VerificationURIComplete == "" {
		return Pending{}, fmt.Errorf("飞书没有返回建应用链接")
	}
	link, err := r.link(body.VerificationURIComplete)
	if err != nil {
		return Pending{}, err
	}
	expires := body.ExpiresIn
	if expires <= 0 {
		expires = body.ExpireIn // 协议字段是 expire_in，两种拼法都认
	}
	return Pending{
		DeviceCode:              body.DeviceCode,
		UserCode:                body.UserCode,
		VerificationURI:         body.VerificationURI,
		VerificationURIComplete: link,
		Interval:                secondsOr(body.Interval, 5),
		ExpiresAt:               time.Now().Add(secondsOr(expires, 600)),
	}, nil
}

// link 把预设的应用名、描述和权限拼进确认页链接。
//
// 链接的主机来自飞书的响应，所以在这里核一遍：用户会在那一页确认在自己
// 企业里建应用，不能被一个异常响应带去别处。
func (r AppRegistration) link(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !feishuHost(u.Hostname()) {
		return "", fmt.Errorf("飞书返回的建应用链接不可信: %q", raw)
	}
	q := u.Query()
	q.Set("from", "sdk")
	q.Set("tp", "sdk")
	q.Set("source", "go-sdk/mcp-hub")
	q.Set("createOnly", "true")
	if r.Name != "" {
		q.Set("name", r.Name)
	}
	if r.Desc != "" {
		q.Set("desc", r.Desc)
	}
	if len(r.UserScopes) > 0 {
		addons, err := encodeAddons(r.UserScopes)
		if err != nil {
			return "", err
		}
		q.Set("addons", addons)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func feishuHost(h string) bool {
	return h == "feishu.cn" || strings.HasSuffix(h, ".feishu.cn")
}

// encodeAddons 是确认页预填权限的编码：JSON → gzip → base64url（无填充），
// 和官方 SDK 一致。
func encodeAddons(userScopes []string) (string, error) {
	b, err := json.Marshal(map[string]any{"scopes": map[string]any{"user": userScopes}})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// Poll 查一次建应用的状态。
func (r AppRegistration) Poll(ctx context.Context, hc *http.Client, deviceCode string) (AppCredentials, error) {
	var body struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		UserInfo     struct {
			OpenID      string `json:"open_id"`
			TenantBrand string `json:"tenant_brand"`
		} `json:"user_info"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	form := url.Values{"action": {"poll"}, "device_code": {deviceCode}}
	if err := post(ctx, hc, r.URL, form, &body); err != nil {
		return AppCredentials{}, err
	}
	if body.UserInfo.TenantBrand == "lark" {
		return AppCredentials{}, ErrLarkTenant
	}
	if body.ClientID != "" && body.ClientSecret != "" {
		return AppCredentials{ClientID: body.ClientID, ClientSecret: body.ClientSecret, OpenID: body.UserInfo.OpenID}, nil
	}
	if body.Error == "" {
		return AppCredentials{}, ErrPending
	}
	return AppCredentials{}, describe(body.Error, body.ErrorDescription)
}

// Await 轮询到应用建好为止。
func (r AppRegistration) Await(ctx context.Context, hc *http.Client, p Pending) (AppCredentials, error) {
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return AppCredentials{}, ctx.Err()
		case <-time.After(interval):
		}
		if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
			return AppCredentials{}, ErrExpired
		}
		c, err := r.Poll(ctx, hc, p.DeviceCode)
		switch {
		case err == nil:
			return c, nil
		case errors.Is(err, ErrPending):
		case errors.Is(err, ErrSlowDown):
			interval += 5 * time.Second
		default:
			return AppCredentials{}, err
		}
	}
}

// IssuerClientID 返回签发这份令牌的应用 ID。
//
// 新授权的令牌组里直接记着（ClientID）。早一些的没有记，但飞书的访问令牌
// 是 JWT，载荷里带 client_id —— 这里只读不验签：它只用来选「拿哪个应用去
// 续期」，选错的结果是续期被飞书拒绝，不会让任何东西被冒用。
func (t TokenSet) IssuerClientID() string {
	if t.ClientID != "" {
		return t.ClientID
	}
	parts := strings.Split(t.AccessToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(raw, &claims) != nil || !strings.HasPrefix(claims.ClientID, "cli_") {
		return ""
	}
	return claims.ClientID
}
