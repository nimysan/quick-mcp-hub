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
	if _, ok := ByID("github"); !ok {
		t.Error("github 声明不在目录里")
	}
}

func TestGitHubSpecShape(t *testing.T) {
	s, ok := ByID("github")
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
		ID: "demo", Name: "Demo", Version: 1, Integration: "demo",
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
		{"占用多账号保留参数名", "保留名", func(s *Spec) {
			s.Tools[0].Params = append(s.Tools[0].Params, Param{Name: ReservedAccountParam, In: InQuery})
		}},
		{"login 缺 login 段", "缺失", func(s *Spec) { s.Auth = Auth{Kind: AuthLogin}; s.Test = Endpoint{} }},
		{"login 体里没有 secret", "必须出现一次", func(s *Spec) {
			s.Auth = Auth{Kind: AuthLogin, Login: &Login{Path: "/login", Body: map[string]any{"u": "{{account}}"}}}
			s.Test = Endpoint{}
		}},
		{"login 体引用不存在的字段", "不存在的字段", func(s *Spec) {
			s.Auth = Auth{Kind: AuthLogin, Login: &Login{Path: "/login",
				Body: map[string]any{"p": "{{secret}}", "a": "{{field:nope}}"}}}
			s.Test = Endpoint{}
		}},
		{"login 体里有不认识的标记", "不认识", func(s *Spec) {
			s.Auth = Auth{Kind: AuthLogin, Login: &Login{Path: "/login",
				Body: map[string]any{"p": "{{secret}}", "x": "{{env:PATH}}"}}}
			s.Test = Endpoint{}
		}},
		{"login 渠道还声明了 test", "不要声明 test", func(s *Spec) {
			s.Auth = Auth{Kind: AuthLogin, Login: &Login{Path: "/login",
				Body: map[string]any{"p": "{{secret}}"}}}
		}},
		{"非 login 渠道带 login 段", "只有 auth.kind=login", func(s *Spec) {
			s.Auth.Login = &Login{Path: "/login", Body: map[string]any{"p": "{{secret}}"}}
		}},
		{"POST 只读但没声明 postReads", "postReads", func(s *Spec) {
			s.Tools[0].Method = "POST"
			s.Tools[0].Params = []Param{{Name: "id", In: InBody}}
			s.Tools[0].Path = "/items"
		}},
		{"声明了 postReads 却没用到", "去掉它", func(s *Spec) { s.PostReads = true }},
		{"bodyPath 用在非 body 参数上", "只能用在 in=body", func(s *Spec) {
			s.Tools[0].Params[0].BodyPath = "data.id"
		}},
		{"bodyPath 形状不对", "形状不对", func(s *Spec) {
			s.Tools[0].Method = "POST"
			s.Tools[0].Path = "/items"
			s.Tools[0].ReadOnly = false
			s.Tools[0].Params = []Param{{Name: "id", In: InBody, BodyPath: "data..id"}}
		}},
		{"object 参数用在 query 上", "只能用在 in=body", func(s *Spec) {
			s.Tools[0].Params = []Param{{Name: "id", In: InQuery, Type: "object"}}
			s.Tools[0].Path = "/items"
		}},
		{"columns 和 columnsFromParam 同时给", "只能有一个", func(s *Spec) {
			s.Tools[0].Result = Result{Columns: []string{"a"}, ColumnsFromParam: "id"}
		}},
		{"columnsFromParam 指向不存在的参数", "不是这个工具的参数", func(s *Spec) {
			s.Tools[0].Result = Result{ColumnsFromParam: "nope"}
		}},
		{"version 缺失", "version 必须", func(s *Spec) { s.Version = 0 }},
		{"version 为负", "version 必须", func(s *Spec) { s.Version = -1 }},
		{"标签大写", "不合法", func(s *Spec) { s.Tags = []string{"ERP"} }},
		{"标签带空格", "不合法", func(s *Spec) { s.Tags = []string{"my erp"} }},
		{"标签重复", "重复", func(s *Spec) { s.Tags = []string{"erp", "erp"} }},
		{"标签太多", "最多 6 个", func(s *Spec) {
			s.Tags = []string{"a", "b", "c", "d", "e", "f", "g"}
		}},
		{"failWhen 缺 path", "failWhen 必须", func(s *Spec) { s.FailWhen = &FailWhen{} }},
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
	_, err := Parse([]byte(`{"id":"demo","name":"D","version":1,"launch":{"command":"sh"}}`))
	if err == nil || !strings.Contains(err.Error(), "launch") {
		t.Fatalf("应当因未知字段 launch 报错，实际: %v", err)
	}
}

// 标签是开放集合：下发新分类时，老客户端不该因为不认识一个标签就整个
// 渠道加载失败。
func TestUnknownTagIsAccepted(t *testing.T) {
	s := base()
	s.Tags = []string{"erp", "some-new-category"}
	if err := s.Validate(); err != nil {
		t.Fatalf("没见过的标签不该让声明失效: %v", err)
	}
}

// 每个渠道都得有 id、显示名、版本号和标签 —— 这是目录里认人的四样东西。
func TestBuiltinCatalogMetadata(t *testing.T) {
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, s := range All() {
		if ids[s.ID] {
			t.Errorf("id %q 重复", s.ID)
		}
		if names[s.Name] {
			t.Errorf("显示名 %q 重复", s.Name)
		}
		ids[s.ID], names[s.Name] = true, true
		if s.Version < 1 {
			t.Errorf("%s 没有版本号", s.ID)
		}
		if len(s.Tags) == 0 {
			t.Errorf("%s 没有标签，页面上筛不出来", s.ID)
		}
		if s.Summary == "" {
			t.Errorf("%s 没有一行说明", s.ID)
		}
	}
}
