package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/localmail"
)

// 本机邮件的工具必须全部标成只读，而且一个写工具都没有。来源打不开时
// 调用要返回工具错误（IsError），不能让会话断掉。
func TestLocalMailToolsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	hub := localmail.NewHub(localmail.Config{Sources: []localmail.SourceConfig{
		{ID: localmail.SourceOutlookLegacy, Dir: t.TempDir()}, // 空目录：打不开
	}})
	if n := RegisterLocalMail(srv, hub); n != 3 {
		t.Fatalf("注册 %d 个工具，想要 3", n)
	}
	if n0 := RegisterLocalMail(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil),
		localmail.NewHub(localmail.Config{})); n0 != 0 {
		t.Errorf("没有来源时不应注册工具")
	}

	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if !strings.HasPrefix(tl.Name, "localmail_") {
			t.Errorf("意外的工具 %s", tl.Name)
		}
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s 没有 readOnlyHint", tl.Name)
		}
	}

	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "localmail_search", Arguments: map[string]any{"limit": 3}})
	if err != nil {
		t.Fatalf("来源打不开应是工具错误而不是协议错误: %v", err)
	}
	if !r.IsError {
		t.Error("来源打不开时应返回 IsError")
	}
}

// since/before 是本地日历日：东八区的「2026-11-21」从本地零点开始，不是 UTC 零点。
func TestParseLocalDateUsesLocalZone(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("CST", 8*3600)
	defer func() { time.Local = old }()

	got, err := parseLocalDate("2026-11-21")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 21, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("parseLocalDate = %v，想要本地零点 %v", got, want)
	}
	// 本地 11-21 03:00 收到的信（UTC 11-20 19:00）必须落在 since=11-21 里。
	if early := time.Date(2026, 11, 20, 19, 0, 0, 0, time.UTC); early.Before(got) {
		t.Error("本地凌晨收到的信被 since 排除了")
	}
	if z, err := parseLocalDate(""); err != nil || !z.IsZero() {
		t.Errorf("空串应为零值，得到 %v %v", z, err)
	}
	if _, err := parseLocalDate("2026/11/21"); err == nil {
		t.Error("格式不对应报错")
	}
}
