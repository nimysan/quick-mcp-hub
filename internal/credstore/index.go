package credstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 系统密码管理器没有「列出我写过哪些条目」的通用 API，所以在磁盘上另存一份
// 索引，只记录「哪个集成的哪个账号已配置」这类元数据。
//
// 索引里不含任何凭据，可以随便看、随便备份；删掉它只会让 -list-secrets 变空，
// 钥匙串里的凭据不受影响。

type indexEntry struct {
	Integration string    `json:"integration"`
	Account     string    `json:"account"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type indexFile struct {
	Entries []indexEntry `json:"entries"`
}

func (s *Store) indexPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户配置目录失败: %w", err)
	}
	return filepath.Join(dir, s.namespace, "credentials-index.json"), nil
}

func (s *Store) loadIndex() (indexFile, string, error) {
	path, err := s.indexPath()
	if err != nil {
		return indexFile{}, "", err
	}
	var idx indexFile
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return idx, path, nil
	case err != nil:
		return idx, path, fmt.Errorf("读取凭据索引 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return indexFile{}, path, fmt.Errorf("凭据索引 %s 内容损坏: %w", path, err)
	}
	return idx, path, nil
}

func (s *Store) saveIndex(idx indexFile, path string) error {
	sort.Slice(idx.Entries, func(i, j int) bool {
		if idx.Entries[i].Integration != idx.Entries[j].Integration {
			return idx.Entries[i].Integration < idx.Entries[j].Integration
		}
		return idx.Entries[i].Account < idx.Entries[j].Account
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入凭据索引 %s 失败: %w", path, err)
	}
	return nil
}

func (s *Store) indexAdd(r Ref) error {
	idx, path, err := s.loadIndex()
	if err != nil {
		return err
	}
	now := time.Now()
	for i := range idx.Entries {
		if idx.Entries[i].Integration == r.Integration && idx.Entries[i].Account == r.Account {
			idx.Entries[i].UpdatedAt = now
			return s.saveIndex(idx, path)
		}
	}
	idx.Entries = append(idx.Entries, indexEntry{r.Integration, r.Account, now})
	return s.saveIndex(idx, path)
}

func (s *Store) indexRemove(r Ref) error {
	idx, path, err := s.loadIndex()
	if err != nil {
		return err
	}
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if e.Integration == r.Integration && e.Account == r.Account {
			continue
		}
		kept = append(kept, e)
	}
	idx.Entries = kept
	return s.saveIndex(idx, path)
}

// Configured 是 List 返回的一条记录：某集成的某账号已配置凭据。
// 不含凭据本身。
type Configured struct {
	Ref       Ref
	UpdatedAt time.Time
}

// List 列出已登记的凭据条目。只读索引文件，不访问钥匙串，
// 所以不会触发授权弹框、也不会把任何明文解出来。
func (s *Store) List() ([]Configured, error) {
	idx, _, err := s.loadIndex()
	if err != nil {
		return nil, err
	}
	out := make([]Configured, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		out = append(out, Configured{Ref{e.Integration, e.Account}, e.UpdatedAt})
	}
	return out, nil
}

// IndexPath 返回索引文件路径，供 CLI 提示用户。
func (s *Store) IndexPath() string {
	p, err := s.indexPath()
	if err != nil {
		return "(无法定位)"
	}
	return p
}
