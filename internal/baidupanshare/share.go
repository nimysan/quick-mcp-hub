package baidupanshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Host 是接口的站点。独立成变量只为测试能换成 httptest 的地址 —— 不是配置项，
// 别下发给用户填。
var Host = "https://pan.baidu.com"

// UserAgent 必须像个浏览器：这些是网页版接口，拿默认的 Go-http-client 会被拒。
const UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// appID 和 channel 是网页版自己带的固定参数，不是我们申请的应用。
const (
	appID   = "250528"
	channel = "chunlei"
)

// pageSize 是一页的条数上限，百度不接受更大的值。
const pageSize = 100

// maxPages 封住翻页的总量：一个目录 5000 条已经远超模型能用的规模，再多只会
// 把上下文灌满。到顶时结果里会标出被截断，不假装取全了。
const maxPages = 50

// retryWaits 是被限频（errno -62）后的等待序列，取自此前实测撞出来的值。
// 刻意不做指数计算：这三档是观察来的，比算出来的数更有依据。
var retryWaits = []time.Duration{45 * time.Second, 90 * time.Second, 180 * time.Second}

// 可判别的失败。调用方要据此决定是让用户改输入，还是等一会儿重试。
var (
	// ErrWrongCode 提取码错了（errno -9）。
	ErrWrongCode = errors.New("提取码不对")
	// ErrBadCode 提取码格式不对（errno -12）：空的或长度不对。
	ErrBadCode = errors.New("提取码格式不对（通常是 4 位）")
	// ErrRateLimited 被百度限频（errno -62），退避重试仍未恢复。
	ErrRateLimited = errors.New("百度网盘限频，过几分钟再试")
	// ErrShareGone 链接失效、被删或被取消分享。
	ErrShareGone = errors.New("分享链接已失效或被取消")
	// ErrBadLink 不是一个能认出来的分享链接。
	ErrBadLink = errors.New("不是百度网盘分享链接")
)

// Entry 是分享里的一个文件或目录。字段只留模型用得上的，不透传整个响应 ——
// 原始响应一条就有二十多个字段，大部分是网页版渲染用的。
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	// Size 是字节数；目录是 0。
	Size int64 `json:"size,omitempty"`
	// FsID 是这个条目在百度侧的 id，转存时要用它，所以留着。
	FsID string `json:"fs_id,omitempty"`
}

// Session 是一次验证过提取码之后的匿名会话。
//
// 它不是使用者的凭据 —— 里面只有这个公开分享链接的访问资格，换个人拿去也只能
// 读同一个分享。所以不进密码管理器，由调用方按 surl 在内存里缓存即可。
type Session struct {
	Surl    string
	ShareID string
	UK      string
	// Created 用来判断缓存是否该弃用。接口侧的有效期没有公开说明，观察到的量级
	// 是小时级，调用方自己定 TTL。
	Created time.Time

	client *http.Client
}

// Listing 是一个目录的内容。
type Listing struct {
	Dir     string  `json:"dir"`
	Entries []Entry `json:"entries"`
	// Truncated 为真表示到了 maxPages 上限，后面还有没取。宁可明说，也不让调用方
	// 以为自己看到了全部。
	Truncated bool `json:"truncated,omitempty"`
}

// ParseLink 从各种形态的分享链接里取出 surl 和链接自带的提取码。
//
// 认这几种：
//
//	https://pan.baidu.com/s/1AbCdEf?pwd=abcd
//	https://pan.baidu.com/s/1AbCdEf
//	pan.baidu.com/s/1AbCdEf
//	1AbCdEf        （直接给 id）
//
// 返回的 surl 已经去掉开头那个 "1" —— 接口参数要的是去掉之后的部分，这是最容易
// 写错的一处。
func ParseLink(link string) (surl, pwd string, err error) {
	s := strings.TrimSpace(link)
	if s == "" {
		return "", "", ErrBadLink
	}
	// 用户常常连「链接：... 提取码：abcd」整段粘进来，先把 id 和 pwd 捞出来。
	if m := regexp.MustCompile(`/s/1([0-9A-Za-z_-]+)`).FindStringSubmatch(s); m != nil {
		surl = m[1]
	} else if m := regexp.MustCompile(`^1([0-9A-Za-z_-]{5,})$`).FindStringSubmatch(s); m != nil {
		surl = m[1]
	} else {
		return "", "", ErrBadLink
	}
	if m := regexp.MustCompile(`[?&]pwd=([0-9A-Za-z]+)`).FindStringSubmatch(s); m != nil {
		pwd = m[1]
	} else if m := regexp.MustCompile(`提取码[:：]\s*([0-9A-Za-z]{4})`).FindStringSubmatch(s); m != nil {
		pwd = m[1]
	}
	return surl, pwd, nil
}

// Open 验证提取码，拿到一个可以用来列目录的会话。
//
// 两步：取一次 init 页（拿 cookie、shareid、share_uk），再 POST 提取码换 BDCLND。
func Open(ctx context.Context, link, pwd string) (*Session, error) {
	surl, linkPwd, err := ParseLink(link)
	if err != nil {
		return nil, err
	}
	if pwd == "" {
		pwd = linkPwd
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	s := &Session{Surl: surl, Created: time.Now(), client: &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}}

	if err := s.loadInit(ctx); err != nil {
		return nil, err
	}
	if err := s.verify(ctx, pwd); err != nil {
		return nil, err
	}
	return s, nil
}

// loadInit 取分享页。跟随 302 之后落在 /share/init，HTML 里带着 shareid 和
// share_uk —— 这是不用再调一个接口去查 id 的原因。
func (s *Session) loadInit(ctx context.Context) error {
	u := Host + "/s/1" + s.Surl
	body, err := s.get(ctx, u, u)
	if err != nil {
		return err
	}
	s.ShareID = firstSubmatch(`"shareid"\s*:\s*"?(\d+)"?`, body)
	s.UK = firstSubmatch(`"share_uk"\s*:\s*"?(\d+)"?`, body)
	if s.ShareID == "" || s.UK == "" {
		// 链接失效时百度回的是一个「链接不存在」的页面，里面没有这两个 id。
		return ErrShareGone
	}
	return nil
}

// verify 用提取码换 BDCLND。
func (s *Session) verify(ctx context.Context, pwd string) error {
	q := url.Values{
		"surl":       {s.Surl},
		"t":          {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		"channel":    {channel},
		"web":        {"1"},
		"app_id":     {appID},
		"bdstoken":   {""},
		"logid":      {""},
		"clienttype": {"0"},
	}
	form := url.Values{"pwd": {pwd}, "vcode": {""}, "vcode_str": {""}}
	ref := Host + "/share/init?surl=" + s.Surl

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		Host+"/share/verify?"+q.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", ref)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var out struct {
		Errno  int    `json:"errno"`
		ErrMsg string `json:"err_msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("verify 返回的不是 JSON：%.120s", raw)
	}
	return errnoToError(out.Errno, out.ErrMsg)
}

// List 列一个目录。dir 为空表示根目录。
//
// 根目录和子目录是两种参数形态：根要 root=1，子目录要 dir=<完整路径>。拿 dir=/
// 列根目录会得到 errno 2，不是空结果 —— 这一条只有实测才会知道。
func (s *Session) List(ctx context.Context, dir string) (Listing, error) {
	out := Listing{Dir: dir}
	if out.Dir == "" {
		out.Dir = "/"
	}
	for page := 1; page <= maxPages; page++ {
		batch, err := s.listPage(ctx, dir, page)
		if err != nil {
			return Listing{}, err
		}
		out.Entries = append(out.Entries, batch...)
		if len(batch) < pageSize {
			return out, nil
		}
	}
	out.Truncated = true
	return out, nil
}

func (s *Session) listPage(ctx context.Context, dir string, page int) ([]Entry, error) {
	q := url.Values{
		"uk":         {s.UK},
		"shareid":    {s.ShareID},
		"order":      {"other"},
		"desc":       {"1"},
		"showempty":  {"0"},
		"web":        {"1"},
		"page":       {strconv.Itoa(page)},
		"num":        {strconv.Itoa(pageSize)},
		"channel":    {channel},
		"app_id":     {appID},
		"bdstoken":   {""},
		"logid":      {""},
		"clienttype": {"0"},
	}
	if dir == "" || dir == "/" {
		q.Set("root", "1")
	} else {
		q.Set("dir", dir)
	}

	raw, err := s.get(ctx, Host+"/share/list?"+q.Encode(), Host+"/s/1"+s.Surl)
	if err != nil {
		return nil, err
	}
	var out struct {
		Errno int `json:"errno"`
		List  []struct {
			Name  string  `json:"server_filename"`
			Path  string  `json:"path"`
			IsDir flexInt `json:"isdir"`
			Size  flexInt `json:"size"`
			FsID  flexInt `json:"fs_id"`
		} `json:"list"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("list 返回的不是 JSON：%.120s", raw)
	}
	if err := errnoToError(out.Errno, ""); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(out.List))
	for _, f := range out.List {
		entries = append(entries, Entry{
			Name:  f.Name,
			Path:  f.Path,
			IsDir: f.IsDir.Int64() == 1,
			Size:  f.Size.Int64(),
			FsID:  strconv.FormatInt(f.FsID.Int64(), 10),
		})
	}
	return entries, nil
}

// get 发一个带 Referer 的 GET，并在被限频时按 retryWaits 退避重试。
//
// 只对 -62 重试：网络错误、链接失效、提取码不对都不是「等一下就会好」的问题，
// 重试只会让人多等。
func (s *Session) get(ctx context.Context, u, referer string) (string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		body, err := s.getOnce(ctx, u, referer)
		if err == nil {
			if !strings.Contains(body, `"errno":-62`) {
				return body, nil
			}
			lastErr = ErrRateLimited
		} else {
			return "", err
		}
		if attempt >= len(retryWaits) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryWaits[attempt]):
		}
	}
}

func (s *Session) getOnce(ctx context.Context, u, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", referer)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 分享页有十几 KB，目录响应随条数变化；封 8MB 够用，也挡住异常大的响应。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("百度返回 HTTP %d", resp.StatusCode)
	}
	return string(raw), nil
}

func errnoToError(errno int, msg string) error {
	switch errno {
	case 0:
		return nil
	case -9:
		return ErrWrongCode
	case -12:
		return ErrBadCode
	case -62:
		return ErrRateLimited
	case 2:
		// 调用方传了个站不住的 dir。把原因说清，否则看到 "errno 2" 没人知道何意。
		return errors.New("目录参数不对（列根目录要留空，不要传 /）")
	case -1, 105, 117:
		return ErrShareGone
	}
	if msg != "" {
		return fmt.Errorf("百度返回 errno %d：%s", errno, msg)
	}
	return fmt.Errorf("百度返回 errno %d", errno)
}

func firstSubmatch(pattern, s string) string {
	if m := regexp.MustCompile(pattern).FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// flexInt 认数字也认字符串形式的数字。
//
// 同一个响应里 isdir 和 size 是字符串（"1"、"0"），fs_id 有时是数字 —— 写死成
// 任一种都会在另一种上解析失败。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

func (f flexInt) Int64() int64 { return int64(f) }
