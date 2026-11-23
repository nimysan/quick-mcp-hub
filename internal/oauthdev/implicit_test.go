package oauthdev

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestImplicitURL(t *testing.T) {
	c := Implicit{
		AuthorizeURL: "https://openapi.baidu.com/oauth/2.0/authorize",
		ClientID:     "pub-app",
		RedirectURI:  "oob",
		Scopes:       []string{"basic", "netdisk"},
	}
	raw, err := c.URL("st1")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if u.Host != "openapi.baidu.com" || q.Get("response_type") != "token" || q.Get("client_id") != "pub-app" ||
		q.Get("redirect_uri") != "oob" || q.Get("state") != "st1" {
		t.Fatalf("授权地址不对: %s", raw)
	}
	// 百度的 scope 用逗号分隔。
	if q.Get("scope") != "basic,netdisk" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	if _, err := (Implicit{AuthorizeURL: "http://x.test/a", ClientID: "c"}).URL(""); err == nil {
		t.Fatal("明文授权地址必须拒绝")
	}
}

func TestParsePasted(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	full := "https://openapi.baidu.com/oauth/2.0/login_success#expires_in=2592000&access_token=123.abc&session_secret=&session_key=&scope=basic+netdisk&state=st1"
	tok, err := ParsePasted(full, "st1", now)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "123.abc" || tok.Marker != TokenMarker || !tok.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("令牌组不对: %+v", tok)
	}
	if tok.RefreshToken != "" || tok.CanRefresh(now) {
		t.Fatal("隐式授权没有刷新令牌")
	}

	// 只粘了 # 后面那段。
	if tok, err := ParsePasted("#expires_in=60&access_token=frag", "", now); err != nil || tok.AccessToken != "frag" {
		t.Fatalf("片段: %+v %v", tok, err)
	}
	// 光一个令牌：不过期信息未知，照样收下。
	if tok, err := ParsePasted("  121.bare-token  ", "st1", now); err != nil || tok.AccessToken != "121.bare-token" || !tok.ExpiresAt.IsZero() {
		t.Fatalf("光令牌: %+v %v", tok, err)
	}
	// state 对不上：误粘了别的会话的地址。
	if _, err := ParsePasted(full, "other", now); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("state 不一致必须拒绝: %v", err)
	}
	// 用户在授权页点了拒绝。
	if _, err := ParsePasted("https://openapi.baidu.com/oauth/2.0/login_success#error=access_denied", "", now); !errors.Is(err, ErrDenied) {
		t.Fatalf("拒绝授权: %v", err)
	}
	// 没授权就复制了授权页本身的地址。
	if _, err := ParsePasted("https://openapi.baidu.com/oauth/2.0/authorize?response_type=token&client_id=x", "", now); err == nil {
		t.Fatal("没有 access_token 的地址必须拒绝")
	}
	for _, bad := range []string{"", "   ", "随便 一段话"} {
		if _, err := ParsePasted(bad, "", now); err == nil {
			t.Fatalf("%q 应当拒绝", bad)
		}
	}
	// 片段优先于查询串。
	if tok, _ := ParsePasted("https://x.test/cb?access_token=q#access_token=f", "", now); tok.AccessToken != "f" {
		t.Fatalf("片段应当优先: %q", tok.AccessToken)
	}
}

func TestTokenFromPasted(t *testing.T) {
	cases := map[string]string{
		"plain-token": "plain-token",
		"https://openapi.baidu.com/oauth/2.0/login_success#expires_in=1&access_token=123.x&state=s": "123.x",
		"expires_in=1&access_token=abc": "abc",
		"  spaced  ":                    "spaced",
	}
	for in, want := range cases {
		if got := TokenFromPasted(in); got != want {
			t.Errorf("TokenFromPasted(%q) = %q, 想要 %q", in, got, want)
		}
	}
}
