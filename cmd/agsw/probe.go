package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/kylesean/agsw/internal/probe"
)

// cmdProbe 启动拦截探针。
//
// 两种模式：
//   - -mode http    反向代理形态，配 AGY_GATEWAY_URL 用，记录线格式
//   - -mode connect 极简 CONNECT 代理，配 AGY_PROXY_URL/http_proxy 用，记录上游 host:port
//
// 本命令只读，不修改系统 Keyring。
func cmdProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	mode := fs.String("mode", "http", "探针模式: http | connect")
	listen := fs.String("listen", "127.0.0.1:7897", "监听地址")
	upstream := fs.String("upstream", "", "上游地址；留空则仅记录不转发")
	out := fs.String("out", "", "JSONL 日志路径；留空则新建临时文件")
	bodyMax := fs.Int("body-max", 8192, "单条记录保留的请求体字节数")
	quiet := fs.Bool("q", false, "只写文件，不往 stderr 刷")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("probe 不接受位置参数")
	}

	logPath := *out
	if logPath == "" {
		f, err := os.CreateTemp("", "agsw-probe-*.jsonl")
		if err != nil {
			return fmt.Errorf("建日志文件失败: %w", err)
		}
		logPath = f.Name()
		_ = f.Close()
	} else if dir := filepath.Dir(logPath); dir != "" && dir != "." {
		// -out 指向的目录可能还不存在。
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建日志目录失败: %w", err)
		}
	}

	// 必须先 OpenFile 建出文件，chmod 才有对象。
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志失败: %w", err)
	}
	defer f.Close()

	// 日志含协议细节，收紧权限（OpenFile 的 mode 受 umask 影响，这里强制纠正）。
	if err := os.Chmod(logPath, 0o600); err != nil {
		return fmt.Errorf("设置日志权限失败: %w", err)
	}
	if dir := filepath.Dir(logPath); dir != "" {
		_ = os.Chmod(dir, 0o700)
	}

	lg := log.New(os.Stderr, "[probe] ", log.LstdFlags|log.Lmsgprefix)
	if *quiet {
		lg.SetOutput(io.Discard)
	}

	rec := probe.New(f, *bodyMax)
	lg.Printf("日志: %s", logPath)

	switch *mode {
	case "http":
		lg.Printf("接入: AGY_GATEWAY_URL=http://%s agy --print 'hi'", *listen)
		s := &probe.HTTPServer{
			Listen:   *listen,
			Upstream: *upstream,
			Recorder: rec,
			Log:      lg,
		}
		return s.Run(ctx)
	case "connect":
		lg.Printf("接入: AGY_PROXY_URL=http://%s agy --print 'hi'", *listen)
		s := &probe.CONNECTServer{Listen: *listen, Recorder: rec, Log: lg}
		return s.Run(ctx)
	default:
		return fmt.Errorf("未知 -mode %q（可用: http | connect）", *mode)
	}
}
