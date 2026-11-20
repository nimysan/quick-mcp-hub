package tools

import (
	"context"
	"strings"
	"testing"

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
