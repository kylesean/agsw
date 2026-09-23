package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const maxBody = 2 << 20 // 2 MiB 上限，防日志爆炸

// HTTPServer 是探针主体：记录请求 → 可选转发 → 记录响应。
// upstream 为空时仅记录、不转发，返回 502 并在响应体中注明。
type HTTPServer struct {
	Listen   string
	Upstream string // 为空 = 只记录
	Recorder *Recorder
	Log      *log.Logger
}

func (s *HTTPServer) logger() *log.Logger {
	if s.Log != nil {
		return s.Log
	}
	return log.Default()
}

// Run 启动并阻塞，直到 ctx 被取消。
func (s *HTTPServer) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{
		Addr:              s.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.Listen, err)
	}
	s.logger().Printf("探针已监听 %s（上游=%s）", s.Listen, orUnset(s.Upstream))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *HTTPServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	reqRec := FromRequest("request", r, body, s.Recorder.BodyMax())
	if err := s.Recorder.Add(reqRec); err != nil {
		s.logger().Printf("写记录失败: %v", err)
	}
	s.logger().Printf("捕获 %s %s%s (%d bytes, proto=%s)",
		r.Method, reqRec.Host, reqRec.Path, reqRec.BodyLen, r.Proto)

	if s.Upstream == "" {
		// 只记录模式：明确告诉 agy 这是探针，别让它误以为是真实错误。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "agsw-probe: no upstream configured (observation only)",
		})
		return
	}

	s.forward(w, r, body)
}

// forward 把请求转给上游，并记录状态码。
func (s *HTTPServer) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	up, err := url.Parse(s.Upstream)
	if err != nil {
		http.Error(w, "bad upstream: "+err.Error(), http.StatusBadGateway)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = up.Scheme
			req.URL.Host = up.Host
			req.Host = up.Host
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			s.logger().Printf("转发失败 %s%s: %v", req.Host, req.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "agsw-probe forward: " + err.Error(),
			})
		},
	}

	// body 已读完，重新灌回去再转发。
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	r.ContentLength = int64(len(body))

	rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	proxy.ServeHTTP(rw, r)

	rec := Record{
		Kind:   "upstream",
		Method: r.Method,
		Host:   up.Host,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Status: rw.status,
		Note:   "forwarded",
	}
	_ = s.Recorder.Add(rec)
	s.logger().Printf("转发 %s%s → %s [%d]", r.Method, r.URL.Path, up.Host, rw.status)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 让流式转发保持可用。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func orUnset(s string) string {
	if s == "" {
		return "(unset, 记录不转发)"
	}
	return s
}

// CONNECTServer 是一个极简 HTTP CONNECT 代理，用于
// 当 agy 走 AGY_PROXY_URL / http_proxy 时观察它真正连去哪个 host:port。
type CONNECTServer struct {
	Listen   string
	Recorder *Recorder
	Log      *log.Logger
}

func (s *CONNECTServer) logger() *log.Logger {
	if s.Log != nil {
		return s.Log
	}
	return log.Default()
}

// Run 启动并阻塞，直到 ctx 取消。
func (s *CONNECTServer) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.Listen, err)
	}
	s.logger().Printf("CONNECT 探针已监听 %s", s.Listen)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept 失败: %w", err)
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *CONNECTServer) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	br := newBufReader(conn)
	req, err := br.ReadRequest()
	if err != nil {
		return
	}

	target := req.Host
	if target == "" {
		target = req.URL.Host
	}

	_ = s.Recorder.Add(Record{
		Kind:   "connect",
		Method: req.Method,
		Target: target,
		Proto:  req.Proto,
		Note:   "CONNECT tunnel requested",
	})
	s.logger().Printf("CONNECT → %s", target)

	// 必须经环境变量里的代理拨号：本机直连 Google IPv4 不通。
	up, upstreamRest, err := dialTarget(ctx, target)
	if err != nil {
		_ = s.Recorder.Add(Record{Kind: "connect", Target: target, Note: "dial failed: " + err.Error()})
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	// 两侧各自被缓冲区压住的字节都要补上，少一边隧道就坏：
	//   clientRest —— 客户端请求行之后立刻发的 TLS ClientHello 开头
	//   upstreamRest —— 握手响应之后上游立刻发的数据
	if rest := br.BufferedBytes(); len(rest) > 0 {
		if _, err := up.Write(rest); err != nil {
			return
		}
	}
	if len(upstreamRest) > 0 {
		if _, err := conn.Write(upstreamRest); err != nil {
			return
		}
	}

	pipe(ctx, conn, up)
}
