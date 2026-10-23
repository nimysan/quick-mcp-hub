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

## 接入 Amazon Quick（本地 connector）

Desktop App → Customize → Connectors → Create local connector → Local：

| 字段 | 值 |
|---|---|
| Command | 二进制的绝对路径，如 `C:\Tools\mcp-hub.exe` |
| Arguments | 留空 |
| Environment variables | `MCPHUB_MAIL_USER`（授权码走密码管理器，不填这里） |

或用 Import 导入配置文件：

```json
{
  "mcpServers": {
    "channels": {
      "command": "C:\\Tools\\mcp-hub.exe",
      "args": [],
      "env": {
        "MCPHUB_MAIL_USER": "12345@qq.com"
      }
    }
  }
}
```

Windows 上**不需要** `cmd /c` 包一层 —— 那是 `npx` 这类 `.cmd` 垫片才需要的，
这里 Command 直接就是一个 PE 可执行文件。

Quick 的代码沙箱没有公网出口，所有对外访问都必须走 connector；
本程序是独立 OS 进程，不在那个沙箱里，所以 IMAP/SMTP 直连可用。

## 声明式 HTTPS 渠道

一份 JSON 就是一个渠道。`internal/httpchan/catalog/github.json` 是第一个，
`internal/httpchan/spec.go` 是完整的 schema。骨架：

```jsonc
{
  "type": "github",                       // 渠道 id，工具名必须以 github_ 开头
  "integration": "github",                // 凭据在密码管理器里的分段
  "baseUrl": "https://api.github.com",    // 固定接入点；自建系统改用 baseUrlField
  "account": { "name": "login", "label": "GitHub 用户名" },
  "secret":  { "name": "token", "label": "个人访问令牌 (PAT)" },
  "auth":    { "kind": "bearer" },        // bearer | header | basic | query
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

## 已知限制

- 只处理 `text/plain`（没有就退回 `text/html` 原文，不做 HTML 转纯文本）。
- 附件只列文件名，不下载内容。
- 未做代码签名，首次在 Windows 上运行可能被 SmartScreen 拦（本地编译本地拷贝通常无感）。
- Windows 控制台代码页可能让 stderr 上的中文日志显示成乱码，不影响功能。
- 渠道目录只有内置一层（`go:embed`）。远端下发 + 验签、用户私有目录这两层还没做，
  但 `Parse` / `Validate` 已经是三层共用的入口，设计见 `docs/server-design.md`。
- 邮件渠道仍从环境变量读配置，页面里保存的邮件渠道还没接到 MCP 注册链路上，
  也还没有多账号分派；声明式 HTTPS 渠道两件都已具备。
- 客户端不会自动刷新工具列表：在页面里新增渠道后，Amazon Quick 那边要手动点一次 Sync。

## 依赖

| 用途 | 库 |
|---|---|
| MCP 协议 | `github.com/modelcontextprotocol/go-sdk` |
| 系统密码管理器 | `github.com/zalando/go-keyring` |
| IMAP | `github.com/emersion/go-imap/v2` |
| MIME / 字符集 | `github.com/emersion/go-message`（导入 `charset` 即注册 GB2312/GBK/GB18030 等） |
| SMTP | `github.com/wneessen/go-mail` |
