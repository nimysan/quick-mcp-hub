package httpchan

import (
	"strings"
	"testing"
)

// feishuBase 是一份最小的飞书方言声明。
func feishuBase() Spec {
	s := base()
	s.Fields = []Field{{Name: "appId", Label: "App ID"}}
	s.OAuth = &OAuthSpec{
		Flow: "device", Dialect: "feishu", ClientIDField: "appId",
		ClientSecret:  &Credential{Name: "appSecret", Label: "App Secret"},
		DeviceCodeURL: "https://accounts.feishu.cn/oauth/v1/device_authorization",
		TokenURL:      "https://open.feishu.cn/open-apis/authen/v2/oauth/token",
		RegisterURL:   "https://accounts.feishu.cn/oauth/v1/app/registration",
	}
	return s
}

func TestFeishuDialectValidation(t *testing.T) {
	if err := feishuBase().Validate(); err != nil {
		t.Fatalf("最小飞书声明应当合法: %v", err)
	}
	cases := []struct {
		name, want string
		mut        func(*Spec)
	}{
		{"写死 clientId", "clientIdField", func(s *Spec) { s.OAuth.ClientIDField, s.OAuth.ClientID = "", "cli_x" }},
		{"缺 clientSecret", "clientSecret", func(s *Spec) { s.OAuth.ClientSecret = nil }},
		{"clientSecret 和字段同名", "同名", func(s *Spec) { s.OAuth.ClientSecret.Name = "appId" }},
		{"clientSecret 和令牌同名", "同名", func(s *Spec) { s.OAuth.ClientSecret.Name = "token" }},
		{"registerUrl 明文", "registerUrl", func(s *Spec) { s.OAuth.RegisterURL = "http://accounts.feishu.cn/x" }},
		{"填了 verificationUri", "verificationUri", func(s *Spec) { s.OAuth.VerificationURI = "https://x.feishu.cn/v" }},
	}
	for _, c := range cases {
		s := feishuBase()
		c.mut(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 期望含 %q 的错误，实际 %v", c.name, c.want, err)
		}
	}
}

// 非飞书方言不能带应用密钥或一键建应用：那两项只在机密客户端上有意义。
func TestSecretAndRegisterOnlyForFeishu(t *testing.T) {
	s := feishuBase()
	s.OAuth.Dialect = ""
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "feishu") {
		t.Fatalf("标准方言带 clientSecret 应被拒，实际 %v", err)
	}
	s = feishuBase()
	s.OAuth.Dialect, s.OAuth.ClientSecret = "", nil
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "registerUrl") {
		t.Fatalf("标准方言带 registerUrl 应被拒，实际 %v", err)
	}
}

func TestBodyJSONStringValidation(t *testing.T) {
	post := func() Spec {
		s := base()
		s.Tools = append(s.Tools, Tool{
			Name: "demo_send", Description: "发", Method: "POST", Path: "/send",
			Params:         []Param{{Name: "text", In: InBody, BodyPath: "content.text", Required: true}},
			BodyJSONString: []string{"content"},
		})
		return s
	}
	if err := post().Validate(); err != nil {
		t.Fatalf("应当合法: %v", err)
	}
	s := post()
	s.Tools[1].BodyJSONString = []string{"payload"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "没有任何") {
		t.Fatalf("路径下没有参数应被拒，实际 %v", err)
	}
	s = base()
	s.Tools[0].BodyJSONString = []string{"content"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "GET") {
		t.Fatalf("GET 带 bodyJsonString 应被拒，实际 %v", err)
	}
}

func TestFailWhenNotEqualsExclusive(t *testing.T) {
	s := base()
	s.FailWhen = &FailWhen{Path: "code", NotEquals: 0.0, Exists: true}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "notEquals") {
		t.Fatalf("notEquals 和 exists 同时用应被拒，实际 %v", err)
	}
}

func TestParamFormatAndArrayValidation(t *testing.T) {
	with := func(p Param) error {
		s := base()
		s.Tools = append(s.Tools, Tool{Name: "demo_post", Description: "x", Method: "POST", Path: "/p", Params: []Param{p}})
		return s.Validate()
	}
	if err := with(Param{Name: "ids", In: InBody, Type: "array", ItemObject: map[string]any{"type": "user", "user_id": "{{item}}"}}); err != nil {
		t.Fatalf("合法的 array 参数被拒: %v", err)
	}
	if err := with(Param{Name: "t", In: InQuery, Format: "unix"}); err != nil {
		t.Fatalf("合法的 format 参数被拒: %v", err)
	}
	bad := []Param{
		{Name: "ids", In: InQuery, Type: "array"},
		{Name: "ids", In: InBody, Type: "string", ItemObject: map[string]any{"a": "{{item}}"}},
		{Name: "ids", In: InBody, Type: "array", ItemObject: map[string]any{"a": "x"}},
		{Name: "t", In: InQuery, Format: "iso"},
		{Name: "t", In: InBody, Type: "number", Format: "unix"},
	}
	for _, p := range bad {
		if err := with(p); err == nil {
			t.Errorf("应被拒: %+v", p)
		}
	}
}
