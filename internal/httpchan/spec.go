// Package httpchan 是声明式 HTTPS 渠道：一个系统怎么接，全部写成数据（Spec），
// 由一个通用执行器去跑，不需要为每个系统写 Go 代码。
//
// 这是「远端下发 connector」的落点：客户机上只有本程序这一个二进制，
// 下发的内容是 Spec（数据），绝不是代码或需要 Python/Node 的子进程。
//
// 安全底线落在 Validate 里：
//
//   - baseUrl 只能是 https，且要么写死在 spec 里，要么来自用户自己填的字段
//     （比如自建 Jira 的站点地址）——绝不允许用 spec 里的模板拼出主机名，
//     否则一份被篡改的目录就能把用户凭据送去任意地址。
//   - 路径只能用 {name} 占位，且每个占位都必须有对应的已声明参数。
//   - 工具名必须以渠道类型为前缀，避免多个渠道的工具互相覆盖。
//   - 非只读工具（会改外部系统的）要渠道上显式打开写开关才注册给模型。
package httpchan

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// AuthKind 决定凭据往哪里放。
type AuthKind string

const (
	// AuthBearer: Authorization: Bearer <secret>
	AuthBearer AuthKind = "bearer"
	// AuthHeader: <auth.name>: <secret>，用于 X-API-Key 这类自定义头
	AuthHeader AuthKind = "header"
	// AuthBasic: HTTP Basic，用户名取账号字段，密码取 secret（Jira Cloud 就是这样）
	AuthBasic AuthKind = "basic"
	// AuthQuery: ?<auth.name>=<secret>，少数老接口用
	AuthQuery AuthKind = "query"
)

// ParamIn 决定参数放在请求的哪个位置。
type ParamIn string

const (
	InQuery ParamIn = "query"
	InPath  ParamIn = "path"
	InBody  ParamIn = "body"
)

// Field 是渠道表单上的一个附加字段（账号和密码之外的，如站点地址）。
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
}

// Credential 描述账号字段和密码字段的叫法。
type Credential struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

// Auth 描述凭据怎么带上。
type Auth struct {
	Kind AuthKind `json:"kind"`
	// Name 是 header 或 query 方式下的键名，bearer/basic 不用填。
	Name string `json:"name,omitempty"`
}

// Endpoint 是一次 HTTP 调用的形状。
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Result 裁剪响应。测试连通只需要身份那几个字段，不需要整个响应。
	Result Result `json:"result,omitempty"`
}

// Result 描述怎么把响应裁成给模型看的结果。
//
// 不裁的话一次 GitHub issue 列表就有上万 token 的噪音，模型既慢又容易看错。
type Result struct {
	// Path 取响应里的子树，点号分隔，如 "items"。留空表示整个响应。
	Path string `json:"path,omitempty"`
	// Fields 是每条记录保留的键（点号路径，如 "user.login"）。留空表示全留。
	Fields []string `json:"fields,omitempty"`
	// Limit 是数组结果的最大条数，0 表示不限。
	Limit int `json:"limit,omitempty"`
}

// Param 是一个工具参数。
type Param struct {
	Name        string   `json:"name"`
	In          ParamIn  `json:"in"`
	Type        string   `json:"type,omitempty"` // string | number | boolean，默认 string
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// Tool 是一个声明出来的 MCP 工具。
type Tool struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	// ReadOnly 为真表示只查不改。只读工具客户端可以免确认直接调；
	// 非只读的必须渠道上打开写开关才注册。
	ReadOnly bool    `json:"readOnly,omitempty"`
	Method   string  `json:"method"`
	Path     string  `json:"path"`
	Params   []Param `json:"params,omitempty"`
	Result   Result  `json:"result,omitempty"`
}

// Spec 是一个渠道的完整声明。
type Spec struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Icon        string `json:"icon,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Integration string `json:"integration"`
	HelpURL     string `json:"helpUrl,omitempty"`

	// BaseURL 写死在 spec 里的接入点（SaaS 用这个）。
	BaseURL string `json:"baseUrl,omitempty"`
	// BaseURLField 指向 Fields 里的一个字段，由用户自己填站点地址（自建系统用这个）。
	// 两者必须且只能有一个。
	BaseURLField string `json:"baseUrlField,omitempty"`

	Account Credential        `json:"account"`
	Secret  Credential        `json:"secret"`
	Auth    Auth              `json:"auth"`
	Headers map[string]string `json:"headers,omitempty"`
	Fields  []Field           `json:"fields,omitempty"`

	// Test 是「测试连通」用的只读接口，保存渠道前会先打一次。
	Test  Endpoint `json:"test"`
	Tools []Tool   `json:"tools"`
}

// AllowWriteField 是写开关的字段名。只要 spec 里有非只读工具，
// 描述符就会自动长出这个开关，默认关。
const AllowWriteField = "allowWrite"

// ReservedAccountParam 是多实例时由工具层自动加上的参数名，
// 渠道声明里不能用它做业务参数，否则账号分派会被业务值顶掉。
const ReservedAccountParam = "channel_account"

// HasWriteTools 报告这个 spec 是否含会改动外部系统的工具。
func (s Spec) HasWriteTools() bool {
	for _, t := range s.Tools {
		if !t.ReadOnly {
			return true
		}
	}
	return false
}

// Tool 按名字取工具。
func (s Spec) Tool(name string) (Tool, bool) {
	for _, t := range s.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

var (
	typeRe        = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)
	toolNameRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)
	paramNameRe   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,40}$`)
	placeholderRe = regexp.MustCompile(`\{([^{}]*)\}`)
	methodsAll    = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
)

// Validate 校验一份 spec。远端下发的目录必须逐条过这里才会被接受。
func (s Spec) Validate() error {
	if !typeRe.MatchString(s.Type) {
		return fmt.Errorf("type %q 不合法（小写字母、数字、下划线，2-31 字符）", s.Type)
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%s: name 不能为空", s.Type)
	}
	if !typeRe.MatchString(s.Integration) {
		return fmt.Errorf("%s: integration %q 不合法", s.Type, s.Integration)
	}

	switch {
	case s.BaseURL != "" && s.BaseURLField != "":
		return fmt.Errorf("%s: baseUrl 和 baseUrlField 只能有一个", s.Type)
	case s.BaseURL != "":
		if err := checkBaseURL(s.BaseURL); err != nil {
			return fmt.Errorf("%s: baseUrl %w", s.Type, err)
		}
	case s.BaseURLField != "":
		if _, ok := s.Field(s.BaseURLField); !ok {
			return fmt.Errorf("%s: baseUrlField %q 在 fields 里不存在", s.Type, s.BaseURLField)
		}
	default:
		return fmt.Errorf("%s: 必须声明 baseUrl 或 baseUrlField", s.Type)
	}

	if !paramNameRe.MatchString(s.Account.Name) || !paramNameRe.MatchString(s.Secret.Name) {
		return fmt.Errorf("%s: account.name 和 secret.name 必须是合法字段名", s.Type)
	}
	if s.Account.Name == s.Secret.Name {
		return fmt.Errorf("%s: account 和 secret 不能是同一个字段", s.Type)
	}
	switch s.Auth.Kind {
	case AuthBearer, AuthBasic:
	case AuthHeader, AuthQuery:
		if strings.TrimSpace(s.Auth.Name) == "" {
			return fmt.Errorf("%s: auth.kind=%s 必须给出 auth.name", s.Type, s.Auth.Kind)
		}
	default:
		return fmt.Errorf("%s: 不支持的 auth.kind %q", s.Type, s.Auth.Kind)
	}
	for _, f := range s.Fields {
		if !paramNameRe.MatchString(f.Name) {
			return fmt.Errorf("%s: 字段名 %q 不合法", s.Type, f.Name)
		}
		if f.Name == s.Account.Name || f.Name == s.Secret.Name || f.Name == AllowWriteField {
			return fmt.Errorf("%s: 字段名 %q 与保留字段冲突", s.Type, f.Name)
		}
	}

	if err := checkEndpoint(s.Test.Method, s.Test.Path, nil); err != nil {
		return fmt.Errorf("%s: test %w", s.Type, err)
	}
	if s.Test.Method != "GET" {
		return fmt.Errorf("%s: test 必须是 GET——测试连通不能改动用户的系统", s.Type)
	}
	if len(s.Tools) == 0 {
		return fmt.Errorf("%s: 至少要声明一个工具", s.Type)
	}

	seen := map[string]bool{}
	for _, t := range s.Tools {
		if !toolNameRe.MatchString(t.Name) {
			return fmt.Errorf("%s: 工具名 %q 不合法", s.Type, t.Name)
		}
		// 前缀约束：两个渠道各自声明 list_issues 时，后注册的会覆盖前一个。
		if !strings.HasPrefix(t.Name, s.Type+"_") {
			return fmt.Errorf("%s: 工具名 %q 必须以 %s_ 开头", s.Type, t.Name, s.Type)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: 工具名 %q 重复", s.Type, t.Name)
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("%s/%s: description 不能为空——模型靠它选工具", s.Type, t.Name)
		}
		if t.ReadOnly && t.Method != "GET" {
			return fmt.Errorf("%s/%s: 只有 GET 能标 readOnly（现在是 %s）", s.Type, t.Name, t.Method)
		}
		if err := checkEndpoint(t.Method, t.Path, t.Params); err != nil {
			return fmt.Errorf("%s/%s: %w", s.Type, t.Name, err)
		}
		pseen := map[string]bool{}
		for _, p := range t.Params {
			if !paramNameRe.MatchString(p.Name) {
				return fmt.Errorf("%s/%s: 参数名 %q 不合法", s.Type, t.Name, p.Name)
			}
			if p.Name == ReservedAccountParam {
				return fmt.Errorf("%s/%s: 参数名 %s 是保留名（多账号分派用）", s.Type, t.Name, p.Name)
			}
			if pseen[p.Name] {
				return fmt.Errorf("%s/%s: 参数 %q 重复", s.Type, t.Name, p.Name)
			}
			pseen[p.Name] = true
			switch p.In {
			case InQuery, InPath, InBody:
			default:
				return fmt.Errorf("%s/%s: 参数 %s 的 in=%q 不支持", s.Type, t.Name, p.Name, p.In)
			}
			switch p.Type {
			case "", "string", "number", "boolean":
			default:
				return fmt.Errorf("%s/%s: 参数 %s 的 type=%q 不支持", s.Type, t.Name, p.Name, p.Type)
			}
			if p.In == InBody && t.Method == "GET" {
				return fmt.Errorf("%s/%s: GET 不能带 body 参数 %s", s.Type, t.Name, p.Name)
			}
		}
	}
	return nil
}

// Field 按名字取附加字段。
func (s Spec) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func checkBaseURL(raw string) error {
	// 先查占位符：主机名里带 { 会让 url.Parse 以「非法字符」失败，
	// 那个报错看不出真正的原因是「不允许用模板拼主机名」。
	if strings.ContainsAny(raw, "{}") {
		return fmt.Errorf("%q 不允许含占位符——主机名不能由模板拼出", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q 解析失败: %w", raw, err)
	}
	// 只允许 https：凭据在请求头里，明文 http 等于把令牌交给网络。
	if u.Scheme != "https" {
		return fmt.Errorf("%q 必须是 https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q 缺少主机名", raw)
	}
	return nil
}

// checkEndpoint 校验方法和路径，并确保路径里每个 {name} 都有对应的 path 参数。
func checkEndpoint(method, path string, params []Param) error {
	if !methodsAll[method] {
		return fmt.Errorf("method %q 不支持", method)
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("path %q 必须以 / 开头", path)
	}
	if strings.Contains(path, "://") || strings.Contains(path, "//") {
		return fmt.Errorf("path %q 不能是绝对地址", path)
	}
	declared := map[string]bool{}
	for _, p := range params {
		if p.In == InPath {
			declared[p.Name] = true
		}
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(path, -1) {
		if !declared[m[1]] {
			return fmt.Errorf("path 占位符 {%s} 没有对应的 in=path 参数", m[1])
		}
	}
	for name := range declared {
		if !strings.Contains(path, "{"+name+"}") {
			return fmt.Errorf("参数 %s 声明为 in=path，但 path 里没有 {%s}", name, name)
		}
	}
	return nil
}
