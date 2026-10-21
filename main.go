// qqmail-mcp 是一个单文件可执行的 MCP 服务器，通过 IMAP/SMTP 访问
// QQ 邮箱等国内邮箱，供 Amazon Quick、Claude Desktop 等 MCP 客户端调用。
//
// 凭据存在操作系统自己的密码管理器里（macOS 钥匙串 / Windows 凭据管理器 /
// Linux Secret Service），按集成分段、惰性取用。
//
//	qqmail-mcp -set-secret                 把授权码写进系统密码管理器（不回显）
//	qqmail-mcp -list-secrets               列出已配置的集成和账号（不打印凭据）
//	qqmail-mcp -rm-secret                  删除一条凭据
//	qqmail-mcp -selftest                   验证 IMAP/SMTP 能否登录，不发信
//	qqmail-mcp                             跑 MCP 服务（stdio）
//
// 关键约束：stdio 传输下 stdout 是 JSON-RPC 协议通道，只能出协议报文。
// 本程序所有日志走 stderr（Go 标准库 log 的默认行为）；上面那些子命令是
// 唯一往 stdout 打人类可读文本的模式，而它们都不跑协议。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/nimysan/qqmail-mcp/internal/config"
	"github.com/nimysan/qqmail-mcp/internal/credstore"
	"github.com/nimysan/qqmail-mcp/internal/mailbox"
	"github.com/nimysan/qqmail-mcp/internal/tools"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		selftest    = flag.Bool("selftest", false, "验证 IMAP/SMTP 能否登录，然后退出（不发送邮件）")
		setSecret   = flag.Bool("set-secret", false, "把凭据写入系统密码管理器后退出")
		listSecrets = flag.Bool("list-secrets", false, "列出已配置的凭据条目（不打印凭据内容）后退出")
		rmSecret    = flag.Bool("rm-secret", false, "删除一条凭据后退出")
		integration = flag.String("integration", config.IntegrationMail, "凭据所属集成名，默认 mail")
		account     = flag.String("account", "", "凭据所属账号，默认取 QQMAIL_USER")
		showVersion = flag.Bool("version", false, "打印版本号后退出")
	)
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("qqmail-mcp: ")
	log.SetOutput(os.Stderr) // 显式声明：绝不能是 os.Stdout
	mailbox.Version = version

	if *showVersion {
		fmt.Println(version)
		return
	}

	store := credstore.New(config.Namespace)

	// 凭据管理子命令不需要邮箱配置，先处理掉。
	switch {
	case *listSecrets:
		os.Exit(runListSecrets(store))
	case *setSecret:
		os.Exit(runSetSecret(store, *integration, resolveAccount(*account)))
	case *rmSecret:
		os.Exit(runRemoveSecret(store, *integration, resolveAccount(*account)))
	}

	cfg, err := config.Load(store)
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

	// 启动时只报非敏感信息，并且不触发凭据读取 —— 保持惰性，
	// 免得在锁屏状态下于启动阶段弹出钥匙串授权框。
	log.Printf("启动 %s（%s，账号 %s，发信%s，凭据后端 %s）",
		version, cfg.Provider, cfg.User, enabledText(cfg.AllowSend), credstore.BackendName())
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}

func resolveAccount(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	for _, k := range []string{"QQMAIL_USER", "EMAIL_USER"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func runSetSecret(store *credstore.Store, integration, account string) int {
	if account == "" {
		fmt.Fprintln(os.Stderr, "需要账号：用 -account 指定，或设置 QQMAIL_USER")
		return 2
	}
	secret, err := readSecret(fmt.Sprintf("请输入 %s/%s 的凭据（QQ 邮箱填 16 位授权码，输入不回显）: ", integration, account))
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取输入失败: %v\n", err)
		return 1
	}
	if secret == "" {
		fmt.Fprintln(os.Stderr, "凭据为空，未写入")
		return 2
	}
	if err := store.Put(credstore.Ref{Integration: integration, Account: account}, secret); err != nil {
		fmt.Fprintf(os.Stderr, "写入失败: %v\n", err)
		return 1
	}
	fmt.Printf("已写入%s：%s/%s\n", credstore.BackendName(), integration, account)
	fmt.Printf("索引文件：%s（只含集成名和账号，不含凭据）\n", store.IndexPath())
	fmt.Println("现在可以把客户端配置里的 QQMAIL_PASSWORD 环境变量删掉了。")
	return 0
}

func runListSecrets(store *credstore.Store) int {
	items, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取索引失败: %v\n", err)
		return 1
	}
	fmt.Printf("凭据后端：%s\n", credstore.BackendName())
	fmt.Printf("索引文件：%s\n\n", store.IndexPath())
	if len(items) == 0 {
		fmt.Println("还没有登记任何凭据。用 -set-secret 写入第一条。")
		return 0
	}
	fmt.Printf("%-12s %-32s %s\n", "集成", "账号", "更新时间")
	for _, it := range items {
		fmt.Printf("%-12s %-32s %s\n", it.Ref.Integration, it.Ref.Account, it.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
	return 0
}

func runRemoveSecret(store *credstore.Store, integration, account string) int {
	if account == "" {
		fmt.Fprintln(os.Stderr, "需要账号：用 -account 指定，或设置 QQMAIL_USER")
		return 2
	}
	if err := store.Delete(credstore.Ref{Integration: integration, Account: account}); err != nil {
		fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
		return 1
	}
	fmt.Printf("已从%s删除：%s/%s\n", credstore.BackendName(), integration, account)
	return 0
}

// readSecret 读取一行凭据。终端下关闭回显；被管道喂入时直接读一行，
// 方便 `echo xxx | qqmail-mcp -set-secret`。
//
// 凭据绝不从命令行参数接收：argv 对同机其他进程可见（ps），也会进 shell 历史。
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func runSelfTest(cfg config.Config) int {
	fmt.Printf("服务商     : %s\n", cfg.Provider)
	fmt.Printf("账号       : %s\n", cfg.User)
	fmt.Printf("IMAP       : %s:%d\n", cfg.IMAPHost, cfg.IMAPPort)
	fmt.Printf("SMTP       : %s:%d (SSL=%t)\n", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPPort == 465)
	fmt.Printf("发信开关   : %s\n", enabledText(cfg.AllowSend))
	fmt.Printf("凭据后端   : %s\n", credstore.BackendName())

	// 这一步会真正触发凭据读取，所以 macOS 上可能弹一次授权框。
	if _, err := cfg.Password(); err != nil {
		fmt.Printf("凭据来源   : 取不到\n\n")
		fmt.Printf("凭据失败: %v\n", err)
		if errors.Is(err, credstore.ErrNotFound) {
			fmt.Printf("\n写入方式: %s -set-secret -account %s\n", os.Args[0], cfg.User)
		}
		return 1
	}
	fmt.Printf("凭据来源   : %s\n\n", cfg.SecretSource())

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
