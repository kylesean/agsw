package main

import (
	"github.com/kylesean/agsw/internal/keyring"
)

// keyringCurrentAdapter 把 keyring 的返回值适配给 serve 命令，
// 让 serve 不必直接感知 keyring 包的签名变化。
func keyringCurrentAdapter() (*keyring.Secret, string, error) {
	return keyring.Current()
}
