package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/kylesean/agsw/internal/keyring"
	"github.com/kylesean/agsw/internal/pool"
)

// cmdAdd 把当前 keyring 里的凭据捕获进账号池。
//
// 注册第二个号的流程：先在 agy 里正常登一次该号，然后 agsw add <name>。
// 捕获之后那个号就永久免登了。
func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: agsw add <name>")
	}
	name := fs.Arg(0)
	if err := pool.ValidateName(name); err != nil {
		return err
	}

	// 幂等：同名账号要求显式覆盖，避免误冲。
	if _, err := pool.Load(name); err == nil {
		return fmt.Errorf("池里已有 %q，要覆盖请先 agsw drop %s", name, name)
	}

	sec, email, err := keyring.Current()
	if err != nil {
		return fmt.Errorf("读取当前凭据失败: %w", err)
	}
	claims, err := sec.Claims()
	if err != nil {
		return fmt.Errorf("解析身份失败: %w", err)
	}

	a := &pool.Account{
		Name:         name,
		Email:        email,
		ClientID:     claims.Audience(),
		AddedAt:      time.Now().UTC(),
		AccessToken:  sec.Token.AccessToken,
		RefreshToken: sec.Token.RefreshToken,
		Expiry:       sec.Token.Expiry.Time,
		IDToken:      sec.IDToken,
		AuthMethod:   sec.AuthMethod,
	}
	if err := pool.Save(a); err != nil {
		return err
	}

	fmt.Printf("已捕获 → %s\n", name)
	fmt.Printf("  邮箱       %s\n", email)
	fmt.Printf("  过期时间   %s\n", fmtExpiry(a.Expiry))
	fmt.Printf("  client_id  %s\n", orDash(a.ClientID))
	if a.RefreshToken == "" {
		fmt.Printf("  ⚠ 无 refresh_token，之后无法自动续期\n")
	}
	dir, _ := pool.Dir()
	fmt.Printf("  存放       %s (0600)\n", dir)
	return nil
}

// cmdList 列出账号池。
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("list 不接受参数")
	}

	accounts, err := pool.List()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		dir, _ := pool.Dir()
		fmt.Printf("账号池为空（%s）\n", dir)
		fmt.Println("用法: 先在 agy 登录某号，再 agsw add <name>")
		return nil
	}

	// 当前 keyring 账号用于标注，读不到也不该让 list 挂掉。
	curEmail := ""
	if _, email, err := keyring.Current(); err == nil {
		curEmail = email
	}

	type row struct {
		name    string
		email   string
		expiry  string
		refresh string
		current string
	}

	rows := []row{
		{
			name:    "名字",
			email:   "邮箱",
			expiry:  "过期时间",
			refresh: "refresh",
			current: "当前",
		},
	}

	for _, a := range accounts {
		refresh := "是"
		if a.RefreshToken == "" {
			refresh = "否"
		}
		mark := ""
		if a.Email == curEmail && curEmail != "" {
			mark = "●"
		}
		rows = append(rows, row{
			name:    a.Name,
			email:   a.Email,
			expiry:  fmtExpiry(a.Expiry),
			refresh: refresh,
			current: mark,
		})
	}

	var wName, wEmail, wExpiry, wRefresh int
	for _, r := range rows {
		if w := visualWidth(r.name); w > wName {
			wName = w
		}
		if w := visualWidth(r.email); w > wEmail {
			wEmail = w
		}
		if w := visualWidth(r.expiry); w > wExpiry {
			wExpiry = w
		}
		if w := visualWidth(r.refresh); w > wRefresh {
			wRefresh = w
		}
	}

	const colGap = 3
	for _, r := range rows {
		fmt.Printf("%s%s%s%s%s\n",
			padRight(r.name, wName+colGap),
			padRight(r.email, wEmail+colGap),
			padRight(r.expiry, wExpiry+colGap),
			padRight(r.refresh, wRefresh+colGap),
			r.current,
		)
	}

	if curEmail != "" {
		fmt.Printf("\nkeyring 当前: %s\n", curEmail)
	} else {
		fmt.Printf("\nkeyring 当前: (读取失败或未登录)\n")
	}
	return nil
}

// cmdStatus 展示当前 keyring 账号与池状态。
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("status 不接受参数")
	}

	fmt.Println("== keyring 当前身份 ==")
	sec, email, err := keyring.Current()
	if err != nil {
		fmt.Printf("  读取失败: %v\n", err)
	} else {
		fmt.Printf("  邮箱       %s\n", email)
		fmt.Printf("  auth       %s\n", orDash(sec.AuthMethod))
		fmt.Printf("  过期时间   %s\n", fmtExpiry(sec.Token.Expiry.Time))
		fmt.Printf("  refresh    %s\n", present(sec.Token.RefreshToken != ""))
	}

	fmt.Println("\n== 账号池 ==")
	accounts, err := pool.List()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("  (空)")
		return nil
	}
	for _, a := range accounts {
		extra := ""
		if !a.CooldownUntil.IsZero() && time.Now().Before(a.CooldownUntil) {
			extra = " [冷却中]"
		}
		fmt.Printf("  %s %s  过期 %s%s\n",
			padRight(a.Name, 12), a.Email, fmtExpiry(a.Expiry), extra)
	}
	dir, _ := pool.Dir()
	fmt.Printf("\n池目录: %s\n", dir)
	return nil
}

// cmdDrop 删除池中一个账号。
func cmdDrop(args []string) error {
	fs := flag.NewFlagSet("drop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: agsw drop <name>")
	}
	name := fs.Arg(0)
	if err := pool.Delete(name); err != nil {
		return err
	}
	fmt.Printf("已删除 %s\n", name)
	return nil
}

func fmtExpiry(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Until(t)
	if d < 0 {
		return "已过期"
	}
	if d < time.Hour {
		return fmt.Sprintf("%s 后", d.Round(time.Minute))
	}
	return t.Local().Format("01-02 15:04")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func present(ok bool) string {
	if ok {
		return "有"
	}
	return "无"
}

func runeWidth(r rune) int {
	if r >= 0x20 && r <= 0x7e {
		return 1
	}
	if r == '●' {
		return 1
	}
	// CJK 表意字符、全角符号、韩文、日文假名及 Emoji 占用 2 个视觉列宽
	if (r >= 0x1100 && r <= 0x115f) ||
		(r >= 0x2e80 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f9ff) {
		return 2
	}
	return 1
}

func visualWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func padRight(s string, width int) string {
	vw := visualWidth(s)
	if vw >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vw)
}
