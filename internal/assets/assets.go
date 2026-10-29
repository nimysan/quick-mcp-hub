// Package assets 放需要编译进二进制的静态资源。
//
// 为什么有这个包：go:embed 只能嵌入**自己包目录下**的文件，而图标同时被三处
// 用到 —— 配置页面的 favicon（没有它，--app 窗口在任务栏里是个地球）、
// Windows 托盘图标、以及 exe 自己的资源。前两处要嵌进二进制，所以必须在某个
// 包目录里有一份。
//
// packaging/windows/mcp-hub.ico 是给打包工具用的那一份（rsrc 嵌 exe 资源、
// Inno Setup 的 SetupIconFile），路径写在脚本和 .iss 里。两份内容必须一致，
// 这件事由 assets_test.go 钉住 —— 不然改了 logo 只更新一边，页面和托盘会
// 悄悄留着旧图标。
package assets

import _ "embed"

// Icon 是 Windows .ico 格式的程序图标，含 16/32/48/256 各档。
//
// 浏览器认 .ico 当 favicon；Windows 托盘要的也是 .ico 字节。
//
//go:embed mcp-hub.ico
var Icon []byte
