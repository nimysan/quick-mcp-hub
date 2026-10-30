// Package clientstat 记下「最近一次有 AI 客户端真的连上来」。
//
// 为什么需要它：用户照着页面上的说明在 Amazon Quick 里加完连接器之后，**没有任何
// 反馈**——他只能切回 Quick 里看工具列表。而这个产品的用户是财务和运营，让他
// 自己判断「到底成了没有」是不合理的。
//
// 时机很要紧。进程启动**不**等于连上了：客户端 spawn 起这个进程只说明它想连，
// 握手还可能失败（这个项目就记着 Node 版把日志打到 stdout 导致 Quick 报
// handshake_failed 的事）。所以记录点在 MCP 的 InitializedHandler 里——那是
// 客户端发完 notifications/initialized 之后，协议已经真的通了。
//
// 客户端名字是它在 initialize 里**自报**的。展示用没问题，但它不是身份证明，
// 绝不能用来做任何授权判断。
package clientstat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Record 是一次成功握手留下的记录。
type Record struct {
	At time.Time `json:"at"`
	// Client / Version 来自 initialize 的 clientInfo，由客户端自报，可能为空。
	Client  string `json:"client,omitempty"`
	Version string `json:"version,omitempty"`
	// Tools 是那次注册给它的工具数。
	Tools int `json:"tools,omitempty"`
}

// Dir 返回存放位置，和 channels.json 同一个目录。
func Dir(namespace string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, namespace), nil
}

// Path 返回记录文件路径。
func Path(namespace string) (string, error) {
	dir, err := Dir(namespace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "last-client.json"), nil
}

// Write 原子地写下一条记录。
//
// 多个客户端会各起一个服务进程，有可能同时写同一个文件，所以走「同目录临时
// 文件 + rename」，不会出现读到半截 JSON 的情况。最后写的赢——这正是我们要的
// 语义（“最近一次”）。
//
// 失败只是没有这条记录，绝不能影响 MCP 服务：它是个展示用的信号，不是功能。
func Write(namespace string, r Record) error {
	path, err := Path(namespace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "last-client-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // rename 成功后这是无害的 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Read 读出记录。第二个返回值为 false 表示还没有任何客户端成功连接过
// ——页面据此决定把接入说明默认展开。
//
// 文件损坏等同于「没有记录」：这是个展示信号，不该为它报错。
func Read(namespace string) (Record, bool) {
	path, err := Path(namespace)
	if err != nil {
		return Record{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil || r.At.IsZero() {
		return Record{}, false
	}
	return r, true
}
