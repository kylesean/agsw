package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kylesean/agsw/internal/keyring"
	"github.com/kylesean/agsw/internal/pool"
)

// gatewayURL 把监听地址转换成 agy 可用的本地 Gateway URL。
// 通配监听只对本机开放，客户端统一使用 127.0.0.1 连接。
func gatewayURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func csvContains(values, want string) bool {
	for _, value := range strings.Split(values, ",") {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func defaultGUIlogPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "agsw", "gui.log")
}

func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// ensureNoProxy 确保本地 Gateway 不会被 HTTP(S)_PROXY 再次代理。
// 同时维护大小写两套变量，兼容不同 HTTP 客户端。
func ensureNoProxy(env []string, host string) []string {
	out := append([]string(nil), env...)
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		prefix := key + "="
		found := false
		for i, item := range out {
			if !strings.HasPrefix(item, prefix) {
				continue
			}
			found = true
			values := strings.TrimPrefix(item, prefix)
			if !csvContains(values, host) {
				if values == "" {
					out[i] = prefix + host
				} else {
					out[i] = prefix + values + "," + host
				}
			}
		}
		if !found {
			out = append(out, prefix+host)
		}
	}
	return out
}

func waitGateway(ctx context.Context, addr string, serverErr <-chan error) error {
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case err := <-serverErr:
			if err == nil {
				err = fmt.Errorf("serve 在 Gateway ready 前退出")
			}
			return fmt.Errorf("启动 Gateway 失败: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var storeKeyring = keyring.Store

type accountSwitch struct {
	name  string
	email string
}

func secretFromAccount(a *pool.Account) *keyring.Secret {
	if a == nil {
		return nil
	}
	return &keyring.Secret{
		Token: keyring.Token{
			AccessToken:  a.AccessToken,
			TokenType:    "Bearer",
			RefreshToken: a.RefreshToken,
			Expiry:       keyring.ExpiryTime{Time: a.Expiry},
		},
		AuthMethod: a.AuthMethod,
		IDToken:    a.IDToken,
	}
}

func syncKeyringAccount(sw accountSwitch) error {
	a, err := pool.Load(sw.name)
	if err != nil {
		return fmt.Errorf("加载切换账号 %q 失败: %w", sw.name, err)
	}
	if !strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(sw.email)) {
		return fmt.Errorf("账号 %s 身份已变化，拒绝写入 Keyring", sw.name)
	}
	if a.RefreshToken == "" && a.IDToken == "" {
		return fmt.Errorf("账号 %s 缺少可写入的凭据", sw.name)
	}
	if err := storeKeyring(secretFromAccount(a)); err != nil {
		return fmt.Errorf("同步账号 %s 到 Keyring 失败: %w", sw.name, err)
	}
	return nil
}

type managedAgy struct {
	cmd  *exec.Cmd
	done chan error
}

func startAgy(env []string, args []string) *managedAgy {
	cmd := exec.Command("agy", args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	p := &managedAgy{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Run() }()
	return p
}

func stopAgy(p *managedAgy, timeout time.Duration) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// cmdGUI 统一启动 Gateway 和 agy，避免用户手动设置 AGY_GATEWAY_URL。

func isOneShotAgy(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "-p", "--print", "--prompt":
			return true
		}
	}
	return false
}

// 用法：agsw gui [serve flags] [-- agy flags]
func cmdGUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7897", "Gateway 监听地址")
	account := fs.String("account", "", "只使用指定账号")
	threshold := fs.Float64("quota-threshold", 0, "额度耗尽阈值")
	interval := fs.Duration("quota-interval", time.Minute, "额度轮询间隔")
	verbose := fs.Bool("v", false, "打印每个请求的详细选号日志")
	logFile := fs.String("log-file", defaultGUIlogPath(), "Gateway/GUI 日志文件")
	noModelAlias := fs.Bool("no-model-alias", false, "不拉取模型表")
	syncKeyring := fs.Bool("sync-keyring", true, "切换账号时同步 Keyring 并重启 agy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	agyArgs := fs.Args()
	oneShot := isOneShotAgy(agyArgs)
	if *syncKeyring && oneShot {
		fmt.Fprintln(os.Stderr, "[gui] 一次性 agy 命令不启用 Keyring 自动重启")
	}
	if _, err := exec.LookPath("agy"); err != nil {
		return fmt.Errorf("找不到 agy，请先安装并加入 PATH: %w", err)
	}
	guiLogFile, err := openLogFile(*logFile)
	if err != nil {
		return fmt.Errorf("打开 GUI 日志失败: %w", err)
	}
	defer guiLogFile.Close()
	guiLog := log.New(guiLogFile, "[gui] ", log.LstdFlags|log.Lmsgprefix)

	serveArgs := []string{
		"-listen", *listen,
		"-log-file", *logFile,
		"-quota-threshold", strconv.FormatFloat(*threshold, 'g', -1, 64),
		"-quota-interval", interval.String(),
	}
	if *verbose {
		serveArgs = append(serveArgs, "-v")
	}
	if *account != "" {
		serveArgs = append(serveArgs, "-account", *account)
	}
	if *noModelAlias {
		serveArgs = append(serveArgs, "-no-model-alias")
	}

	syncOnSwitch := *syncKeyring && !oneShot
	switchCh := make(chan accountSwitch, 1)
	hooks := serveHooks{}
	var switchMu sync.Mutex
	if syncOnSwitch {
		hooks.onSwitch = func(name, email string) {
			sw := accountSwitch{name: name, email: email}
			switchMu.Lock()
			defer switchMu.Unlock()
			// 只保留最新状态：启动时 A 可能先到，额度检查后 B 必须覆盖它。
			select {
			case <-switchCh:
			default:
			}
			switchCh <- sw
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		if err := cmdServeWithHooks(runCtx, serveArgs, hooks); err != nil {
			serverErr <- err
			cancel()
		}
	}()

	gateway := gatewayURL(*listen)
	if err := waitGateway(runCtx, *listen, serverErr); err != nil {
		return err
	}

	// 启动前的首次 Pick 也可能已经把首选账号从 Keyring 当前账号切走。
	if syncOnSwitch {
		select {
		case sw := <-switchCh:
			if err := syncKeyringAccount(sw); err != nil {
				cancel()
				return err
			}
		default:
		}
	}

	env := ensureNoProxy(os.Environ(), "127.0.0.1")
	env = setEnv(env, "AGY_GATEWAY_URL", gateway)
	agy := startAgy(env, agyArgs)

	for {
		select {
		case sw := <-switchCh:
			guiLog.Printf("切换账号 %s (%s)，同步 Keyring 并重启 agy", sw.name, sw.email)
			if err := syncKeyringAccount(sw); err != nil {
				stopAgy(agy, 5*time.Second)
				cancel()
				return err
			}
			stopAgy(agy, 5*time.Second)
			agy = startAgy(env, agyArgs)
			guiLog.Println("agy 已重启，请执行 /resume 恢复会话")
		case err := <-agy.done:
			cancel()
			if runCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("agy 退出: %w", err)
		case err := <-serverErr:
			stopAgy(agy, 5*time.Second)
			cancel()
			return fmt.Errorf("Gateway 退出: %w", err)
		case <-ctx.Done():
			stopAgy(agy, 5*time.Second)
			cancel()
			return nil
		}
	}
}
