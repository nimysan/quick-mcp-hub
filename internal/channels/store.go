package channels

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Instance 是一个已配置的渠道。
//
// Settings 只装非敏感字段 —— 密码类字段在 Descriptor.Validate 里就被摘走，
// 送进系统密码管理器，永远不进这个结构，也就永远不进配置文件。
type Instance struct {
	Type      string            `json:"type"`
	Account   string            `json:"account"`
	Settings  map[string]string `json:"settings,omitempty"`
	Enabled   bool              `json:"enabled"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// Key 是渠道实例的唯一标识。
func (i Instance) Key() string { return i.Type + "/" + i.Account }

// Setting 取一个非敏感设置，缺失时回退到描述符的默认值。
func (i Instance) Setting(d Descriptor, name string) string {
	if v, ok := i.Settings[name]; ok && v != "" {
		return v
	}
	if f, ok := d.Field(name); ok {
		return f.Default
	}
	return ""
}

// SettingBool 把一个设置读成布尔值。
func (i Instance) SettingBool(d Descriptor, name string) bool {
	switch i.Setting(d, name) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SettingInt 把一个设置读成正整数，解析失败时返回 def。
func (i Instance) SettingInt(d Descriptor, name string, def int) int {
	var v int
	if _, err := fmt.Sscanf(i.Setting(d, name), "%d", &v); err == nil && v > 0 {
		return v
	}
	return def
}

type file struct {
	Channels []Instance `json:"channels"`
}

// Store 把渠道实例存成一个 JSON 文件。
//
// 配置文件是故意可读可编辑的：它只含主机、端口、开关这类非敏感值，
// 用户直接改也没有泄露风险。凭据不在这里。
type Store struct {
	path string
}

// NewStore 在用户配置目录下定位 channels.json。
func NewStore(namespace string) (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return &Store{path: filepath.Join(dir, namespace, "channels.json")}, nil
}

// Path 返回配置文件路径，供界面和 CLI 提示用户。
func (s *Store) Path() string { return s.path }

// ModTime 返回配置文件的修改时间，用于让常驻的 MCP 进程发现「页面改过配置」。
// 文件不存在时返回零值而不是错误。
func (s *Store) ModTime() time.Time {
	fi, err := os.Stat(s.path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// List 读出全部渠道实例，按 Key 排序。文件不存在时返回空列表。
func (s *Store) List() ([]Instance, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return []Instance{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取渠道配置 %s 失败: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("渠道配置 %s 内容损坏: %w", s.path, err)
	}
	if f.Channels == nil {
		f.Channels = []Instance{}
	}
	sort.Slice(f.Channels, func(i, j int) bool { return f.Channels[i].Key() < f.Channels[j].Key() })
	return f.Channels, nil
}

// Get 按类型和账号取一个实例。
func (s *Store) Get(typ, account string) (Instance, bool, error) {
	list, err := s.List()
	if err != nil {
		return Instance{}, false, err
	}
	for _, in := range list {
		if in.Type == typ && in.Account == account {
			return in, true, nil
		}
	}
	return Instance{}, false, nil
}

// Upsert 新增或覆盖一个渠道实例。
func (s *Store) Upsert(in Instance) error {
	if in.Type == "" || in.Account == "" {
		return fmt.Errorf("渠道类型和账号都不能为空")
	}
	list, err := s.List()
	if err != nil {
		return err
	}
	in.UpdatedAt = time.Now()
	replaced := false
	for i := range list {
		if list[i].Type == in.Type && list[i].Account == in.Account {
			list[i] = in
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, in)
	}
	return s.save(list)
}

// Delete 删除一个渠道实例。不存在时视作成功（幂等）。
func (s *Store) Delete(typ, account string) error {
	list, err := s.List()
	if err != nil {
		return err
	}
	kept := make([]Instance, 0, len(list))
	for _, in := range list {
		if in.Type == typ && in.Account == account {
			continue
		}
		kept = append(kept, in)
	}
	return s.save(kept)
}

func (s *Store) save(list []Instance) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Key() < list[j].Key() })
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(file{Channels: list}, "", "  ")
	if err != nil {
		return err
	}
	// 先写临时文件再 rename：避免写一半被常驻的 MCP 进程读到残缺 JSON。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入渠道配置失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换渠道配置失败: %w", err)
	}
	return nil
}
