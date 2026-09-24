package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sync"
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
	var wg sync.WaitGroup
	results := make([]string, len(accounts))
	errs := make([]error, len(accounts))

	for i, a := range accounts {
		wg.Add(1)
		go func(idx int, acc *pool.Account) {
			defer wg.Done()
			accountCtx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()

			needsRefresh := acc.AccessToken == "" ||
				(!acc.Expiry.IsZero() && !time.Now().Before(acc.Expiry.Add(-5*time.Minute)))
			if needsRefresh && acc.RefreshToken != "" {
				if err := refreshAccount(accountCtx, acc); err != nil {
					results[idx] = formatUsageError(acc, err)
					errs[idx] = err
					return
				}
			}
			sum, err := quota.Fetch(accountCtx, *upstream, acc.AccessToken, defaultUserAgent)
			if err != nil {
				results[idx] = formatUsageError(acc, err)
				errs[idx] = err
				return
			}
			results[idx] = formatUsageLine(acc, sum)
		}(i, a)
	}
	wg.Wait()

	for _, line := range results {
		if line != "" {
			fmt.Println(line)
		}
	}
	return errors.Join(errs...)
}
