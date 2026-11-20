# mcp-hub

<img src="packaging/logo.png" width="96" alt="MCP Hub">

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg?style=flat-square)](LICENSE)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey.svg?style=flat-square)
<!-- ALL-CONTRIBUTORS-BADGE:START - Do not remove or modify this section -->
[![All Contributors](https://img.shields.io/badge/all_contributors-2-orange.svg?style=flat-square)](#contributors-)
<!-- ALL-CONTRIBUTORS-BADGE:END -->

**让 [Amazon Quick](https://aws.amazon.com/cn/quick/desktop/) 用上企业现有系统，不需要先做一个整合项目。**

装一个程序，填一次账号，Quick 里就多出一批功能：公司邮箱、金蝶云之家、飞书、QQ、GitHub、Jira。

单个可执行文件，**不需要 Node、Python 或任何运行时**；双击打开就是配置页面，没有命令行。
Claude Desktop、Cursor、Cline、Kiro 这类读 MCP 配置的客户端同样能用。

---

## 它解决什么问题

Quick 要看得到企业数据才能办事。标准路径是一个对接项目：IT 立项 → 注册应用 →
开放网络 → 安全评审。而国内的 HR、财务、OA 多是私有化部署，业务部门推不动。

mcp-hub 换个位置接入 —— 连接点放在员工自己的电脑上：

- **不需要 IT 立项**
- **不需要注册应用**（飞书那类扫码代办）
- **不需要给内网开公网入口**
- **不需要把密码交给云端**

---

## 集成架构

![集成架构：Amazon Quick Desktop 与 mcp-hub 同在员工电脑这个信任边界内，两者走本机通道不过网络；mcp-hub 以员工本人的账号访问企业系统（内网 OA、财务、人事或 SaaS）](docs/images/architecture.png)

| 设计 | 带来什么 |
|---|---|
| 连接器跟 Quick 同在本机 | 不过网络，天然在内网里，不用开公网入口 |
| 用员工本人的账号 | 权限等于这个人的权限，不需要管理员授权 |
| 一个连接器汇总所有系统 | Quick 侧只配一次，加系统不用再动 Quick |

**没有新增数据外发面** —— 员工本来就能把邮件粘进 Quick，这里只是省掉粘贴。

---

## 支持的渠道

| 渠道 | 接入方式 | 只读 | 需开写开关 |
|---|---|---|---|
| 金蝶云之家 | 扫码授权 | 11 项（消息、审批、日程、通讯录） | 同意审批、发群消息、发私聊 |
| 飞书（国内版） | 扫码建应用 + 扫码授权 | 18 项（消息、日程、任务、文档、多维表格） | 10 项（发消息、建日程、建任务、改记录……） |
| 企业邮箱 | 邮箱地址 + 密码 | 3 项（最近邮件、搜索、读正文） | 发送邮件 |
| QQ 邮箱 | 邮箱地址 + 授权码 | 同上 | 发送邮件 |
| QQ（本机 NapCat） | 自动配置 | 5 项（群、好友、聊天记录） | 发群消息、发私聊 |
| GitHub | 授权登录或个人令牌 | 5 项（仓库、Issue、搜索） | 新建 Issue |
| Jira Cloud | 站点 + 邮箱 + API Token | 8 项（项目、JQL、详情、评论） | 建/改/指派/流转/评论 |

新增渠道不需要改程序，所以一线遇到的系统可以做成渠道合进主干，所有客户共享。

---

## 开始用

一个安装包，不要管理员权限。双击 → 在「内置渠道」里点渠道 → 扫码或填账号 →
点「测试并保存」→ 把右侧那段配置粘进 [Amazon Quick Desktop](https://aws.amazon.com/cn/quick/desktop/)。

![MCP Hub 配置页面：上方是三步进度，左边「我的连接」是已配好的账号，下面是「内置渠道」，右边是「接入 Amazon Quick」](docs/images/config-mcp-done.png)

下载地址和详细步骤见 [`docs/使用说明.md`](docs/使用说明.md)，
[Word 版](docs/MCP-Hub-使用说明.docx)可以直接发客户。

---

## 凭据怎么保存

- **密码只交给操作系统** —— Windows 凭据管理器、macOS 钥匙串、Linux Secret Service。
  不进配置文件、不进日志、不上传。
- **AI 拿不到密码** —— 模型只能让程序以使用者本人的身份办事。
- **配置文件里没有秘密** —— 只记渠道、账号、服务器地址和开关，可以直接发给别人排查。
- **卸载不删密码** —— 要清除，去系统凭据管理器删 `mcp-hub` 开头的条目。
- **没有遥测、没有数据回传、没有我方服务端。**
- **不代为下载第三方程序** —— QQ 渠道依赖的 NapCat 由使用者自行安装。

**写操作默认关闭。** 发邮件、同意审批、发消息这类不可逆动作默认不交给模型，
必须逐个渠道、逐个账号手动打开开关。关着的功能模型看不到。

---

## 适用边界

适合业务部门试点、系统在内网、一人一机各用自己的账号。

不适合需要集中授权和统一审计的场景 —— 它没有管理端。

---

## 合作模式

开分支或 Fork → 提 PR 到 `main` → 三平台测试通过后由维护者合并。

新增渠道的 PR 要写清哪些算写操作，并附一个真实账号跑通的证据 ——
写操作标错等于默认把不可逆动作交给模型。

`main` 已开分支保护：必须走 PR、CI 必须绿、禁止 force push。

---

## 贡献与致谢

**百盈云创** —— 贡献了**飞书渠道**（[PR #27](https://github.com/nimysan/quick-mcp-hub/pull/27)）。
百盈集团旗下数字科技企业，AWS 亚马逊云科技 GSPP 全球解决方案提供商、
大湾区亚马逊云科技行业数字化赋能中心（简称：大湾区 IDAC）运营单位，
聚焦企业上云与全球化出海服务。

### Contributors ✨

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tbody>
    <tr>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/nimysan"><img src="https://avatars.githubusercontent.com/u/55108?v=4&size=100" width="100px" height="100px" alt="Sean Ye"/><br /><sub><b>Sean Ye</b></sub></a><br /><a href="https://github.com/nimysan/quick-mcp-hub/commits?author=nimysan" title="Code">💻</a> <a href="https://github.com/nimysan/quick-mcp-hub/commits?author=nimysan" title="Documentation">📖</a> <a href="#infra-nimysan" title="Infrastructure (Hosting, Build-Tools, etc)">🚇</a></td>
      <td align="center" valign="top" width="14.28%"><a href="https://github.com/Jeremyforwork"><img src="https://avatars.githubusercontent.com/u/143385324?v=4&size=100" width="100px" height="100px" alt="Jeremy Zhong"/><br /><sub><b>Jeremy Zhong</b></sub></a><br /><a href="https://github.com/nimysan/quick-mcp-hub/commits?author=Jeremyforwork" title="Code">💻</a></td>
    </tr>
  </tbody>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<!-- ALL-CONTRIBUTORS-LIST:END -->

加贡献者：在任意 issue 或 PR 里评论 `@all-contributors please add @用户名 for code`
（[图标含义](https://allcontributors.org/docs/en/emoji-key)）。

---

## 许可与免责

本项目依据 **Apache License, Version 2.0** 发布，全文见 [`LICENSE`](LICENSE)。

> 本软件按「**现状**」（AS IS）提供，不附带任何形式的明示或默示担保，包括但不
> 限于对适销性、特定用途适用性及不侵权的担保。在任何情况下，版权持有人及贡献
> 者均不对因使用本软件而产生的任何索赔、损害或其他责任承担义务。
>
> 完整条款见 LICENSE 第 7 条（Disclaimer of Warranty）与第 8 条
> （Limitation of Liability）。

使用者自行承担的部分：

- **账号与数据属于使用者自己。** 本软件以使用者本人的身份访问其自行配置的系统，
  由此产生的一切操作与后果（含误发邮件、误批单据、误发消息）由使用者负责。
- **写开关一旦打开，模型即可执行不可逆动作**，是否打开由使用者决定。
- **第三方系统的接口、条款和可用性不受本项目控制**，随时可能变化。
- **使用第三方系统须遵守其各自的服务条款。** QQ 渠道依赖的 NapCat 使用非官方协议，
  账号风险落在使用者登录的那个 QQ 号上；该组件不由本项目提供。
- **本软件未做代码签名**，首次运行会被 Windows SmartScreen 或 macOS Gatekeeper 拦下。

第三方组件的许可与归属声明见 [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md)
和 [`NOTICE`](NOTICE)。所有依赖均为宽松型许可，不含传染性条款。

---

## 文档

| 看什么 | 去哪 |
|---|---|
| 下载、安装、配置、接到 Quick | [`docs/使用说明.md`](docs/使用说明.md) · [Word 版](docs/MCP-Hub-使用说明.docx) |
| 发布包里给客户的那份 | [`packaging/快速开始.md`](packaging/快速开始.md) |
| 怎么构建、为什么这样实现 | [`docs/实现笔记.md`](docs/实现笔记.md) |
| 第三方开源组件清单 | [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md) |
