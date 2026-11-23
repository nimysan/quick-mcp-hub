package baidupanshare

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestReal 打真实的 pan.baidu.com。默认跳过：单测不该依赖外网，也不该在 CI 上
// 对别人的站点发请求。
//
// 跑法（链接里带 ?pwd= 的话第二个变量可以省）：
//
//	BAIDUPAN_SHARE_LINK='https://pan.baidu.com/s/1xxxx?pwd=abcd' \
//	  go test ./internal/baidupanshare/ -run TestReal -v
//
// 它验的是假服务器验不了的那部分：真实 init 页里确实带着 shareid 和 share_uk、
// 真实接口确实只接受 root=1 列根目录、中文路径确实能正确编码。
func TestReal(t *testing.T) {
	link := os.Getenv("BAIDUPAN_SHARE_LINK")
	if link == "" {
		t.Skip("设 BAIDUPAN_SHARE_LINK 才跑这个测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s, err := Open(ctx, link, os.Getenv("BAIDUPAN_SHARE_PWD"))
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	t.Logf("surl=%s shareid=%s uk=%s", s.Surl, s.ShareID, s.UK)
	if s.ShareID == "" || s.UK == "" {
		t.Fatal("没能从真实页面里取到 shareid / share_uk")
	}

	root, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("列根目录失败：%v", err)
	}
	if len(root.Entries) == 0 {
		t.Fatal("根目录是空的，这个链接测不出东西")
	}
	for _, e := range root.Entries {
		t.Logf("  %s %s (%d 字节)", map[bool]string{true: "[目录]", false: "[文件]"}[e.IsDir], e.Name, e.Size)
	}

	// 再往下进一级：子目录走的是 dir 参数这条分支，和根目录不是同一条。
	for _, e := range root.Entries {
		if !e.IsDir {
			continue
		}
		sub, err := s.List(ctx, e.Path)
		if err != nil {
			t.Fatalf("列子目录 %s 失败：%v", e.Path, err)
		}
		t.Logf("  进入 %s：%d 条，截断=%v", e.Path, len(sub.Entries), sub.Truncated)
		break
	}

	// 错误码：故意填错一次，确认报出来的是「提取码不对」而不是一个裸 errno。
	if _, err := Open(ctx, link, "0000"); err != ErrWrongCode {
		t.Logf("注意：错误提取码返回的是 %v，不是 ErrWrongCode", err)
	}
}
