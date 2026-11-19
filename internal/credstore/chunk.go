package credstore

// 大凭据分段存储。
//
// 系统密码管理器对单条的大小有上限，而且各家不一样：
//
//	Windows 凭据管理器  密码 2560 字节（go-keyring 在写入前就拒掉）
//	macOS 钥匙串        go-keyring 经 security 命令写入，整条命令 4096 字节
//
// 普通密码和长期令牌离这个上限很远，但 OAuth 令牌组不是：飞书的
// access_token 和 refresh_token 各约 1.9KB，序列化成一份令牌组约 3.9KB，
// macOS 上贴着上限、Windows 上直接超出。超出的表现是「授权成功了，保存
// 失败」，而且只在真实令牌上出现，用短字符串的测试永远发现不了。
//
// 做法：超过 chunkSize 的值拆成若干段，各存一条（账号名后缀 #1、#2…），
// 原来那一条只存一个指向它们的头。读的时候认出头就拼回去；不是头的值
// 照原样返回，所以已经存着的单条凭据不需要迁移。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zalando/go-keyring"
)

// chunkSize 每段的字节数。取 2000 是给 Windows 的 2560 和 macOS 那条命令里
// service、账号名、转义留出余量。
const chunkSize = 2000

// chunkHead 是头条目的前缀，后面跟段数。不会和真实凭据撞上：它不是 JSON，
// 也不像任何一家的令牌格式。
const chunkHead = "mcphub-chunked:v1:"

// maxChunks 拒绝明显不合理的段数，免得一个损坏的头让读取去打几千次后端。
const maxChunks = 64

func chunkAccount(account string, i int) string {
	return account + "#" + strconv.Itoa(i)
}

// parseHead 判断一条值是不是分段头，是的话返回段数。
func parseHead(v string) (int, bool) {
	if !strings.HasPrefix(v, chunkHead) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, chunkHead))
	if err != nil || n < 1 || n > maxChunks {
		return 0, false
	}
	return n, true
}

// readEntry 读一条凭据，认出分段头就把各段拼回去。
func readEntry(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		return "", err
	}
	n, ok := parseHead(v)
	if !ok {
		return v, nil
	}
	var b strings.Builder
	for i := 1; i <= n; i++ {
		part, err := keyring.Get(service, chunkAccount(account, i))
		if err != nil {
			// 缺一段就是整份凭据坏了，不能拼一个残缺的值交出去。
			return "", fmt.Errorf("凭据分段 %d/%d 读取失败: %w", i, n, err)
		}
		b.WriteString(part)
	}
	return b.String(), nil
}

// writeEntry 写一条凭据，超长时分段。
//
// 先写各段、最后写头：中途失败时头还是旧的，读到的仍是上一份完整的值，
// 而不是一个指向半截新数据的头。旧值若分段更多，多出来的段在最后清掉。
func writeEntry(service, account, value string) error {
	oldN := 0
	if old, err := keyring.Get(service, account); err == nil {
		oldN, _ = parseHead(old)
	}
	newN := 0
	if len(value) > chunkSize {
		newN = (len(value) + chunkSize - 1) / chunkSize
		if newN > maxChunks {
			return fmt.Errorf("凭据太大（%d 字节）", len(value))
		}
		for i := 1; i <= newN; i++ {
			end := i * chunkSize
			if end > len(value) {
				end = len(value)
			}
			if err := keyring.Set(service, chunkAccount(account, i), value[(i-1)*chunkSize:end]); err != nil {
				return err
			}
		}
		if err := keyring.Set(service, account, chunkHead+strconv.Itoa(newN)); err != nil {
			return err
		}
	} else if err := keyring.Set(service, account, value); err != nil {
		return err
	}
	for i := newN + 1; i <= oldN; i++ {
		if err := keyring.Delete(service, chunkAccount(account, i)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	return nil
}

// deleteEntry 删一条凭据及其分段。头不存在时返回 keyring.ErrNotFound，
// 和直接删单条的语义一致。
func deleteEntry(service, account string) error {
	n := 0
	if v, err := keyring.Get(service, account); err == nil {
		n, _ = parseHead(v)
	}
	for i := 1; i <= n; i++ {
		if err := keyring.Delete(service, chunkAccount(account, i)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	return keyring.Delete(service, account)
}
