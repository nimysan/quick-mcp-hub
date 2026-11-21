package feishuweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

// DefaultUIPort 和 main.go 里 -ui-port 的默认值一致。
const DefaultUIPort = 7801

// UIPortEnv 让 MCP 进程找到改过端口的配置页面。MCP 进程由客户端拉起，
// 拿不到 -ui-port 参数，所以用环境变量。
const UIPortEnv = "MCPHUB_UI_PORT"

// JobPath 是配置页面上接收任务的端点。
const JobPath = "/api/feishu-web/job"

// Client 是 MCP 进程那一端：把任务交给本机配置页面进程里的 Bridge。
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient 按环境变量或默认端口指向本机配置页面。
func NewClient() *Client {
	port := DefaultUIPort
	if v, err := strconv.Atoi(os.Getenv(UIPortEnv)); err == nil && v > 0 && v < 65536 {
		port = v
	}
	return &Client{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		// 读一篇长文档要开标签页、等加载、滚到底，给足时间；Bridge 侧自己有 90 秒上限。
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

// ErrHubNotRunning 表示配置页面（托盘程序）没在跑。
var ErrHubNotRunning = errors.New("mcp-hub 配置页面（托盘程序）没在运行：飞书网页文档要靠它和浏览器扩展通信，先双击打开 mcp-hub")

// Do 交一个任务并等结果。
func (c *Client) Do(ctx context.Context, kind, docURL string) (Result, error) {
	return c.post(ctx, map[string]string{"kind": kind, "url": docURL})
}

// Search 交一次检索。host 可空。
func (c *Client) Search(ctx context.Context, query, host string) (Result, error) {
	return c.post(ctx, map[string]string{"kind": JobSearch, "query": query, "host": host})
}

func (c *Client) post(ctx context.Context, job map[string]string) (Result, error) {
	body, _ := json.Marshal(job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+JobPath, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return Result{}, ErrHubNotRunning
		}
		return Result{}, fmt.Errorf("连不上本机 mcp-hub 配置页面: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Result
		Err string `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Result{}, fmt.Errorf("配置页面回的不是 JSON（端口上可能是别的程序）: %w", err)
	}
	if out.Err != "" {
		return Result{}, errors.New(out.Err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("配置页面返回 %s", resp.Status)
	}
	return out.Result, nil
}
