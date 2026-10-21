package credstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

// MigrateFrom 把旧 namespace 下的凭据搬到当前 namespace，一次性执行。
//
// 项目改名会改掉钥匙串里的 service 前缀，旧条目就读不到了。这个函数让用户
// 不必重新输入授权码。
//
// 它先 stat 旧索引文件：文件不存在就立刻返回，不碰钥匙串 —— 所以迁移完成后
// （索引已删）再次启动的代价是一次 stat，惰性不受影响。只有真有东西要搬的
// 那一次会访问后端，macOS 上可能因此弹一次授权框。
//
// 返回搬动的条目数。部分失败不中断：能搬的搬走，失败的留在原处并汇总报错。
func (s *Store) MigrateFrom(oldNamespace string) (int, error) {
	if oldNamespace == "" || oldNamespace == s.namespace {
		return 0, nil
	}
	old := New(oldNamespace)

	oldIndex, oldIndexPath, err := old.loadIndex()
	if err != nil {
		return 0, err
	}
	if len(oldIndex.Entries) == 0 {
		// 索引为空或不存在：没有要搬的，顺手清掉空索引文件。
		removeIfExists(oldIndexPath)
		return 0, nil
	}

	moved := 0
	var failures []error
	for _, e := range oldIndex.Entries {
		ref := Ref{Integration: e.Integration, Account: e.Account}

		secret, err := keyring.Get(old.service(ref), ref.Account)
		if err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				continue // 索引里有记录但钥匙串里已没有，跳过
			}
			failures = append(failures, fmt.Errorf("读取旧条目 %s: %w", ref, err))
			continue
		}
		if err := s.Put(ref, secret); err != nil {
			failures = append(failures, fmt.Errorf("写入新条目 %s: %w", ref, err))
			continue
		}
		if err := keyring.Delete(old.service(ref), ref.Account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			failures = append(failures, fmt.Errorf("删除旧条目 %s: %w", ref, err))
		}
		moved++
	}

	if len(failures) > 0 {
		return moved, errors.Join(failures...)
	}

	// 全部搬完才清掉旧索引和旧配置目录，否则下次启动还能重试。
	removeIfExists(oldIndexPath)
	_ = os.Remove(filepath.Dir(oldIndexPath)) // 只在目录已空时成功
	return moved, nil
}

func removeIfExists(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
