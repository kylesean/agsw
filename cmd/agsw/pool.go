package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
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

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "名字\t邮箱\t过期时间\trefresh\t当前")
	for _, a := range accounts {
		refresh := "是"
		if a.RefreshToken == "" {
			refresh = "否"
		}
		mark := ""
		if a.Email == curEmail && curEmail != "" {
			mark = "●"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			a.Name, a.Email, fmtExpiry(a.Expiry), refresh, mark)
	}
	_ = w.Flush()

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
		fmt.Printf("  %-12s %s  过期 %s%s\n",
			a.Name, a.Email, fmtExpiry(a.Expiry), extra)
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
