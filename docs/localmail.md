# 本机邮件（只读）

> 只读：没有任何写工具。默认关闭，要在配置页添加并勾选来源后才注册工具。

离线只读读取本机邮件客户端的本地缓存，不联网、不改动邮件。
mcp-hub 直接打开邮件客户端已经保存在这台电脑上的数据文件，把文件夹、邮件列表和正文交给 Amazon Quick。
不连邮件服务器，不需要密码，也不会往钥匙串或凭据管理器里写任何东西。

能读到的只有邮件客户端**已经同步到本机**的那部分邮件。服务器上有、本机没缓存的邮件读不到。

> **合规提示**：读取公司邮箱前，请先遵守所在组织的数据政策。
> 本功能读到的内容会交给你在 Amazon Quick 里使用的模型，和你手工把邮件粘贴进去是同一件事。

## 支持的来源

| 来源 ID | 客户端 | 平台 | 数据位置（默认） | 前提条件 | 已知限制 |
|---|---|---|---|---|---|
| `apple_mail` | Apple Mail（「邮件」App） | macOS | `~/Library/Mail`（`V<n>/MailData/Envelope Index` 加 `.emlx` 文件） | 给 Amazon Quick 授「完全磁盘访问权限」 | 正文只读本机已下载的部分；列表不带附件标记 |
| `outlook_legacy` | 旧版 Outlook for Mac | macOS | `~/Library/Group Containers/UBF8T346G9.Office/Outlook/Outlook 15 Profiles/Main Profile/Data`（`Outlook.sqlite`） | 一般不需要授权，个别系统版本也会要求完全磁盘访问权限 | 本机常常只缓存了摘要，没下载完整正文，这时 `body_note` 会说明 |
| `outlook` | 新版 Outlook for Mac | macOS | 同上的 `Main Profile` 目录（`HxStore.hxd`） | 同上 | 只缓存一部分邮件；已读状态和附件读不出，部分文件夹名和收件人可能缺失（见 `incomplete`）；格式没有公开文档，Outlook 升级后可能失效 |
| `outlook_classic_win` | 经典版 Outlook for Windows | Windows | 自动查找：Outlook 配置文件里登记的数据文件，再看 `%LOCALAPPDATA%\Microsoft\Outlook` 和「文档\Outlook Files」（`.ost` / `.pst`） | 读的时候经典版 Outlook 不能在运行 | Outlook 运行时会锁住数据文件，这时工具提示先完全退出 Outlook（任务管理器里也不能有 `OUTLOOK.EXE`）再试，来源不会被停用；只读本机已同步的部分 |
| `outlook_new_win` | 新版 Outlook for Windows（olk） | Windows | `%LOCALAPPDATA%\Microsoft\Olk\EBWebView\Default\IndexedDB` | 无；新版 Outlook 开着也能读 | 只缓存一部分邮件：离线设置里选中的文件夹、离线天数内（默认 30 天）的邮件；附件只有文件名和大小；格式没有公开文档，Outlook 升级后可能失效 |

每个来源都可以在配置页的「高级设置」里改数据目录。经典版 Outlook for Windows 可以填 `.pst` / `.ost` 文件或目录，多个用英文分号 `;` 隔开。

一台电脑只显示本平台的来源：Mac 上看不到 Windows 来源，反过来也一样。

### 格式没有公开文档的两个来源

新版 Outlook for Mac 的 `HxStore.hxd` 和新版 Outlook for Windows 的 IndexedDB 缓存都没有公开的格式文档，读取方式是按实际文件分析出来的。
Outlook 升级后格式可能变化：

- 打开时做格式自检，不达标就报 `ErrUnsupportedFormat`（「存储格式不支持」）。Mac 版看版本号和主题、发件人、时间的读取成功率；Windows 版看邮件所在的对象仓库在不在、记录能不能解开。
- mcp-hub 先重开重试一次；仍然失败就**停用这个来源**，工具返回一行说明，其他来源照常可用。
- 停用后每次调用会看一下数据文件的大小和修改时间（只 stat，不读内容）。文件变了（例如 Outlook 又写了一次）就自动解除停用、重新打开。

### `incomplete` 字段

有的来源取不到某些字段，就在这封邮件的 `incomplete` 里列出字段名，例如 `read`、`recipients`、`folder_name`。
**列在 `incomplete` 里的字段是零值，不代表真实状态**：比如 `read=false` 加上 `incomplete` 里有 `read`，意思是「不知道读没读过」，不是「未读」。
文件夹读不到真名时，`name_incomplete=true`，`path` 是生成的标签（如 `folder-3`）。

## 安装和启用

先按 [`使用说明.md`](使用说明.md) 装好 mcp-hub 并接入 Amazon Quick。

### macOS

1. 系统设置 → 隐私与安全性 → **完全磁盘访问权限** → 加入 **Amazon Quick** 并打开开关。读 Apple Mail 必须授权；两个 Outlook 来源的数据在 Group Containers 下，系统也可能要求授权。
   授权对象是真正拉起 MCP 服务的程序（Amazon Quick），不是配置页面。
2. 运行 `mcp-hub -ui`（或双击 mcp-hub）打开配置页 → 内置渠道 →「本机邮件（只读）」→ 勾选要用的来源 →「测试并保存」。
   - 提示「被 macOS 隐私保护拦下」：这是配置页这个进程自己的权限，不影响保存。
   - 提示「文件权限不足 / 不是 macOS 隐私保护」：数据目录的属主或权限不对，或者高级设置里填错了目录。开完全磁盘访问权限解决不了，要先修目录。
3. 在 Amazon Quick 里重启连接器：Customize → Connectors → 把 Mcp Hub 的开关关掉再打开，点刷新。刚授完完全磁盘访问权限时，要**完全退出 Amazon Quick 再打开**，新授权才生效。
4. 在 Quick 里问「列一下我本机邮件的文件夹」验证。没授权时工具会返回授权步骤，服务和其他来源照常可用。

Mac 第一次运行未签名的 mcp-hub 会被 Gatekeeper 拦下，处理方法见使用说明的「第二步：安装」。

### Windows

1. 安装或解压 mcp-hub。第一次运行可能弹 SmartScreen「已保护你的电脑」（程序没有代码签名），点「更多信息」→「仍要运行」。
2. 双击 mcp-hub（或运行 `mcp-hub -ui`）打开配置页 → 内置渠道 →「本机邮件（只读）」→ 勾选「经典版 Outlook for Windows」和 / 或「新版 Outlook for Windows」→「测试并保存」。
3. 用经典版来源时，读邮件前先完全退出经典版 Outlook。新版 Outlook 不用退出。
4. 在 Amazon Quick 里重启连接器：Customize → Connectors → 把 Mcp Hub 的开关关掉再打开，点刷新。
5. 在 Quick 里问「列一下我本机邮件的文件夹」验证。

mcp-hub 以别的用户身份运行时（例如以 SYSTEM 运行），默认目录指向的是那个用户的配置，要在高级设置里填实际用户的数据目录。

### 配置项

存在 `channels.json` 里，没有凭据：

| 字段 | 默认 | 说明 |
|---|---|---|
| `apple_mail`、`outlook_legacy`、`outlook`、`outlook_classic_win`、`outlook_new_win` | false | 来源开关，字段名就是 `source` 的取值 |
| `<来源 ID>_dir` | 空 | 自定义数据目录，空表示用默认位置 |
| `maxBodyBytes` | 32768 | 正文上限（字节），超出截断并标 `body_truncated` |
| `maxResults` | 50 | 每页上限，最大 200 |

多条本机邮件连接只用第一条，其余记一条日志。

## 工具

三个工具都标 `readOnlyHint=true`、`destructiveHint=false`、`openWorldHint=false`。
只启用一个来源时 `source` 可以不填；启用多个时 `localmail_search` 和 `localmail_get` 必须填，程序不替模型猜。

### `localmail_list_folders`

| 参数 | 说明 |
|---|---|
| `source` | 可选；不填列出全部已启用的来源 |

返回 `sources[]`，每项：

| 字段 | 说明 |
|---|---|
| `source` | 来源 ID |
| `error` | 这个来源打不开时的说明（其他来源不受影响） |
| `folders[]` | `id`、`path`、`account`（来源内部的账户标识）、`special`（inbox / sent / drafts…，认得时才有）、`total`（-1 表示没有统计）、`unread`、`name_incomplete`；新版 Outlook for Windows 另有 `server_total`（服务器上的总数，`total` 是本地缓存的封数） |

### `localmail_search`

| 参数 | 说明 |
|---|---|
| `source` | 来源 ID |
| `folder_id` | 只看这个文件夹，取 `localmail_list_folders` 的 `id`；不填为全部文件夹 |
| `from` | 发件人显示名或地址包含这段文字 |
| `subject` | 主题包含这段文字 |
| `since` | 起始日期（含），`YYYY-MM-DD`，按本机时区 |
| `before` | 截止日期（不含），`YYYY-MM-DD`，按本机时区 |
| `limit` | 最多返回几封，默认 20，不超过每页上限 |
| `offset` | 翻页，填上一次的 `next_offset` |

返回 `source`、`count`、`messages[]`、`has_more`、`next_offset`、`total`（来源能给出总数时才有）。
每封邮件：`id`、`folder_id`、`subject`、`from`（显示名，没有时是地址）、`from_address`、`to`、`date`（接收时间）、`read`、`has_attachment`、`preview`（不超过 200 字）、`incomplete`。

没有正文全文检索。同时启用多个来源时不能一次跨来源搜索，要逐个指定 `source`。

### `localmail_get`

| 参数 | 说明 |
|---|---|
| `source` | 来源 ID |
| `id` | 邮件 id，来自 `localmail_search` |

返回上面的每封字段，再加：`cc`、`message_id`、`body`（不超过正文上限）、`body_truncated`、`body_note`（正文不完整或读不出的原因）、`attachments[]`（`name`、`size`（-1 表示未知）、`content_type`，**不含附件内容**）。

## 安全设计

| 做法 | 说明 |
|---|---|
| 只读打开 | Apple Mail、旧版 Outlook 的 SQLite 用 `mode=ro` 加 `query_only` 打开；缺 `-wal` 或 `-shm` 时改用 `immutable=1`，只读主库。PST / OST 和 olk 的文件只用只读方式打开，从不写、不压实、不打开 LevelDB 的 `LOCK` |
| 共享模式 | Windows 上以最宽的共享模式打开（允许别人同时读、写、删除），不挡住 Outlook 写入和 olk 压实。Outlook 拒绝共享时报「数据文件正被占用」，等用户退出 Outlook 后再读。经典版来源空闲 30 秒后关闭全部文件句柄 |
| 不在邮件客户端目录里新建文件 | SQLite 只有 `-wal` 和 `-shm` 都已存在时才用 `mode=ro`，否则用 `immutable=1`，避免在客户端目录里新建这两个文件。新版 Outlook for Mac 在系统临时目录做 APFS 克隆再读，克隆失败就只读读原文件；不复制邮件目录 |
| 日志不记内容 | 日志只记来源 ID、操作名、错误类型和数量，不记主题、人名、地址和正文 |
| recover 兜底 | 所有对来源代码的调用都包了 recover。读取代码 panic 时转成一行工具错误，服务不退出，其他来源照常可用。日志只记 panic 类型和调用栈的函数名、文件名、行号；只有运行时错误（越界、空指针）才记它的说明，因为自定义 panic 值可能带邮件数据 |
| 来源停用和自动恢复 | 格式不支持或 panic 时先重开重试一次，仍然失败才停用这个来源。停用时等正在进行的调用结束才关闭连接。数据文件的大小或修改时间变了就自动恢复。文件被占用不算格式错误，不停用 |
| 惰性打开、失败不缓存 | 来源第一次被调用时才打开；没授权或文件被占用时不缓存失败，授权或退出 Outlook 后下一次调用就能读 |
| 返回限额 | 正文默认 32 KB，每页默认 50、最多 200，摘要 200 字，防止一次调用撑满上下文 |
| 遵守系统隐私保护 | macOS 的隐私保护（TCC）拦下时返回授权步骤，由用户在系统设置里授权 |

## 测试

所有命令在仓库根目录执行。e2e 用例默认测 `dist/mcp-hub`，所以**先 `make build`**。

### 单测和 fuzz

```
go vet ./...
GOOS=windows go vet ./...
go test -p 2 ./...
go test -race ./internal/localmail/...
```

`go test ./...` 里读真实数据的用例默认跳过，只跑合成夹具。Mac 专属的读取包在其他平台上整包跳过。

fuzz 一次跑一个目标：

```
go test -run '^$' -fuzz FuzzPST -fuzztime 60s ./internal/localmail/outlookwin/
go test -run '^$' -fuzz FuzzSnappy -fuzztime 60s ./internal/localmail/olkwin/
```

olkwin 还有 `FuzzValue`、`FuzzKey`、`FuzzLog`、`FuzzTable`。`testdata/fuzz/` 下的语料随 `go test` 当回归用例跑。

### e2e

`e2e/` 按 Amazon Quick 的方式拉起 `mcp-hub -serve`，走 MCP stdio。每个被测进程用独立的临时 HOME 和 TMPDIR，不读写真实的 mcp-hub 配置，也不碰钥匙串。用例不打印邮件内容。

| 环境变量 | 作用 |
|---|---|
| `MCPHUB_E2E_BIN` | 被测二进制，默认 `dist/mcp-hub`；不存在时跳过全部用例 |
| `MCPHUB_E2E_REAL=1` | 打开读真实本机邮件的用例（macOS） |
| `MCPHUB_E2E_APPLE_DIR` | Apple Mail 目录，默认 `~/Library/Mail` |
| `MCPHUB_E2E_LEGACY_DIR` | 旧版 Outlook 的 Data 目录，默认 Main Profile 下的 Data |
| `MCPHUB_E2E_OUTLOOK_DIR` | 新版 Outlook for Mac 的配置文件目录，默认 Main Profile |
| `MCPHUB_E2E_BASELINE_BIN` | main 分支构建的二进制；设置后跑 7 个现有渠道的 `tools/list` 回归对比，要求工具一个不变 |
| `MCPHUB_E2E_PST_DIR` | （Windows）放 `.pst` / `.ost` 样本的目录，跑经典版的用例 |
| `MCPHUB_E2E_OLK_DIR` | （Windows）新版 Outlook 的 IndexedDB 目录，跑新版和经典版同时启用的用例 |

读取包自己的真机测试：

| 环境变量 | 包 | 作用 |
|---|---|---|
| `APPLEMAIL_REAL=1` | `applemail` | 读真实 Apple Mail，只打印数量和日期 |
| `OUTLOOK_LEGACY_REAL=1`、`OUTLOOK_LEGACY_DATA`、`OUTLOOK_LEGACY_SAMPLE` | `outlooklegacy` | 读真实旧版 Outlook；指定目录和抽样封数 |
| `HXSTORE_REAL=1`、`HXSTORE_DIR`、`HXSTORE_SNAPDIR`、`HXSTORE_SAMPLE` | `hxstore` | 读真实新版 Outlook for Mac；指定目录、快照目录和抽样封数 |
| `OLKWIN_REAL=1`、`OLKWIN_DIR` | `olkwin` | 读真实新版 Outlook for Windows 缓存 |
| `OLKWIN_DUMP`（及 `_N`、`_STORE`、`_MATCH`） | `olkwin` | 打印 IndexedDB 的库、仓库和字段结构，用于分析格式变化；不打印正文 |
| `OUTLOOKWIN_SAMPLES` | `outlookwin` | 放公开 PST / OST 样本的目录，统计字段抽取率并核对文件没被改动 |

`olkwin` 的 `TestReal` 和 `e2e/olkwin_test.go` 会断言测试邮箱里两封自造的测试邮件（主题以 `mcp-hub-test` 开头），在别的邮箱上跑会失败，只适合作者的测试环境。

配置页另有 `node e2e/webui-save.mjs <mcp-hub> <chromium>`，用无头 Chromium 点配置页，检查两个标签页保存时不互相覆盖。

### 在自己的机器上验证

1. `make build`，然后 `go test ./e2e`，确认默认用例通过。
2. macOS：给运行测试的终端授完全磁盘访问权限，跑 `MCPHUB_E2E_REAL=1 go test -count=1 -v -run Real ./e2e`。
3. Windows：`set MCPHUB_E2E_OLK_DIR=<IndexedDB 目录>` 后跑 `go test -count=1 -v -run Windows ./e2e`；有 PST / OST 样本时再设 `MCPHUB_E2E_PST_DIR`。
4. 跑前跑后各记一次邮件客户端数据目录的文件清单、大小和修改时间，确认没有变化，也没有多出文件。
5. 用临时 HOME 写一份只有本机邮件的 `channels.json`，拉起 `mcp-hub -serve`，调三个工具，确认返回字段。

CI 里只跑合成数据：GitHub Actions 的 runner 上没有邮件客户端数据，真机用例都会跳过。

## 验证结果摘要

| 环境 | 结果 |
|---|---|
| macOS 真机，Apple Mail / 旧版 Outlook / 新版 Outlook for Mac | 三个来源的列文件夹、过滤、翻页、读信都通过；并发和来源隔离用例通过；`-race` 构建的服务端无数据竞争；跑完后邮件客户端数据目录的文件清单、大小和修改时间不变 |
| Windows Server 2025 真机，新版 Outlook 运行中 | 读到离线缓存里的 275 封邮件（4 个文件夹），列文件夹、搜索、取信、附件元数据通过；读完后 olk 目录的数据文件没有变化 |
| Windows，经典版 Outlook | 用 java-libpst 的公开 PST / OST 样本验证；用另一个进程独占锁住文件，确认工具提示先退出 Outlook、来源不停用、句柄按时释放。没有在装着经典版 Outlook 的真机上测 |
| Windows 独立验收 | 3 轮 stdio e2e 共 142 项断言，141 项通过，失败的 1 项是测试脚本本身的问题。验收中发现的 3 个解析问题（损坏的缓存或 PST 让进程 panic 或卡死）已修复并补了 fuzz 和回归用例 |
| 回归 | 现有 7 个渠道的工具列表和 main 完全一致（写开关关时 50 个、开时 72 个）；加上本机邮件正好多出 3 个 `localmail_*` 工具 |

## 第三方资料和许可证

| 资料 | 用途 | 许可 / 说明 |
|---|---|---|
| [[MS-PST]: Outlook Personal Folders (.pst) File Format](https://learn.microsoft.com/en-us/openspecs/office_file_formats/ms-pst/141923d5-15ab-4ef1-a524-6dce75aae546) | 经典版 Outlook for Windows 读取包的实现依据，包括 5.1 节的置换表 | Microsoft Open Specifications，按开放规范承诺使用 |
| [java-libpst](https://github.com/rjohnsondev/java-libpst) 的公开测试样本（如 `dist-list.pst`、`passworded.pst`、`example-2013.ost`） | 只在本地测试时读取 | **没有提交进仓库**，用 `OUTLOOKWIN_SAMPLES` / `MCPHUB_E2E_PST_DIR` 指向自己下载的目录 |

新增的 Go 依赖（版本以 `go.mod` 为准，都会编译进程序）：

| 组件 | 版本 | 许可证 | 用途 |
|---|---|---|---|
| modernc.org/sqlite | v1.62.0 | BSD-3-Clause（内含的 SQLite 源码为 public domain） | 纯 Go 读 Apple Mail 和旧版 Outlook 的 SQLite |
| modernc.org/libc | v1.80.1 | BSD-3-Clause（随附 musl 等上游声明，见其 `LICENSE-3RD-PARTY.md`） | sqlite 的运行时 |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause | sqlite 间接依赖 |
| modernc.org/memory | v1.12.1 | BSD-3-Clause | sqlite 间接依赖 |
| github.com/pierrec/lz4/v4 | v4.1.33 | BSD-3-Clause | 解压新版 Outlook for Mac 的数据块 |
| github.com/dustin/go-humanize | v1.1.0 | MIT | sqlite 间接依赖 |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | sqlite 间接依赖 |
| github.com/mattn/go-isatty | v0.0.24 | MIT | sqlite 间接依赖 |
| github.com/ncruces/go-strftime | v1.1.0 | MIT | sqlite 间接依赖 |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause | sqlite 间接依赖 |
| golang.org/x/text | v0.43.0 | BSD-3-Clause | 字符集解码（原为间接依赖，改为直接依赖） |

LevelDB、Snappy、IndexedDB 键编码、V8 反序列化（新版 Outlook for Windows）和 LZFu（PST 的 RTF 压缩正文）都是本仓库自己写的只读实现，没有引入依赖。
完整的第三方组件清单见 [`THIRD-PARTY-NOTICES.md`](../THIRD-PARTY-NOTICES.md)。
