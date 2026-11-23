package baidupanshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseLink(t *testing.T) {
	cases := []struct {
		in       string
		surl     string
		pwd      string
		wantFail bool
	}{
		// 开头那个 "1" 必须被去掉：接口参数要的是去掉之后的部分，
		// 整条实现里这是最容易写错的一处。
		{in: "https://pan.baidu.com/s/1RU2huy0nkpJ1_fMxDMIM3g?pwd=mccz", surl: "RU2huy0nkpJ1_fMxDMIM3g", pwd: "mccz"},
		{in: "https://pan.baidu.com/s/1RU2huy0nkpJ1_fMxDMIM3g", surl: "RU2huy0nkpJ1_fMxDMIM3g"},
		{in: "pan.baidu.com/s/1AbCdEfGh", surl: "AbCdEfGh"},
		{in: "1AbCdEfGh", surl: "AbCdEfGh"},
		// 用户常把整段话粘进来。
		{in: "介质下载链接：https://pan.baidu.com/s/1RU2huy0nkpJ1_fMxDMIM3g?pwd=mccz 提取码: mccz",
			surl: "RU2huy0nkpJ1_fMxDMIM3g", pwd: "mccz"},
		{in: "链接 https://pan.baidu.com/s/1AbCdEfGh 提取码：9k2m", surl: "AbCdEfGh", pwd: "9k2m"},
		{in: "", wantFail: true},
		{in: "https://example.com/s/1abc", surl: "abc"}, // 只认 /s/1 这个形状，域名不限
		{in: "随便一段话", wantFail: true},
	}
	for _, c := range cases {
		surl, pwd, err := ParseLink(c.in)
		if c.wantFail {
			if err == nil {
				t.Errorf("ParseLink(%q) 应该失败，却得到 surl=%q", c.in, surl)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLink(%q) 失败：%v", c.in, err)
			continue
		}
		if surl != c.surl || pwd != c.pwd {
			t.Errorf("ParseLink(%q) = (%q,%q)，想要 (%q,%q)", c.in, surl, pwd, c.surl, c.pwd)
		}
	}
}

// fake 是一台假的 pan.baidu.com，按真实响应的形状回话。
type fake struct {
	verifyErrno int
	listErrno   int
	// pages[i] 是第 i+1 页的条目数。
	pages []int
	// noIDs 模拟失效链接：页面里没有 shareid / share_uk。
	noIDs bool
	// rateLimitFirst 让前 N 次 list 回 -62，用来验退避。
	rateLimitFirst int

	gotRoot    []string // 每次 list 收到的 root 参数
	gotDir     []string // 每次 list 收到的 dir 参数
	gotPage    []string
	gotReferer []string
	gotUA      []string
	listCalls  int
}

func (f *fake) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		f.gotReferer = append(f.gotReferer, r.Header.Get("Referer"))
		f.gotUA = append(f.gotUA, r.Header.Get("User-Agent"))
		if f.noIDs {
			fmt.Fprint(w, `<html>你访问的页面不存在</html>`)
			return
		}
		// 真实 init 页就是这样把两个 id 混在一大段 yunData 里。
		fmt.Fprint(w, `<html><script>var yunData={"shareid":52961973064,`+
			`"share_uk":"1100220948307","uk":0};</script>输入提取码</html>`)
	})

	mux.HandleFunc("/share/verify", func(w http.ResponseWriter, r *http.Request) {
		if got := r.FormValue("pwd"); got == "" && f.verifyErrno == 0 {
			t.Errorf("verify 没收到 pwd")
		}
		fmt.Fprintf(w, `{"errno":%d,"err_msg":"","randsk":"x"}`, f.verifyErrno)
	})

	mux.HandleFunc("/share/list", func(w http.ResponseWriter, r *http.Request) {
		f.listCalls++
		f.gotRoot = append(f.gotRoot, r.URL.Query().Get("root"))
		f.gotDir = append(f.gotDir, r.URL.Query().Get("dir"))
		f.gotPage = append(f.gotPage, r.URL.Query().Get("page"))
		f.gotReferer = append(f.gotReferer, r.Header.Get("Referer"))

		if f.listCalls <= f.rateLimitFirst {
			fmt.Fprint(w, `{"errno":-62,"list":[]}`)
			return
		}
		if f.listErrno != 0 {
			fmt.Fprintf(w, `{"errno":%d,"list":[]}`, f.listErrno)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		n := 0
		// 翻页时第 N 次调用对应第 N 页；减去被限频挡掉的那几次。
		idx := page - 1
		if idx < len(f.pages) {
			n = f.pages[idx]
		}
		var b strings.Builder
		b.WriteString(`{"errno":0,"list":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			// isdir 和 size 刻意写成字符串：真实响应就是字符串，
			// 解析成 int 会直接失败。
			fmt.Fprintf(&b, `{"server_filename":"f%d-p%d","path":"/d/f%d","isdir":"%d",`+
				`"size":"%d","fs_id":66220903824977%d}`,
				i, page, i, i%2, i*1024, i)
		}
		b.WriteString(`]}`)
		fmt.Fprint(w, b.String())
	})

	srv := httptest.NewServer(mux)
	old := Host
	Host = srv.URL
	t.Cleanup(func() { Host = old; srv.Close() })
	return srv
}

func TestOpenAndListRoot(t *testing.T) {
	f := &fake{pages: []int{3}}
	f.server(t)

	s, err := Open(context.Background(), "https://pan.baidu.com/s/1AbCdEfGh?pwd=mccz", "")
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	if s.ShareID != "52961973064" || s.UK != "1100220948307" {
		t.Fatalf("没从页面里取对 id：shareid=%q uk=%q", s.ShareID, s.UK)
	}

	got, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List 失败：%v", err)
	}
	if len(got.Entries) != 3 {
		t.Fatalf("想要 3 条，得到 %d", len(got.Entries))
	}
	// 根目录必须发 root=1、且不能发 dir —— 发 dir=/ 真实接口会回 errno 2。
	if f.gotRoot[0] != "1" {
		t.Errorf("列根目录没带 root=1，带的是 %q", f.gotRoot[0])
	}
	if f.gotDir[0] != "" {
		t.Errorf("列根目录不该带 dir，却带了 %q", f.gotDir[0])
	}
	// isdir/size 是字符串，要解析成 bool/int64。
	if !got.Entries[1].IsDir {
		t.Errorf("第 2 条的 isdir=\"1\" 应解析为目录")
	}
	if got.Entries[2].Size != 2048 {
		t.Errorf("size 字符串没解析对：%d", got.Entries[2].Size)
	}
	if got.Entries[0].FsID == "" {
		t.Errorf("fs_id 丢了")
	}
	// 每个请求都要带 Referer 和浏览器 UA，否则真实接口拒请求。
	for i, r := range f.gotReferer {
		if r == "" {
			t.Errorf("第 %d 个请求没带 Referer", i)
		}
	}
	if !strings.Contains(f.gotUA[0], "Chrome") {
		t.Errorf("UA 不像浏览器：%q", f.gotUA[0])
	}
}

func TestListSubdirUsesDirParam(t *testing.T) {
	f := &fake{pages: []int{2}}
	f.server(t)
	s, err := Open(context.Background(), "1AbCdEfGh", "mccz")
	if err != nil {
		t.Fatal(err)
	}
	// 带中文和空格的真实路径必须被正确编码。
	const dir = "/山海代运营/交付介质/10月/E中文介质"
	if _, err := s.List(context.Background(), dir); err != nil {
		t.Fatalf("List 失败：%v", err)
	}
	if f.gotDir[0] != dir {
		t.Errorf("dir 参数编码错了：收到 %q，想要 %q", f.gotDir[0], dir)
	}
	if f.gotRoot[0] != "" {
		t.Errorf("列子目录不该带 root，却带了 %q", f.gotRoot[0])
	}
}

func TestListPaginates(t *testing.T) {
	// 第一页满 100 条就必须继续翻 —— 不翻会静悄悄只看到前 100 个。
	f := &fake{pages: []int{pageSize, pageSize, 7}}
	f.server(t)
	s, err := Open(context.Background(), "1AbCdEfGh", "mccz")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.List(context.Background(), "/d")
	if err != nil {
		t.Fatal(err)
	}
	if want := pageSize*2 + 7; len(got.Entries) != want {
		t.Fatalf("想要 %d 条，得到 %d", want, len(got.Entries))
	}
	if got.Truncated {
		t.Errorf("没到上限，不该标 Truncated")
	}
	if strings.Join(f.gotPage, ",") != "1,2,3" {
		t.Errorf("page 参数没递增：%v", f.gotPage)
	}
}

func TestListTruncatesAtCap(t *testing.T) {
	// 每页都满的情况下必须在 maxPages 停住，并如实标出被截断。
	pages := make([]int, maxPages+5)
	for i := range pages {
		pages[i] = pageSize
	}
	f := &fake{pages: pages}
	f.server(t)
	s, _ := Open(context.Background(), "1AbCdEfGh", "mccz")
	got, err := s.List(context.Background(), "/d")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Errorf("到了 maxPages 必须标 Truncated，否则调用方以为取全了")
	}
	if len(got.Entries) != pageSize*maxPages {
		t.Errorf("条数不对：%d", len(got.Entries))
	}
}

func TestErrnoMapping(t *testing.T) {
	cases := []struct {
		verify int
		list   int
		want   error
	}{
		{verify: -9, want: ErrWrongCode},
		{verify: -12, want: ErrBadCode},
		{verify: -62, want: ErrRateLimited},
		{list: -9, want: ErrWrongCode},
		{list: -1, want: ErrShareGone},
	}
	for _, c := range cases {
		f := &fake{verifyErrno: c.verify, listErrno: c.list, pages: []int{1}}
		f.server(t)
		s, err := Open(context.Background(), "1AbCdEfGh", "mccz")
		if err == nil && c.list != 0 {
			_, err = s.List(context.Background(), "")
		}
		if !errors.Is(err, c.want) {
			t.Errorf("verify=%d list=%d 得到 %v，想要 %v", c.verify, c.list, err, c.want)
		}
	}
}

func TestErrno2SaysWhatToDo(t *testing.T) {
	// errno 2 最常见的原因就是拿 dir=/ 去列根目录。光报 "errno 2" 没人看得懂。
	f := &fake{listErrno: 2, pages: []int{1}}
	f.server(t)
	s, _ := Open(context.Background(), "1AbCdEfGh", "mccz")
	_, err := s.List(context.Background(), "/d")
	if err == nil || !strings.Contains(err.Error(), "根目录") {
		t.Errorf("errno 2 的说明要指出根目录该怎么传，得到：%v", err)
	}
}

func TestRateLimitRetriesThenSucceeds(t *testing.T) {
	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryWaits = old }()

	f := &fake{pages: []int{2}, rateLimitFirst: 2}
	f.server(t)
	s, err := Open(context.Background(), "1AbCdEfGh", "mccz")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.List(context.Background(), "/d")
	if err != nil {
		t.Fatalf("前两次 -62、第三次成功，应该重试到成功，却得到 %v", err)
	}
	if len(got.Entries) != 2 {
		t.Errorf("条数不对：%d", len(got.Entries))
	}
}

func TestRateLimitGivesUpWithClearError(t *testing.T) {
	old := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryWaits = old }()

	f := &fake{pages: []int{2}, rateLimitFirst: 100}
	f.server(t)
	s, err := Open(context.Background(), "1AbCdEfGh", "mccz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(context.Background(), "/d"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("一直 -62 应该报 ErrRateLimited，得到 %v", err)
	}
}

func TestDeadShareLink(t *testing.T) {
	f := &fake{noIDs: true}
	f.server(t)
	if _, err := Open(context.Background(), "1AbCdEfGh", "mccz"); !errors.Is(err, ErrShareGone) {
		t.Errorf("页面里没有 id 时应报 ErrShareGone，得到 %v", err)
	}
}

func TestFlexInt(t *testing.T) {
	// 同一个响应里 isdir/size 是字符串、fs_id 是数字，两种都得认。
	var v struct {
		A flexInt `json:"a"`
		B flexInt `json:"b"`
		C flexInt `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a":"12","b":34,"c":null}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.Int64() != 12 || v.B.Int64() != 34 || v.C.Int64() != 0 {
		t.Errorf("得到 %d %d %d", v.A.Int64(), v.B.Int64(), v.C.Int64())
	}
}
