# 渠道目录服务器（设计）

客户机上只有 `mcp-hub` 一个二进制。服务器只做一件事：**把 connector 声明
（数据）签名后分发出去**。

## 最硬的一条约束

**服务器永不存储客户的 connector 账号和凭据。** 那些只存在于客户机的操作系统
密码管理器里，连「客户启用了哪些渠道、用的什么账号」都不上报。

由此推出的设计后果，每一条都是可检查的：

- 数据库里**没有**这些表：客户账号、凭据、渠道实例、客户使用记录。
- 客户端到服务器只有 **GET**，没有任何上报接口。
- 「测试连通」只在客户端跑：令牌从钥匙串出来，直接打客户自己的系统，
  不经过本服务器。服务器没有任何代理客户流量的路径。
- 连 connector 作者调试自己的声明（`try`）也放在客户端 CLI，不做成服务端接口
  —— 做成接口就等于服务器上出现了一条「接收令牌」的代码路径，哪怕只在内存里。
- 遥测如果以后要做，只能是匿名计数（某个 connector 被下载了多少次），
  不带机器标识、不带账号。

## 形状

```
  发布侧（写，低频）                     分发侧（读，高频）
  ┌──────────────────────┐             ┌───────────────────┐
  │ Next.js 控制台       │  publish    │ S3 + CloudFront   │
  │ Cloudscape UI        │ ──────────► │ catalog.json(签名)│
  │ 作者/审核/审计       │             │ keys.json         │
  └──────────┬───────────┘             └─────────┬─────────┘
             │ 校验用同一个二进制                │ 只读 GET，带 ETag
             ▼                                   ▼
     mcp-hub catalog validate            客户机 mcp-hub.exe
     （与客户端完全同一份规则）          验签 → 本地缓存 → 页面列出渠道
                                         用户填凭据 → 系统密码管理器
```

关键是**读写分离**：客户端运行时只读 CloudFront 上的静态对象，不连 Next.js。
控制台宕了、数据库挂了，没有任何客户受影响；目录是一个签过名的静态文件。

## 数据模型

Postgres + Prisma（都是写侧的低频数据，不需要 DynamoDB 的扩展性）。

| 表 | 内容 |
|---|---|
| `connector` | type id、显示名、图标、integration、归属团队、可见性（public/private）|
| `connector_version` | connector_id、版本号、`spec_json`、changelog、状态（draft/published/deprecated/yanked）、发布人、发布时间、spec 的 sha256 |
| `release_channel` | stable / beta 各指向哪个版本 |
| `entitlement` | 私有 connector 对哪些许可证 key 可见（可选，P2 才要）|
| `publisher` | 账号、角色（viewer / author / publisher）|
| `audit_log` | 谁在什么时候发布/下架了哪个版本的哪个 hash |

版本是**不可变**的：改声明就是发新版本，published 之后 `spec_json` 不再可改。
出问题走 `yanked`，不走原地修改——客户端缓存里已经有那份内容了，原地改等于
两台机器跑着不同的「v3」。

**渠道 id 不可改**：客户机上的 `channels.json` 和密码管理器条目都按 id 定位
（`mcp-hub:<integration>` + 账号），改 id 等于让用户已配好的凭据失联。所以
id 要一次命名对——按**具体那套接口**命名，不是按厂商（`k3cloud` 不是
`kingdee`，金蝶有七八条产品线、接口彼此不通）。真要改名，目录里得带一张
`renamedFrom` 映射、客户端据它迁移配置和凭据，这件事现在还没做。

## 接口

**分发侧**（静态对象，无鉴权，可被 CDN 缓存）

```
GET /catalog/stable.json      签名后的完整目录
GET /catalog/keys.json        验签公钥（含 kid，支持轮换）
```

`stable.json` 的形状：

```jsonc
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-23T05:00:00Z",
  "connectors": [ { "type": "github", "version": 3, "spec": { ... } } ],
  "revoked":    [ { "type": "k3cloud", "version": 2, "reason": "接口变更" } ],
  "signature":  { "kid": "2026-10", "alg": "ed25519", "sig": "base64…" }
}
```

签名签的是**规范化序列化**（键排序、无多余空白）后的 `connectors + revoked +
generatedAt`，不是 HTTP 响应体——否则换个 CDN 压缩方式签名就失效了。

**控制台侧**（Next.js Route Handlers，需登录）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/connectors` | 建 connector |
| POST | `/api/connectors/:type/versions` | 存一个 draft 版本 |
| POST | `/api/validate` | 校验一份 spec，返回逐条错误 |
| POST | `/api/connectors/:type/versions/:v/publish` | 发布：校验 → 签名 → 写 S3 → 失效 CDN → 记审计 |
| POST | `/api/connectors/:type/versions/:v/yank` | 下架，进 `revoked` |

## 校验必须只有一份规则

两套校验（Go 一套、TypeScript 一套）一定会漂移，而漂移的后果是服务器放行了
客户端装不上的声明。所以 publish 的校验闸门**直接调 Go 二进制**：

```
mcp-hub catalog validate --stdin   # 退出码 + JSON 格式的逐条错误
```

Next.js 容器里带一个 linux/amd64 的 `mcp-hub`，`/api/validate` 就是
`spawn` 它、喂 stdin、读 stdout。规则是字面上的同一份代码（`Spec.Validate`），
不存在漂移。TS 侧只做表单级的轻校验（必填、JSON 能不能解析）。

这需要客户端加两个子命令，顺带也给 connector 作者用：

```
mcp-hub catalog validate ./github.json    # schema + 安全底线
mcp-hub catalog try ./github.json         # 真起一次，列出工具和测试连通
```

## 客户端要加的东西

一个 `internal/catalog` 包，三层来源，优先级 本地 > 远端 > 内置：

1. `go:embed`（现在就有的那层，离线可用）
2. 远端缓存：`~/.../mcp-hub/catalog/remote.json`，启动时异步刷新一次（带 ETag，
   没变就是 304）；拉不到或验签失败 → 用旧缓存并在页面上显出黄条，**绝不**
   因为目录拉不到就让已配好的渠道失效
3. 用户私有目录：`~/.../mcp-hub/catalog/*.json`，给自建系统和作者本地调试

两条升级规则，都要落成代码：

- **已启用渠道的敏感字段变了要重新确认**：新版本改了 `baseUrl`、`auth`、
  密码字段的含义，不能静默生效——那等于悄悄把用户凭据指向另一个地址。
  页面上标「需确认」，用户点过才用新版本。
- **被 yank 的渠道停用并说明原因**，但不删用户的凭据（可能只是接口暂时变了）。

## 控制台界面（Cloudscape）

| 页面 | 主要组件 |
|---|---|
| `/connectors` | `Table` + `TextFilter` + `Pagination` + `Badge`（状态/可见性）|
| `/connectors/new` | `Wizard` 三步：基本信息 → 认证与字段 → 工具 |
| `/connectors/[type]` | `Tabs`：概览 / 版本 / 工具 / 审计 |
| 版本编辑 | `CodeEditor` 编 spec JSON，校验结果走 `Flashbar` |
| 发布 | `Modal` 二次确认 + 带写入类工具时要求第二人批准 |

骨架是 `AppLayout` + `SideNavigation` + `BreadcrumbGroup`。

**先花半天验一件事再动工**：Cloudscape 是纯客户端 React 组件库，在 App Router
下每个用到它的文件都要 `"use client"`，而 `AppLayout` 和 Next.js 的嵌套
layout/路由配合得不好（官方 discussion #1112 讨论过，2023 年的结论是
避开 `AppLayout` 或不用 App Router）。两条退路：把 `AppLayout` 放在一个
单独的客户端 shell 里、路由在 shell 内部走 `next/link`；或者控制台直接做成
Vite SPA，Next.js 只留 API。这一步不先验，后面会在布局上反复返工。

## 权限

控制台用 NextAuth 接你们的 IdP。三个角色：viewer 看、author 改 draft、
publisher 能发布。**带写入类工具的 connector 发布需要第二人批准**——那份声明
一旦下发，客户机上就可能注册出能改对方系统的工具。

## 分期

| 期 | 内容 | 产出 |
|---|---|---|
| P0 | 客户端 `internal/catalog` + `catalog validate/try` 子命令；目录是手工签名后 `aws s3 cp` 上去的静态文件 | **下发这件事已经成立**，不需要任何服务器代码 |
| P1 | Next.js + Cloudscape 控制台，publish 自动签名写 S3 | 非工程师也能加 connector |
| P2 | 私有 connector 的 entitlement、审批流、审计查询 | 对外开放给第三方作者 |

P0 就能让「远端加一个 connector、客户不升级就能用」跑通。先做 P0，控制台
是在它之上的便利层，不是前置条件。
