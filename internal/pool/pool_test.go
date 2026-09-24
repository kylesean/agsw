package pool

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFindByEmailNormalizesAndFindsExistingAccount(t *testing.T) {
	withTmpDir(t)
	if err := Save(&Account{Name: "work", Email: "User@Example.COM"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := FindByEmail(" user@example.com ")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if got.Name != "work" {
		t.Fatalf("FindByEmail = %q, want work", got.Name)
	}
}

func TestUniquePrefersFreshCredentialPerEmail(t *testing.T) {
	old := &Account{Name: "old", Email: "User@Example.com", AccessToken: "OLD", Expiry: time.Now().Add(-time.Hour), AddedAt: time.Now().Add(-2 * time.Hour)}
	fresh := &Account{Name: "fresh", Email: "user@example.com", AccessToken: "NEW", Expiry: time.Now().Add(time.Hour), AddedAt: time.Now()}
	other := &Account{Name: "other", Email: "other@example.com", AccessToken: "OTHER", Expiry: time.Now().Add(time.Hour)}

	got := Unique([]*Account{old, other, fresh})
	if len(got) != 2 {
		t.Fatalf("Unique returned %d accounts, want 2", len(got))
	}
	if got[0].Name != "fresh" || got[1].Name != "other" {
		t.Fatalf("Unique selection/order = %q, %q; want fresh, other", got[0].Name, got[1].Name)
	}
}

// withTmpDir 把池重定向到临时目录，绝不碰真实 ~/.local/share/agsw。
func withTmpDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGSW_DATA_DIR", dir)
	return dir
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTmpDir(t)

	want := &Account{
		Name:         "work",
		Email:        "a@example.com",
		ClientID:     "123.apps.googleusercontent.com",
		AddedAt:      time.Now().UTC().Truncate(time.Second),
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		Expiry:       time.Now().UTC().Truncate(time.Second).Add(time.Hour),
		AuthMethod:   "consumer",
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load("work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Email != want.Email || got.AccessToken != want.AccessToken ||
		got.RefreshToken != want.RefreshToken || got.ClientID != want.ClientID {
		t.Errorf("往返不一致: got %+v want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) {
		t.Errorf("Expiry = %v, want %v", got.Expiry, want.Expiry)
	}
}

func TestPermissionsAre0600And0700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 POSIX 文件权限位 (0700/0600)")
	}
	dir := withTmpDir(t)

	if err := Save(&Account{Name: "a", Email: "a@example.com"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Save 会 MkdirAll，确认池目录是 0700。
	poolDir := filepath.Join(dir, "pool")
	di, err := os.Stat(poolDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("池目录权限 = %04o, 想要 0700", perm)
	}

	fi, err := os.Stat(filepath.Join(poolDir, "a.json"))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("账号文件权限 = %04o, 想要 0600", perm)
	}
}

func TestSaveCreatesPrivateDirOnSecondWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不支持 POSIX 文件权限位 (0700/0600)")
	}
	dir := withTmpDir(t)
	if err := Save(&Account{Name: "one"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 目录权限宽松时，Save 必须主动纠正为 0700。
	poolDir := filepath.Join(dir, "pool")
	if err := os.Chmod(poolDir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Save(&Account{Name: "two"}); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	di, err := os.Stat(poolDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("二次写入后目录权限 = %04o, 想要 0700", perm)
	}
}

func TestValidateNameRejectsTraversal(t *testing.T) {
	// 常见的路径穿越与非法命名输入
	bad := []string{
		"..", ".", "../..", "a/b", "/etc/passwd", `a\b`,
		"-flag", "", "a b", "a\n", strings_Repeat("x", 65),
		"..json", "a..b",
	}
	for _, name := range bad {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) 应当拒绝", name)
		}
	}

	good := []string{"a", "A", "work", "dev-1", "x.y_z", "0"}
	for _, name := range good {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) 应当接受, 得到 %v", name, err)
		}
	}
}

func TestDeleteRefusesTraversal(t *testing.T) {
	dir := withTmpDir(t)

	// 在池目录之外放一个诱饵文件，确认 delete .. 碰不到它。
	outside := filepath.Join(dir, "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatalf("写诱饵: %v", err)
	}

	if err := Delete(".."); err == nil {
		t.Fatal("Delete(\"..\") 应当报错")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("诱饵文件被碰了: %v", err)
	}
}

func TestListEmptyWhenDirMissing(t *testing.T) {
	withTmpDir(t)
	got, err := List()
	if err != nil {
		t.Fatalf("目录不存在时应当返回空而非报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %d", len(got))
	}
}

func TestListSkipsCorruptFileAndSorts(t *testing.T) {
	dir := withTmpDir(t)

	if err := Save(&Account{Name: "zeta", Email: "z@x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(&Account{Name: "alpha", Email: "a@x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 塞一个损坏文件，List 不应因此挂掉。
	corrupt := filepath.Join(dir, "pool", "broken.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写损坏文件: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 (跳过损坏项), got %d", len(got))
	}
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("排序不对: %s, %s", got[0].Name, got[1].Name)
	}
}

func TestSaveRejectsEmptyName(t *testing.T) {
	withTmpDir(t)
	if err := Save(&Account{Email: "x@y"}); err == nil {
		t.Error("缺 name 应当报错")
	}
	if err := Save(nil); err == nil {
		t.Error("nil 账号应当报错")
	}
}

// strings_Repeat 让测试少一个 import 冲突。
func strings_Repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
