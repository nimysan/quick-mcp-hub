package oauthdev

// 隐式授权（OAuth 2.0 implicit grant）+「把授权后的地址粘回来」。
//
// 为什么要它：百度网盘的设备流换令牌必须带应用密钥（client_secret），
// 没有一个能写进声明发给所有人的公共应用。唯一不用用户自己注册开发者应用
// 的路，是百度在官方 MCP 文档里给个人用户的那条：用官方 MCP 的公开应用
// 走 response_type=token、redirect_uri=oob，授权后浏览器停在一个地址上，
// 访问令牌就在地址的 # 后面。
//
// 浏览器跳到哪里本程序看不见（配置窗口是系统浏览器，不是内嵌 webview），
// 所以最后一步是用户把那个地址复制回来。这一步不能省，也不该假装能省。
//
// 代价写清楚：隐式授权没有刷新令牌，访问令牌一般 30 天过期，到期后要
// 重新点一次连接。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Implicit 是一次隐式授权的参数。
type Implicit struct {
	AuthorizeURL string
	ClientID     string
	RedirectURI  string
	Scopes       []string
}

// NewState 生成一次授权的 state。粘回来的地址里带着它时要对得上：
// 这样用户误粘了别的会话（或别人发来的）地址，会被当场认出来。
func NewState() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// URL 拼出要让用户打开的授权页地址。
func (c Implicit) URL(state string) (string, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return "", fmt.Errorf("缺少 client id")
	}
	u, err := url.Parse(c.AuthorizeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("授权地址不合法: %q", c.AuthorizeURL)
	}
	q := u.Query()
	q.Set("response_type", "token")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI)
	if len(c.Scopes) > 0 {
		// 百度的 scope 用逗号分隔（basic,netdisk），不是 RFC 6749 的空格。
		q.Set("scope", strings.Join(c.Scopes, ","))
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ParsePasted 从用户粘回来的内容里取出令牌组。
//
// 接受三种形状，因为用户会粘什么不由我们决定：
//
//	整个地址   https://openapi.baidu.com/oauth/2.0/login_success#expires_in=2592000&access_token=…
//	# 后那段   expires_in=2592000&access_token=…
//	光一个令牌 123.abc…
//
// wantState 非空且粘来的内容带 state 时，两者必须一致。不带 state 的
// （光令牌、或手工截掉了）照样接受：state 防的是误粘别的会话，而光令牌
// 本来就无从核对，拒掉它只会把手工复制令牌的人挡在外面。
func ParsePasted(pasted, wantState string, now time.Time) (TokenSet, error) {
	s := strings.TrimSpace(pasted)
	if s == "" {
		return TokenSet{}, fmt.Errorf("没有粘贴任何内容")
	}
	vals, isParams := pastedParams(s)
	if !isParams {
		if strings.ContainsAny(s, " \t\r\n?#&=") {
			return TokenSet{}, fmt.Errorf("没认出粘贴的内容：请复制授权后浏览器地址栏里的整个地址")
		}
		return TokenSet{Marker: TokenMarker, AccessToken: s, ObtainedAt: now}, nil
	}
	if e := vals.Get("error"); e != "" {
		return TokenSet{}, describe(e, vals.Get("error_description"))
	}
	if got := vals.Get("state"); got != "" && wantState != "" && got != wantState {
		return TokenSet{}, fmt.Errorf("粘贴的地址不是这次授权的（state 对不上）：请重新点连接，用新打开的那一页")
	}
	tok := vals.Get("access_token")
	if tok == "" {
		return TokenSet{}, fmt.Errorf("粘贴的地址里没有 access_token：确认已经在授权页点了「授权」，再复制跳转后的地址")
	}
	t := TokenSet{
		Marker:       TokenMarker,
		AccessToken:  tok,
		RefreshToken: vals.Get("refresh_token"),
		TokenType:    vals.Get("token_type"),
		Scope:        vals.Get("scope"),
		ObtainedAt:   now,
	}
	if n, err := strconv.Atoi(vals.Get("expires_in")); err == nil && n > 0 {
		t.ExpiresAt = now.Add(time.Duration(n) * time.Second)
	}
	return t, nil
}

// pastedParams 判断粘来的是不是「带参数的东西」，是的话取出参数。
// 片段（#）里的优先：隐式授权的令牌按规范放在片段里，查询串里的同名参数
// 不该盖过它。
func pastedParams(s string) (url.Values, bool) {
	if !strings.Contains(s, "=") {
		return nil, false
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return nil, false
		}
		out := u.Query()
		if frag, err := url.ParseQuery(u.Fragment); err == nil {
			for k, v := range frag {
				out[k] = v
			}
		}
		return out, true
	}
	v, err := url.ParseQuery(strings.TrimLeft(s, "#?"))
	if err != nil {
		return nil, false
	}
	return v, true
}

// TokenFromPasted 给「手动填令牌」那条后路用：用户把授权后的整个地址粘进了
// 令牌框，取出里面的 access_token；不像地址就原样返回。
func TokenFromPasted(secret string) string {
	s := strings.TrimSpace(secret)
	if !strings.Contains(s, "access_token=") {
		return s
	}
	if vals, ok := pastedParams(s); ok {
		if tok := vals.Get("access_token"); tok != "" {
			return tok
		}
	}
	return s
}
