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

十三个工具，读写分开：

| 读 | 写（要开写开关） |
|---|---|
| `jira_whoami` 当前身份 | `jira_create_issue` 建事项 |
| `jira_list_projects` 列项目 | `jira_update_issue` 改标题/描述/优先级 |
| `jira_search_issues` JQL 搜索 | `jira_assign_issue` 指派负责人 |
| `jira_list_issue_types` 列事项类型 | `jira_transition_issue` 流转状态 |
| `jira_search_users` 找人拿 accountId | `jira_add_comment` 加评论 |
| `jira_list_transitions` 列可用流转 | |
| `jira_get_issue` 读详情 | |
| `jira_list_comments` 读评论 | |

左边三个「查 id」的工具是右边写操作的前置，Jira 的三个坑决定了它们必须存在：

- **事项类型按项目配置**，通用习惯里的 Story / Bug 不一定有（实测一个新建的
  Scrum 项目只有 Epic / Subtask / Feature / Task），类型名写错直接拒
  → `jira_list_issue_types`
- **指派要 accountId**，不是显示名也不是邮箱 → `jira_search_users`
- **可用流转取决于当前状态和项目工作流**，不是固定几个，而且流转要传 id
  不是状态名 → `jira_list_transitions`（它把目标状态名一起带出来，
  否则模型只看到一串 id，不知道哪个是「完成」）

这三条模型都推不出来，所以写在了对应工具的描述里。

两个决定值得记下来，它们都来自 Atlassian 的接口现状：

- **搜索必须用 `/rest/api/3/search/jql`**，旧的 `/rest/api/3/search` 已被完全
  下线。新端点**不传 `fields` 就只回事项 ID**，所以声明里给了默认值
  `summary,status,assignee,priority,issuetype,updated`；翻页也从 `startAt`
  换成了 `nextPageToken`。
- **读详情和写评论走 `/rest/api/2/`，不是 v3。** v2 和 v3 的操作集一样，差别
  是富文本：v3 的 `description` 和评论 `body` 是 ADF（深层嵌套的 JSON 文档），
  v2 是纯文本字符串。模型读不了 ADF，也没法可靠地构造 ADF，所以这两个工具
  刻意用 v2。搜索用 v3 是因为它只返回标量字段，不碰富文本。

## 渠道目录的三层来源

```
本地私有  ~/.../mcp-hub/catalog/*.json       用户自建系统、贡献者本地调试
远端下发  ~/.../mcp-hub/catalog/remote.json  服务器发布的签名目录（缓存）
内置      go:embed                           编译进二进制，离线可用
```

优先级**本地 > 远端 > 内置**，同一个 id 出现在多层时靠上的那层生效，
`-catalog-list` 会标出是哪一层在生效、压住了哪些层——「我改了声明怎么没效果」
必须一眼查得出来。

```
mcp-hub -catalog-list                       列出生效的声明及来源层
mcp-hub -catalog-validate ./jira.json       校验一份声明（和加载时同一套规则）
mcp-hub -catalog-sync https://.../cat.json  抓取并验签远端目录，写入缓存
```

往 `~/.../mcp-hub/catalog/` 里丢一份 JSON，**不用重新编译、不用重启配置页面**，
刷新页面就能在渠道市场里看到它，配好账号后 MCP 工具立即可用。

三条约束：

- **启动不联网**。远端目录只在显式 `-catalog-sync` 时抓，MCP 服务启动只读磁盘。
  stdio 握手要是卡在一次网络请求上，客户端会直接判超时。
- **拉不到不影响已配好的渠道**。验签失败、文件损坏、网络不通都只让那一层失效
  并记一条错误，内置层照常工作。
- **签名覆盖的是「意思」而不是字节**。验签前先把每份声明解析成 `Spec` 再重新
  序列化，所以服务器换个 JSON 缩进不会让签名失效；而 `Parse` 拒绝未知字段，
  所以也不存在「字段被悄悄丢掉后签名照过」。这两条互为前提。

公钥 `catalog.TrustedKeys` 是**编译期固定**的（公钥不是秘密，写死才是重点——
它是信任的起点，不能从网上取）。默认为空：没有内置信任锚的构建会**拒绝一切**
远端目录，而不是无条件接受。下架走目录里的 `revoked`，只影响该版本及以前，
并且**不删用户的凭据**——下架往往只是接口暂时变了。

### `catalog try`：声明说的和接口做的是不是一回事

`-catalog-validate` 只看声明的形状，`-catalog-try` 回答的是另一半问题：
**接口真的这么返回吗**。它拿真实凭据跑一遍，逐条核对 `result.fields` 和
`result.meta` 里声明的路径在真实响应里到底存不存在。

不跑它的后果不是报错，而是**看起来成功**：取不到的路径会被静默丢掉，结果
「少几个字段」而没人发现。今天这个命令第一次真跑就抓到两件事：

- Jira 的 `/search/jql` **不接受无限定条件的 JQL**（`Unbounded JQL queries
  are not allowed`）。我早先的临时探测脚本把这个 400 读成了「项目里还没有
  事项」—— 工具把我自己看漏的东西翻出来了。
- `nextPageToken` 只在有下一页时出现，缺失是正常的。

它刻意区分三种情况，混成一类就会对正常数据报警，作者很快会学会无视诊断：

| 诊断 | 含义 | 要不要改声明 |
|---|---|---|
| `没有键 fields.priority` | 响应里根本没这个键 | 可能要改，也可能是该项目没启用这个字段 |
| `fields.assignee 是 null` | 字段在、值是空 | **不用改**，这是数据状态（没负责人） |
| `values（整个子树都没取到）` | `result.path` 指错了 | 要改 |

## OAuth「连接」按钮（设备流）

有些渠道不必让用户去对方系统手工生成一个长期令牌再粘过来：页面上点一下
「连接」，在对方网站上确认，令牌就进了本机的密码管理器。GitHub 渠道已经支持。

### 为什么是设备流，不是浏览器回调

桌面程序**存不住 `client_secret`** —— 它跟着 exe 发给每一个客户，不是秘密。
而 GitHub 的 web 流换 token 必须带 secret（[官方文档](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)
把它标为 Required，PKCE 的 `code_verifier` 是额外加固，不替代 secret）。

设备授权流（[RFC 8628](https://datatracker.ietf.org/doc/html/rfc8628)）只要
`client_id`：程序显示一个 8 位码，用户去对方网站输入，程序轮询直到授权完成。
它本来就是给没有浏览器的设备设计的，正好适合一个发给客户的 exe。

### 用起来的样子

```
页面 -> 渠道市场 -> GitHub 「添加」
  填 OAuth App Client ID（高级设置里）
  点「连接 GitHub」
    -> ① 复制验证码 WDJB-MJHT
    -> ② 打开 github.com/login/device 粘贴并确认
    -> ③ 等待授权…  ✅ 已连接为 nimysan，渠道已保存
```

账号名**不用用户填** —— 授权完成后向对方系统问一次身份就知道了。用户填的
名字和令牌实际属于谁可能不一致，而凭据是按账号名定位的，填错等于把令牌存到
另一个条目下。

### 要先注册一个 OAuth App

Client ID 只能由你自己注册的 App 提供（公共客户端的 client_id 不是秘密，
正式发布时应该把你自己 App 的 id 写进声明的 `oauth.clientId`）：

1. github.com -> Settings -> Developer settings -> OAuth Apps -> New
2. **勾上 Enable Device Flow** —— 不勾的话连接会以 `device_flow_disabled` 失败
3. 把 Client ID 填进页面的「OAuth App Client ID」

### 两条路并存

同一个凭据槽既能装用户手填的长期令牌（PAT），也能装连接拿到的令牌组，靠
`oauthdev` 的标记区分。保留手填是因为企业里两种都有人用 —— 有的组织根本不
允许给桌面程序注册 OAuth App。

### 声明怎么写

```jsonc
"auth": { "kind": "bearer" },
"oauth": {
  "flow": "device",
  "clientIdField": "oauthClientId",   // 或 "clientId": "Iv23li..." 写死
  "deviceCodeUrl": "https://github.com/login/device/code",
  "tokenUrl": "https://github.com/login/oauth/access_token",
  "scopes": ["repo", "read:user"],
  "setupUrl": "https://github.com/settings/developers"
}
```

校验规则里最要紧的一条和 `baseUrl` 同理：**授权服务地址不能由下发内容模板
拼出**，必须是写死的 https 地址。否则一份被篡改的目录就能把「请去这里授权」
指向攻击者，而用户在那一页输入的是自己真实账号的密码。页面上也把这件事摆出来：
点按钮前会显示「会把你送到 **github.com** 授权」和申请的权限。

### 轮询放在服务端

授权服务规定了最小轮询间隔，收到 `slow_down` 还要把间隔**加 5 秒**（不是原速
重试，那会一直撞限流）。这套节流只有一份实现才可靠，而且放在服务端的话，用户
切走页面再回来，授权照样在进行。设备码不下发到前端 —— 它等价于一次授权的凭据。

### 几件要知道的事

- **OAuth 的权限是粗粒度的。** `repo` 这个作用域包含写权限，所以令牌在 API
  层面是能改仓库的。hub 的写开关只决定「哪些工具注册给模型」，决定不了令牌
  本身能做什么 —— 要真正收窄，得在对方系统那侧用更细的权限（GitHub 的话是
  改用 GitHub App 的 fine-grained permissions）。
- **GitHub 的刷新走不通，而且不需要。** OAuth App 的刷新接口要求
  `client_secret`，公共客户端没有它。对应做法是不要开 App 的「过期令牌」、
  也不要申请 `offline_access`，让令牌长期有效，被吊销时重新点连接。刷新代码
  留着是给支持公共客户端刷新的提供方用的。
- **刷新成功但写不回密码管理器 = 致命错误**，不是记一笔然后继续。很多提供方
  刷新时会轮换 `refresh_token`，存不下来就意味着库里那个已作废，继续跑会变成
  「这次好了，之后全坏」。
- 连接失败不会留下半个渠道：凭据和实例都是拿到令牌、验证可用之后才写。

## wasm 插件渠道

声明式渠道能覆盖「HTTP 请求 + 裁剪响应」，但有些系统真的需要执行代码（多步握手、
算签名、带状态机）。那类 connector 走 wasm 插件：**代码由第三方编译成 `.wasm`，
作为数据下发，在客户机的 exe 里沙箱执行** —— 客户不装 Python、不装 Node、也不用
升级程序。

它已经接进 MCP 工具注册和配置页面，和邮件、声明式 HTTPS 并列成为第三种渠道类型。
`ghwasm` 是随仓库带的示范插件（`plugins/ghwasm/`，一个第三方风格手写的 GitHub
connector，不 import hub 的任何包）。

### 装一个插件

```bash
plugins/build.sh                                            # 构建到 plugins/dist/<id>/
mcp-hub -plugin-install plugins/dist/ghwasm/manifest.json   # 装进插件目录
mcp-hub -plugin-list                                        # 看装了什么、能访问哪些主机
```

插件装在 `<用户配置目录>/mcp-hub/plugins/<id>/`，一个插件一个目录，里面是
`manifest.json` 加它的 `.wasm`。**目录名必须等于插件 id** —— 否则「改个目录名」
就能换掉一个已配好渠道背后的代码，而用户配置是按 id 定位的。

装完之后它出现在页面的渠道市场里，填凭据、测连通、保存，和别的渠道完全一样。
工具要客户端重连一次才可见（Quick 还要点 Sync）。

### 沙箱是「只有这些」，不是「拦住了什么」

wasm 的指令集里没有系统调用，所以模块想碰外界只能调宿主导出的函数。一共三个：

| 宿主给的 | 说明 |
|---|---|
| `fetch(ptr,len) -> len` | 受限 HTTP。只允许 https、只允许 manifest 声明的主机、只允许常规方法 |
| `read(ptr)` | 取回上一次 fetch 的响应（分两步是为了不从宿主回调插件，重入会把 Go 运行时搅乱） |
| `log(ptr,len)` | 诊断输出，宿主决定要不要显示 |

没有文件、没有 socket、没有钥匙串、没有起进程。和原生 `.dll` 插件的区别是结构性的：
dll 一旦加载进进程就和宿主同权限，拦不住。

**凭据不交给插件。** manifest 里声明 `auth.kind`，宿主在 `fetch` 时注入请求头；
插件自己设的 `Authorization` 会被丢掉。所以第三方模块只能「以这个身份干活」，
拿不到也带不走 token。

**权威是 manifest，不是模块。** 工具清单、参数表、允许访问的主机都来自签名过的
manifest，所以风险披露在任何第三方代码跑起来之前就能摆给用户，模块也无法给自己
扩权。两处门禁都在插件执行前：**写开关**（关着的写工具根本不注册给模型），
以及**必填参数校验**（「参数够不够」不该由被审的那一方说）。

页面上这件事是看得见的：市场卡片带一个 `🧩 第三方代码` 标记和「沙箱只允许它访问
api.github.com」，填凭据的表单顶部还有一块橙框说明。

### 模块是懒加载的

发现（读 manifest）和加载（编译 wasm）刻意分开。工具注册只要 manifest，而编译
一个 3MB 的模块要上百毫秒到一秒 —— 卡在 stdio 握手上会被客户端判超时。模块在
**第一次工具调用**时才加载，之后在进程内复用。加载失败会被记住，不在每次调用时
重试编译。

### 这个示范插件验了什么

| 验的事 | 结果 |
|---|---|
| 6 个工具的结果、meta、请求路径、query、请求体 vs 声明式执行器 | 逐字节一致 |
| 对比测试本身会不会失败 | 变异验证过：插件少回一个字段 → 测试失败 |
| 真实 api.github.com | 通（令牌从钥匙串惰性取用，宿主注入） |
| 经真的 MCP 协议 tools/list + tools/call | 通（5 个只读工具，写工具被开关挡住；开关打开后 6 个） |
| MCP 启动时真实注册 | 通（1 个账号 5 个工具；2 个账号其一可写 → 6 个） |
| 多账号分派 | 不指定 `channel_account` 被拒并列出可选项；未开写开关的账号的写操作在执行前被拒 |
| 插件打未声明的主机 / 明文 http / `file://` / 非常规方法 | 沙箱拒绝并记录 |
| 插件自己塞 `Authorization` | 被丢掉，宿主注入的是最终值 |
| 装了个坏插件 | 只让自己失效，其余插件照常；目录名和 id 不一致被拒 |

```bash
go test ./internal/wasmplug/ ./internal/tools/        # 沙箱边界 + MCP 注册
MCPHUB_PLUGIN_REAL=1 GITHUB_ACCOUNT=<你的用户名> \
  go test ./internal/wasmplug/ -run Real -v           # 真实环境
```

### 要知道的代价

- **ABI 一旦发布就改不动**：已发布的模块是照这套函数签名编译的。所以 manifest 必须
  声明 `abi` 版本，宿主对不认识的版本干净地拒绝（`ABIVersion` 常量 + 加载期握手，
  模块报的版本和 manifest 不一致也拒绝加载）。
- **体积**：Go 编译出来 3.2MB（运行时固定开销）。Rust / TinyGo 会小一个数量级。
- **一个实例不能并发调用**（里面是一份 Go 运行时），所以 `Plugin.Call` 是串行的。
  要并发得起实例池，还没做。
- **超时会连带关掉模块实例**（`WithCloseOnContextDone`）：一个跑飞的第三方模块该被
  拆掉，但现在拆掉之后不会自动重建。
- **wasm 做不到的事仍然做不到**：WASI 没有原始 TCP、不能监听端口，所以 IMAP 那类
  协议、以及 OAuth 回调，都得由宿主以高层能力的形式提供，不是模块自己能解决的。
- **`-plugin-install` 不做签名验证**：手工装一个插件等于用户自己承担来源风险，和手工
  往 catalog 目录放一份声明同理。签名是目录下发那一层的事，还没做。
- **沙箱挡不住数据外流到已声明的主机**：那是它正常工作所必需的能力。所以「这个
  connector 会执行代码、要访问哪台主机」必须在用户点「添加」之前就摆出来。

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
- 远端目录只在显式 `-catalog-sync` 时抓取，不在 MCP 启动路径上自动刷新
  （stdio 握手卡在网络请求上会被客户端判超时）。自动定时刷新还没做。
- `-catalog-try` 默认只跑**只读**工具。写工具要 `-try-write`，而且需要
  `-try-tool` 单独指定并给参数 —— 一份声明的作者自测不该顺手在别人的系统里
  建单据。
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
| wasm 运行时（插件沙箱） | `github.com/tetratelabs/wazero`（纯 Go、零依赖、不用 CGO，交叉编译照常） |
