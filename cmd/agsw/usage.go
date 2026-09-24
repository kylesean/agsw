package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/kylesean/agsw/internal/pool"
	"github.com/kylesean/agsw/internal/quota"
)

func formatUsageLine(a *pool.Account, sum *quota.Summary) string {
	g := sum.Gemini()
	if g == nil {
		return fmt.Sprintf("%s\t%s\t未找到 GEMINI 额度组", a.Name, a.Email)
	}
	return fmt.Sprintf("%s\t%s\t%s", a.Name, a.Email, g.Describe())
}

func formatUsageError(a *pool.Account, err error) string {
	return fmt.Sprintf("%s\t%s\t查询失败: %v", a.Name, a.Email, err)
}

// cmdUsage 直接查询账号池的真实额度，不依赖 agy GUI 的 /usage 命令。
func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	account := fs.String("account", "", "只查询指定账号")
	upstream := fs.String("upstream", DefaultUpstream, "CloudCode 上游地址")
	timeout := fs.Duration("timeout", 15*time.Second, "每个账号的额度查询超时")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage 不接受位置参数")
	}
	if *timeout <= 0 {
		return fmt.Errorf("-timeout 必须大于 0")
	}

	accounts, err := resolveCandidates(*account)
	if err != nil {
		return err
	}
	fmt.Println("账号\t邮箱\t额度")
	var errs []error
	for _, a := range accounts {
		accountCtx, cancel := context.WithTimeout(context.Background(), *timeout)
		needsRefresh := a.AccessToken == "" ||
			(!a.Expiry.IsZero() && !time.Now().Before(a.Expiry.Add(-5*time.Minute)))
		if needsRefresh && a.RefreshToken != "" {
			if err := refreshAccount(accountCtx, a); err != nil {
				cancel()
				fmt.Println(formatUsageError(a, err))
				errs = append(errs, err)
				continue
			}
		}
		sum, err := quota.Fetch(accountCtx, *upstream, a.AccessToken, defaultUserAgent)
		cancel()
		if err != nil {
			fmt.Println(formatUsageError(a, err))
			errs = append(errs, err)
			continue
		}
		fmt.Println(formatUsageLine(a, sum))
	}
	return errors.Join(errs...)
}
