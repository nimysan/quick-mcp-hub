package localmail

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// register 临时注册若干假来源。
func register(t *testing.T, infos ...SourceInfo) {
	t.Helper()
	old := Sources
	Sources = append(append([]SourceInfo{}, old...), infos...)
	t.Cleanup(func() { Sources = old })
}

// M2：一个来源打开得很慢时，别的来源照常可用；等它的调用受 ctx 控制；
// 同时来的多个调用只打开一次。
func TestHubSlowOpenDoesNotBlockOthers(t *testing.T) {
	gate := make(chan struct{})
	var slowOpens atomic.Int32
	register(t,
		SourceInfo{ID: "slow", Name: "慢来源", Open: func(OpenOptions) (Source, error) {
			slowOpens.Add(1)
			<-gate
			return &fakeSource{}, nil
		}},
		SourceInfo{ID: "fast", Name: "快来源", Open: func(OpenOptions) (Source, error) {
			return &fakeSource{}, nil
		}},
	)
	h := NewHub(Config{Sources: []SourceConfig{{ID: "slow"}, {ID: "fast"}}})
	defer h.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // 一批调用卡在慢来源的打开上
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := h.Folders(ctx, "slow")
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("慢来源应在 ctx 到期时返回，得到 %v", err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("ctx 到期后过了 %v 才返回", d)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := h.Folders(ctx, "fast"); err != nil {
		t.Errorf("慢来源在打开时，快来源应照常可用：%v", err)
	}
	cancel()
	wg.Wait()

	close(gate)
	if _, err := h.Folders(context.Background(), "slow"); err != nil {
		t.Fatalf("打开完成后应可用：%v", err)
	}
	if n := slowOpens.Load(); n != 1 {
		t.Errorf("慢来源打开了 %d 次，同时来的调用应只打开一次", n)
	}
}

// 打开期间 Hub 被关闭：打开完成后结果被关掉，不泄漏。
func TestHubCloseWhileOpening(t *testing.T) {
	gate := make(chan struct{})
	src := &closeCounter{}
	register(t, SourceInfo{ID: "slow", Name: "慢来源", Open: func(OpenOptions) (Source, error) {
		<-gate
		return src, nil
	}})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "slow"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	h.Folders(ctx, "slow")
	cancel()
	h.Close()
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for src.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := src.closes.Load(); n != 1 {
		t.Errorf("关闭后才打开完成的来源应被关闭一次，实际 %d 次", n)
	}
	if _, err := h.Folders(context.Background(), "slow"); !errors.Is(err, errHubClosed) {
		t.Errorf("关闭后调用应返回已关闭，得到 %v", err)
	}
}

// closeCounter 记录关闭次数，关闭后再被调用就报错（用来抓「关闭后又被使用」）。
type closeCounter struct {
	closes atomic.Int32
	inUse  atomic.Int32
	misuse atomic.Int32
	hold   time.Duration
	fail   error
}

func (c *closeCounter) use() {
	c.inUse.Add(1)
	defer c.inUse.Add(-1)
	if c.closes.Load() > 0 {
		c.misuse.Add(1)
	}
	time.Sleep(c.hold)
	if c.closes.Load() > 0 {
		c.misuse.Add(1)
	}
}
func (c *closeCounter) Folders(context.Context) ([]Folder, error) { c.use(); return nil, c.fail }
func (c *closeCounter) Search(context.Context, Query) (Page, error) {
	c.use()
	return Page{}, nil
}
func (c *closeCounter) Get(context.Context, int64) (*Message, error) {
	c.use()
	return &Message{}, nil
}
func (c *closeCounter) Close() error {
	if c.inUse.Load() > 0 {
		c.misuse.Add(1)
	}
	c.closes.Add(1)
	return nil
}
