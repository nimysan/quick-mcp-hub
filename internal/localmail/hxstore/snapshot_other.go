//go:build !darwin

package hxstore

import "errors"

// cloneSnapshot 只在 macOS（APFS clonefile）上可用；其他平台直接只读读原文件。
func cloneSnapshot(src, dir string) (string, func(), error) {
	return "", nil, errors.New("当前平台不支持 clonefile")
}
