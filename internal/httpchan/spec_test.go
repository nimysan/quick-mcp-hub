package httpchan

import (
	"strings"
	"testing"
)

// 内置目录必须条条合法：一条坏声明在运行时只会被跳过，很难被发现。
func TestBuiltinCatalogValid(t *testing.T) {
	if errs := Errors(); len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("内置目录加载失败: %v", err)
		}
	}
	if len(All()) == 0 {
		t.Fatal("内置目录是空的")
	}
	if _, ok := ByType("github"); !ok {
		t.Error("github 声明不在目录里")
	}
}

func TestGitHubSpecShape(t *testing.T) {
	s, ok := ByType("github")
	if !ok {
		t.Fatal("没有 github 声明")
	}
	if !s.HasWriteTools() {
		t.Error("github 声明里应当有写入类工具（create_issue）")
	}
	for _, name := range []string{"github_whoami", "github_list_issues", "github_create_issue"} {
		if _, ok := s.Tool(name); !ok {
			t.Errorf("缺少工具 %s", name)
		}
	}
	if tool, _ := s.Tool("github_create_issue"); tool.ReadOnly {
		t.Error("create_issue 不能标成只读")
	}
}

// base 是一份最小可用声明，各用例在它上面改一处来验证校验规则。
func base() Spec {
	return Spec{
		Type: "demo", Name: "Demo", Integration: "demo",
		BaseURL: "https://api.example.com",
		Account: Credential{Name: "user", Label: "用户"},
		Secret:  Credential{Name: "token", Label: "令牌"},
		Auth:    Auth{Kind: AuthBearer},
		Test:    Endpoint{Method: "GET", Path: "/me"},
		Tools: []Tool{{
			Name: "demo_get", Description: "取一条", ReadOnly: true,
			Method: "GET", Path: "/items/{id}",
			Params: []Param{{Name: "id", In: InPath, Required: true}},
		}},
	}
}

func TestValidateAcceptsBase(t *testing.T) {
	if err := base().Validate(); err != nil {
		t.Fatalf("最小声明应当合法: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		want string
		mut  func(*Spec)
	}{
		{"明文 http", "https", func(s *Spec) { s.BaseURL = "http://api.example.com" }},
		{"主机名含模板", "占位符", func(s *Spec) { s.BaseURL = "https://{site}.example.com" }},
		{"既无 baseUrl 也无字段", "必须声明", func(s *Spec) { s.BaseURL = "" }},
		{"baseUrlField 指向不存在的字段", "不存在", func(s *Spec) {
			s.BaseURL, s.BaseURLField = "", "site"
		}},
		{"两个来源都给", "只能有一个", func(s *Spec) {
			s.BaseURLField = "site"
			s.Fields = []Field{{Name: "site", Label: "站点"}}
		}},
		{"工具名没有渠道前缀", "必须以", func(s *Spec) { s.Tools[0].Name = "get_item" }},
		{"工具名重复", "重复", func(s *Spec) { s.Tools = append(s.Tools, s.Tools[0]) }},
		{"描述为空", "description", func(s *Spec) { s.Tools[0].Description = " " }},
		{"POST 标只读", "readOnly", func(s *Spec) { s.Tools[0].Method = "POST" }},
		{"路径占位符没有参数", "占位符", func(s *Spec) { s.Tools[0].Params = nil }},
		{"路径参数没有占位符", "没有 {", func(s *Spec) {
			s.Tools[0].Path = "/items"
		}},
		{"绝对地址当路径", "必须以 /", func(s *Spec) {
			s.Tools[0].Path = "https://evil.example.com/x"
			s.Tools[0].Params = nil
		}},
		{"test 不是 GET", "必须是 GET", func(s *Spec) { s.Test.Method = "POST" }},
		{"auth 缺头名", "auth.name", func(s *Spec) { s.Auth = Auth{Kind: AuthHeader} }},
		{"auth 类型不认识", "不支持", func(s *Spec) { s.Auth = Auth{Kind: "oauth-magic"} }},
		{"字段名撞写开关", "保留字段", func(s *Spec) {
			s.Fields = []Field{{Name: AllowWriteField, Label: "x"}}
		}},
		{"没有工具", "至少要声明", func(s *Spec) { s.Tools = nil }},
		{"GET 带 body 参数", "不能带 body", func(s *Spec) {
			s.Tools[0].Params = append(s.Tools[0].Params, Param{Name: "x", In: InBody})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base()
			c.mut(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("应当被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息里应提到 %q，实际: %v", c.want, err)
			}
		})
	}
}

// 未知字段必须报错：下发内容用了更新的 schema 时，静默忽略会让渠道
// 以「少了一半行为」的样子跑起来。
func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`{"type":"demo","name":"D","launch":{"command":"sh"}}`))
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("应当因未知字段 launch 报错，实际: %v", err)
	}
}
