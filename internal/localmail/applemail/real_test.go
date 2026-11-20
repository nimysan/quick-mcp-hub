package applemail

import (
	"context"
	"os"
	"testing"
)

// TestRealMailStats 只在 APPLEMAIL_REAL=1 时对本机真实 ~/Library/Mail 跑一遍，
// 只打印数量、日期和"正文能否定位"，不输出任何主题、地址或正文内容。
// 运行进程需要「完全磁盘访问权限」。
func TestRealMailStats(t *testing.T) {
	if os.Getenv("APPLEMAIL_REAL") != "1" {
		t.Skip("设置 APPLEMAIL_REAL=1 才读取真实邮件数据")
	}
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	mbs, err := s.ListMailboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, m := range mbs {
		total += max(m.Total, 0)
	}
	t.Logf("版本=%s immutable=%v epoch偏移=%d 邮箱数=%d 邮箱统计合计=%d", s.Version(), s.Immutable(), s.epoch, len(mbs), total)

	page, err := s.ListMessages(ctx, Query{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) == 0 {
		t.Fatal("索引里一封邮件都没有")
	}
	t.Logf("最新接收时间=%s", page.Messages[0].DateReceived.Format("2006-01-02 15:04"))
	found := 0
	for _, m := range page.Messages {
		full, err := s.GetMessage(ctx, m.ID, 4096)
		if err != nil {
			t.Fatalf("id=%d: %v", m.ID, err)
		}
		if full.BodyError == "" {
			found++
		}
	}
	t.Logf("最新 %d 封里能定位并解析 .emlx 的有 %d 封", len(page.Messages), found)
}
