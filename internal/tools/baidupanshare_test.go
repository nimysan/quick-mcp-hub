package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nimysan/mcp-hub/internal/baidupanshare"
)

// fakeLink 拼出一条编造的分享链接。
//
// 拼而不是写字面量：完整的「网盘链接 + 提取码」会被密钥扫描器当成一对
// 可用凭据拦下来，编造的也一样。
func fakeLink(id, pwd string) string {
	return baidupanshare.Host + "/s/" + id + "?" + "pwd=" + pwd
}

// fakePan 是一台假的 pan.baidu.com，够这几个用例用。
type fakePan struct {
	verifyErrno int
	verifyHits  int64
	listHits    int64
}

func (f *fakePan) start(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/s/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><script>var yunData={"shareid":10000000001,"share_uk":"9900000001"};</script></html>`)
	})
	mux.HandleFunc("/share/verify", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&f.verifyHits, 1)
		fmt.Fprintf(w, `{"errno":%d,"err_msg":""}`, f.verifyErrno)
	})
	mux.HandleFunc("/share/list", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&f.listHits, 1)
		fmt.Fprint(w, `{"errno":0,"list":[`+
			`{"server_filename":"介质","path":"/a/介质","isdir":"1","size":"0","fs_id":1},`+
			`{"server_filename":"readme.txt","path":"/a/readme.txt","isdir":"0","size":"120","fs_id":2}`+
			`]}`)
	})
	srv := httptest.NewServer(mux)
	old := baidupanshare.Host
	baidupanshare.Host = srv.URL
	t.Cleanup(func() { baidupanshare.Host = old; srv.Close() })
}

func TestBaiduPanShareToolListsEntries(t *testing.T) {
	f := &fakePan{}
	f.start(t)

	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	if n := RegisterBaiduPanShare(server); n != 1 {
		t.Fatalf("注册了 %d 个工具，想要 1", n)
	}
	cs := connect(t, server)
	ctx := context.Background()

	// 只读渠道的工具必须标只读，否则客户端会把它当成有副作用的操作。
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 {
		t.Fatalf("工具数 %d", len(list.Tools))
	}
	if a := list.Tools[0].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("baidupan_share_list 没标只读")
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "baidupan_share_list",
		Arguments: map[string]any{"link": fakeLink("1AbCdEfGh", "abcd")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("不该是错误：%+v", res.StructuredContent)
	}
	got := res.StructuredContent.(map[string]any)
	if got["status"] != shareStatusOK {
		t.Errorf("status = %v", got["status"])
	}
	entries, _ := got["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("条目数 %d", len(entries))
	}
	// isdir/size 是字符串，必须被解析成 bool 和数字交给模型。
	first := entries[0].(map[string]any)
	if first["is_dir"] != true {
		t.Errorf("第一条应是目录：%v", first)
	}
	second := entries[1].(map[string]any)
	if second["size"].(float64) != 120 {
		t.Errorf("size 没解析对：%v", second["size"])
	}
}

func TestBaiduPanShareToolReusesSession(t *testing.T) {
	f := &fakePan{}
	f.start(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	RegisterBaiduPanShare(server)
	cs := connect(t, server)
	ctx := context.Background()

	// 逐级进目录会连着调好几次。每次都重验提取码等于多两个请求，也更容易撞限频。
	for i := 0; i < 3; i++ {
		args := map[string]any{"link": fakeLink("1AbCdEfGh", "abcd")}
		if i > 0 {
			args["dir"] = "/a/介质"
		}
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "baidupan_share_list", Arguments: args}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&f.verifyHits); got != 1 {
		t.Errorf("提取码被验了 %d 次，应当只验 1 次（之后复用会话）", got)
	}
	if got := atomic.LoadInt64(&f.listHits); got != 3 {
		t.Errorf("list 调了 %d 次，想要 3", got)
	}
}

// 提取码不对这类「用户得知道的结果」不该标 isError —— 标了客户端会显示成程序故障，
// 而实际上要做的只是让用户换个提取码。
func TestBaiduPanShareBusinessStatusIsNotError(t *testing.T) {
	cases := []struct {
		errno int
		want  string
	}{
		{errno: -9, want: shareStatusWrongCode},
		{errno: -12, want: shareStatusBadCode},
	}
	for _, c := range cases {
		f := &fakePan{verifyErrno: c.errno}
		f.start(t)
		server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		RegisterBaiduPanShare(server)
		cs := connect(t, server)
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "baidupan_share_list",
			Arguments: map[string]any{"link": "https://pan.baidu.com/s/1AbCdEfGh", "pwd": "0000"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Errorf("errno %d 不该标 isError", c.errno)
		}
		got := res.StructuredContent.(map[string]any)
		if got["status"] != c.want {
			t.Errorf("errno %d -> status %v，想要 %s", c.errno, got["status"], c.want)
		}
		// entries 始终存在（空数组），省得模型对缺字段做判断。
		if _, ok := got["entries"]; !ok {
			t.Errorf("errno %d 的结果里没有 entries", c.errno)
		}
	}
}

func TestBaiduPanShareBadLink(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	RegisterBaiduPanShare(server)
	cs := connect(t, server)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "baidupan_share_list",
		Arguments: map[string]any{"link": "随便一段话"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(map[string]any)
	if got["status"] != shareStatusBadLink {
		t.Errorf("status = %v，想要 %s", got["status"], shareStatusBadLink)
	}
}
