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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/nimysan/mcp-hub/internal/applog"
	"github.com/nimysan/mcp-hub/internal/assets"
	"github.com/nimysan/mcp-hub/internal/catalog"
	"github.com/nimysan/mcp-hub/internal/channels"
	"github.com/nimysan/mcp-hub/internal/clientstat"
	"github.com/nimysan/mcp-hub/internal/config"
	"github.com/nimysan/mcp-hub/internal/credstore"
	"github.com/nimysan/mcp-hub/internal/feishuweb"
	"github.com/nimysan/mcp-hub/internal/httpchan"
	"github.com/nimysan/mcp-hub/internal/localmail"
	"github.com/nimysan/mcp-hub/internal/mailbox"
	"github.com/nimysan/mcp-hub/internal/tools"
	"github.com/nimysan/mcp-hub/internal/tray"
	"github.com/nimysan/mcp-hub/internal/wasmplug"
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
		uiFlag      = flag.Bool("ui", false, "打开配置页面（本机 127.0.0.1 web 服务）")
		serveFlag   = flag.Bool("serve", false, "强制以 MCP 服务模式运行（默认由启动方式自动判断）")
		noBrowser   = flag.Bool("no-browser", false, "打开配置页面时不自动弹出窗口")
		uiPort      = flag.Int("ui-port", 7801, "渠道配置页面端口，默认 7801")
		integration = flag.String("integration", config.IntegrationMail, "凭据所属集成名，默认 mail")
		account     = flag.String("account", "", "凭据所属账号，默认取 MCPHUB_MAIL_USER")
		catList     = flag.Bool("catalog-list", false, "列出当前生效的渠道声明及其来源层后退出")
		catValidate = flag.String("catalog-validate", "", "校验一份渠道声明文件（JSON）后退出")
		catSync     = flag.String("catalog-sync", "", "从该 https 地址抓取并验签远端渠道目录，写入本地缓存后退出")
		catTry      = flag.String("catalog-try", "", "用真实凭据跑一份渠道声明，逐条核对字段路径后退出")
		tryTool     = flag.String("try-tool", "", "只跑这一个工具（配合 -catalog-try）")
		tryWrite    = flag.Bool("try-write", false, "连写工具一起跑（默认只跑只读工具）")
		pluginInst  = flag.String("plugin-install", "", "安装一个 wasm 插件（给它的 manifest.json 路径）后退出")
		pluginList  = flag.Bool("plugin-list", false, "列出已安装的 wasm 插件后退出")
		showVersion = flag.Bool("version", false, "打印版本号后退出")
	)
	tryFields := fieldFlags{}
	flag.Var(tryFields, "field", "渠道字段值，形如 site=https://…，可重复（配合 -catalog-try）")
	tryArgs := fieldFlags{}
	flag.Var(tryArgs, "try-arg", "工具参数，形如 issueKey=ENG-1，可重复（配合 -try-tool）")

	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("mcp-hub: ")
	// 日志同时写文件。Windows 上的 exe 用 -H windowsgui 编译，没有控制台，
	// stderr 没有地方可去——没有这一步，程序出错就是彻底静默，用户看到的
	// 是双击之后什么都没发生。
	logFile, logErr := applog.Open(config.Namespace)
	defer logFile.Close()
	logPath := logFile.Path
	log.SetOutput(logFile.Writer) // 显式声明：绝不能是 os.Stdout
	if logErr != nil {
		log.Printf("日志文件打不开，只写 stderr: %v", logErr)
	}
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

	if *pluginList {
		os.Exit(runPluginList())
	}
	if *pluginInst != "" {
		os.Exit(runPluginInstall(*pluginInst))
	}

	chanStore, err := channels.NewStore(config.Namespace)
	if err != nil {
		log.Fatalf("渠道配置错误: %v", err)
	}
	mgr := channels.NewManager(chanStore, store)

	// 插件发现只读 manifest，不编译 wasm —— 模块由 Registry 在第一次调用
	// 时才加载，所以这一步对启动时间没有影响。
	installed, pluginErrs := wasmplug.Discover(config.Namespace)
	for _, err := range pluginErrs {
		log.Printf("插件已跳过: %v", err)
	}
	registry := wasmplug.NewRegistry(installed)
	mgr.Plugins = registry

	// 默认行为由启动方式决定，而不是让用户记住该加什么参数：
	// 被 AI 客户端 spawn 时 stdin 是管道 -> 跑 MCP 服务；
	// 用户双击时 stdin 是终端 / NUL / 拿不到 -> 打开配置页面。
	//
	// 这个程序的用户是财务和运营。「双击主程序会一闪而过，要去双击另一个
	// 辅助脚本」是不能接受的，所以把默认值做对，而不是写进文档。
	// -ui / -serve 两个显式参数仍然保留：自动判断只是默认值，猜错了也有救。
	if *uiFlag || (!*serveFlag && !stdinIsPipe()) {
		os.Exit(runUI(mgr, logPath, *uiPort, *noBrowser))
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

	registered := 0

	// 记下「真的有客户端连上来了」。
	//
	// 时机刻意选在 InitializedHandler 而不是进程启动：被 spawn 起来只说明
	// 客户端想连，握手还可能失败。这个回调触发时协议已经通了。
	//
	// 配置页面读这条记录来决定接入说明要不要默认展开 —— 用户照着说明在 Quick
	// 里加完连接器之后，否则完全没有反馈。
	//
	// 写失败只记日志：它是个展示信号，不是功能，绝不能影响服务。
	opts := &mcp.ServerOptions{
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			rec := clientstat.Record{At: time.Now(), Tools: registered}
			if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
				rec.Client, rec.Version = p.ClientInfo.Name, p.ClientInfo.Version
			}
			if err := clientstat.Write(config.Namespace, rec); err != nil {
				log.Printf("记录客户端连接失败（不影响服务）: %v", err)
				return
			}
			log.Printf("客户端已连接：%s %s，本次注册 %d 个工具", rec.Client, rec.Version, rec.Tools)
		},
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-hub",
		Title:   "MCP Hub",
		Version: version,
	}, opts)

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

	// wasm 插件渠道：第三方代码在沙箱里跑，凭据由宿主注入，模块懒加载。
	defer registry.Close(context.Background())
	pluginGroups, err := mgr.PluginGroups()
	if err != nil {
		log.Printf("读取插件渠道配置失败: %v", err)
	}
	for _, g := range pluginGroups {
		n := tools.RegisterPlugin(server, registry, g.Manifest, g.Instances)
		registered += n
		accounts := make([]string, 0, len(g.Instances))
		for _, in := range g.Instances {
			label := in.Account
			if in.AllowWrite() {
				label += "(可写)"
			}
			accounts = append(accounts, label)
		}
		log.Printf("%s 插件渠道：%d 个账号 [%s]，注册 %d 个工具（允许访问 %s）",
			g.Manifest.Name, len(g.Instances), strings.Join(accounts, " "), n,
			strings.Join(g.Manifest.AllowedHosts, " "))
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

	// 本机邮件渠道：只读本机邮件客户端的数据文件，没有凭据。来源惰性打开 ——
	// 没授权的来源只在被调用时返回授权指引，不影响启动，也不影响别的来源。
	localCfg, localOK, localIgnored, err := mgr.LocalMailConfig()
	if err != nil {
		log.Printf("读取本机邮件渠道配置失败: %v", err)
	}
	if len(localIgnored) > 0 {
		log.Printf("本机邮件渠道只用第一条，已忽略：%s", strings.Join(localIgnored, " "))
	}
	if localOK {
		hub := localmail.NewHub(localCfg)
		defer hub.Close()
		if n := tools.RegisterLocalMail(server, hub); n > 0 {
			registered += n
			log.Printf("本机邮件渠道（只读）：来源 [%s]，注册 %d 个工具",
				strings.Join(hub.Enabled(), " "), n)
		} else {
			log.Printf("本机邮件渠道已添加，但一个来源都没打开，未注册工具")
		}
	}

	// 飞书网页文档：工具调用经本机配置页面进程转给浏览器扩展。全部只读。
	if on, err := mgr.BrowserEnabled(); err != nil {
		log.Printf("读取飞书网页文档渠道配置失败: %v", err)
	} else if on {
		n := tools.RegisterFeishuWeb(server, feishuweb.NewClient())
		registered += n
		log.Printf("飞书网页文档渠道：注册 %d 个只读工具（经本机配置页面和浏览器扩展通信）", n)
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

// runUI 跑配置页面，并让这个程序表现得像一个普通软件。
//
// 三件事合起来才成立，单做任何一件都更糟：
//
// 单实例。第二次双击主程序时，旧行为是绑端口失败，用户看到一句
// address already in use——而在没有控制台的 Windows 上是什么都看不到。
// 现在先探一下那个端口，认出是自己就把已经在跑的那份的页面打开。
//
// 托盘图标。去掉黑窗口之后，如果再没有托盘，用户关掉配置页面就只剩一个
// 看不见的进程：没有入口把页面叫回来，也没有办法退出它。
//
// 失败要看得见。-H windowsgui 下 log.Fatalf 只写进日志文件，所以起不来
// 时弹一个消息框——否则「双击了没反应」是用户唯一的信息。
func runUI(mgr *channels.Manager, logPath string, port int, noBrowser bool) int {
	if url, ok := webui.ProbeRunning(port); ok {
		log.Printf("已经有一份 mcp-hub 在运行，打开它的页面：%s", url)
		// -no-browser 在这条路上也得算数：服务器场景下不该弹窗口。
		if !noBrowser {
			webui.OpenInBrowser(url)
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := webui.New(mgr, version)
	srv.LogPath = logPath

	// 端口为 0 时由系统分配，真实地址只有 OnReady 知道，所以页面地址用
	// atomic 存——托盘的回调在另一个 goroutine 上读它。
	var pageURL atomic.Value
	pageURL.Store(fmt.Sprintf("http://127.0.0.1:%d", port))
	srv.OnReady = func(url string) {
		pageURL.Store(url)
		if !noBrowser {
			webui.OpenInBrowser(url)
		}
	}

	srvErr := make(chan error, 1)
	go func() {
		err := srv.Listen(ctx, port)
		srvErr <- err
		if err != nil {
			// 服务没起来就别留一个点了没反应的托盘图标。
			tray.Quit()
		}
	}()

	openPage := func() { webui.OpenInBrowser(pageURL.Load().(string)) }
	// 退出要退干净：先关掉配置页面窗口，再停服务。
	//
	// 两件事有先后：那个窗口属于浏览器进程，不属于我们，所以必须在服务还活着
	// 的时候让它正常关掉（WM_CLOSE，等价于用户点 X）。顺序反了就只是把一个
	// 刷新即报错的死窗口留在屏幕上 —— 用户看到的是「托盘点了退出，配置页面
	// 还开着，任务栏图标也还在」。
	//
	// 关闭窗口（点 X）仍然只是收起页面，程序留在托盘里 —— 这是 Windows 上
	// 的常规形态，也是托盘图标存在的意义：它就是「程序还在」的那个信号。
	quit := func() {
		webui.CloseWindow()
		stop()
	}
	err := tray.Run(tray.Options{
		Icon:    assets.Icon,
		Tooltip: "mcp-hub · 渠道配置",
		OnOpen:  openPage,
		OnLog:   func() { webui.OpenInBrowser(pageURL.Load().(string) + "/api/log") },
		ShowLog: logPath != "",
		OnQuit:  quit,
	})
	if errors.Is(err, tray.ErrUnsupported) {
		// 没有托盘的平台退回原来的行为：等服务结束或收到退出信号。
		select {
		case <-ctx.Done():
		case e := <-srvErr:
			return reportUIError(e)
		}
	}
	// 托盘退出后给服务一点时间收尾，顺带把它的错误取出来。
	select {
	case e := <-srvErr:
		return reportUIError(e)
	case <-time.After(2 * time.Second):
		return 0
	}
}

func reportUIError(err error) int {
	if err == nil {
		return 0
	}
	log.Printf("配置页面退出: %v", err)
	tray.Alert("mcp-hub 起不来", err.Error()+
		"\n\n最常见的原因是那个端口被别的程序占用了。可以用 -ui-port 换一个端口。")
	return 1
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

// runPluginList 列出已安装的 wasm 插件，含它们被允许访问的主机。
//
// 主机清单要跟着一起列：一个插件最该被关注的性质就是「它能把数据发到哪」，
// 而那是 manifest 说的、宿主强制的。
func runPluginList() int {
	dir, err := wasmplug.Dir(config.Namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "定位插件目录失败: %v\n", err)
		return 1
	}
	items, errs := wasmplug.Discover(config.Namespace)
	fmt.Printf("插件目录: %s\n", dir)
	if len(items) == 0 && len(errs) == 0 {
		fmt.Println("（没有已安装的插件）")
		return 0
	}
	for _, it := range items {
		m := it.Manifest
		var write []string
		for _, t := range m.Tools {
			if !t.ReadOnly {
				write = append(write, t.Name)
			}
		}
		fmt.Printf("\n%-10s v%d  %s  (ABI v%d)\n", m.ID, m.Version, m.Name, m.ABI)
		fmt.Printf("  模块      %s\n", it.ModulePath)
		fmt.Printf("  可访问主机 %s\n", strings.Join(m.AllowedHosts, " "))
		fmt.Printf("  工具      %d 个", len(m.Tools))
		if len(write) > 0 {
			fmt.Printf("，其中写操作 %d 个（%s），需打开写开关",
				len(write), strings.Join(write, " "))
		}
		fmt.Println()
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "\n跳过: %v\n", err)
	}
	return 0
}

// runPluginInstall 把一份插件装进插件目录。
//
// 刻意不做签名验证：手工装一个插件等于用户自己承担来源风险，和手工往 catalog
// 目录放一份声明同理。签名是目录下发那一层的事。
func runPluginInstall(manifestPath string) int {
	it, err := wasmplug.Install(config.Namespace, manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "安装失败: %v\n", err)
		return 1
	}
	m := it.Manifest
	fmt.Printf("已安装 %s v%d (%s)\n", m.ID, m.Version, m.Name)
	fmt.Printf("  装到      %s\n", it.Dir)
	fmt.Printf("  会执行第三方代码，允许访问: %s\n", strings.Join(m.AllowedHosts, " "))
	fmt.Printf("  工具 %d 个；到配置页面填凭据后才会注册给模型\n", len(m.Tools))
	return 0
}

// stdinIsPipe 报告 stdin 是不是一个管道 —— 也就是「我是被别的程序启动的」。
//
// AI 客户端（Claude / Cursor / Kiro / Quick）都用 stdio 传输，spawn 子进程时
// 给的是管道。用户在 Finder / 资源管理器里双击时，stdin 是终端、是 NUL、
// 或者根本拿不到。
//
// 拿不到时按「不是管道」处理：双击的场景比脚本调用常见得多，而且猜错的
// 代价不对称 —— 该开页面却跑了服务，用户看到窗口一闪而过、毫无头绪；
// 该跑服务却开了页面，客户端会报连接失败，而那条路可以用 -serve 钉死。
func stdinIsPipe() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}
