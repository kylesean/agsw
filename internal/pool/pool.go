// Package pool 管理多账号凭据池：每个账号对应一个独立的 JSON 凭据文件。
//
// 目录采用 0700、文件采用 0600 权限，写入采用同目录临时文件原子 rename。
// 池文件包含 refresh_token，权限必须严格收紧。
package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// nameRe 允许的账号名白名单正则，杜绝路径穿越。
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Account 是池中单个账号的完整凭据与状态。
type Account struct {
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	ClientID     string    `json:"client_id,omitempty"`
	AddedAt      time.Time `json:"added_at"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	IDToken      string    `json:"id_token,omitempty"`
	AuthMethod   string    `json:"auth_method,omitempty"`

	// 额度与冷却状态
	QuotaExhausted bool      `json:"quota_exhausted,omitempty"`
	CooldownUntil  time.Time `json:"cooldown_until,omitempty"`
	LastQuotaCheck time.Time `json:"last_quota_check,omitempty"`
}

// Dir 返回账号池目录。环境变量 AGSW_DATA_DIR 可覆盖，便于测试。
func Dir() (string, error) {
	if base := os.Getenv("AGSW_DATA_DIR"); base != "" {
		return filepath.Join(base, "pool"), nil
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("取不到家目录: %w", err)
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(xdg, "agsw", "pool"), nil
}

// ValidateName 校验账号名合法性，拒绝非法字符与路径分隔符。
func ValidateName(name string) error {
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("账号名 %q 含路径分隔符", name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("账号名 %q 不合法：只允许字母/数字/._-，以字母或数字开头，最长 64", name)
	}
	return nil
}

func filePath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name+".json"), nil
}

// Save 原子写入一个账号，保证目录 0700、文件 0600。
func Save(a *Account) error {
	if a == nil {
		return errors.New("账号为 nil")
	}
	if a.Name == "" {
		return errors.New("账号缺少 name")
	}
	path, err := filePath(a.Name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("创建池目录失败: %w", err)
	}
	// MkdirAll 对已存在目录不改权限，这里强制纠正。
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("设置池目录权限失败: %w", err)
	}

	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号失败: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后此调用无副作用

	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("落盘失败: %w", err)
	}
	return os.Chmod(path, filePerm)
}

// Load 读取单个账号。
func Load(name string) (*Account, error) {
	path, err := filePath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("账号 %q 不在池中", name)
		}
		return nil, fmt.Errorf("读取账号 %q 失败: %w", name, err)
	}
	var a Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("账号 %q 文件损坏: %w", name, err)
	}
	return &a, nil
}

// List 按名字排序返回全部账号。目录不存在时返回空切片而非报错。
func List() ([]*Account, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取池目录失败: %w", err)
	}
	var out []*Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		a, err := Load(name)
		if err != nil {
			// 单个损坏文件不应让整个 list 挂掉。
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// normalizeEmail 统一邮箱大小写和首尾空白，避免同一账号因格式差异重复。
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// FindByEmail 按规范化邮箱查找账号。找不到时返回 (nil, nil)，
// 便于调用方把“没有重复”与池读取错误区分开。
func FindByEmail(email string) (*Account, error) {
	accounts, err := List()
	if err != nil {
		return nil, err
	}
	want := normalizeEmail(email)
	if want == "" {
		return nil, nil
	}
	for _, a := range accounts {
		if normalizeEmail(a.Email) == want {
			return a, nil
		}
	}
	return nil, nil
}

// betterSameAccount 判断 candidate 是否比 current 更适合作为同邮箱账号的代表。
// 优先保留可用且更新的凭据，最后按名称保证结果稳定。
func betterSameAccount(candidate, current *Account) bool {
	if (candidate.AccessToken != "") != (current.AccessToken != "") {
		return candidate.AccessToken != ""
	}
	if !candidate.Expiry.Equal(current.Expiry) {
		return candidate.Expiry.After(current.Expiry)
	}
	if (candidate.RefreshToken != "") != (current.RefreshToken != "") {
		return candidate.RefreshToken != ""
	}
	if !candidate.AddedAt.Equal(current.AddedAt) {
		return candidate.AddedAt.After(current.AddedAt)
	}
	return candidate.Name < current.Name
}

// Unique 按邮箱去重账号。邮箱为空时按名称保留独立记录；
// 输出顺序按首次出现位置保持稳定。
func Unique(accounts []*Account) []*Account {
	positions := make(map[string]int, len(accounts))
	out := make([]*Account, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		key := normalizeEmail(a.Email)
		if key == "" {
			key = "\x00" + a.Name
		}
		if i, ok := positions[key]; ok {
			if betterSameAccount(a, out[i]) {
				out[i] = a
			}
			continue
		}
		positions[key] = len(out)
		out = append(out, a)
	}
	return out
}

// Delete 移除单个账号。同样校验名字，绝不接受 .. 或斜杠。
func Delete(name string) error {
	path, err := filePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("账号 %q 不在池中", name)
		}
		return fmt.Errorf("删除账号 %q 失败: %w", name, err)
	}
	return nil
}
