# mcp-hub

单文件可执行的 MCP 服务器：把你用的各个系统当「渠道」接进来，凭据存在操作系统
自己的密码管理器里。渠道分两类：

- **邮件**（QQ 邮箱 / 163 / 126 / Gmail / Outlook，走 IMAP/SMTP）——有状态协议，
  实现编译在程序里。
- **声明式 HTTPS 渠道**——一个系统怎么接全部写成 JSON 数据，由通用执行器去跑。
  第一个是 GitHub。加一个这样的渠道**不写 Go 代码**，往
  `internal/httpchan/catalog/` 放一个 JSON 文件即可，将来可以由服务器下发。

目标机**不需要 Node、不需要 Python、不需要 npm**：一个二进制，Windows / macOS /
Linux 都是本地交叉编译产出。

## 为什么是 Go

同类的 Node 实现（`mcp-email`）在 stdio 下会把启动日志打到 stdout，
而 **stdio 传输的 stdout 就是 JSON-RPC 协议通道**，一行非 JSON 就让客户端握手失败
（Amazon Quick 报 `handshake_failed`）。Go 标准库 `log` 默认写 stderr，
往 stdout 输出必须显式 `os.Stdout`，所以这类 bug 不会因为手滑发生。
本程序唯一往 stdout 打人类可读文本的是那几个管理子命令，它们都不跑协议。

## 凭据

凭据**不进配置文件、不进日志、不经命令行参数**（argv 对同机其他进程可见，
还会进 shell 历史），只存在操作系统的密码管理器里：

| 平台 | 后端 |
|---|---|
| macOS | 钥匙串（Keychain） |
| Windows | 凭据管理器（Credential Manager / DPAPI） |
| Linux | Secret Service（D-Bus，需 gnome-keyring / KWallet 之类） |

三家后端都不需要 cgo，所以 `CGO_ENABLED=0` 的交叉编译不受影响。

条目以 `Ref{集成, 账号}` 为单位，在密码管理器里按 `mcp-hub:<集成名>` 分段存放。
每个集成一个**惰性句柄**：取句柄不访问后端，第一次真正用到它的 tool 被调用时才读，
之后整个进程复用（`sync.Once`）。所以有 8 个渠道而本次只用邮件时，另外 7 份凭据
不会被解出来放进内存；也不存在「每次调用都查钥匙串」的开销。

```bash
mcp-hub -set-secret -account 12345@qq.com   # 写入（不回显，从 stdin 读）
mcp-hub -list-secrets                        # 列出已配置条目，不打印凭据
mcp-hub -rm-secret -account 12345@qq.com     # 删除
```

本地索引文件 `<用户配置目录>/mcp-hub/credentials-index.json` 只记「哪个集成的
哪个账号已配置」，不含凭据；删掉它只会让 `-list-secrets` 变空。

## 邮件渠道的工具

| tool | 只读 | 说明 |
|---|---|---|
| `list_recent` | 是 | 最近 N 封的信头摘要（UID/发件人/主题/时间/大小/未读），不含正文 |
| `search_mail` | 是 | 按发件人、关键词、日期范围、未读状态检索，条件之间是「且」 |
| `read_mail` | 是 | 按 UID 取纯文本正文 + 附件文件名。用 `BODY.PEEK` 读，不标记已读 |
| `send_mail` | 否 | 发信。**默认不注册**，需 `MCPHUB_MAIL_ALLOW_SEND=true` 才启用 |

三个只读 tool 都带 `readOnlyHint`，客户端据此区分查询和动作
（Quick 的日志里会显示 `read=3 write=0` 而不是把全部归为 `unknown`）。

## 配置

| 环境变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `MCPHUB_MAIL_USER` | 是 | — | 完整邮箱地址，如 `12345@qq.com` |
| `MCPHUB_MAIL_PASSWORD` | 否 | — | **16 位授权码**。建议改用 `-set-secret` 存进密码管理器，这个只是兜底 |
| `MCPHUB_MAIL_ALLOW_SEND` | 否 | `false` | 置 `true` 才注册 `send_mail` |
| `MCPHUB_MAIL_IMAP_HOST` / `_IMAP_PORT` | 否 | 按域名推断 | 覆盖 IMAP 接入点 |
| `MCPHUB_MAIL_SMTP_HOST` / `_SMTP_PORT` | 否 | 按域名推断 | 覆盖 SMTP 接入点 |
| `MCPHUB_MAIL_MAX_BODY_BYTES` | 否 | `32768` | `read_mail` 正文上限，防止一封大信撑爆上下文 |
| `MCPHUB_TIMEOUT_SECONDS` | 否 | `30` | 连接与读写超时 |

**优先级：密码管理器 > 环境变量。** 所以把凭据搬进密码管理器只要
`-set-secret` 一次，不必同时去客户端面板里删环境变量。

兼容读取改名历史上用过的 `CHANNELS_MAIL_*`、`QQMAIL_*` 和 `mcp-email` 的 `EMAIL_*`，旧配置不用动也能跑。
旧版写进密码管理器的 `mcp-channels:*` 和 `qqmail-mcp:*` 条目会在启动时一次性迁移过来，不需要重新输入授权码。

**SMTP 默认用 465 直连 SSL**，不是腾讯文档里也列着的 587。587 走 STARTTLS，
在部分网络出口上不可达（实测过一次 IPv4 直连超时、IPv6 无路由），465 更稳。
要用 587 就显式设 `MCPHUB_MAIL_SMTP_PORT=587`。

QQ 邮箱的授权码在：邮箱设置 → 账户 → 开启 IMAP/SMTP 服务 → 生成授权码。

## 构建

```bash
make build                 # 当前平台
make all                   # windows/darwin/linux 全套到 dist/
MCPHUB_MAIL_USER=... make selftest
```

`proxy.golang.org` 在部分网络不可达，Makefile 默认用 `https://goproxy.cn,direct`。
直接跑 `go build` 时自己带上：

```bash
GOPROXY=https://goproxy.cn,direct go build ./...
```

## 验证连通性（不发信）

```bash
export MCPHUB_MAIL_USER='12345@qq.com'
./dist/mcp-hub -set-secret     # 输入授权码，不回显
./dist/mcp-hub -selftest
```

输出会报凭据来自哪里（`keychain` 还是 `env:XXX`），以及 IMAP 登录和 SMTP 认证的
结果，全通返回 0。

## 接到 MCP 客户端

渠道在页面上配好之后，客户端那边**只需要一个二进制路径**：没有环境变量、
没有密码。账号在 `channels.json` 里，凭据在系统密码管理器里，一个 server
把所有渠道的工具一起暴露出去。

```json
{
  "mcpServers": {
    "mcp-hub": {
      "command": "/绝对路径/mcp-hub",
      "args": []
    }
  }
}
```

这份 `mcpServers` 片段适用于 Claude Desktop、Cursor、Cline、Kiro
（`~/.kiro/settings/mcp.json`）等任何读这个格式的客户端。

### Amazon Quick（本地 connector）

Desktop App → Customize → Connectors → Create local connector → Local：

| 字段 | 值 |
|---|---|
| Command | 二进制的绝对路径，如 `C:\Tools\mcp-hub.exe` |
| Arguments | 留空 |
| Environment variables | 留空 |

也可以用同一个对话框的 Import 导入上面那段 JSON。

Windows 上**不需要** `cmd /c` 包一层 —— 那是 `npx` 这类 `.cmd` 垫片才需要的，
这里 Command 直接就是一个 PE 可执行文件。

Quick 的代码沙箱没有公网出口，所有对外访问都必须走 connector；
本程序是独立 OS 进程，不在那个沙箱里，所以 IMAP/SMTP 直连可用。
注意 Quick 不会自动刷新工具列表：页面上新增渠道后要手动点一次 Sync。

### 环境变量兜底（可选）

`MCPHUB_MAIL_*` 那套环境变量仍然有效，用于还没迁到页面配置的老 connector。
同一个邮箱两边都有时，页面配置优先。

## 声明式 HTTPS 渠道

一份 JSON 就是一个渠道。`internal/httpchan/catalog/github.json` 是第一个，
`internal/httpchan/spec.go` 是完整的 schema。

每份声明的头部有四样认人的东西：

| 字段 | 规则 |
|---|---|
| `id` | 纯小写英文缩写，**全目录唯一**，一经发布不可更改。按具体那套接口命名，不按厂商（`k3cloud` 不是 `kingdee`）|
| `name` | 给人读的显示名，可以是中文，可以改；同样不可重复（页面上两个同名条目用户没法分辨）|
| `version` | 从 1 起的正整数。改声明就是发新版本，客户端据它判断缓存要不要更新、已启用渠道是否需要重新确认 |
| `tags` | 分类标签，最多 6 个，小写英文短词。页面按它筛选 |

`id` 和 `name` 重复都是**加载期硬错误**：`id` 重复会让后加载的声明静默顶掉
前一份，用户的凭据指向就变了。标签是**开放集合**——下发的目录带来新分类时，
老客户端不该因为不认识一个标签就整个渠道加载失败，`KnownTags` 只是给作者的
建议词表，页面上没翻译的标签原样显示。

骨架：

```jsonc
{
  "id": "github",                         // 渠道标识，工具名必须以 github_ 开头
  "name": "GitHub",
  "version": 1,
  "tags": ["devtools", "project"],
  "integration": "github",                // 凭据在密码管理器里的分段
  "baseUrl": "https://api.github.com",    // 固定接入点；自建系统改用 baseUrlField
  "account": { "name": "login", "label": "GitHub 用户名" },
  "secret":  { "name": "token", "label": "个人访问令牌 (PAT)" },
  "auth":    { "kind": "bearer" },        // bearer | header | basic | query | login
  "test":    { "method": "GET", "path": "/user" },
  "tools": [{
    "name": "github_list_issues", "description": "……", "readOnly": true,
    "method": "GET", "path": "/repos/{owner}/{repo}/issues",
    "params": [
      { "name": "owner", "in": "path",  "required": true, "description": "……" },
      { "name": "state", "in": "query", "default": "open" }
    ],
    "result": { "limit": 50, "fields": ["number", "title", "state", "user.login"] }
  }]
}
```

页面、表单、凭据存放、工具注册都由这份数据驱动，没有一行渠道专属代码。
`result` 是必要的：不裁剪的话一次 issue 列表就有上万 token 的噪音。

列表类工具还要声明 `result.meta`，把响应根上的 `total` / `isLast` /
`nextPageToken` 一起带出来。少了它，模型分不清「这是全部」还是「第一页」，
而声明里给的翻页参数也没法用 —— 它拿不到该传什么。被 `limit` 本地截断时
结果里会多一个 `truncatedTo`，同样是为了别把「前 N 条」当成「全部」。

**返回给客户端的永远是一个 JSON 对象**：列表被包成 `{count, items, …meta}`。
MCP 规范较新的修订允许 `structuredContent` 是任意 JSON 值，但现实中的客户端
中间层普遍只接受 dict，直接回数组会在客户端那边解析失败 —— 表现为「工具明明
拿到了数据，用户看到的却是一个报错」。

### 工具描述是写给模型的，不是写给开发者的

`description` 是模型选工具的唯一依据，所以它要回答三件事：**什么时候用它、
返回什么、接下来该调谁**。实现细节一个字都不要写——模型不会因为知道你用了
`BODY.PEEK` 或者做了 GB2312 解码而做出更好的决定，那些字只是在挤占上下文。

```
✗ 按 UID 读取一封邮件的纯文本正文（自动做 GB2312/GBK 等字符集解码），
  并列出附件文件名。用 BODY.PEEK 读取，不会把邮件标记为已读。

✓ 读取一封邮件的正文内容和附件文件名。需要 UID，从 list_recent 或
  search_mail 的结果里取。只读取不下载附件，也不会把邮件变成已读。
```

两条都提到了「不改已读状态」，因为那是**行为事实**，会影响模型要不要提醒
用户；而 `BODY.PEEK` 是实现手段，删掉。

几条具体的：

- 用用户的话说事（「我最近有什么邮件」），不用接口的话说事（「信头摘要」）
- 明确说返回的是列表还是正文，否则模型会以为一次就拿到了全部内容
- 指向兄弟工具：`list_recent` 要说「已经知道找谁时用 search_mail 更快」，
  `github_list_issues` 要说「要看正文拿编号调 github_get_issue」
- 不可逆动作要写清后果和应有的谨慎：「发出后无法撤回，发送前应当先把收件人
  和内容给用户确认」
- 领域里的坑要写（「GitHub 把 PR 也算作 issue」），这是模型没法自己推出来的
- 参数的说明放在参数的 `description` 里，不要堆进工具描述

账号那部分由程序自动补在描述末尾，也按同一个标准写成能据以行动的句子：

```
单账号：… 当前邮箱是 7709433@qq.com。
多账号：… 配了 3 个邮箱，必须用 channel_account 指定一个：a@gmail.com、b@163.com、c@qq.com。
         其中只有 c@qq.com 允许发信，其他邮箱会被拒绝。
```

**校验在加载时就做**（`Spec.Validate`，远端下发的内容走同一个入口），几条底线：

- `baseUrl` 只能是 https，且不允许含占位符——主机名不能由目录模板拼出。
  自建系统用 `baseUrlField` 让**用户自己**填站点地址，运行时再校一次 https。
- 路径只能用 `{name}` 占位，每个占位都要有对应的 `in: path` 参数；
  参数值里的 `/` 会被转义，模型给 `../../x` 也逃不出声明的路径。
- 工具名必须以渠道 id 为前缀，避免两个渠道的工具互相覆盖。
- `readOnly` 只能给 GET。非只读工具要渠道上打开「允许写入操作」才注册给模型
  ——不注册比注册后拒绝更安全，模型看不到的工具不会被诱导去调。
- 解析时拒绝未知字段：下发内容用了更新的 schema 时，静默忽略会让渠道以
  「少了一半行为」的样子跑起来。

GitHub 渠道自带 6 个工具：`github_whoami`、`github_list_my_repos`、
`github_search_repos`、`github_list_issues`、`github_get_issue`，以及写开关控制的
`github_create_issue`。

### 一个渠道配多个账号

同一个渠道类型可以配任意多个实例（两个 GitHub 账号、一套测试一套生产），
它们**仍然只注册一套工具**，由自动加上的 `channel_account` 参数选账号。

| | 单实例 | 多实例 |
|---|---|---|
| 工具数 | 和声明里一样 | 不变（不随账号数膨胀）|
| `channel_account` | 不出现 | 必填，枚举出可用账号 |

不给工具名加账号后缀是因为：客户端单 server 有 100 个工具的上限，而且新增
工具后要用户手动 Sync 才可见——固定的工具名把这两个麻烦都避开了。多实例时
`channel_account` 是**必填**而不是有默认值：替模型猜一个账号，在写入类工具上
就是选错对象。

写开关是**每个实例各自**的：A 账号可写、B 账号只读是成立的。工具只要有一个
账号可写就注册，调用时再按选中的账号校一次，工具描述里也会列出哪些账号可写。

### 两种渠道形状

GitHub 是 REST：Bearer 令牌、GET 读、字段裁剪。金蝶云·星空（K/3 Cloud）是 RPC 风格，差别大到
需要 spec 支持另外四件事：

| K3Cloud 的做法 | 声明里怎么写 |
|---|---|
| 先 POST `ValidateUser` 换 `kdservice-sessionid` cookie，后续请求带着它 | `auth.kind: "login"` + `login.body`（只认 `{{account}}` / `{{secret}}` / `{{field:名字}}` 三个标记）|
| body 是 `{"data":{"FormId":…}}`，而 View/Save 的 formid 在顶层 | 参数的 `bodyPath: "data.FormId"`；写死的部分放 `bodyConst` |
| 业务失败也回 HTTP 200，错误在 `Result.ResponseStatus` 里（还被数组包着）| 渠道级 `failWhen` |
| `ExecuteBillQuery` 回二维数组，列名在请求的 `FieldKeys` 里 | `result.columnsFromParam: "fieldKeys"` |

会话是**每实例一份、惰性登录、整进程复用**的：不是每次调用都登一遍（ERP 会把那
当成攻击），但会话过期时自动重登并重试一次（`login.reloginOn` 列出判断字样）。

#### `postReads`：方法不能作证的时候

`readOnly` 默认只能给 GET，因为它决定客户端是否免确认直接调、以及工具是否绕过
写开关。对 REST 接口，GET 本身就是证据；对金蝶这类接口，`ExecuteBillQuery` 和
`Save` 都是 POST，校验器区分不了。

所以 spec 顶层的 `postReads: true` **不是关掉检查**，而是把「方法不能作证」记录
下来交给人：页面上该渠道会带一个「复核」标记，目录服务器上这类声明的发布要求
第二人批准。没有 POST 只读工具却声明它，校验会报错——免得它被顺手加到每份
声明上而失去信号意义。

### 金蝶云·星空渠道（`k3cloud`）

**先对上系统**：金蝶有好几条产品线，接口彼此不通。这个渠道只对
**金蝶云·星空（K/3 Cloud）**的 BOS WebAPI，也就是
`Kingdee.BOS.WebApi.ServicesStub.*` 这组地址。

| 金蝶产品 | 这个渠道能用吗 |
|---|---|
| 金蝶云·星空（K/3 Cloud） | ✅ 就是它 |
| 金蝶云·苍穹（Cosmic） | ❌ 另一套 openapi |
| 金蝶云·星瀚（Constellation） | ❌ 另一套 openapi |
| 金蝶云·星辰 | ❌ 另一套接口 |
| 金蝶 KIS / K/3 WISE / EAS | ❌ 老产品，不是 BOS WebAPI |

所以渠道 id 是 `k3cloud` 而不是 `kingdee`，工具名也是 `k3cloud_*`：按产品线命名，
而不是按厂商命名。以后接苍穹就是另一份声明 `cosmic.json`，两者可以并存。

三个工具：`k3cloud_query_bill`（通用单据查询）、`k3cloud_view_bill`（读单据详情）、
写开关控制的 `k3cloud_save_bill`。要填站点地址（到 `/k3cloud` 为止，必须 https）、
账套 ID、账号、密码。

**权限建在星空那一侧**：填进来的是能登录 ERP 的真人密码，权限完全等于该账号在
星空里的权限。建议单建一个只分配查询权限的集成账号，而不是用你本人的账号——
hub 的写开关只能决定「不把写工具给模型」，决定不了那个账号本身能做什么。

**未经真实环境验证**：接口路径、登录判定（`LoginResultType == 1`）、失败响应形状
都来自公开文档，执行器是对着一个仿真的 K/3 Cloud 服务测通的（登录换 cookie、
嵌套 body、二维数组列名、业务失败、会话过期重登、写开关，8 个用例）。真实的
字段标识和响应嵌套要用你自己的账套核一遍。金蝶没有公开的测试环境或测试账号，
文档站 `open.kingdee.com/K3CloudDoc/` 对匿名访问也是 403。

### Jira 渠道（`jira`）

针对 **Jira Cloud**（`xxx.atlassian.net`）。要填站点地址、Atlassian 账号邮箱和
[API Token](https://id.atlassian.com/manage-profile/security/api-tokens)，认证是
Basic(邮箱:Token)。Jira Server / Data Center 自建版接口不完全相同，这份声明没覆盖。

八个工具：`jira_whoami`、`jira_list_projects`、`jira_search_issues`（JQL）、
`jira_list_issue_types`、`jira_get_issue`、`jira_list_comments`，以及写开关控制的
`jira_create_issue` 和 `jira_add_comment`。

`jira_list_issue_types` 是 `jira_create_issue` 的前置：事项类型是**按项目配置**的，
通用习惯里的 Story / Bug 不一定存在（实测一个新建的 Scrum 项目只有
Epic / Subtask / Feature / Task），类型名写错 Jira 会直接拒。

两个决定值得记下来，它们都来自 Atlassian 的接口现状：

- **搜索必须用 `/rest/api/3/search/jql`**，旧的 `/rest/api/3/search` 已被完全
  下线。新端点**不传 `fields` 就只回事项 ID**，所以声明里给了默认值
  `summary,status,assignee,priority,issuetype,updated`；翻页也从 `startAt`
  换成了 `nextPageToken`。
- **读详情和写评论走 `/rest/api/2/`，不是 v3。** v2 和 v3 的操作集一样，差别
  是富文本：v3 的 `description` 和评论 `body` 是 ADF（深层嵌套的 JSON 文档），
  v2 是纯文本字符串。模型读不了 ADF，也没法可靠地构造 ADF，所以这两个工具
  刻意用 v2。搜索用 v3 是因为它只返回标量字段，不碰富文本。

## 渠道命名规矩

渠道 `id` 和工具名要对得上**具体那套接口**，不是厂商、也不是产品大类：

- 厂商有多条产品线时，用产品线命名（`k3cloud` 不是 `kingdee`）
- 同一产品的不同接口体系要分开（星空的 BOS WebAPI 和苍穹的 openapi 是两份声明）
- 工具名必须以渠道 `id` 为前缀，这条由 `Spec.Validate` 强制
- `id` 一旦发布就不该改：客户机上的 `channels.json` 和密码管理器条目都按它定位，
  改名等于让用户已配好的渠道凭据失联（远端下发时这件事要靠目录里的改名映射
  来兜，现在还没做）

声明里的 `id` 落到客户机的 `channels.json` 里就是实例的 `type` 字段：前者是
「这份声明是谁」，后者是「这条实例属于哪类渠道」，同一个值的两层叫法。

## 已知限制

- 只处理 `text/plain`（没有就退回 `text/html` 原文，不做 HTML 转纯文本）。
- 附件只列文件名，不下载内容。
- 未做代码签名，首次在 Windows 上运行可能被 SmartScreen 拦（本地编译本地拷贝通常无感）。
- Windows 控制台代码页可能让 stderr 上的中文日志显示成乱码，不影响功能。
- 渠道目录只有内置一层（`go:embed`）。远端下发 + 验签、用户私有目录这两层还没做，
  但 `Parse` / `Validate` 已经是三层共用的入口，设计见 `docs/server-design.md`。
- 客户端不会自动刷新工具列表：在页面里新增渠道后，Amazon Quick 那边要手动点一次 Sync。
- 邮件工具的 `channel_account` 参数在 schema 里**永远是可选的**，即使配了多个邮箱
  （描述里会写「必须指定」，真的不传则在调用时报错并列出可选账号，模型可以自己纠正）。
  原因是邮件工具走泛型 `AddTool`，schema 由结构体推断，没法按账号数量动态增删。
  声明式渠道是动态生成 schema 的，多账号时 `channel_account` 会被标成 required。

## 依赖

| 用途 | 库 |
|---|---|
| MCP 协议 | `github.com/modelcontextprotocol/go-sdk` |
| 系统密码管理器 | `github.com/zalando/go-keyring` |
| IMAP | `github.com/emersion/go-imap/v2` |
| MIME / 字符集 | `github.com/emersion/go-message`（导入 `charset` 即注册 GB2312/GBK/GB18030 等） |
| SMTP | `github.com/wneessen/go-mail` |
