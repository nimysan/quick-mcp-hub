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
	// failFolders 为真时 Folders 返回格式不支持（模拟运行中途发现格式变了）。
	failFolders atomic.Bool
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
func (c *closeCounter) Folders(context.Context) ([]Folder, error) {
	c.use()
	if c.failFolders.Load() {
		return nil, errors.Join(ErrUnsupported, errors.New("中途发现格式变了"))
	}
	return nil, nil
}
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

// staleCounter 是能报告过期的 closeCounter。
type staleCounter struct {
	closeCounter
	stale atomic.Bool
}

func (s *staleCounter) Stale() bool { return s.stale.Load() }

// M3：运行中发现格式不支持时，停用来源，但要等正在进行的调用都结束才关闭；
// 停用之后的调用直接拿到说明，不再用这个来源。
func TestHubUnsupportedWaitsForInFlightCalls(t *testing.T) {
	src := &closeCounter{hold: 30 * time.Millisecond}
	register(t, SourceInfo{ID: "x", Name: "来源X", Open: func(OpenOptions) (Source, error) { return src, nil }})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x"}}})
	defer h.Close()
	ctx := context.Background()
	if _, err := h.Search(ctx, "x", Query{}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				h.Search(ctx, "x", Query{})
			} else {
				h.Get(ctx, "x", 1)
			}
		}(i)
	}
	time.Sleep(10 * time.Millisecond) // 让上面的调用先拿到来源
	// 模拟一次调用中途报格式不支持：它自己还持有引用时走 fail。
	e, err := h.acquire(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	h.fail("x", e, errors.Join(ErrUnsupported, errors.New("中途发现格式变了")))
	if src.closes.Load() != 0 {
		t.Error("本次调用还持有来源时不应关闭")
	}
	h.release(e)
	wg.Wait()

	if n := src.misuse.Load(); n != 0 {
		t.Errorf("来源在关闭时还有调用在用，或关闭后又被使用：%d 次", n)
	}
	if n := src.closes.Load(); n != 1 {
		t.Errorf("来源应在最后一个调用结束后关闭一次，实际 %d 次", n)
	}
	if _, err := h.Search(ctx, "x", Query{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("停用后应直接返回格式不支持，得到 %v", err)
	}
}

// 真实调用路径：来源自己的方法返回 ErrUnsupported，并发调用照样不会用到已关闭的来源。
func TestHubUnsupportedFromSourceUnderLoad(t *testing.T) {
	src := &closeCounter{hold: 5 * time.Millisecond}
	register(t, SourceInfo{ID: "x", Name: "来源X", Open: func(OpenOptions) (Source, error) { return src, nil }})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x"}}})
	defer h.Close()
	ctx := context.Background()
	h.Search(ctx, "x", Query{}) // 先打开
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				h.Search(ctx, "x", Query{})
			case 1:
				h.Get(ctx, "x", 1)
			default:
				h.Folders(ctx, "x")
			}
		}(i)
	}
	src.failFolders.Store(true) // Folders 从现在起报格式不支持
	wg.Wait()
	if _, err := h.Folders(ctx, "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("应已停用，得到 %v", err)
	}
	if n := src.misuse.Load(); n != 0 {
		t.Errorf("关闭时还有调用在用，或关闭后又被使用：%d 次", n)
	}
	if n := src.closes.Load(); n != 1 {
		t.Errorf("应只关闭一次，实际 %d 次", n)
	}
}

// 过期的连接（例如 Mail.app 启动后的 immutable 连接）在下一次调用时重开；
// 旧连接等还在用它的调用结束后才关闭。
func TestHubReopensStaleSource(t *testing.T) {
	var opened []*staleCounter
	var mu sync.Mutex
	register(t, SourceInfo{ID: "x", Name: "来源X", Open: func(OpenOptions) (Source, error) {
		mu.Lock()
		defer mu.Unlock()
		s := &staleCounter{closeCounter: closeCounter{hold: 40 * time.Millisecond}}
		opened = append(opened, s)
		return s, nil
	}})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x"}}})
	ctx := context.Background()
	h.Get(ctx, "x", 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); h.Get(ctx, "x", 1) }() // 还在用旧连接
	time.Sleep(10 * time.Millisecond)
	opened[0].stale.Store(true)
	if _, err := h.Get(ctx, "x", 1); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	h.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 2 {
		t.Fatalf("过期后应重开一次，共打开 %d 次", len(opened))
	}
	for i, s := range opened {
		if n := s.misuse.Load(); n != 0 {
			t.Errorf("第 %d 个连接在用时被关闭或关闭后被使用：%d 次", i, n)
		}
		if n := s.closes.Load(); n != 1 {
			t.Errorf("第 %d 个连接应关闭一次，实际 %d 次", i, n)
		}
	}
}

// Hub.Close 不打断正在进行的调用：来源在调用结束后才关闭。
func TestHubCloseWaitsForInFlight(t *testing.T) {
	src := &closeCounter{hold: 50 * time.Millisecond}
	register(t, SourceInfo{ID: "x", Name: "来源X", Open: func(OpenOptions) (Source, error) { return src, nil }})
	h := NewHub(Config{Sources: []SourceConfig{{ID: "x"}}})
	ctx := context.Background()
	h.Folders(ctx, "x")
	done := make(chan struct{})
	go func() { h.Get(ctx, "x", 1); close(done) }()
	time.Sleep(10 * time.Millisecond)
	h.Close()
	<-done
	if src.misuse.Load() != 0 || src.closes.Load() != 1 {
		t.Errorf("misuse=%d closes=%d，想要 0 / 1", src.misuse.Load(), src.closes.Load())
	}
}
