# qqmail-mcp

单文件可执行的 MCP 服务器，用 IMAP/SMTP 访问 QQ 邮箱（及 163 / 126 / Gmail / Outlook），
供 Amazon Quick、Claude Desktop 等 MCP 客户端调用。

目标机**不需要 Node、不需要 Python、不需要 npm**：一个二进制，配两个环境变量。

## 为什么是 Go

同类的 Node 实现（`mcp-email`）在 stdio 下会把启动日志打到 stdout，
而 **stdio 传输的 stdout 就是 JSON-RPC 协议通道**，一行非 JSON 就让客户端握手失败
（Amazon Quick 报 `handshake_failed`）。Go 标准库 `log` 默认写 stderr，
往 stdout 输出必须显式 `os.Stdout`，所以这类 bug 不会因为手滑发生。
本程序唯一往 stdout 打人类可读文本的是 `-selftest`，而它不跑协议。

## 工具

| tool | 只读 | 说明 |
|---|---|---|
| `list_recent` | 是 | 最近 N 封的信头摘要（UID/发件人/主题/时间/大小/未读），不含正文 |
| `search_mail` | 是 | 按发件人、关键词、日期范围、未读状态检索，条件之间是「且」 |
| `read_mail` | 是 | 按 UID 取纯文本正文 + 附件文件名。用 `BODY.PEEK` 读，不标记已读 |
| `send_mail` | 否 | 发信。**默认不注册**，需 `QQMAIL_ALLOW_SEND=true` 才启用 |

三个只读 tool 都带 `readOnlyHint`，客户端据此区分查询和动作
（Quick 的日志里会显示 `read=3 write=0` 而不是把全部归为 `unknown`）。

## 配置

| 环境变量 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `QQMAIL_USER` | 是 | — | 完整邮箱地址，如 `12345@qq.com` |
| `QQMAIL_PASSWORD` | 是 | — | **16 位授权码**，不是登录密码 |
| `QQMAIL_ALLOW_SEND` | 否 | `false` | 置 `true` 才注册 `send_mail` |
| `QQMAIL_IMAP_HOST` / `QQMAIL_IMAP_PORT` | 否 | 按域名推断 | 覆盖 IMAP 接入点 |
| `QQMAIL_SMTP_HOST` / `QQMAIL_SMTP_PORT` | 否 | 按域名推断 | 覆盖 SMTP 接入点 |
| `QQMAIL_MAX_BODY_BYTES` | 否 | `32768` | `read_mail` 正文上限，防止一封大信撑爆上下文 |
| `QQMAIL_TIMEOUT_SECONDS` | 否 | `30` | 连接与读写超时 |

兼容读取 `EMAIL_USER` / `EMAIL_PASSWORD` / `EMAIL_IMAP_*` / `EMAIL_SMTP_*`，
方便从 `mcp-email` 的配置平移过来。

**SMTP 默认用 465 直连 SSL**，不是腾讯文档里也列着的 587。587 走 STARTTLS，
在部分网络出口上不可达（实测过一次 IPv4 直连超时、IPv6 无路由），465 更稳。
要用 587 就显式设 `QQMAIL_SMTP_PORT=587`。

QQ 邮箱的授权码在：邮箱设置 → 账户 → 开启 IMAP/SMTP 服务 → 生成授权码。

## 构建

```bash
make build                 # 当前平台
make all                   # windows/darwin/linux 全套到 dist/
QQMAIL_USER=... QQMAIL_PASSWORD=... make selftest
```

`proxy.golang.org` 在部分网络不可达，Makefile 默认用 `https://goproxy.cn,direct`。
直接跑 `go build` 时自己带上：

```bash
GOPROXY=https://goproxy.cn,direct go build ./...
```

## 验证连通性（不发信）

```bash
export QQMAIL_USER='12345@qq.com'
export QQMAIL_PASSWORD='你的16位授权码'
./dist/qqmail-mcp -selftest
```

输出会分别报 IMAP 登录和 SMTP 认证的结果，全通返回 0。

## 接入 Amazon Quick（本地 connector）

Desktop App → Customize → Connectors → Create local connector → Local：

| 字段 | 值 |
|---|---|
| Command | 二进制的绝对路径，如 `C:\Tools\qqmail-mcp.exe` |
| Arguments | 留空 |
| Environment variables | `QQMAIL_USER`、`QQMAIL_PASSWORD` |

或用 Import 导入配置文件：

```json
{
  "mcpServers": {
    "qqmail": {
      "command": "C:\\Tools\\qqmail-mcp.exe",
      "args": [],
      "env": {
        "QQMAIL_USER": "12345@qq.com",
        "QQMAIL_PASSWORD": "你的16位授权码"
      }
    }
  }
}
```

Windows 上**不需要** `cmd /c` 包一层 —— 那是 `npx` 这类 `.cmd` 垫片才需要的，
这里 Command 直接就是一个 PE 可执行文件。

Quick 的代码沙箱没有公网出口，所有对外访问都必须走 connector；
本程序是独立 OS 进程，不在那个沙箱里，所以 IMAP/SMTP 直连可用。

## 已知限制

- 只处理 `text/plain`（没有就退回 `text/html` 原文，不做 HTML 转纯文本）。
- 附件只列文件名，不下载内容。
- 未做代码签名，首次在 Windows 上运行可能被 SmartScreen 拦（本地编译本地拷贝通常无感）。
- Windows 控制台代码页可能让 stderr 上的中文日志显示成乱码，不影响功能。

## 依赖

| 用途 | 库 |
|---|---|
| MCP 协议 | `github.com/modelcontextprotocol/go-sdk` |
| IMAP | `github.com/emersion/go-imap/v2` |
| MIME / 字符集 | `github.com/emersion/go-message`（导入 `charset` 即注册 GB2312/GBK/GB18030 等） |
| SMTP | `github.com/wneessen/go-mail` |
