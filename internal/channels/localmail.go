// 本机邮件渠道：只读本机邮件客户端（Mac 上的 Apple Mail、Outlook，Windows 上的
// 经典版和新版 Outlook）的数据文件，不连任何服务器，也就没有凭据。来源按平台过滤，
// 见 localmail.Sources。
//
// 和其他渠道并列成为第四种类型。它和别的渠道不同的地方只有两点：没有密码类
// 字段（保存时不碰系统密码管理器），以及「连通性测试」测的是本机文件能不能读，
// 而且结果取决于**哪个进程**在读 —— 配置页进程和 Amazon Quick 拉起来的
// MCP 进程各自有各自的 macOS 隐私授权（Windows 上没有这一层）。
package channels

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/nimysan/mcp-hub/internal/localmail"
	"github.com/nimysan/mcp-hub/internal/mailbox"
)

// KindLocalMail 是本机邮件渠道。
const KindLocalMail = "localmail"

// 本机邮件渠道的字段名。来源开关的字段名就是来源标识（apple_mail …），
// 自定义目录是「来源标识 + _dir」。
const (
	localMailType        = "localmail"
	localMailNameField   = "name"
	localMailMaxBody     = "maxBodyBytes"
	localMailMaxResults  = "maxResults"
	localMailDirSuffix   = "_dir"
	localMailDefaultName = "本机"
)

// localMailTools 是本机邮件渠道注册的工具，全部只读。
var localMailTools = []ToolMeta{
	{Name: "localmail_list_folders", Title: "列出文件夹", Desc: "列出本机邮件客户端里的文件夹和邮件数"},
	{Name: "localmail_search", Title: "搜索邮件", Desc: "按发件人、主题、日期、文件夹查本机邮件"},
	{Name: "localmail_get", Title: "读取邮件", Desc: "查看一封本机邮件的正文和附件名"},
}

func localMailDescriptor() Descriptor {
	fields := []Field{{
		Name: localMailNameField, Label: "名称", Kind: KindText, Required: true,
		Default: localMailDefaultName, Help: "只用来在列表里认出这条连接，一般不用改",
	}}
	for _, s := range localmail.Sources {
		fields = append(fields, Field{
			Name: s.ID, Label: "读取 " + s.Name, Kind: KindBool, Default: "false", Help: s.Help,
		})
	}
	for _, s := range localmail.Sources {
		fields = append(fields, Field{
			Name: s.ID + localMailDirSuffix, Label: s.DirLabel, Kind: KindText,
			Advanced: true, Help: s.DirHelp,
		})
	}
	fields = append(fields,
		Field{
			Name: localMailMaxBody, Label: "正文长度上限（字节）", Kind: KindNumber, Advanced: true,
			Default: fmt.Sprint(localmail.DefaultMaxBodyBytes),
			Help:    "一封信返回给模型的正文最多这么长，超出部分截掉。中文大约每字 3 字节",
		},
		Field{
			Name: localMailMaxResults, Label: "每页最多几封", Kind: KindNumber, Advanced: true,
			Default: fmt.Sprint(localmail.DefaultMaxResults),
			Help:    fmt.Sprintf("搜索一次最多返回几封，更多的要翻页。最大 %d", localmail.HardMaxResults),
		},
	)
	names := make([]string, 0, len(localMailTools))
	for _, t := range localMailTools {
		names = append(names, t.Name)
	}
	return Descriptor{
		Type:         localMailType,
		Name:         "本机邮件（只读）",
		Icon:         "🗂️",
		Kind:         KindLocalMail,
		Version:      1,
		Tags:         []string{"email"},
		Summary:      localMailSummary(runtime.GOOS),
		Integration:  "localmail",
		AccountField: localMailNameField,
		Available:    len(localmail.Sources) > 0, // 这个平台上一个来源都没有时不可添加
		Platform:     runtime.GOOS,
		Tools:        names,
		ToolInfo:     localMailTools,
		Fields:       fields,
	}
}

func localMailSummary(goos string) string {
	if goos == "windows" {
		return "直接读这台电脑上经典版 Outlook 的数据文件和新版 Outlook 的离线缓存 · 不联网、无需密码"
	}
	return "直接读这台 Mac 上邮件 App 的本地数据 · 不联网、无需密码"
}

// LocalMailConfig 返回第一条已启用的本机邮件渠道的配置。没有时 ok 为 false。
//
// 只取一条：本机邮件只有一份，两条连接读的是同一批文件，合并它们没有意义。
// 多出来的那几条在 ignored 里返回，由调用方记日志。
func (m *Manager) LocalMailConfig() (cfg localmail.Config, ok bool, ignored []string, err error) {
	list, err := m.store.List()
	if err != nil {
		return localmail.Config{}, false, nil, err
	}
	d := localMailDescriptor()
	for _, in := range list {
		if in.Type != localMailType || !in.Enabled {
			continue
		}
		if ok {
			ignored = append(ignored, in.Account)
			continue
		}
		cfg, ok = localMailConfigOf(d, in), true
	}
	return cfg, ok, ignored, nil
}

func localMailConfigOf(d Descriptor, in Instance) localmail.Config {
	cfg := localmail.Config{
		MaxBodyBytes: in.SettingInt(d, localMailMaxBody, localmail.DefaultMaxBodyBytes),
		MaxResults:   in.SettingInt(d, localMailMaxResults, localmail.DefaultMaxResults),
	}
	for _, s := range localmail.Sources {
		if in.SettingBool(d, s.ID) {
			cfg.Sources = append(cfg.Sources, localmail.SourceConfig{
				ID: s.ID, Dir: in.Setting(d, s.ID+localMailDirSuffix),
			})
		}
	}
	return cfg
}

// localMailInstance 取表单临时值或已保存的实例。
func (m *Manager) localMailInstance(d Descriptor, account string, values map[string]string) (Instance, error) {
	if len(values) > 0 {
		acc, _, settings, err := d.Validate(values)
		if err != nil {
			return Instance{}, err
		}
		return Instance{Type: d.Type, Account: acc, Settings: settings}, nil
	}
	got, ok, err := m.store.Get(d.Type, account)
	if err != nil {
		return Instance{}, err
	}
	if !ok {
		return Instance{}, fmt.Errorf("找不到渠道 %s/%s", d.Type, account)
	}
	return got, nil
}

// localMailProbe 逐个打开已选的来源并列一次文件夹。只读元数据，不读任何一封信。
func (m *Manager) localMailProbe(ctx context.Context, d Descriptor, account string, values map[string]string) ([]mailbox.CheckStep, error) {
	in, err := m.localMailInstance(d, account, values)
	if err != nil {
		return nil, err
	}
	cfg := localMailConfigOf(d, in)
	if len(cfg.Sources) == 0 {
		return nil, fmt.Errorf("至少打开一个来源（%s）", localMailSourceNames())
	}
	hub := localmail.NewHub(cfg)
	defer hub.Close()
	steps := make([]mailbox.CheckStep, 0, len(cfg.Sources))
	for _, sc := range cfg.Sources {
		info, _ := localmail.SourceByID(sc.ID)
		st := mailbox.CheckStep{Key: sc.ID, Label: info.Name + " 可读"}
		start := time.Now()
		folders, err := hub.Folders(ctx, sc.ID)
		st.Took = time.Since(start).Milliseconds()
		switch {
		case err == nil:
			st.State, st.Note = "ok", fmt.Sprintf("%d 个文件夹", len(folders))
		case errors.Is(err, localmail.ErrAccess):
			st.State, st.Error = "fail", err.Error()
			st.Note = "这是配置页这个进程的权限。Amazon Quick 能不能读，取决于 Amazon Quick 自己有没有授权，保存不受影响"
		default:
			st.State, st.Error = "fail", err.Error()
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// localMailTest 用于「测试并保存」。没权限不算失败：配置页进程和 Amazon Quick
// 是两个不同的程序，各自授权，这里读不到不代表那边读不到。找不到数据或别的错误
// 才算失败 —— 那多半是选错了来源或填错了目录。
func (m *Manager) localMailTest(ctx context.Context, d Descriptor, account string, values map[string]string) (string, error) {
	steps, err := m.localMailProbe(ctx, d, account, values)
	if err != nil {
		return "", err
	}
	var parts, fails []string
	for _, st := range steps {
		switch {
		case st.State == "ok":
			parts = append(parts, st.Label+"："+st.Note)
		case st.Note != "": // 权限问题
			parts = append(parts, strings.TrimSuffix(st.Label, " 可读")+"：本页面没有完全磁盘访问权限，记得给 Amazon Quick 授权")
		default:
			fails = append(fails, st.Error)
		}
	}
	if len(fails) > 0 {
		return "", errors.New(strings.Join(fails, "；"))
	}
	return strings.Join(parts, " · "), nil
}

func localMailSourceNames() string {
	names := make([]string, 0, len(localmail.Sources))
	for _, s := range localmail.Sources {
		names = append(names, s.Name)
	}
	return strings.Join(names, " / ")
}
