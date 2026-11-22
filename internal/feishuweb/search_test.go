package feishuweb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testdata/search.json 是 extension/search.js 在造的首页 + 搜索弹层（testdata/search-page.html）上
// 跑出来的原始候选（testdata/search-regress.mjs 生成并比对）。这里验 mcp-hub 侧的再处理。
func TestParseSearchItemsFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/search.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw RawResult
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	got := ParseSearchItems(raw.Items)
	want := []Item{
		{Title: "2026 周报 第 47 周", URL: "https://x.feishu.cn/docx/AbCdEf1234567890", Type: "docx", Source: "search"},
		{Title: "知识库：周报模板", URL: "https://x.feishu.cn/wiki/WikiTok12345678", Type: "wiki", Source: "search"},
		{Title: "周报统计表", URL: "https://x.feishu.cn/sheets/SheetTok1234567", Type: "sheet", Source: "search"},
		{Title: "周报看板", URL: "https://x.feishu.cn/base/BaseTok12345678", Type: "bitable", Source: "search"},
		{Title: `\[点我\](javascript:alert(1))\<img src=x onerror=alert(1)\>`, URL: "https://x.feishu.cn/docx/Hostile000000001", Type: "docx", Source: "search"},
	}
	if len(got) != len(want) {
		t.Fatalf("得到 %d 条：%+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条\n得到 %+v\n期望 %+v", i, got[i], want[i])
		}
	}
}

// 扩展哪天出错把危险条目带回来，mcp-hub 侧也要全部丢掉。
func TestParseSearchItemsHostile(t *testing.T) {
	raw := []Item{
		{Title: "js", URL: "javascript:alert(1)"},
		{Title: "data", URL: "data:text/html,<script>alert(1)</script>"},
		{Title: "http", URL: "http://x.feishu.cn/docx/AbCdEf1234567890"},
		{Title: "外站", URL: "https://evil.example/docx/AbCdEf1234567890"},
		{Title: "后缀伪装", URL: "https://x.feishu.cn.evil.example/docx/AbCdEf1234567890"},
		{Title: "裸后缀", URL: "https://feishu.cn/docx/AbCdEf1234567890"},
		{Title: "带账号", URL: "https://a:b@x.feishu.cn/docx/AbCdEf1234567890"},
		{Title: "带端口", URL: "https://x.feishu.cn:8443/docx/AbCdEf1234567890"},
		{Title: "文件夹", URL: "https://x.feishu.cn/drive/folder/AbCdEf1234567890"},
		{Title: "token 太短", URL: "https://x.feishu.cn/docx/abc"},
		{Title: "token 带符号", URL: "https://x.feishu.cn/docx/AbCd%22Ef1234"},
		{Title: "控制字符", URL: "https://x.feishu.cn/docx/AbCd\x01Ef1234567890"},
		{Title: "相对地址", URL: "/docx/AbCdEf1234567890"},
		{Title: "", URL: "https://x.larksuite.com/wiki/AbCdEf1234567890?x=1#y"},
		{Title: "补上的标题\u200b", URL: "https://x.larksuite.com/wiki/AbCdEf1234567890"},
	}
	got := ParseSearchItems(raw)
	if len(got) != 1 {
		t.Fatalf("只应留下一条：%+v", got)
	}
	if got[0].URL != "https://x.larksuite.com/wiki/AbCdEf1234567890" || got[0].Title != "补上的标题" || got[0].Type != "wiki" {
		t.Errorf("结果不对：%+v", got[0])
	}

	many := make([]Item, 0, 80)
	for i := 0; i < 80; i++ {
		many = append(many, Item{Title: "t", URL: "https://x.feishu.cn/docx/AbCdEf12345678" + string(rune('A'+i/26)) + string(rune('a'+i%26))})
	}
	if n := len(ParseSearchItems(many)); n != MaxSearchItems {
		t.Errorf("应当截到 %d 条，得到 %d", MaxSearchItems, n)
	}
}

func TestCleanQueryAndHost(t *testing.T) {
	if q, err := CleanQuery(" 周报\t\x00 2026\n"); err != nil || q != "周报 2026" {
		t.Errorf("CleanQuery: %q %v", q, err)
	}
	for _, bad := range []string{"", " \n ", strings.Repeat("a", MaxQueryRunes+1)} {
		if _, err := CleanQuery(bad); err == nil {
			t.Errorf("CleanQuery(%q) 应当报错", bad)
		}
	}
	for in, want := range map[string]string{
		"":                                   "",
		"X.Feishu.cn":                        "x.feishu.cn",
		"https://abc.larksuite.com/docx/xyz": "abc.larksuite.com",
		"a-b.larkoffice.com":                 "a-b.larkoffice.com",
		// 用户 TC05：带 scheme 的租户地址要归一化，而不是原样拼进地址。
		"https://amzn-chn.feishu.cn":           "amzn-chn.feishu.cn",
		"https://amzn-chn.feishu.cn/":          "amzn-chn.feishu.cn",
		"amzn-chn.feishu.cn/":                  "amzn-chn.feishu.cn",
		"amzn-chn.feishu.cn/drive/home/?x=1#y": "amzn-chn.feishu.cn",
		"HTTP://X.Feishu.CN/":                  "x.feishu.cn",
		" x.feishu.cn. ":                       "x.feishu.cn",
	} {
		if got, err := CleanSearchHost(in); err != nil || got != want {
			t.Errorf("CleanSearchHost(%q) = %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"evil.example", "feishu.cn", "accounts.feishu.cn", "www.feishu.cn", "https://www.feishu.cn/404",
		"x.feishu.cn.evil.example", "-x.feishu.cn", "x_y.feishu.cn", "x .feishu.cn", "x.feishu.cn:8443", "https://u@x.feishu.cn",
		"ftp://x.feishu.cn", "https://", "javascript:alert(1)"} {
		got, err := CleanSearchHost(bad)
		var se *StatusErr
		if !errors.As(err, &se) || se.Status != StatusInvalidArgument {
			t.Errorf("CleanSearchHost(%q) 应当回 invalid_argument，得到 %q %v", bad, got, err)
		}
	}
	if _, err := CleanQuery(""); ErrorResult(err).Status != StatusInvalidArgument {
		t.Errorf("空关键词应当是 invalid_argument：%v", err)
	}
}

func TestFinishSearchStatuses(t *testing.T) {
	b := &Bridge{}
	res := b.finishSearch(RawResult{Status: StatusOK, URL: "https://x.feishu.cn/drive/home/?query=secret",
		SearchDiag: &SearchCheck{Strategy: "result_box", Candidates: 0, EmptyHint: true}})
	if res.Status != StatusOK || len(res.Items) != 0 || res.Items == nil || res.Message == "" || res.URL != "https://x.feishu.cn/drive/home/" {
		t.Errorf("空结果不对：%+v", res)
	}
	for st, want := range map[string]string{
		StatusLoginRequired:    StatusLoginRequired,
		StatusStructureUnknown: StatusStructureUnknown,
		StatusNoHost:           StatusInvalidArgument,
		"weird":                StatusError,
	} {
		r := b.finishSearch(RawResult{Status: st, Items: []Item{{Title: "不该出现", URL: "https://x.feishu.cn/docx/AbCdEf1234567890"}}})
		if r.Status != want || r.Message == "" || len(r.Items) != 0 || r.Items == nil {
			t.Errorf("%s: %+v", st, r)
		}
	}
	st := b.Status()
	if st.FirstSearch == nil || st.FirstSearch.Strategy != "result_box" || st.LastSearch == nil {
		t.Errorf("检索自检没记下来：%+v", st)
	}
}

// 扩展连上 -> 交检索 -> 假扩展回候选 -> 拿到规整过的结果；任务里带着规整过的关键词和域名。
func TestBridgeSearchRoundTrip(t *testing.T) {
	b, srv := newBridgeServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := dial(ctx, srv, ExtensionOrigin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","token":"`+testToken+`"}`))
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	jobs := make(chan Job, 1)
	go func() {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m wireMsg
			_ = json.Unmarshal(data, &m)
			if m.Type != "job" || m.Job == nil {
				continue
			}
			jobs <- *m.Job
			res := RawResult{Status: StatusOK, URL: "https://x.feishu.cn/drive/home/",
				Items: []Item{
					{Title: "周报 *加粗*", URL: "https://x.feishu.cn/docx/AbCdEf1234567890?from=search"},
					{Title: "坏的", URL: "javascript:alert(1)"},
				},
				SearchDiag: &SearchCheck{Host: "x.feishu.cn", Strategy: "doc_links", Candidates: 2}}
			out, _ := json.Marshal(wireMsg{Type: "result", ID: m.ID, Result: &res})
			_ = c.Write(ctx, websocket.MessageText, out)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !b.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := b.Search(ctx, "周报", "evil.example"); err == nil {
		t.Fatal("外站域名应当在 mcp-hub 侧就被拒")
	}
	res, err := b.Search(ctx, " 周报 ", "X.feishu.cn")
	if err != nil {
		t.Fatal(err)
	}
	job := <-jobs
	if job.Kind != JobSearch || job.Query != "周报" || job.Host != "x.feishu.cn" || job.URL != "" {
		t.Errorf("任务不对：%+v", job)
	}
	if res.Status != StatusOK || len(res.Items) != 1 || res.Items[0].Title != `周报 \*加粗\*` ||
		res.Items[0].URL != "https://x.feishu.cn/docx/AbCdEf1234567890" || res.Notice == "" {
		t.Fatalf("结果不对：%+v", res)
	}
	if st := b.Status(); st.LastSearch == nil || st.LastSearch.Strategy != "doc_links" {
		t.Errorf("检索自检没记下来：%+v", st)
	}
}

// 没给 host 时用最近读过的文档的租户域名，不是 www.feishu.cn（用户 BUG-05）。
func TestRecentHost(t *testing.T) {
	b := &Bridge{}
	if h := b.recentHost(); h != "" {
		t.Errorf("没读过文档时应当回空串，得到 %q", h)
	}
	b.remember(Item{URL: "https://www.feishu.cn/docx/AbCdEf1234567890"})
	b.remember(Item{URL: "https://amzn-chn.feishu.cn/docx/AbCdEf1234567890"})
	if h := b.recentHost(); h != "amzn-chn.feishu.cn" {
		t.Errorf("recentHost = %q", h)
	}
}
