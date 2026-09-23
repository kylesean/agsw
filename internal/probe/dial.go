package probe

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// resolveProxy 解析某目标该走哪个代理。
//
// 可注入，是为了让单测能替换 http.ProxyFromEnvironment —— 后者
// 第一次调用就把环境变量缓存死了（内部 sync.Once），测试里
// t.Setenv 根本改不动它。
var resolveProxy = http.ProxyFromEnvironment

// connectTimeout 是经代理握手的总时长上限。
const connectTimeout = 30 * time.Second

// maxConnectResp 限制代理握手响应的大小，防对端灌数据打爆内存。
const maxConnectResp = 64 << 10

// dialTarget 连接目标地址（host:port），若环境变量配置了 HTTP 代理则先通过代理建立 CONNECT 隧道。
// 返回的第二个参数是握手完成后缓冲区残留的字节切片，调用方须原样转发给客户端以保证数据完整性。
func dialTarget(ctx context.Context, target string) (net.Conn, []byte, error) {
	target, err := normalizeTarget(target)
	if err != nil {
		return nil, nil, err
	}

	pu, err := envProxyFor(target)
	if err != nil {
		return nil, nil, err
	}

	var d net.Dialer
	if pu == nil {
		conn, err := d.DialContext(ctx, "tcp", target)
		return conn, nil, err
	}

	conn, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return nil, nil, fmt.Errorf("连代理 %s 失败: %w", pu.Host, err)
	}

	// 握手给个上限：否则上游不回状态行时，这条协程会永远挂住。
	deadline := time.Now().Add(connectTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, nil, err
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("向代理 %s 发 CONNECT 失败: %w", pu.Host, err)
	}

	rest, err := readConnectResponse(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	// 交给 pipe 自己管理超时，别让握手 deadline 把长隧道掐断。
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, rest, nil
}

// normalizeTarget 补默认端口并校验格式。
func normalizeTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("CONNECT 目标为空")
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		// 没带端口：CONNECT 隧道基本都是冲着 TLS 去的，补 443。
		target = net.JoinHostPort(target, "443")
	}
	return target, nil
}

// envProxyFor 返回该目标应当使用的代理地址；不需代理时返回 nil。
//
// 复用 http.ProxyFromEnvironment，好让 HTTPS_PROXY / https_proxy /
// NO_PROXY / no_proxy 这套大家熟悉的语义原样生效（含大小写优先级
// 与 NO_PROXY 的 CIDR / 域名后缀匹配），不必自己重写一遍还写错。
func envProxyFor(target string) (*url.URL, error) {
	// ProxyFromEnvironment 认 URL 的 scheme 来挑变量：
	// https:// 就看 HTTPS_PROXY / https_proxy。
	u, err := url.Parse("https://" + target)
	if err != nil {
		return nil, fmt.Errorf("解析目标 %q 失败: %w", target, err)
	}
	req := &http.Request{Method: http.MethodConnect, URL: u, Host: u.Host}
	pu, err := resolveProxy(req)
	if err != nil {
		return nil, fmt.Errorf("解析代理环境变量失败: %w", err)
	}
	if pu == nil {
		return nil, nil
	}
	// 我们手工发的是 HTTP CONNECT，只有 http/https 代理能接；
	// socks5 得另说（本机 ALL_PROXY 就是 socks5h，幸好 https_proxy 优先）。
	if pu.Scheme != "http" && pu.Scheme != "https" {
		return nil, fmt.Errorf("代理 %q 的协议 %s 暂不支持（只支持 http/https）", pu, pu.Scheme)
	}
	if pu.Host == "" {
		return nil, fmt.Errorf("代理地址 %q 缺 host", pu)
	}
	return pu, nil
}

// readConnectResponse 手工读代理的 CONNECT 响应，返回响应后残留的字节。
//
// 刻意不用 http.ReadResponse：它对 CONNECT 200 有特殊的响应体语义，
// 会把隧道里涌来的 TLS 数据当成 body 去读，一读就把隧道读坏了。
// 这里只需要状态码 + 头结束，读到空行为止即可。
func readConnectResponse(conn net.Conn) ([]byte, error) {
	br := bufio.NewReaderSize(conn, 4096)

	var sb strings.Builder
	statusLine := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("读代理响应失败: %w", err)
		}
		sb.WriteString(line)
		if sb.Len() > maxConnectResp {
			return nil, fmt.Errorf("代理响应头过大（>%d 字节）", maxConnectResp)
		}
		if statusLine == "" {
			statusLine = line
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return nil, fmt.Errorf("代理响应状态行格式不对: %q", statusLine)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("代理响应状态码不是数字: %q", fields[1])
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("代理拒绝 CONNECT: %s", strings.TrimSpace(statusLine))
	}

	// 握手响应之后紧跟的上游字节，必须原样带回给客户端。
	rest := br.Buffered()
	if rest == 0 {
		return nil, nil
	}
	buf := make([]byte, rest)
	if _, err := br.Read(buf); err != nil {
		return nil, fmt.Errorf("取代理响应残留字节失败: %w", err)
	}
	return buf, nil
}
