// Package proxy 实现反向代理、凭据注入与 Gemini REST ↔ CloudCode 信封双向改写。
// 基于 httputil.ReverseProxy 实现，保持实时刷写与低延迟流式响应。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Account 是转发时要用的最小账号视图。
type Account struct {
	Name        string
	Email       string
	AccessToken string
}

// Picker 决定这一次请求用哪个账号。
//
// 带 ctx 是因为「选号」可能要现刷 access_token——
// 那是一次网络往返，必须能随请求取消，否则客户端断开后
// 我们还会挂着刷一个没人要的 token。
type Picker interface {
	Pick(ctx context.Context) (*Account, error)
}

// StatusReporter 是 Picker 的可选扩展接口，用于接收上游 HTTP 状态码反馈。
type StatusReporter interface {
	ReportStatus(name string, statusCode int)
}

type staticPicker struct{ a *Account }

// Static 返回一个始终给出同一账号的 Picker，便于测试与显式指定。
func Static(a *Account) Picker { return &staticPicker{a} }

func (p *staticPicker) Pick(context.Context) (*Account, error) {
	if p.a == nil || p.a.AccessToken == "" {
		return nil, errors.New("没有可用账号")
	}
	return p.a, nil
}

// ctxAccount 是把选定账号带进 ReverseProxy.Director 的载体。
type ctxAccount struct{}

// StripFields 指定从请求体顶层删除的 JSON 字段名。
// 逐字段保留原始字面量（使用 map[string]json.RawMessage），避免重新编码带来的数值精度丢失。
func (s *Server) StripFields(fields ...string) {
	s.stripFields = append([]string(nil), fields...)
}

// maxStripBody 限制可改写的请求体上限。
// 实测 agy 最大的一次是 59879 字节（59KB 上下文），留足余量即可；
// 超过就原样放行，宁可让上游报错也不要 OOM 或悄悄丢数据。
const maxStripBody = 8 << 20

// maxErrBody 是读入上游错误响应的上限，maxErrLog 是写进日志的上限。
// 错误响应本该很小，但防止上游抽风返回一整个页面把日志冲爆。
const (
	maxErrBody = 64 << 10
	maxErrLog  = 4 << 10
)

// clipForLog 截断超长内容，避免一条错误日志把终端刷穿。
func clipForLog(b []byte, limit int) string {
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + fmt.Sprintf("…(还有 %d 字节)", len(b)-limit)
}

// Server 是反向代理。
type Server struct {
	upstream *url.URL
	picker   Picker
	rp       *httputil.ReverseProxy
	log      *log.Logger

	// stripFields 是要从请求体顶层删掉的 JSON 字段名。
	// 见 StripFields 的说明。
	stripFields []string

	// envelope 开启 Gemini REST ↔ CloudCode v1internal 双向改写。
	envelope bool

	aliasMu      sync.RWMutex
	// modelAliases 是客户端请求模型名至上游实际模型标识的别名映射表。
	modelAliases map[string]string

	// userAgent 是发送给上游的客户端身份标识头。
	userAgent string
}

// New 构造代理。upstream 必须带 scheme 与 host。
func New(rawUpstream string, picker Picker, lg *log.Logger) (*Server, error) {
	u, err := url.Parse(rawUpstream)
	if err != nil {
		return nil, fmt.Errorf("解析上游地址失败: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("上游地址需要带 scheme，例如 https://host（收到 %q）", rawUpstream)
	}
	if picker == nil {
		return nil, errors.New("picker 不能为 nil")
	}
	if lg == nil {
		lg = log.Default()
	}

	s := &Server{upstream: u, picker: picker, log: lg}
	tmp := httputil.NewSingleHostReverseProxy(u)
	baseDirector := tmp.Director

	s.rp = &httputil.ReverseProxy{
		// 每次收到数据后立即刷写，保证 SSE 流式实时吐字
		FlushInterval: -1,
		Director: func(req *http.Request) {
			baseDirector(req)
			req.Host = u.Host
			if a, ok := req.Context().Value(ctxAccount{}).(*Account); ok && a.AccessToken != "" {
				req.Header.Set("Authorization", "Bearer "+a.AccessToken)
			}
			req.Header.Set("User-Agent", s.UserAgentForUpstream())
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			lg.Printf("转发失败 %s%s: %v", r.Host, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "agsw proxy: " + err.Error(),
			})
		},
		// 上游报错时把响应体记下来。agy 只转述 message，
		// 像「Request contains an invalid argument.」这种泛化文案
		// 会把真正的原因（哪个字段、哪个值）藏在 details 里，
		// 不看原始响应根本无从下手。
		//
		// 只在 4xx/5xx 时读：成功的 SSE 是流式的，读了就断流。
		ModifyResponse: func(resp *http.Response) error {
			if a, ok := resp.Request.Context().Value(ctxAccount{}).(*Account); ok && a != nil {
				if rep, ok := s.picker.(StatusReporter); ok {
					rep.ReportStatus(a.Name, resp.StatusCode)
				}
			}
			// 成功响应走信封解包；失败响应保留原始 body 打日志。
			if resp.StatusCode < 400 {
				return s.unwrapResponse(resp)
			}
			if resp.Body == nil || resp.ContentLength == 0 {
				return nil
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
			_ = resp.Body.Close()
			if err != nil {
				return fmt.Errorf("读上游错误响应失败: %w", err)
			}
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			lg.Printf("上游 %d %s: %s",
				resp.StatusCode, resp.Request.URL.Path, clipForLog(b, maxErrLog))
			return nil
		},
	}
	return s, nil
}

// EnableEnvelope 打开信封改写（默认关闭，保持纯透传语义）。
func (s *Server) EnableEnvelope() { s.envelope = true }

// Envelope 返回是否已打开信封改写，供启动日志展示。
func (s *Server) Envelope() bool { return s.envelope }

// SetUserAgent 覆盖发给上游的 User-Agent；留空则用 upstreamUserAgent。
func (s *Server) SetUserAgent(ua string) { s.userAgent = ua }

// SetModelAliases 设置「agy 的模型名 → 上游真实模型键」映射。
// 传 nil 或空表表示不做映射。
func (s *Server) SetModelAliases(m map[string]string) {
	s.aliasMu.Lock()
	defer s.aliasMu.Unlock()
	if len(m) == 0 {
		s.modelAliases = nil
		return
	}
	s.modelAliases = make(map[string]string, len(m))
	for k, v := range m {
		s.modelAliases[k] = v
	}
}

// ModelAliases 返回别名表副本，供启动日志展示。
func (s *Server) ModelAliases() map[string]string {
	s.aliasMu.RLock()
	defer s.aliasMu.RUnlock()
	if len(s.modelAliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.modelAliases))
	for k, v := range s.modelAliases {
		out[k] = v
	}
	return out
}

// UserAgentForUpstream 返回本次要用的 User-Agent。
// Director 用它盖掉客户端发来的头，serve 用它拉模型表（同一个闸门）。
func (s *Server) UserAgentForUpstream() string {
	if s.userAgent != "" {
		return s.userAgent
	}
	return upstreamUserAgent
}

// Upstream 返回上游地址，便于状态展示。
func (s *Server) Upstream() *url.URL { return s.upstream }

// Stripped 返回会被剥离的请求体顶层字段名，供启动日志展示。
func (s *Server) Stripped() []string { return append([]string(nil), s.stripFields...) }

// rewriteBody 读出请求体、删掉指定顶层字段、写回。
//
// 只在 ContentLength >= 0（有明确长度、非分块）时改写：我们得一次性
// 读完才能改，而流式请求体没有终点可等。实测 agy 两次请求都是
// Content-Length: 824 / 59879，符合要求。
func (s *Server) rewriteBody(r *http.Request) error {
	if r.Body == nil || r.ContentLength == 0 || r.ContentLength < 0 {
		// 空请求体无事可做；分块请求体（ContentLength -1）等不到终点，
		// 原样放行让上游自己报错，好过我们半途截断。
		return nil
	}
	if r.ContentLength > maxStripBody {
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxStripBody+1))
	_ = r.Body.Close()
	if err != nil {
		return fmt.Errorf("读请求体失败: %w", err)
	}
	if len(raw) == 0 {
		r.Body = http.NoBody
		return nil
	}

	out, err := stripTopLevel(raw, s.stripFields)
	if err != nil {
		return err
	}
	if len(out) == len(raw) {
		// 一个字段都没删到，保持原样，省一次拷贝。
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}

	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.Header.Del("Transfer-Encoding")
	return nil
}

// applyEnvelope 做请求方向的信封改写：解析路径 → 包壳 → 改写路径。
//
// 路径对不上（非 /v1beta/models/{model}:{method}）时原样放行，
// 这样健康检查、以及将来 agy 发的其它接口都能直接穿过去，
// 不会因为我们认不出而凭空 400。
func (s *Server) applyEnvelope(r *http.Request) error {
	model, method, ok := parseGeminiPath(r.URL.Path)
	if !ok {
		return nil
	}

	// 模型名映射：agy 发的是它自己的叫法，上游的键可能带 -tiered 后缀。
	// 漏了这一步，能通的请求会变成 404 NOT_FOUND。
	s.aliasMu.RLock()
	real, aliased := s.modelAliases[model]
	s.aliasMu.RUnlock()
	if aliased {
		model = real
	}

	// 空 body（例如 GET 或纯方法调用）也要有 request 字段，
	// 否则上游会因缺 message 报 INVALID_ARGUMENT。
	var inner []byte
	if r.Body != nil && r.ContentLength != 0 {
		b, exceeded, err := readAllLimited(r.Body, maxStripBody)
		discardClose(r.Body)
		if err != nil {
			return fmt.Errorf("读请求体失败: %w", err)
		}
		if exceeded {
			return fmt.Errorf("请求体超过 %d 字节，放弃改写", maxStripBody)
		}
		inner = b
	}
	if len(inner) == 0 {
		inner = []byte("{}")
	}

	enveloped, err := buildEnvelope(model, inner)
	if err != nil {
		return err
	}

	r.Body = bytesReader(enveloped)
	r.ContentLength = int64(len(enveloped))
	r.Header.Set("Content-Length", strconv.Itoa(len(enveloped)))
	r.Header.Del("Transfer-Encoding")

	// ReverseProxy 的 Director 会把 upstream.Path 拼在前面，
	// 所以这里只需换成 /v1internal:{method}，query（?alt=sse）原样保留。
	r.URL.Path = v1internalPrefix + method
	r.URL.RawPath = ""
	return nil
}

// unwrapResponse 做响应方向的解壳：SSE 按行流式解，JSON 整体解。
//
// 只在信封模式且 2xx 时调用；出任何岔子都必须返回 nil 并放行原响应 ——
// 改坏一个本来能用的响应，比不解更糟。
func (s *Server) unwrapResponse(resp *http.Response) error {
	if !s.envelope || resp.Body == nil || resp.StatusCode >= 300 {
		return nil
	}
	ct := resp.Header.Get("Content-Type")

	switch {
	case strings.Contains(ct, "text/event-stream"):
		// 流式：包一层按行读取器。绝不能 ReadAll —— 那会把整段回答
		// 攒在内存里等流结束，agy 表现为卡住不吐字。
		resp.Body = newSSEUnwrapper(resp.Body)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil

	case strings.Contains(ct, "application/json"):
		b, exceeded, err := readAllLimited(resp.Body, maxErrBody*16)
		if err != nil {
			discardClose(resp.Body)
			return fmt.Errorf("读取上游响应失败: %w", err)
		}
		if exceeded {
			resp.Body = &multiReadCloser{Reader: io.MultiReader(bytes.NewReader(b), resp.Body), Closer: resp.Body}
			return nil
		}
		discardClose(resp.Body)
		out, ok := unwrapJSONBody(b, maxErrBody*16)
		if !ok {
			resp.Body = bytesReader(b)
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		}
		resp.Body = bytesReader(out)
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
	return nil
}

// stripTopLevel 删掉 JSON 对象的指定顶层字段，其余字段逐字节原样保留。
//
// 用 map[string]json.RawMessage 而不是 interface{}：后者会把温度、
// token 数这类值重新编码，可能把 0.7 写成 0.6999999999999999、
// 把大整数丢精度。RawMessage 让每个字段保持原始字面量。
func stripTopLevel(raw []byte, fields []string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	changed := false
	for _, f := range fields {
		if _, ok := m[f]; ok {
			delete(m, f)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("重新序列化失败: %w", err)
	}
	return out, nil
}

// maxRetryBody 是允许为 429 重试缓存的最大请求体。超过它宁可不做透明重试，
// 也不能为了重试把大请求全部留在内存里。
const maxRetryBody = 8 << 20

// retryResponseWriter 在收到 429 时暂存错误响应而不写给客户端；
// 成功、4xx/5xx 以外的响应则立即透传，保留 SSE 的实时性。
type retryResponseWriter struct {
	dst       http.ResponseWriter
	header    http.Header
	status    int
	body      bytes.Buffer
	suppress  bool
	wroteHead bool
}

func newRetryResponseWriter(dst http.ResponseWriter) *retryResponseWriter {
	return &retryResponseWriter{
		dst:    dst,
		header: make(http.Header),
	}
}

func (w *retryResponseWriter) Header() http.Header { return w.header }

func (w *retryResponseWriter) flushHeaders() {
	dstH := w.dst.Header()
	for k, vv := range w.header {
		for _, v := range vv {
			dstH.Add(k, v)
		}
	}
}

func (w *retryResponseWriter) WriteHeader(code int) {
	if w.wroteHead {
		return
	}
	w.status = code
	w.wroteHead = true
	if code == http.StatusTooManyRequests {
		w.suppress = true
		return
	}
	w.flushHeaders()
	w.dst.WriteHeader(code)
}

func (w *retryResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if w.suppress {
		if w.body.Len() < maxErrBody {
			_, _ = w.body.Write(p)
		}
		return len(p), nil
	}
	return w.dst.Write(p)
}

func (w *retryResponseWriter) Flush() {
	if !w.suppress {
		if f, ok := w.dst.(http.Flusher); ok {
			f.Flush()
		}
	}
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}

func retryRequestBody(r *http.Request) ([]byte, bool, error) {
	if r.Body == nil || r.ContentLength < 0 || r.ContentLength > maxRetryBody {
		return nil, false, nil
	}
	if r.Body == http.NoBody || r.ContentLength == 0 {
		return nil, true, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRetryBody+1))
	if err != nil {
		// 读失败时不重试，把已经读出的部分放回去，交给原有转发路径处理。
		r.Body = &multiReadCloser{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
		return nil, false, nil
	}
	if len(body) > maxRetryBody {
		r.Body = &multiReadCloser{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
		return nil, false, nil
	}
	return body, true, nil
}

// ServeHTTP 改写请求体 → （信封改写）→ 选号 → 注入 → 转发。
//
// 改写必须在选号之前完成：它可能直接失败（body 不是 JSON），
// 那时还没必要去动账号、更不该发起刷新。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(s.stripFields) > 0 {
		if err := s.rewriteBody(r); err != nil {
			s.log.Printf("改写请求体失败 %s%s: %v", r.Host, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w,
				`{"error":{"code":400,"message":"agsw: 改写请求体失败: `+err.Error()+`"}}`)
			return
		}
	}

	// 信封改写必须排在 strip 之后（strip 作用于 agy 的原始 body），
	// 并且同样在选号之前：路径对不上是请求本身的问题，与账号无关。
	if s.envelope {
		if err := s.applyEnvelope(r); err != nil {
			s.log.Printf("信封改写失败 %s%s: %v", r.Host, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w,
				`{"error":{"code":400,"message":"agsw: `+err.Error()+`"}}`)
			return
		}
	}

	body, retryable, bodyErr := retryRequestBody(r)
	if bodyErr != nil {
		s.log.Printf("读取请求体失败 %s%s: %v", r.Host, r.URL.Path, bodyErr)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	for attempt := 0; ; attempt++ {
		a, err := s.picker.Pick(r.Context())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		ctx := context.WithValue(r.Context(), ctxAccount{}, a)

		req := r.Clone(ctx)
		if retryable {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
		s.log.Printf("→ %s (%s) %s%s", a.Name, a.Email, r.Host, r.URL.RequestURI())

		if !retryable {
			s.rp.ServeHTTP(w, req)
			return
		}

		rw := newRetryResponseWriter(w)
		s.rp.ServeHTTP(rw, req)
		if rw.status != http.StatusTooManyRequests || attempt >= 1 {
			if rw.status == http.StatusTooManyRequests {
				rw.header.Set("Content-Length", strconv.Itoa(rw.body.Len()))
				rw.flushHeaders()
				w.WriteHeader(rw.status)
				_, _ = w.Write(rw.body.Bytes())
			}
			return
		}

		s.log.Printf("上游 429，账号 %s 冷却，重试下一个账号", a.Name)
	}
}
