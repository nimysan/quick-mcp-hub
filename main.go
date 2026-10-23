// mcp-hub 是一个单文件可执行的 MCP 服务器，通过 IMAP/SMTP 访问
// QQ 邮箱等国内邮箱，供 Amazon Quick、Claude Desktop 等 MCP 客户端调用。
//
// 凭据存在操作系统自己的密码管理器里（macOS 钥匙串 / Windows 凭据管理器 /
// Linux Secret Service），按集成分段、惰性取用。
//
//	mcp-hub -set-secret                 把授权码写进系统密码管理器（不回显）
//	mcp-hub -list-secrets               列出已配置的集成和账号（不打印凭据）
//	mcp-hub -rm-secret                  删除一条凭据
//	mcp-hub -selftest                   验证 IMAP/SMTP 能否登录，不发信
//	mcp-hub                             跑 MCP 服务（stdio）
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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/nimysan/mcp-hub/internal/catalog"
	"github.com/nimysan/mcp-hub/internal/channels"
	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/mailbox"
	"github.com/nimysan/mcp-hub/internal/tools"
	"github.com/nimysan/mcp-hub/internal/webui"
)

// version 由 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		selftest    = flag.Bool("selftest", false, "验证 IMAP/SMTP 能否登录，然后退出（不发送邮件）")
		setSecret   = flag.Bool("set-secret", false, "把凭据写入系统密码管理器后退出")
		listSecrets = flag.Bool("list-secrets", false, "列出已配置的凭据条目（不打印凭据内容）后退出")
		rmSecret    = flag.Bool("rm-secret", false, "删除一条凭据后退出")
		uiFlag      = flag.Bool("ui", false, "启动渠道配置页面（本机 127.0.0.1 web 服务）")
		uiPort      = flag.Int("ui-port", 7801, "渠道配置页面端口，默认 7801")
		integration = flag.String("integration", config.IntegrationMail, "凭据所属集成名，默认 mail")
		account     = flag.String("account", "", "凭据所属账号，默认取 MCPHUB_MAIL_USER")
		catList     = flag.Bool("catalog-list", false, "列出当前生效的渠道声明及其来源层后退出")
		catValidate = flag.String("catalog-validate", "", "校验一份渠道声明文件（JSON）后退出")
		catSync     = flag.String("catalog-sync", "", "从该 https 地址抓取并验签远端渠道目录，写入本地缓存后退出")
		catTry      = flag.String("catalog-try", "", "用真实凭据跑一份渠道声明，逐条核对字段路径后退出")
		tryTool     = flag.String("try-tool", "", "只跑这一个工具（配合 -catalog-try）")
		tryWrite    = flag.Bool("try-write", false, "连写工具一起跑（默认只跑只读工具）")
		showVersion = flag.Bool("version", false, "打印版本号后退出")
	)
	tryFields := fieldFlags{}
	flag.Var(tryFields, "field", "渠道字段值，形如 site=https://…，可重复（配合 -catalog-try）")
	tryArgs := fieldFlags{}
	flag.Var(tryArgs, "try-arg", "工具参数，形如 issueKey=ENG-1，可重复（配合 -try-tool）")

	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("mcp-hub: ")
	log.SetOutput(os.Stderr) // 显式声明：绝不能是 os.Stdout
	mailbox.Version = version

	if *showVersion {
		fmt.Println(version)
		return
	}

	store := credstore.New(config.Namespace)

	// 项目改名前的凭据搬过来，一次性。旧索引文件不存在时这只是一次 stat。
	for _, old := range config.LegacyNamespaces {
		if n, err := store.MigrateFrom(old); err != nil {
			log.Printf("旧凭据迁移未完成（%s，已搬 %d 条）: %v", old, n, err)
		} else if n > 0 {
			log.Printf("已把 %d 条凭据从 %s 迁移到 %s", n, old, config.Namespace)
		}
	}

	// 目录子命令不碰凭据，也不联网（除 -catalog-sync），先处理掉。
	switch {
	case *catList:
		os.Exit(runCatalogList())
	case *catValidate != "":
		os.Exit(runCatalogValidate(*catValidate))
	case *catSync != "":
		os.Exit(runCatalogSync(*catSync))
	}

	if *catTry != "" {
		os.Exit(runCatalogTry(store, tryOptions{
			path: *catTry, account: *account, fields: tryFields,
			tool: *tryTool, args: tryArgs, write: *tryWrite,
		}))
	}

	// 凭据管理子命令不需要邮箱配置，先处理掉。
	switch {
	case *listSecrets:
		os.Exit(runListSecrets(store))
	case *setSecret:
		os.Exit(runSetSecret(store, *integration, resolveAccount(*account)))
	case *rmSecret:
		os.Exit(runRemoveSecret(store, *integration, resolveAccount(*account)))
	}

	chanStore, err := channels.NewStore(config.Namespace)
	if err != nil {
		log.Fatalf("渠道配置错误: %v", err)
	}
	mgr := channels.NewManager(chanStore, store)

	if *uiFlag {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := webui.New(mgr, version).Listen(ctx, *uiPort); err != nil {
			log.Fatalf("渠道配置页面退出: %v", err)
		}
		return
	}

	// 邮件渠道仍走环境变量那套配置；缺失不再是致命错误 —— 只配了
	// GitHub 这类声明式渠道的用户没有邮箱环境变量，服务照样要起得来。
	cfg, mailErr := config.Load(store)

	if *selftest {
		if mailErr != nil {
			fmt.Fprintf(os.Stderr, "邮件渠道未配置: %v\n", mailErr)
			os.Exit(2)
		}
		os.Exit(runSelfTest(cfg))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-hub",
		Title:   "MCP Hub",
		Version: version,
	}, nil)

	registered := 0

	// 邮件渠道：以页面里保存的配置为准，环境变量只作兜底 —— 老的 Quick
	// connector 配置是靠 MCPHUB_MAIL_* 跑的，不能因为改成读配置文件就把
	// 它们弄坏。同一个邮箱两边都有时，页面配置优先。
	mailCfgs, err := mgr.MailConfigs()
	if err != nil {
		log.Printf("读取邮件渠道配置失败: %v", err)
	}
	if mailErr == nil {
		dup := false
		for _, c := range mailCfgs {
			if c.User == cfg.User {
				dup = true
				break
			}
		}
		if !dup {
			mailCfgs = append(mailCfgs, cfg)
			log.Printf("邮件渠道（来自环境变量）：账号 %s", cfg.User)
		}
	}
	if n := tools.RegisterMail(server, mailCfgs); n > 0 {
		registered += n
		labels := make([]string, 0, len(mailCfgs))
		for _, c := range mailCfgs {
			label := c.User
			if c.AllowSend {
				label += "(可发信)"
			}
			labels = append(labels, label)
		}
		log.Printf("邮件渠道：%d 个账号 [%s]，注册 %d 个工具",
			len(mailCfgs), strings.Join(labels, " "), n)
	} else {
		log.Printf("邮件渠道未注册：页面里没有邮件渠道，也没有 MCPHUB_MAIL_USER（%v）", mailErr)
	}

	// 声明式 HTTPS 渠道来自渠道配置（页面里保存的），凭据在系统密码管理器里。
	for _, err := range httpchan.Errors() {
		log.Printf("渠道目录里有一条声明无效，已跳过: %v", err)
	}
	runtimes, err := mgr.HTTPGroups()
	if err != nil {
		log.Printf("读取渠道配置失败: %v", err)
	}
	for _, g := range runtimes {
		n := tools.RegisterHTTP(server, g.Runtimes)
		registered += n
		accounts := make([]string, 0, len(g.Runtimes))
		for _, rt := range g.Runtimes {
			label := rt.Account
			if rt.AllowWrite() {
				label += "(可写)"
			}
			accounts = append(accounts, label)
		}
		log.Printf("%s 渠道：%d 个账号 [%s]，注册 %d 个工具",
			g.Spec.Name, len(g.Runtimes), strings.Join(accounts, " "), n)
	}

	if registered == 0 {
		log.Fatalf("没有任何可用渠道。用 %s -ui 打开配置页面添加一个", os.Args[0])
	}

	// 启动时只报非敏感信息，并且不触发凭据读取 —— 保持惰性，
	// 免得在锁屏状态下于启动阶段弹出钥匙串授权框。
	log.Printf("启动 %s（凭据后端 %s，渠道配置 %s）",
		version, credstore.BackendName(), chanStore.Path())
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}

func runCatalogList() int {
	c := catalog.Load(config.Namespace)
	dir, _ := catalog.Dir(config.Namespace)
	fmt.Printf("本地目录文件夹：%s\n", dir)
	if len(catalog.TrustedKeys) == 0 {
		fmt.Println("验签公钥：未内置 —— 这个构建会拒绝一切远端目录")
	} else {
		fmt.Printf("验签公钥：已内置 %d 个\n", len(catalog.TrustedKeys))
	}
	fmt.Println()

	entries := c.Entries()
	fmt.Printf("%-12s %-4s %-9s %-16s %s\n", "渠道", "版本", "来源", "文件", "名称")
	for _, e := range entries {
		shadow := ""
		if len(e.Shadowed) > 0 {
			parts := make([]string, 0, len(e.Shadowed))
			for _, l := range e.Shadowed {
				parts = append(parts, string(l))
			}
			shadow = "（压住了 " + strings.Join(parts, "/") + "）"
		}
		fmt.Printf("%-12s v%-3d %-9s %-16s %s%s\n",
			e.Spec.ID, e.Spec.Version, e.Layer, e.File, e.Spec.Name, shadow)
	}
	fmt.Printf("\n共 %d 个渠道\n", len(entries))

	for _, n := range c.Notes() {
		fmt.Printf("提示：%s\n", n)
	}
	for _, err := range c.Errors() {
		fmt.Fprintf(os.Stderr, "跳过：%v\n", err)
	}
	if len(c.Errors()) > 0 {
		return 1
	}
	return 0
}

func runCatalogValidate(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		return 2
	}
	// 和内置层、远端下发走的是同一个入口，所以这里过了那边也会过。
	s, err := httpchan.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "不合法: %v\n", err)
		return 1
	}
	fmt.Printf("✅ %s v%d（%s）\n", s.ID, s.Version, s.Name)
	fmt.Printf("   标签：%s\n", strings.Join(s.Tags, " "))
	fmt.Printf("   凭据：%s/%s（账号字段 %s，密码字段 %s）\n",
		config.Namespace, s.Integration, s.Account.Name, s.Secret.Name)
	if s.BaseURL != "" {
		fmt.Printf("   接入点：%s\n", s.BaseURL)
	} else {
		fmt.Printf("   接入点：由用户填写字段 %s\n", s.BaseURLField)
	}
	if s.PostReads {
		fmt.Printf("   ⚠ 声明了 postReads：有只读工具是 POST，方法本身不能作证，需人工复核\n")
	}
	fmt.Printf("   工具 %d 个：\n", len(s.Tools))
	for _, t := range s.Tools {
		kind := "读"
		if !t.ReadOnly {
			kind = "写"
		}
		fmt.Printf("     [%s] %-24s %-5s %s\n", kind, t.Name, t.Method, t.Path)
	}
	return 0
}

func runCatalogSync(url string) int {
	dir, err := catalog.Dir(config.Namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	doc, path, err := catalog.Sync(ctx, url, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "同步失败: %v\n", err)
		return 1
	}
	specs := 0
	for range doc.Connectors {
		specs++
	}
	fmt.Printf("已同步 %d 个渠道声明到 %s\n", specs, path)
	fmt.Printf("目录生成时间：%s，签名密钥 %s\n",
		doc.GeneratedAt.Local().Format("2006-01-02 15:04:05"), doc.Signature.KeyID)
	if len(doc.Revoked) > 0 {
		fmt.Printf("其中 %d 条下架记录\n", len(doc.Revoked))
	}
	fmt.Println("用 -catalog-list 看合并后的结果。")
	return 0
}

func resolveAccount(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	for _, k := range []string{"MCPHUB_MAIL_USER", "QQMAIL_USER", "EMAIL_USER"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func runSetSecret(store *credstore.Store, integration, account string) int {
	if account == "" {
		fmt.Fprintln(os.Stderr, "需要账号：用 -account 指定，或设置 MCPHUB_MAIL_USER")
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
	fmt.Println("现在可以把客户端配置里的密码类环境变量删掉了。")
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
		fmt.Fprintln(os.Stderr, "需要账号：用 -account 指定，或设置 MCPHUB_MAIL_USER")
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
// 方便 `echo xxx | mcp-hub -set-secret`。
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
