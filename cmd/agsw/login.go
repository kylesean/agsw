package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/kylesean/agsw/internal/oauth"
	"github.com/kylesean/agsw/internal/pool"
)

// cmdLogin 自己走一遍 Google OAuth（授权码 + PKCE + 本地回环回调），把号直接写进池。
//
// 这是注册新账号的推荐路径：不碰 keyring、不依赖 agy TUI、
// 也没有「必须在登录后立刻 add」的顺序坑。
//
// 万一 Antigravity 的 client_id/secret 哪天被轮换，回退路径是
// 「agy 登录 → agsw add」，那条路保留不删。
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "等待浏览器完成登录的超时")
	noBrowser := fs.Bool("no-browser", false, "只打印授权链接，不自动打开浏览器（SSH/无头场景）")
	hint := fs.String("email", "", "预填的 Google 邮箱（可选）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		// Go 的 flag 包遇到第一个非 flag 参数即停止解析，
		// 所以标志必须写在 <name> 前面 —— 用法串必须如实反映。
		return fmt.Errorf("用法: agsw login [-timeout 5m] [-no-browser] [-email you@example.com] <name>\n（标志要写在 <name> 前面）")
	}
	name := fs.Arg(0)
	if err := pool.ValidateName(name); err != nil {
		return err
	}

	// 与 add 同样防覆盖：已存在的名字要求显式 drop，避免误冲。
	if _, err := pool.Load(name); err == nil {
		return fmt.Errorf("池里已有 %q，要覆盖请先 agsw drop %s", name, name)
	}

	fmt.Printf("开始登录 → %s\n", name)
	res, err := oauth.Login(ctx, oauth.Config{}, oauth.Options{
		Timeout:      *timeout,
		NoBrowser:    *noBrowser,
		LoginHint:    *hint,
		PrintAuthURL: nil, // oauth 包默认打到 stdout
	})
	if err != nil {
		return err
	}

	existing, err := pool.FindByEmail(res.Email)
	if err != nil {
		return fmt.Errorf("登录成功但检查邮箱失败（凭据未保存）: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("邮箱 %s 已存在（账号 %s），请使用不同 Google 账号；如需替换请先 agsw drop %s", res.Email, existing.Name, existing.Name)
	}

	// 与 add 完全一致的池文件 schema，serve/pool/quota 零改动。
	a := &pool.Account{
		Name:         name,
		Email:        res.Email,
		ClientID:     oauth.DefaultClientID(),
		AddedAt:      time.Now().UTC(),
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		Expiry:       res.Expiry,
		IDToken:      res.IDToken,
		AuthMethod:   oauth.AuthMethod,
	}
	if err := pool.Save(a); err != nil {
		return fmt.Errorf("登录成功但入池失败（凭据未保存）: %w", err)
	}

	fmt.Printf("\n已入池 → %s\n", name)
	fmt.Printf("  邮箱       %s\n", a.Email)
	fmt.Printf("  过期时间   %s\n", fmtExpiry(a.Expiry))
	fmt.Printf("  client_id  %s\n", orDash(a.ClientID))
	fmt.Printf("  scope      %d 项\n", len(oauth.DefaultScopes))
	if a.RefreshToken == "" {
		fmt.Printf("  ⚠ 无 refresh_token，之后无法自动续期\n")
	} else {
		fmt.Printf("  refresh    有（该号从此永久免登）\n")
	}
	dir, _ := pool.Dir()
	fmt.Printf("  存放       %s (0600)\n", dir)
	return nil
}
