package probe

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startEcho 起一个原样回显的 TCP 服务，充当隧道的对端。
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// startFakeCONNECTProxy 起一个最小的 HTTP CONNECT 代理。
//
// 它在**一次 Write** 里同时发出 200 响应和一段 "EARLY" 数据，
// 模拟真实上游在握手响应后立刻发数据（TLS 服务端就是这样）——
// 这段字节必须被 dialTarget 捞回来交给客户端，否则隧道刚建立
// 就丢开头，TLS 必然失败。
func startFakeCONNECTProxy(t *testing.T, seen chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				// 读完请求头，否则客户端还没发完我们就回会乱。
				for {
					h, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if h == "\r\n" || h == "\n" {
						break
					}
				}
				f := strings.Fields(line)
				if len(f) < 2 || f[0] != "CONNECT" {
					_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
					return
				}
				seen <- f[1]

				up, err := net.Dial("tcp", f[1])
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()

				// 单次 Write：让客户端的 bufio 一次读全，残留字节可被断言。
				_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\nEARLY"))

				done := make(chan struct{}, 2)
				// 用 br 而非 c：请求行之后可能已缓冲了客户端的 TLS 数据。
				go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return ln.Addr().String()
}

func useProxy(t *testing.T, fn func(*http.Request) (*url.URL, error)) {
	t.Helper()
	old := resolveProxy
	resolveProxy = fn
	t.Cleanup(func() { resolveProxy = old })
}

func TestDialTargetChainsThroughProxy(t *testing.T) {
	target := startEcho(t)
	seen := make(chan string, 1)
	proxyAddr := startFakeCONNECTProxy(t, seen)
	useProxy(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: proxyAddr}, nil
	})

	conn, rest, err := dialTarget(context.Background(), target)
	if err != nil {
		t.Fatalf("dialTarget: %v", err)
	}
	defer conn.Close()

	// 握手后上游立刻发的那段字节必须原样捞回来。
	if string(rest) != "EARLY" {
		t.Errorf("残留字节 = %q, 想要 %q —— 少了它隧道一建立就丢开头", rest, "EARLY")
	}

	select {
	case got := <-seen:
		if got != target {
			t.Errorf("代理收到的目标 = %q, 想要 %q", got, target)
		}
	case <-time.After(2 * time.Second):
		t.Error("代理没收到 CONNECT")
	}

	// 隧道本身要能双向通。
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("回显 = %q", buf)
	}
}

// TestDialTargetDirectWhenNoProxy：没配代理时应当直连，
// 不能凭空找一个代理去拨。
func TestDialTargetDirectWhenNoProxy(t *testing.T) {
	target := startEcho(t)
	useProxy(t, func(*http.Request) (*url.URL, error) { return nil, nil })

	conn, rest, err := dialTarget(context.Background(), target)
	if err != nil {
		t.Fatalf("dialTarget: %v", err)
	}
	defer conn.Close()
	if len(rest) != 0 {
		t.Errorf("直连不该有握手残留字节, got %q", rest)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
}

// TestDialTargetRejectsSocksProxy：本机 ALL_PROXY 是 socks5h，
// 手工发 HTTP CONNECT 是接不上的，必须明说而不是瞎发。
func TestDialTargetRejectsSocksProxy(t *testing.T) {
	useProxy(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "socks5", Host: "127.0.0.1:1080"}, nil
	})
	_, _, err := dialTarget(context.Background(), "example.com:443")
	if err == nil {
		t.Fatal("socks5 代理应当报错")
	}
	if !strings.Contains(err.Error(), "暂不支持") {
		t.Errorf("错误应说明不支持该协议: %v", err)
	}
}

func TestNormalizeTarget(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com:8443", "example.com:8443"},
		{"example.com", "example.com:443"}, // CONNECT 基本都是冲 TLS 去的
		{"  example.com:443  ", "example.com:443"},
	}
	for _, c := range cases {
		got, err := normalizeTarget(c.in)
		if err != nil {
			t.Errorf("normalizeTarget(%q) 出错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeTarget(%q) = %q, 想要 %q", c.in, got, c.want)
		}
	}
	if _, err := normalizeTarget("   "); err == nil {
		t.Error("空目标应报错")
	}
}

func TestEnvProxyForUsesInjectedResolver(t *testing.T) {
	want := &url.URL{Scheme: "http", Host: "127.0.0.1:9999"}
	useProxy(t, func(*http.Request) (*url.URL, error) { return want, nil })

	got, err := envProxyFor("example.com:443")
	if err != nil {
		t.Fatalf("envProxyFor: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, 想要 %+v", got, want)
	}
}

func TestEnvProxyForRejectsEmptyHost(t *testing.T) {
	useProxy(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http"}, nil
	})
	if _, err := envProxyFor("example.com:443"); err == nil {
		t.Error("代理缺 host 应报错")
	} else if !strings.Contains(err.Error(), "缺 host") {
		t.Errorf("错误信息不对: %v", err)
	}
}

func TestEnvProxyForPropagatesResolverError(t *testing.T) {
	useProxy(t, func(*http.Request) (*url.URL, error) {
		return nil, io.ErrUnexpectedEOF
	})
	if _, err := envProxyFor("example.com:443"); err == nil {
		t.Error("解析器出错应向上抛")
	}
}

// TestReadConnectResponseRejectsNon200：代理拒绝时必须报错，
// 否则会把 502/403 的响应体当成隧道数据往下传。
func TestReadConnectResponseRejectsNon200(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() {
		_, _ = io.WriteString(server, "HTTP/1.1 403 Forbidden\r\nX-Reason: no\r\n\r\nBODY")
	}()

	if _, err := readConnectResponse(client); err == nil {
		t.Fatal("非 200 应当报错")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误应带上状态: %v", err)
	}
}

func TestReadConnectResponseRejectsGarbageStatusLine(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() { _, _ = io.WriteString(server, "not-a-response\r\n\r\n") }()

	if _, err := readConnectResponse(client); err == nil {
		t.Fatal("状态行格式不对应当报错")
	}
}
