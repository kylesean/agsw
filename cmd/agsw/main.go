// agsw — Antigravity (agy) 多账号切换器。
//
// 核心能力：
//   - 账号池私有持久化（目录 0700、文件 0600）
//   - 支持独立 OAuth 登录与系统 Keyring 凭据捕获
//   - 拦截探针支持网络协议与线格式分析
//   - 透传代理负责身份注入、协议信封双向改写与额度耗尽自动切号
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const usage = `agsw — agy 多账号切换器

用法:
  agsw                 直接启动 Gateway 并拉起 agy（推荐）
  agsw <命令> [参数]  执行指定子命令

命令:
  probe    启动拦截探针（观察线格式与协议交互）
  login    独立执行 Google OAuth 登录并将凭据存入账号池
  add      捕获当前系统 Keyring 凭据入池
  list     列出账号池中的账号
  status   显示当前系统 Keyring 账号与账号池状态
  drop     从池中移除指定账号
  usage    查询账号池中各账号的真实额度
  serve    启动反向代理（凭据注入、信封改写与自动切号）
  gui      启动 Gateway 并自动拉起 agy（agsw 的兼容别名）

各命令用 "<命令> -h" 查看参数。
`

func main() {
	cmd := "gui"
	var args []string
	if len(os.Args) >= 2 {
		first := os.Args[1]
		if strings.HasPrefix(first, "-") && first != "-h" && first != "--help" {
			cmd = "gui"
			args = os.Args[1:]
		} else {
			cmd, args = first, os.Args[2:]
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "probe":
		err = cmdProbe(ctx, args)
	case "login":
		err = cmdLogin(ctx, args)
	case "add":
		err = cmdAdd(args)
	case "list":
		err = cmdList(args)
	case "status":
		err = cmdStatus(args)
	case "drop":
		err = cmdDrop(args)
	case "usage":
		err = cmdUsage(args)
	case "serve":
		err = cmdServe(ctx, args)
	case "gui":
		err = cmdGUI(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "agsw: "+err.Error())
		os.Exit(1)
	}
}
