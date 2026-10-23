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
	// AuthLogin: 先 POST 一个登录接口换会话 cookie，后续请求带着 cookie。
	// 金蝶云星空（ValidateUser -> kdservice-sessionid）这类老派企业系统用这个。
	AuthLogin AuthKind = "login"
)

// Login 描述「先登录换会话」的流程。
//
// Body 的值里只允许三个取值标记，别的都当字面量：
//
//	{{account}}        账号字段的值
//	{{secret}}         密码字段的值（必须且只能出现一次）
//	{{field:名字}}     某个附加字段的值
//
// 刻意不做成通用模板：能被下发的内容拼出来的东西越少越好，
// 主机名尤其不能，它只来自 baseUrl 或用户自己填的站点字段。
type Login struct {
	Path string         `json:"path"`
	Body map[string]any `json:"body"`
	// SuccessPath/SuccessEquals 判断登录响应是否成功。很多企业接口登录失败
	// 也回 HTTP 200，不看响应内容就会拿着没登上的会话去调业务接口。
	SuccessPath   string `json:"successPath,omitempty"`
	SuccessEquals any    `json:"successEquals,omitempty"`
	// ReloginOn 列出响应里出现哪些字样就认为会话过期，重新登录并重试一次。
	ReloginOn []string `json:"reloginOn,omitempty"`
}

// FailWhen 描述「HTTP 200 但业务失败」的判定。
//
// 金蝶这类接口无论成败都回 200，错误藏在响应体里。不声明这个判定，
// 一次失败的查询会被当成成功，模型拿到一个空结果然后编出结论。
type FailWhen struct {
	Path   string `json:"path"`
	Equals any    `json:"equals,omitempty"`
	// Exists 为真时表示「这个路径存在就算失败」（比如 errcode 字段）。
	Exists bool `json:"exists,omitempty"`
	// MessagePath 指出错误说明在哪，用于拼报错信息。
	MessagePath string `json:"messagePath,omitempty"`
}

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
	// Login 只在 kind=login 时有意义。
	Login *Login `json:"login,omitempty"`
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
	// Columns 给「二维数组」形式的结果命名列，把 [[1,"A"],[2,"B"]] 变成
	// [{"id":1,"name":"A"},…]。金蝶 ExecuteBillQuery 就是这种返回。
	Columns []string `json:"columns,omitempty"`
	// ColumnsFromParam 指出列名来自哪个参数（逗号分隔的字段列表）。
	// 通用查询工具的列名由调用方决定，没法写死在声明里。
	ColumnsFromParam string `json:"columnsFromParam,omitempty"`
	// Meta 是响应根上要一起带出来的字段（点号路径），例如 total、isLast、
	// nextPageToken。Path 取出列表之后这些兄弟字段就丢了，而模型正是靠它们
	// 判断「是不是还有下一页」—— 没有它，声明里给了翻页参数也没用，
	// 因为模型拿不到该传什么。
	Meta []string `json:"meta,omitempty"`
}

// Param 是一个工具参数。
type Param struct {
	Name        string   `json:"name"`
	In          ParamIn  `json:"in"`
	Type        string   `json:"type,omitempty"` // string | number | boolean | object，默认 string
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	// BodyPath 让 body 参数落到嵌套位置，如 "data.FilterString"。
	// 留空则放在 body 顶层，键名就是 Name。
	BodyPath string `json:"bodyPath,omitempty"`
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
	// BodyConst 是写死在声明里的 body 片段，参数再合并到它上面。
	// 用于把一个工具钉死在某张单据上（FormId=BD_MATERIAL），
	// 这样「查物料」是一个工具，而不是让模型自己填表单标识。
	BodyConst map[string]any `json:"bodyConst,omitempty"`
	Result    Result         `json:"result,omitempty"`
}

// Spec 是一个渠道的完整声明。
type Spec struct {
	// ID 是渠道标识：纯小写英文缩写，全目录唯一，一经发布不可更改
	// （客户机的 channels.json 和密码管理器条目都按它定位）。
	// 按**具体那套接口**命名而不是按厂商：金蝶有七八条产品线、接口彼此
	// 不通，所以是 k3cloud 而不是 kingdee。
	ID string `json:"id"`
	// Name 是给人读的显示名，可以是中文，可以改。
	Name string `json:"name"`
	// Version 是这份声明的版本号，从 1 起递增。改声明就是发新版本：
	// 客户端据它判断缓存要不要更新、以及已启用渠道是否需要重新确认。
	Version int      `json:"version"`
	Icon    string   `json:"icon,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Tags    []string `json:"tags,omitempty"`

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

	// FailWhen 判定「HTTP 200 但业务失败」，对本渠道所有调用生效。
	FailWhen *FailWhen `json:"failWhen,omitempty"`

	// PostReads 声明「这个系统的读操作也是 POST」，从而允许 readOnly 的 POST 工具。
	//
	// 默认不允许，因为 readOnly 决定两件事：客户端是否免确认直接调，以及工具
	// 是否绕过写开关。对 REST 接口，方法本身就是证据（GET 不会改数据）；对
	// 金蝶这类 RPC 风格的接口，ExecuteBillQuery 和 Save 都是 POST，校验器
	// 无法区分，只有人读一遍声明才能确认。
	//
	// 所以这个开关不是「关掉检查」，而是把「方法不能作证」这件事记录下来并
	// 交给人：页面上会标出来，目录服务器上这类声明的发布也要第二人批准。
	PostReads bool `json:"postReads,omitempty"`

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
	tagRe         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
	placeholderRe = regexp.MustCompile(`\{([^{}]*)\}`)
	tokenRe       = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
	methodsAll    = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
)

// Validate 校验一份 spec。远端下发的目录必须逐条过这里才会被接受。
func (s Spec) Validate() error {
	if !typeRe.MatchString(s.ID) {
		return fmt.Errorf("id %q 不合法（小写字母、数字、下划线，2-31 字符）", s.ID)
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%s: name 不能为空", s.ID)
	}
	if s.Version < 1 {
		return fmt.Errorf("%s: version 必须是从 1 起的正整数（现在是 %d）", s.ID, s.Version)
	}
	if err := checkTags(s.Tags); err != nil {
		return fmt.Errorf("%s: tags %w", s.ID, err)
	}
	if !typeRe.MatchString(s.Integration) {
		return fmt.Errorf("%s: integration %q 不合法", s.ID, s.Integration)
	}

	switch {
	case s.BaseURL != "" && s.BaseURLField != "":
		return fmt.Errorf("%s: baseUrl 和 baseUrlField 只能有一个", s.ID)
	case s.BaseURL != "":
		if err := checkBaseURL(s.BaseURL); err != nil {
			return fmt.Errorf("%s: baseUrl %w", s.ID, err)
		}
	case s.BaseURLField != "":
		if _, ok := s.Field(s.BaseURLField); !ok {
			return fmt.Errorf("%s: baseUrlField %q 在 fields 里不存在", s.ID, s.BaseURLField)
		}
	default:
		return fmt.Errorf("%s: 必须声明 baseUrl 或 baseUrlField", s.ID)
	}

	if !paramNameRe.MatchString(s.Account.Name) || !paramNameRe.MatchString(s.Secret.Name) {
		return fmt.Errorf("%s: account.name 和 secret.name 必须是合法字段名", s.ID)
	}
	if s.Account.Name == s.Secret.Name {
		return fmt.Errorf("%s: account 和 secret 不能是同一个字段", s.ID)
	}
	switch s.Auth.Kind {
	case AuthBearer, AuthBasic:
	case AuthHeader, AuthQuery:
		if strings.TrimSpace(s.Auth.Name) == "" {
			return fmt.Errorf("%s: auth.kind=%s 必须给出 auth.name", s.ID, s.Auth.Kind)
		}
	case AuthLogin:
		if err := s.checkLogin(); err != nil {
			return fmt.Errorf("%s: auth.login %w", s.ID, err)
		}
	default:
		return fmt.Errorf("%s: 不支持的 auth.kind %q", s.ID, s.Auth.Kind)
	}
	if s.Auth.Kind != AuthLogin && s.Auth.Login != nil {
		return fmt.Errorf("%s: 只有 auth.kind=login 才能带 login 段", s.ID)
	}
	if s.FailWhen != nil && strings.TrimSpace(s.FailWhen.Path) == "" {
		return fmt.Errorf("%s: failWhen 必须给出 path", s.ID)
	}
	for _, f := range s.Fields {
		if !paramNameRe.MatchString(f.Name) {
			return fmt.Errorf("%s: 字段名 %q 不合法", s.ID, f.Name)
		}
		if f.Name == s.Account.Name || f.Name == s.Secret.Name || f.Name == AllowWriteField {
			return fmt.Errorf("%s: 字段名 %q 与保留字段冲突", s.ID, f.Name)
		}
	}

	// 登录式渠道的连通性测试就是登录本身：这类 RPC 风格接口往往一个 GET 都没有，
	// 而「随便挑一个 POST 当测试」要么会改用户数据，要么只能靠声明自称无害。
	if s.Auth.Kind == AuthLogin {
		if s.Test.Path != "" {
			return fmt.Errorf("%s: auth.kind=login 时不要声明 test —— 登录本身就是测试", s.ID)
		}
	} else {
		if err := checkEndpoint(s.Test.Method, s.Test.Path, nil); err != nil {
			return fmt.Errorf("%s: test %w", s.ID, err)
		}
		if s.Test.Method != "GET" {
			return fmt.Errorf("%s: test 必须是 GET——测试连通不能改动用户的系统", s.ID)
		}
	}
	if len(s.Tools) == 0 {
		return fmt.Errorf("%s: 至少要声明一个工具", s.ID)
	}

	postReadUsed := false
	seen := map[string]bool{}
	for _, t := range s.Tools {
		if t.ReadOnly && t.Method != "GET" {
			postReadUsed = true
		}
		if !toolNameRe.MatchString(t.Name) {
			return fmt.Errorf("%s: 工具名 %q 不合法", s.ID, t.Name)
		}
		// 前缀约束：两个渠道各自声明 list_issues 时，后注册的会覆盖前一个。
		if !strings.HasPrefix(t.Name, s.ID+"_") {
			return fmt.Errorf("%s: 工具名 %q 必须以 %s_ 开头", s.ID, t.Name, s.ID)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: 工具名 %q 重复", s.ID, t.Name)
		}
		seen[t.Name] = true
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("%s/%s: description 不能为空——模型靠它选工具", s.ID, t.Name)
		}
		if t.ReadOnly && t.Method != "GET" && !s.PostReads {
			return fmt.Errorf("%s/%s: readOnly 的工具默认必须是 GET（现在是 %s）。"+
				"如果这个系统的读操作本身就是 POST，在声明顶层加 \"postReads\": true，"+
				"该声明会被标记为需要人工复核", s.ID, t.Name, t.Method)
		}
		if err := checkEndpoint(t.Method, t.Path, t.Params); err != nil {
			return fmt.Errorf("%s/%s: %w", s.ID, t.Name, err)
		}
		if len(t.BodyConst) > 0 && t.Method == "GET" {
			return fmt.Errorf("%s/%s: GET 不能带 bodyConst", s.ID, t.Name)
		}
		if t.Result.Columns != nil && t.Result.ColumnsFromParam != "" {
			return fmt.Errorf("%s/%s: columns 和 columnsFromParam 只能有一个", s.ID, t.Name)
		}
		for _, m := range t.Result.Meta {
			if err := checkDotPath(m); err != nil {
				return fmt.Errorf("%s/%s: result.meta %w", s.ID, t.Name, err)
			}
		}
		pseen := map[string]bool{}
		for _, p := range t.Params {
			if !paramNameRe.MatchString(p.Name) {
				return fmt.Errorf("%s/%s: 参数名 %q 不合法", s.ID, t.Name, p.Name)
			}
			if p.Name == ReservedAccountParam {
				return fmt.Errorf("%s/%s: 参数名 %s 是保留名（多账号分派用）", s.ID, t.Name, p.Name)
			}
			if pseen[p.Name] {
				return fmt.Errorf("%s/%s: 参数 %q 重复", s.ID, t.Name, p.Name)
			}
			pseen[p.Name] = true
			switch p.In {
			case InQuery, InPath, InBody:
			default:
				return fmt.Errorf("%s/%s: 参数 %s 的 in=%q 不支持", s.ID, t.Name, p.Name, p.In)
			}
			switch p.Type {
			case "", "string", "number", "boolean":
			case "object":
				if p.In != InBody {
					return fmt.Errorf("%s/%s: 参数 %s 的 type=object 只能用在 in=body", s.ID, t.Name, p.Name)
				}
			default:
				return fmt.Errorf("%s/%s: 参数 %s 的 type=%q 不支持", s.ID, t.Name, p.Name, p.Type)
			}
			if p.In == InBody && t.Method == "GET" {
				return fmt.Errorf("%s/%s: GET 不能带 body 参数 %s", s.ID, t.Name, p.Name)
			}
			if p.BodyPath != "" {
				if p.In != InBody {
					return fmt.Errorf("%s/%s: 参数 %s 的 bodyPath 只能用在 in=body", s.ID, t.Name, p.Name)
				}
				if err := checkDotPath(p.BodyPath); err != nil {
					return fmt.Errorf("%s/%s: 参数 %s 的 bodyPath %w", s.ID, t.Name, p.Name, err)
				}
			}
		}
		if from := t.Result.ColumnsFromParam; from != "" && !pseen[from] {
			return fmt.Errorf("%s/%s: columnsFromParam=%q 不是这个工具的参数", s.ID, t.Name, from)
		}
	}
	// 没用到就不许声明：否则这个开关会被顺手加到每份声明上，失去「这份需要
	// 人工复核」的信号意义。
	if s.PostReads && !postReadUsed {
		return fmt.Errorf("%s: 声明了 postReads 但没有 POST 的只读工具，去掉它", s.ID)
	}
	return nil
}

// checkLogin 校验登录声明，重点是 body 里的取值标记。
func (s Spec) checkLogin() error {
	l := s.Auth.Login
	if l == nil {
		return fmt.Errorf("缺失：auth.kind=login 必须给出 login 段")
	}
	if !strings.HasPrefix(l.Path, "/") {
		return fmt.Errorf("path %q 必须以 / 开头", l.Path)
	}
	if len(l.Body) == 0 {
		return fmt.Errorf("body 不能为空")
	}
	secrets := 0
	for k, v := range l.Body {
		str, ok := v.(string)
		if !ok {
			continue // 数字、布尔等字面量，照原样发
		}
		for _, m := range tokenRe.FindAllStringSubmatch(str, -1) {
			tok := strings.TrimSpace(m[1])
			switch {
			case tok == "account":
			case tok == "secret":
				secrets++
			case strings.HasPrefix(tok, "field:"):
				name := strings.TrimSpace(strings.TrimPrefix(tok, "field:"))
				if _, ok := s.Field(name); !ok {
					return fmt.Errorf("body.%s 引用了不存在的字段 %q", k, name)
				}
			default:
				return fmt.Errorf("body.%s 里的标记 {{%s}} 不认识（只有 account / secret / field:名字）", k, tok)
			}
		}
	}
	if secrets != 1 {
		return fmt.Errorf("body 里 {{secret}} 必须出现一次，现在出现了 %d 次", secrets)
	}
	return nil
}

// KnownTags 是推荐的标签词表。标签是开放集合（下发的目录可以带新词），
// 但词表让页面上的分类不至于一半「ERP」一半「erp系统」。
var KnownTags = []string{
	"email", "erp", "finance", "devtools", "project", "im", "crm", "hr", "storage", "other",
}

// checkTags 校验标签：小写英文短词，最多 6 个，不重复。
//
// 刻意不限制在 KnownTags 内：远端下发新分类时，老客户端不该因为不认识
// 一个标签就整个渠道加载失败。词表只是给作者的建议。
func checkTags(tags []string) error {
	if len(tags) > 6 {
		return fmt.Errorf("最多 6 个，现在有 %d 个", len(tags))
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if !tagRe.MatchString(t) {
			return fmt.Errorf("标签 %q 不合法（小写字母、数字、连字符，1-20 字符）", t)
		}
		if seen[t] {
			return fmt.Errorf("标签 %q 重复", t)
		}
		seen[t] = true
	}
	return nil
}

func checkDotPath(p string) error {
	if strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") || strings.Contains(p, "..") {
		return fmt.Errorf("%q 形状不对", p)
	}
	for _, seg := range strings.Split(p, ".") {
		if !paramNameRe.MatchString(seg) {
			return fmt.Errorf("%q 里的 %q 不是合法键名", p, seg)
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
