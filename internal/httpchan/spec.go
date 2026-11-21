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
//   - 唯一的例外是 local 渠道（连本机上另一个程序，见 Spec.Local）：那里
//     允许明文 http，但主机被收紧成「必须是字面的回环地址」。两条一起换，
//     不是单方面放宽。
//   - 路径只能用 {name} 占位，且每个占位都必须有对应的已声明参数。
//   - 工具名必须以渠道类型为前缀，避免多个渠道的工具互相覆盖。
//   - 非只读工具（会改外部系统的）要渠道上显式打开写开关才注册给模型。
package httpchan

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/nimysan/mcp-hub/internal/oauthdev"
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
	// NotEquals 非空时表示「这个路径存在、且值不等于它就算失败」。飞书这类
	// 接口成功时 code 是 0，失败码五花八门，没法逐个列进 Equals。
	NotEquals any `json:"notEquals,omitempty"`
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
	// ItemObject 只配 type=array：把模型给的每个字符串套进这个对象模板，
	// 模板里值为 "{{item}}" 的那一项换成它。飞书的参会人、任务成员都是
	// [{"type":"user","user_id":"ou_x"}] 这种形状，让模型只给一串 open_id，
	// 形状由声明钉死。
	ItemObject map[string]any `json:"itemObject,omitempty"`
	// ItemFields 只配 type=array：数组的每一项是一个对象，只能有这几个键、
	// 值都是字符串。ItemRequired 是其中必填的。
	//
	// 百度网盘的移动、重命名一次可以处理多个文件，filelist 是
	// [{"path":"/a","dest":"/b","newname":"c"}]。键由声明钉死，模型只填值：
	// 多一个键、少一个必填键都在发出去之前报错，而不是让对方系统猜。
	ItemFields   []string `json:"itemFields,omitempty"`
	ItemRequired []string `json:"itemRequired,omitempty"`
	// ArrayOnly 只配 type=array：只接受真正的数组（或一段 JSON 数组文本），
	// 不把一个字符串按逗号拆开。文件路径里可以有逗号，「/a,b.txt」被拆成
	// 「/a」和「b.txt」再拿去删除，删掉的就是另一个文件。
	ArrayOnly bool `json:"arrayOnly,omitempty"`
	// Format 让参数接受人能读的时间，发送前换成接口要的格式：
	//   unix     秒级时间戳字符串
	//   unix_ms  毫秒级时间戳字符串
	//   rfc3339  2026-11-20T15:00:00+08:00
	// 输入可以是 "2026-11-20 15:00"、"2026-11-20"、RFC3339 或者已经是时间戳。
	// 没有时区的按本机时区。模型换算 Unix 时间戳很容易差几个小时，这一步
	// 不该交给它。
	Format string `json:"format,omitempty"`
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
	// BodyJSONString 列出 body 里要「先序列化成 JSON 字符串再放进去」的路径。
	//
	// 飞书发消息的 content 不是对象而是一段 JSON 文本：
	// {"msg_type":"text","content":"{\"text\":\"你好\"}"}。让模型自己拼这段
	// 转义过的字符串既容易错，也等于把消息结构的控制权交给了模型（它可以
	// 换成卡片、@所有人）。所以参数照常落到 content.text，发送前再把
	// content 整个变成字符串。
	BodyJSONString []string `json:"bodyJsonString,omitempty"`
	// BodyEncoding 是请求体的编码：留空或 "json"＝JSON；"form"＝
	// application/x-www-form-urlencoded。
	//
	// 百度网盘的文件管理（删除、移动、重命名）只收表单。表单是扁平的，所以
	// form 下 bodyPath 不能带点；值是数组或对象的参数整体序列化成一段 JSON
	// 文本放进那一项 —— 百度的 filelist 正是这个形状
	// （filelist=[{"path":"/a","newname":"b"}]）。
	BodyEncoding string `json:"bodyEncoding,omitempty"`
	Result       Result `json:"result,omitempty"`
}

// 请求体编码。
const (
	BodyJSON = "json"
	BodyForm = "form"
)

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

	// Local 声明这个渠道连的不是某个网站，而是**用户自己机器上另一个程序**
	// 在回环地址上开的接口（NapCat 那类本地网关）。
	//
	// 它换掉一条约束，不是关掉它：允许明文 http（回环流量不出网卡，没有
	// 可供窃听的网络路径），但主机必须是字面的回环地址 —— 域名不行，连
	// localhost 也不行，因为域名要靠 DNS 解析，解析到哪里不由这里决定，
	// 而这个例外成立的全部理由正是「这些字节不离开本机」。
	//
	// 代价要说清：同机的其他进程仍可能看到这条明文流量，所以本地网关的
	// 令牌只该是那个网关自己的令牌，不该复用别处的凭据。
	//
	// 页面据它做披露：这类渠道要用户另外装一个程序，必须在点「添加」之前
	// 就看得见，而不是保存失败了才发现。
	Local bool `json:"local,omitempty"`

	Account Credential        `json:"account"`
	Secret  Credential        `json:"secret"`
	Auth    Auth              `json:"auth"`
	Headers map[string]string `json:"headers,omitempty"`
	Fields  []Field           `json:"fields,omitempty"`

	// OAuth 非空时，这个渠道的凭据可以用页面上的「连接」按钮走设备流拿到，
	// 不必让用户去对方系统手工生成一个长期令牌再粘过来。
	//
	// 它不替换手填：同一个凭据槽既能装用户粘来的长期令牌，也能装连接拿到的
	// 令牌组（靠 oauthdev 的标记区分）。两条路并存是因为企业里两种都有人用 ——
	// 有的组织根本不允许给桌面程序注册 OAuth App。
	OAuth *OAuthSpec `json:"oauth,omitempty"`

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
	// WriteHelp 非空时替换写开关下面的默认说明（默认列出受控工具名）。
	// 给业务人员用的渠道可以用一句人话说清打开后会发生什么。
	WriteHelp string `json:"writeHelp,omitempty"`

	// Test 是「测试连通」用的只读接口，保存渠道前会先打一次。
	Test  Endpoint `json:"test"`
	Tools []Tool   `json:"tools"`
}

// AllowWriteField 是写开关的字段名。只要 spec 里有非只读工具，
// 描述符就会自动长出这个开关，默认关。
const AllowWriteField = "allowWrite"

// OAuthSpec 是一个渠道的 OAuth 授权参数。
//
// 不支持浏览器回调（授权码流）：桌面程序存不住 client_secret，而 GitHub
// 这类提供方的 web 流换 token 必须带 secret。支持两种流：
//
//	device    设备流，只要 client_id。GitHub、云之家、飞书走这条。
//	implicit  隐式授权 + 用户把授权后的地址粘回来。百度网盘的设备流换令牌
//	          必须带应用密钥，不注册开发者应用就只剩这条路。没有刷新令牌。
type OAuthSpec struct {
	// Flow 是 "device" 或 "implicit"。写出来是为了以后加别的流时，旧客户端
	// 能干净地拒绝一个它执行不了的声明，而不是半残地跑起来。
	Flow string `json:"flow"`

	// AuthorizeURL 是隐式授权的授权页（只有 flow=implicit 用）。
	AuthorizeURL string `json:"authorizeUrl,omitempty"`
	// RedirectURI 是隐式授权的回跳地址，只能是 "oob"（授权服务自己的
	// 「授权成功」页）或 https 地址。它不能指向本机：那等于声称本程序
	// 能接住回调，而它接不住。
	RedirectURI string `json:"redirectUri,omitempty"`

	// ClientID 写死在声明里（厂商自己发布的 connector 用这个）。
	ClientID string `json:"clientId,omitempty"`
	// ClientIDField 指向 Fields 里的一个字段，由用户自己注册 App 后填入。
	// 两者必须且只能有一个。
	ClientIDField string `json:"clientIdField,omitempty"`

	DeviceCodeURL string   `json:"deviceCodeUrl"`
	TokenURL      string   `json:"tokenUrl"`
	Scopes        []string `json:"scopes,omitempty"`

	// Dialect 选择授权服务的线上格式。留空＝RFC 8628（GitHub 那种）。
	// 写出来是因为「流程一样、字段名不一样」的提供方是常态，而按错误
	// 的格式去打一个真实授权服务，表现是「一直等待授权」—— 看不出原因。
	Dialect string `json:"dialect,omitempty"`

	// VerificationURI 是用户去输码的那一页。RFC 8628 要求授权服务在出码
	// 响应里返回它，所以标准方言不用填；云之家不返回，必须在这里给出。
	VerificationURI string `json:"verificationUri,omitempty"`

	// RefreshURL 是刷新令牌的地址，留空表示和 TokenURL 相同。
	RefreshURL string `json:"refreshUrl,omitempty"`

	// SetupURL 是「去哪注册 App」的地址，页面上给用户一个链接。
	SetupURL string `json:"setupUrl,omitempty"`
	// Help 是填 client id 前要知道的事，显示在输入框旁边。
	Help string `json:"help,omitempty"`

	// ClientSecret 描述「应用密钥」输入框，只有机密客户端方言（飞书）才有。
	//
	// 它不是 Fields 里的一个字段：Fields 会落进 channels.json，而密钥只能进
	// 密码管理器。页面在连接区单独渲染这个框，值随「连接」请求带给服务端，
	// 授权成功后和令牌一起存进同一个密码管理器条目。
	ClientSecret *Credential `json:"clientSecret,omitempty"`
	// RegisterURL 非空时页面提供「一键创建应用」：用户扫码，对方系统在他的
	// 企业里建好应用并返回 App ID 和密钥，然后接着走用户授权。只有飞书方言
	// 支持。RegisterName / RegisterDesc 预填在确认页上（{user} 换成扫码人）。
	RegisterURL  string `json:"registerUrl,omitempty"`
	RegisterName string `json:"registerName,omitempty"`
	RegisterDesc string `json:"registerDesc,omitempty"`
}

// OAuthConfig 把声明和实例设置拼成可执行的设备流参数。
func (s Spec) OAuthConfig(settings map[string]string) (oauthdev.Config, error) {
	if s.OAuth == nil {
		return oauthdev.Config{}, fmt.Errorf("渠道 %s 不支持 OAuth 连接", s.ID)
	}
	clientID := s.OAuth.ClientID
	if s.OAuth.ClientIDField != "" {
		clientID = strings.TrimSpace(settings[s.OAuth.ClientIDField])
		if clientID == "" {
			label := s.OAuth.ClientIDField
			if f, ok := s.Field(s.OAuth.ClientIDField); ok {
				label = f.Label
			}
			return oauthdev.Config{}, fmt.Errorf("请先填写 %s", label)
		}
	}
	return oauthdev.Config{
		ClientID:        clientID,
		DeviceCodeURL:   s.OAuth.DeviceCodeURL,
		TokenURL:        s.OAuth.TokenURL,
		Scopes:          s.OAuth.Scopes,
		Dialect:         s.OAuth.Dialect,
		VerificationURI: s.OAuth.VerificationURI,
		RefreshURL:      s.OAuth.RefreshURL,
	}, nil
}

// Implicit 是 flow=implicit 时的授权参数。
func (s Spec) Implicit() (oauthdev.Implicit, error) {
	if s.OAuth == nil || s.OAuth.Flow != FlowImplicit {
		return oauthdev.Implicit{}, fmt.Errorf("渠道 %s 不走隐式授权", s.ID)
	}
	return oauthdev.Implicit{
		AuthorizeURL: s.OAuth.AuthorizeURL,
		ClientID:     s.OAuth.ClientID,
		RedirectURI:  s.OAuth.RedirectURI,
		Scopes:       s.OAuth.Scopes,
	}, nil
}

// 授权流。
const (
	FlowDevice   = "device"
	FlowImplicit = "implicit"
)

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
		if err := checkBaseURL(s.BaseURL, s.Local); err != nil {
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
	if err := s.checkOAuth(); err != nil {
		return fmt.Errorf("%s: oauth %w", s.ID, err)
	}
	if s.FailWhen != nil && strings.TrimSpace(s.FailWhen.Path) == "" {
		return fmt.Errorf("%s: failWhen 必须给出 path", s.ID)
	}
	if fw := s.FailWhen; fw != nil && fw.NotEquals != nil && (fw.Equals != nil || fw.Exists) {
		return fmt.Errorf("%s: failWhen 的 notEquals 不能和 equals / exists 同时用", s.ID)
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
		if err := s.checkFixedQueryAuth(s.Test.Path); err != nil {
			return fmt.Errorf("%s: test %w", s.ID, err)
		}
		if s.Test.Method != "GET" {
			// 默认只许 GET：方法本身就是「这次测试不会改数据」的证据。
			//
			// 云之家这类系统一个 GET 都没有（GET 直接回认证失败），所以留
			// 一条口子，但不是「postReads 就随便挑个 POST」——测试是在用户
			// 点保存时自动打的，它改了什么用户没有机会先看。所以要求这个
			// 端点必须同时是某个 readOnly 工具的端点：那样它至少和工具走
			// 同一套审查，而不是一个没人看过的路径。
			if !s.PostReads {
				return fmt.Errorf("%s: test 必须是 GET——测试连通不能改动用户的系统（这个系统的读操作本身就是 POST 的话，在声明顶层加 \"postReads\": true）", s.ID)
			}
			matched := false
			for _, t := range s.Tools {
				if t.ReadOnly && t.Method == s.Test.Method && t.Path == s.Test.Path {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s: test 是 %s 而不是 GET，那它必须和某个 readOnly 工具是同一个端点（现在 %s %s 不是）", s.ID, s.Test.Method, s.Test.Method, s.Test.Path)
			}
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
		if err := s.checkFixedQueryAuth(t.Path); err != nil {
			return fmt.Errorf("%s/%s: %w", s.ID, t.Name, err)
		}
		switch t.BodyEncoding {
		case "", BodyJSON:
		case BodyForm:
			if t.Method == "GET" {
				return fmt.Errorf("%s/%s: GET 没有请求体，不能声明 bodyEncoding", s.ID, t.Name)
			}
			if len(t.BodyJSONString) > 0 {
				return fmt.Errorf("%s/%s: bodyEncoding=form 下数组和对象本来就会序列化成 JSON 文本，不要再写 bodyJsonString", s.ID, t.Name)
			}
			for _, p := range t.Params {
				if p.In == InBody && strings.Contains(p.BodyPath, ".") {
					return fmt.Errorf("%s/%s: bodyEncoding=form 是扁平的，参数 %s 的 bodyPath 不能带点", s.ID, t.Name, p.Name)
				}
			}
			for k, v := range t.BodyConst {
				switch v.(type) {
				case string, float64, bool:
				default:
					return fmt.Errorf("%s/%s: bodyEncoding=form 下 bodyConst.%s 只能是字符串、数字或布尔", s.ID, t.Name, k)
				}
			}
		default:
			return fmt.Errorf("%s/%s: bodyEncoding %q 不支持（只有 json / form）", s.ID, t.Name, t.BodyEncoding)
		}
		for _, jp := range t.BodyJSONString {
			if t.Method == "GET" {
				return fmt.Errorf("%s/%s: GET 不能带 bodyJsonString", s.ID, t.Name)
			}
			if err := checkDotPath(jp); err != nil {
				return fmt.Errorf("%s/%s: bodyJsonString %w", s.ID, t.Name, err)
			}
			// 必须有东西落在这个路径下，否则这一条是写错了而不是没生效。
			used := false
			if _, ok := t.BodyConst[strings.SplitN(jp, ".", 2)[0]]; ok {
				used = true
			}
			for _, p := range t.Params {
				k := p.Name
				if p.BodyPath != "" {
					k = p.BodyPath
				}
				if p.In == InBody && (k == jp || strings.HasPrefix(k, jp+".")) {
					used = true
				}
			}
			if !used {
				return fmt.Errorf("%s/%s: bodyJsonString %q 下面没有任何 body 参数或 bodyConst", s.ID, t.Name, jp)
			}
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
			switch p.Format {
			case "", "unix", "unix_ms", "rfc3339":
			default:
				return fmt.Errorf("%s/%s: 参数 %s 的 format=%q 不支持（只有 unix / unix_ms / rfc3339）", s.ID, t.Name, p.Name, p.Format)
			}
			if p.Format != "" && p.Type != "" && p.Type != "string" {
				return fmt.Errorf("%s/%s: 参数 %s 有 format 时 type 只能是 string", s.ID, t.Name, p.Name)
			}
			if p.ArrayOnly && p.Type != "array" {
				return fmt.Errorf("%s/%s: 参数 %s 的 arrayOnly 只能配 type=array", s.ID, t.Name, p.Name)
			}
			if len(p.ItemFields) > 0 || len(p.ItemRequired) > 0 {
				if p.Type != "array" || p.ItemObject != nil {
					return fmt.Errorf("%s/%s: 参数 %s 的 itemFields 只能配 type=array，且不能和 itemObject 同时用", s.ID, t.Name, p.Name)
				}
				fset := map[string]bool{}
				for _, f := range p.ItemFields {
					if !paramNameRe.MatchString(f) || fset[f] {
						return fmt.Errorf("%s/%s: 参数 %s 的 itemFields 里 %q 不合法或重复", s.ID, t.Name, p.Name, f)
					}
					fset[f] = true
				}
				for _, f := range p.ItemRequired {
					if !fset[f] {
						return fmt.Errorf("%s/%s: 参数 %s 的 itemRequired 里 %q 不在 itemFields 中", s.ID, t.Name, p.Name, f)
					}
				}
			}
			if p.ItemObject != nil {
				if p.Type != "array" {
					return fmt.Errorf("%s/%s: 参数 %s 的 itemObject 只能配 type=array", s.ID, t.Name, p.Name)
				}
				holes := 0
				for _, v := range p.ItemObject {
					if v == "{{item}}" {
						holes++
					}
				}
				if holes != 1 {
					return fmt.Errorf("%s/%s: 参数 %s 的 itemObject 里必须恰好有一个 \"{{item}}\"", s.ID, t.Name, p.Name)
				}
			}
			switch p.Type {
			case "", "string", "number", "boolean":
			case "array":
				if p.In != InBody {
					return fmt.Errorf("%s/%s: 参数 %s 的 type=array 只能用在 in=body", s.ID, t.Name, p.Name)
				}
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

// checkOAuth 校验 oauth 段。
//
// 最要紧的一条和 baseUrl 同理：授权服务地址不能由下发内容模板拼出 ——
// 否则一份被篡改的目录就能把「请去这里授权」指向攻击者，而用户在那一页
// 输入的是自己真实账号的凭据。
func (s Spec) checkOAuth() error {
	o := s.OAuth
	if o == nil {
		return nil
	}
	if o.Flow == FlowImplicit {
		return s.checkImplicit()
	}
	if o.Flow != FlowDevice {
		// 只支持这两种流。写死这一条，是为了旧客户端遇到以后新增的流时
		// 干净地拒绝，而不是忽略 flow 字段、按设备流跑出错误的行为。
		return fmt.Errorf("flow %q 不支持（只支持 device / implicit）", o.Flow)
	}
	if o.AuthorizeURL != "" || o.RedirectURI != "" {
		return fmt.Errorf("authorizeUrl / redirectUri 只有 flow=implicit 才用")
	}
	if err := s.checkOAuthAuth(); err != nil {
		return err
	}
	switch {
	case o.ClientID != "" && o.ClientIDField != "":
		return fmt.Errorf("clientId 和 clientIdField 只能有一个")
	case o.ClientID == "" && o.ClientIDField == "":
		return fmt.Errorf("必须给出 clientId 或 clientIdField")
	case o.ClientIDField != "":
		if _, ok := s.Field(o.ClientIDField); !ok {
			return fmt.Errorf("clientIdField %q 在 fields 里不存在", o.ClientIDField)
		}
	}
	// 这三个地址一律按远端的严格规则校（不吃 Local 的例外）：授权服务在
	// 本机上没有意义，而「把用户送去输密码的那一页」更不能是明文。
	if err := checkBaseURL(o.DeviceCodeURL, false); err != nil {
		return fmt.Errorf("deviceCodeUrl %w", err)
	}
	if err := checkBaseURL(o.TokenURL, false); err != nil {
		return fmt.Errorf("tokenUrl %w", err)
	}
	switch o.Dialect {
	case "", oauthdev.DialectRFC8628:
		// 标准方言的验证地址由授权服务返回，声明里填了也用不上 —— 与其
		// 留着一个不生效的字段让人误会，不如直接拒绝。
		if o.VerificationURI != "" {
			return fmt.Errorf("verificationUri 只有非标准方言才需要（标准设备流由授权服务返回）")
		}
	case oauthdev.DialectYunzhijia:
		if strings.TrimSpace(o.VerificationURI) == "" {
			return fmt.Errorf("dialect %s 必须给出 verificationUri（它的出码响应不返回验证地址）", o.Dialect)
		}
	case oauthdev.DialectFeishu:
		// 飞书授权要带应用密钥，所以每个企业必须用自己的应用：写死一个
		// clientId 发给所有人，就等于把那个应用的密钥也发给了所有人。
		if o.ClientIDField == "" {
			return fmt.Errorf("dialect %s 必须用 clientIdField（每个企业用自己的应用），不能写死 clientId", o.Dialect)
		}
		if o.ClientSecret == nil || !paramNameRe.MatchString(o.ClientSecret.Name) {
			return fmt.Errorf("dialect %s 必须给出 clientSecret（换令牌要带应用密钥）", o.Dialect)
		}
		if _, clash := s.Field(o.ClientSecret.Name); clash || o.ClientSecret.Name == s.Account.Name || o.ClientSecret.Name == s.Secret.Name {
			return fmt.Errorf("clientSecret.name %q 不能和已有字段同名（它不能落进配置文件）", o.ClientSecret.Name)
		}
		if o.VerificationURI != "" {
			return fmt.Errorf("dialect %s 的验证地址由授权服务返回，不要填 verificationUri", o.Dialect)
		}
	default:
		return fmt.Errorf("dialect %q 不支持", o.Dialect)
	}
	if o.VerificationURI != "" {
		// 和 deviceCodeUrl 同理，而且更要紧：这是用户真正输入账号密码的
		// 那一页。一份被篡改的目录若能改掉它，就等于指定了钓鱼地址。
		if err := checkBaseURL(o.VerificationURI, false); err != nil {
			return fmt.Errorf("verificationUri %w", err)
		}
	}
	if o.RefreshURL != "" {
		if err := checkBaseURL(o.RefreshURL, false); err != nil {
			return fmt.Errorf("refreshUrl %w", err)
		}
	}
	if o.SetupURL != "" {
		if err := checkBaseURL(o.SetupURL, false); err != nil {
			return fmt.Errorf("setupUrl %w", err)
		}
	}
	if o.Dialect != oauthdev.DialectFeishu {
		if o.ClientSecret != nil {
			return fmt.Errorf("clientSecret 只有机密客户端方言（feishu）才用得上")
		}
		if o.RegisterURL != "" || o.RegisterName != "" || o.RegisterDesc != "" {
			return fmt.Errorf("registerUrl 只有 feishu 方言支持")
		}
	}
	if o.RegisterURL != "" {
		// 和授权地址同样严格：用户在那一页确认「在我的企业里建一个应用」。
		if err := checkBaseURL(o.RegisterURL, false); err != nil {
			return fmt.Errorf("registerUrl %w", err)
		}
	}
	for _, sc := range o.Scopes {
		if strings.TrimSpace(sc) == "" || strings.ContainsAny(sc, " \t") {
			return fmt.Errorf("scope %q 不合法（一个元素一个作用域，不要带空格）", sc)
		}
	}
	return nil
}

// checkOAuthAuth 校验授权拿到的令牌往哪放。令牌组要经过解析（取 access_token、
// 判过期、续期）才能用，只有 bearer 和 query 两条注入路径做了这一步；别的
// 方式会把整段令牌组 JSON 原样塞进请求。
func (s Spec) checkOAuthAuth() error {
	if s.Auth.Kind != AuthBearer && s.Auth.Kind != AuthQuery {
		return fmt.Errorf("只能和 auth.kind=bearer 或 query 搭配（现在是 %s）", s.Auth.Kind)
	}
	return nil
}

// checkImplicit 校验 flow=implicit。
//
// 规则比设备流严：clientId 必须写死（公开应用），设备流和机密客户端的那些
// 键一个都不许出现 —— 写了也不生效的字段只会让人以为它在起作用。
func (s Spec) checkImplicit() error {
	o := s.OAuth
	if err := s.checkOAuthAuth(); err != nil {
		return err
	}
	if strings.TrimSpace(o.ClientID) == "" || o.ClientIDField != "" {
		return fmt.Errorf("flow=implicit 必须写死 clientId（公开应用），不能用 clientIdField")
	}
	if o.DeviceCodeURL != "" || o.TokenURL != "" || o.RefreshURL != "" || o.VerificationURI != "" ||
		o.Dialect != "" || o.ClientSecret != nil || o.RegisterURL != "" || o.RegisterName != "" || o.RegisterDesc != "" {
		return fmt.Errorf("flow=implicit 不能带设备流的键（deviceCodeUrl / tokenUrl / refreshUrl / verificationUri / dialect / clientSecret / register*）")
	}
	// 和设备流的授权地址同理：这是用户真正输入账号密码的那一页。
	if err := checkBaseURL(o.AuthorizeURL, false); err != nil {
		return fmt.Errorf("authorizeUrl %w", err)
	}
	switch r := strings.TrimSpace(o.RedirectURI); {
	case r == "oob":
	case r == "":
		return fmt.Errorf("flow=implicit 必须给出 redirectUri（\"oob\" 或 https 地址）")
	default:
		if err := checkBaseURL(r, false); err != nil {
			return fmt.Errorf("redirectUri %w", err)
		}
	}
	if o.SetupURL != "" {
		if err := checkBaseURL(o.SetupURL, false); err != nil {
			return fmt.Errorf("setupUrl %w", err)
		}
	}
	for _, sc := range o.Scopes {
		if strings.TrimSpace(sc) == "" || strings.ContainsAny(sc, " \t,") {
			return fmt.Errorf("scope %q 不合法（一个元素一个作用域）", sc)
		}
	}
	return nil
}

func checkBaseURL(raw string, local bool) error {
	// 先查占位符：主机名里带 { 会让 url.Parse 以「非法字符」失败，
	// 那个报错看不出真正的原因是「不允许用模板拼主机名」。
	if strings.ContainsAny(raw, "{}") {
		return fmt.Errorf("%q 不允许含占位符——主机名不能由模板拼出", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q 解析失败: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q 缺少主机名", raw)
	}
	// 回环地址必须是字面 IP 才算数。域名（包括 localhost）要靠 DNS 解析，
	// 解析到哪里不由这里决定，而「允许明文」这个例外成立的全部理由就是
	// 这些字节不离开本机 —— 一个待解析的名字证明不了这件事。
	ip := net.ParseIP(u.Hostname())
	loopback := ip != nil && ip.IsLoopback()

	switch {
	case local && !loopback:
		return fmt.Errorf("%q 不是本机地址：local 渠道的主机必须写成字面的回环地址"+
			"（127.0.0.1 或 ::1），不能用域名——包括 localhost", raw)
	case local:
		// 明文只在这一支里放行，而这一支已经被上面那条钉在回环地址上。
		// 这两条合起来就是整个例外的全部：没有「http 到任意主机」的路径。
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%q 必须是 http 或 https", raw)
		}
	default:
		// 只允许 https：凭据在请求头里，明文 http 等于把令牌交给网络。
		//
		// 这里刻意不反向要求「指向回环就必须声明 local」：那只是页面披露上
		// 的讲究，不是安全控制（https 到本机没有任何泄露），而它会连带禁掉
		// 「把普通渠道指向一台本机测试服务」这种完全正当的用法。
		if u.Scheme != "https" {
			return fmt.Errorf("%q 必须是 https", raw)
		}
	}
	return nil
}

// checkEndpoint 校验方法和路径，并确保路径里每个 {name} 都有对应的 path 参数。
//
// 路径可以带一段写死的查询串（/rest/2.0/xpan/file?method=list）：百度网盘
// 这类 RPC 风格的接口靠 method= 区分操作，同一个路径上挂着列目录和删文件。
// 那段查询串只许是字面量 —— 不能有占位符，也不能和参数同名（否则模型给的
// 值和声明钉死的值谁说了算就成了实现细节）。
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
	if strings.Contains(path, "#") {
		return fmt.Errorf("path %q 不能带 #", path)
	}
	fixed, err := fixedQuery(path)
	if err != nil {
		return err
	}
	for _, p := range params {
		if _, clash := fixed[p.Name]; clash {
			return fmt.Errorf("参数 %s 和 path 里写死的查询参数同名", p.Name)
		}
	}
	path, _, _ = strings.Cut(path, "?")
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

// fixedQuery 取出 path 里写死的查询串。没有就返回空。
func fixedQuery(path string) (url.Values, error) {
	_, raw, ok := strings.Cut(path, "?")
	if !ok {
		return nil, nil
	}
	if raw == "" {
		return nil, fmt.Errorf("path %q 的 ? 后面是空的", path)
	}
	if strings.ContainsAny(raw, "{}") {
		return nil, fmt.Errorf("path %q 的查询串不能带占位符（变化的值声明成 in=query 参数）", path)
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return nil, fmt.Errorf("path %q 的查询串解析失败: %w", path, err)
	}
	for k, v := range vals {
		if !paramNameRe.MatchString(k) {
			return nil, fmt.Errorf("path %q 的查询参数名 %q 不合法", path, k)
		}
		if len(v) != 1 {
			return nil, fmt.Errorf("path %q 的查询参数 %s 出现了不止一次", path, k)
		}
	}
	return vals, nil
}

// checkFixedQueryAuth 不许写死的查询串里出现凭据参数名：一份声明若能写死
// access_token=…，就能让所有请求带着它指定的令牌去访问。
func (s Spec) checkFixedQueryAuth(path string) error {
	if s.Auth.Kind != AuthQuery {
		return nil
	}
	fixed, _ := fixedQuery(path)
	if _, clash := fixed[s.Auth.Name]; clash {
		return fmt.Errorf("path 里不能写死凭据参数 %s", s.Auth.Name)
	}
	return nil
}
