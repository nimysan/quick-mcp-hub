// qqmail-mcp 是一个单文件可执行的 MCP 服务器，通过 IMAP/SMTP 访问
// QQ 邮箱等国内邮箱，供 Amazon Quick、Claude Desktop 等 MCP 客户端调用。
//
// 跑 MCP 服务（默认）：
//
//	qqmail-mcp
//
// 不发信、只验证账号能不能连通：
//
//	qqmail-mcp -selftest
//
// 配置全部来自环境变量，见 README。
//
// 关键约束：stdio 传输下 stdout 是 JSON-RPC 协议通道，只能出协议报文。
// 本程序所有日志走 stderr（Go 标准库 log 的默认行为），-selftest 是唯一
// 往 stdout 打人类可读文本的模式，而它不跑协议。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/qqmail-mcp/internal/config"
	"github.com/nimysan/qqmail-mcp/internal/mailbox"
	"github.com/nimysan/qqmail-mcp/internal/tools"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	selftest := flag.Bool("selftest", false, "验证 IMAP/SMTP 能否登录，然后退出（不发送邮件）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("qqmail-mcp: ")
	log.SetOutput(os.Stderr) // 显式声明：绝不能是 os.Stdout
	mailbox.Version = version

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	if *selftest {
		os.Exit(runSelfTest(cfg))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "qqmail-mcp",
		Title:   "QQ 邮箱",
		Version: version,
	}, nil)
	tools.Register(server, cfg)

	log.Printf("启动 %s（%s，账号 %s，发信%s）", version, cfg.Provider, cfg.User, enabledText(cfg.AllowSend))
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}

func runSelfTest(cfg config.Config) int {
	fmt.Printf("服务商   : %s\n", cfg.Provider)
	fmt.Printf("账号     : %s\n", cfg.User)
	fmt.Printf("IMAP     : %s:%d\n", cfg.IMAPHost, cfg.IMAPPort)
	fmt.Printf("SMTP     : %s:%d (SSL=%t)\n", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPPort == 465)
	fmt.Printf("发信开关 : %s\n\n", enabledText(cfg.AllowSend))

	imapErr, smtpErr := mailbox.SelfTest(context.Background(), cfg)
	code := 0
	if imapErr != nil {
		fmt.Printf("IMAP 失败: %v\n", imapErr)
		code = 1
	} else {
		fmt.Println("IMAP 登录成功，INBOX 可打开")
	}
	if smtpErr != nil {
		fmt.Printf("SMTP 失败: %v\n", smtpErr)
		code = 1
	} else {
		fmt.Println("SMTP 认证成功（未发送任何邮件）")
	}
	return code
}

func enabledText(b bool) string {
	if b {
		return "已启用"
	}
	return "已关闭"
}
