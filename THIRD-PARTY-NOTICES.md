# 第三方开源组件

mcp-hub 是单个静态可执行文件，下列组件被编译进程序本体，随程序一起分发。
它们各自的许可条款独立于 mcp-hub 自身的 Apache-2.0 许可，继续适用于相应代码。

许可证类型由各组件仓库内的 LICENSE 文件判定，版本号取自 `go.mod`。

| 组件 | 版本 | 许可证 | 用途 |
|---|---|---|---|
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | Apache-2.0 | MCP 协议实现 |
| github.com/tetratelabs/wazero | v1.12.0 | Apache-2.0 | wasm 插件沙箱运行时 |
| fyne.io/systray | v1.12.2 | Apache-2.0 | Windows 托盘图标 |
| github.com/zalando/go-keyring | v0.2.8 | MIT | 操作系统密码管理器访问 |
| github.com/danieljoos/wincred | v1.2.3 | MIT | Windows 凭据管理器后端 |
| github.com/emersion/go-imap/v2 | v2.0.0-beta.8 | MIT | IMAP 客户端 |
| github.com/emersion/go-message | v0.18.2 | MIT | MIME 解析与字符集解码 |
| github.com/wneessen/go-mail | v0.8.1 | MIT | SMTP 客户端 |
| github.com/google/jsonschema-go | v0.4.3 | MIT | JSON Schema |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause | 系统调用 |
| golang.org/x/text | v0.43.0 | BSD-3-Clause | 字符编码 |
| github.com/godbus/dbus/v5 | v5.2.2 | BSD-2-Clause | Linux Secret Service 后端 |

上述许可证**全部为宽松型**，均不含传染性（copyleft）条款。

## Apache-2.0 组件的随附声明

Apache License 2.0 第 4(d) 条要求：被分发的作品若含 NOTICE 文件，衍生分发须
一并附带其中的归属声明。本产品内含的 Apache-2.0 组件中，仅 wazero 提供了
NOTICE 文件，其内容为：

```
wazero
Copyright 2020-2023 wazero authors
```

## 许可证全文

- Apache-2.0：<https://www.apache.org/licenses/LICENSE-2.0>（本仓库 `LICENSE` 即此文本）
- MIT：<https://opensource.org/licenses/MIT>
- BSD-3-Clause：<https://opensource.org/licenses/BSD-3-Clause>
- BSD-2-Clause：<https://opensource.org/licenses/BSD-2-Clause>

各组件的完整许可证文本随其源码分发，可在本机 Go 模块缓存中查看：

```
go list -m -f '{{.Dir}}' <组件路径>
```

## 不随程序分发的组件

以下内容不被编译进可执行文件，仅用于开发或演示，因此不构成分发义务：

- `plugins/ghwasm` —— 仓库自带的 wasm 示范插件，默认不打进发布包
- NapCat —— QQ 渠道连接的第三方程序，由用户自行获取并安装，mcp-hub 不下载、
  不分发、不包含其任何代码
