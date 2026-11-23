package httpchan

import (
	"strings"
	"testing"
)

// implicitBase 是一份最小的隐式授权声明（百度网盘那种）。
func implicitBase() Spec {
	s := base()
	s.Auth = Auth{Kind: AuthQuery, Name: "access_token"}
	s.OAuth = &OAuthSpec{
		Flow: FlowImplicit, ClientID: "pub-app",
		AuthorizeURL: "https://auth.example.com/authorize", RedirectURI: "oob",
		Scopes: []string{"basic", "netdisk"},
	}
	return s
}

func TestImplicitSpecAccepted(t *testing.T) {
	if err := implicitBase().Validate(); err != nil {
		t.Fatalf("最小隐式授权声明应当合法: %v", err)
	}
	s := implicitBase()
	s.OAuth.RedirectURI = "https://auth.example.com/done"
	if err := s.Validate(); err != nil {
		t.Fatalf("https 回跳地址应当合法: %v", err)
	}
	// 设备流现在也能配 query 认证。
	d := base()
	d.Auth = Auth{Kind: AuthQuery, Name: "access_token"}
	d.OAuth = &OAuthSpec{Flow: FlowDevice, ClientID: "c",
		DeviceCodeURL: "https://auth.example.com/device", TokenURL: "https://auth.example.com/token"}
	if err := d.Validate(); err != nil {
		t.Fatalf("设备流 + query 认证应当合法: %v", err)
	}
}

func TestImplicitSpecRejects(t *testing.T) {
	cases := []struct {
		name string
		want string
		mut  func(*Spec)
	}{
		{"没有授权页", "authorizeUrl", func(s *Spec) { s.OAuth.AuthorizeURL = "" }},
		{"明文授权页", "https", func(s *Spec) { s.OAuth.AuthorizeURL = "http://auth.example.com/a" }},
		{"没有回跳地址", "redirectUri", func(s *Spec) { s.OAuth.RedirectURI = "" }},
		{"回跳到明文本机", "https", func(s *Spec) { s.OAuth.RedirectURI = "http://127.0.0.1:7801/cb" }},
		{"让用户填 client id", "clientId", func(s *Spec) {
			s.OAuth.ClientID, s.OAuth.ClientIDField = "", "appKey"
			s.Fields = []Field{{Name: "appKey", Label: "AppKey"}}
		}},
		{"混进设备流的键", "设备流的键", func(s *Spec) { s.OAuth.TokenURL = "https://auth.example.com/token" }},
		{"混进方言", "设备流的键", func(s *Spec) { s.OAuth.Dialect = "feishu" }},
		{"basic 认证接不住令牌", "bearer 或 query", func(s *Spec) { s.Auth = Auth{Kind: AuthBasic} }},
		{"scope 里带逗号", "scope", func(s *Spec) { s.OAuth.Scopes = []string{"basic,netdisk"} }},
		{"不认识的流", "不支持", func(s *Spec) { s.OAuth.Flow = "password" }},
		{"设备流带隐式的键", "flow=implicit", func(s *Spec) {
			s.OAuth = &OAuthSpec{Flow: FlowDevice, ClientID: "c", AuthorizeURL: "https://a.example.com/x",
				DeviceCodeURL: "https://auth.example.com/device", TokenURL: "https://auth.example.com/token"}
		}},
	}
	for _, c := range cases {
		s := implicitBase()
		c.mut(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 想要含 %q 的错误，得到 %v", c.name, c.want, err)
		}
	}
}

func TestFixedQueryInPath(t *testing.T) {
	ok := implicitBase()
	ok.Test.Path = "/rest/nas?method=uinfo"
	ok.Tools = append(ok.Tools, Tool{Name: "demo_list", Description: "列", ReadOnly: true, Method: "GET",
		Path: "/rest/file?method=list", Params: []Param{{Name: "dir", In: InQuery}}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("path 里写死查询串应当合法: %v", err)
	}

	cases := []struct {
		name, want string
		mut        func(*Spec)
	}{
		{"和参数同名", "同名", func(s *Spec) { s.Tools[1].Params = []Param{{Name: "method", In: InQuery}} }},
		{"写死凭据", "凭据参数", func(s *Spec) { s.Tools[1].Path = "/rest/file?method=list&access_token=x" }},
		{"test 写死凭据", "凭据参数", func(s *Spec) { s.Test.Path = "/rest/nas?access_token=x" }},
		{"查询串里有占位符", "占位符", func(s *Spec) { s.Tools[1].Path = "/rest/file?method={m}" }},
		{"空查询串", "空的", func(s *Spec) { s.Tools[1].Path = "/rest/file?" }},
		{"重复键", "不止一次", func(s *Spec) { s.Tools[1].Path = "/rest/file?a=1&a=2" }},
		{"带 #", "#", func(s *Spec) { s.Tools[1].Path = "/rest/file?a=1#x" }},
	}
	for _, c := range cases {
		s := implicitBase()
		s.Test.Path = ok.Test.Path
		s.Tools = append([]Tool(nil), ok.Tools...)
		s.Tools[1].Params = append([]Param(nil), ok.Tools[1].Params...)
		c.mut(&s)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 想要含 %q 的错误，得到 %v", c.name, c.want, err)
		}
	}
}

func TestFormBodyAndItemFieldsRules(t *testing.T) {
	writeTool := func() Tool {
		return Tool{Name: "demo_rename", Description: "改名", Method: "POST", Path: "/rest/file?method=rename",
			BodyEncoding: BodyForm, BodyConst: map[string]any{"async": "1"},
			Params: []Param{{Name: "filelist", In: InBody, Type: "array", Required: true,
				ItemFields: []string{"path", "newname"}, ItemRequired: []string{"path"}}}}
	}
	s := base()
	s.Tools = append(s.Tools, writeTool())
	if err := s.Validate(); err != nil {
		t.Fatalf("表单 + 对象数组参数应当合法: %v", err)
	}

	cases := []struct {
		name, want string
		mut        func(*Tool)
	}{
		{"不认识的编码", "bodyEncoding", func(t *Tool) { t.BodyEncoding = "xml" }},
		{"表单带嵌套路径", "不能带点", func(t *Tool) { t.Params[0].BodyPath = "a.b" }},
		{"表单带 bodyJsonString", "bodyJsonString", func(t *Tool) { t.BodyJSONString = []string{"filelist"} }},
		{"表单常量是对象", "bodyConst", func(t *Tool) { t.BodyConst = map[string]any{"x": map[string]any{}} }},
		{"itemFields 配字符串参数", "itemFields", func(t *Tool) { t.Params[0].Type = "string" }},
		{"itemRequired 不在 itemFields 里", "itemRequired", func(t *Tool) { t.Params[0].ItemRequired = []string{"dest"} }},
		{"itemFields 重复", "重复", func(t *Tool) { t.Params[0].ItemFields = []string{"path", "path"} }},
		{"arrayOnly 配字符串参数", "arrayOnly", func(t *Tool) {
			t.Params[0].ItemFields, t.Params[0].ItemRequired = nil, nil
			t.Params[0].Type, t.Params[0].ArrayOnly = "string", true
		}},
	}
	for _, c := range cases {
		s := base()
		tl := writeTool()
		c.mut(&tl)
		s.Tools = append(s.Tools, tl)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 想要含 %q 的错误，得到 %v", c.name, c.want, err)
		}
	}

	// GET 不能声明表单编码。
	g := base()
	g.Tools[0].BodyEncoding = BodyForm
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "bodyEncoding") {
		t.Errorf("GET 带 bodyEncoding 应当拒绝: %v", err)
	}
}
