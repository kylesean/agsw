package probe

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// safeBuffer 是并发安全的 bytes.Buffer，避免测试里为了一个 buffer 加锁。
type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestHandleObservesWithoutUpstream 验证不配置上游时，探针仅记录而不转发，且敏感头正确脱敏。
func TestHandleObservesWithoutUpstream(t *testing.T) {
	var buf safeBuffer
	s := &HTTPServer{
		Listen:   "127.0.0.1:0",
		Upstream: "",
		Recorder: New(&buf, 4096),
		Log:      discardLogger(),
	}

	body := `{"model":"gemini-3.1","prompt":"hi"}`
	req := httptest.NewRequest("POST", "http://gw/v1:streamGenerateContent?alt=sse", strings.NewReader(body))
	req.Host = "gw.example.com"
	req.Header.Set("Authorization", "Bearer SECRET-TOKEN-0123456789")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	rec := httptest.NewRecorder()
	s.handle(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, 想要 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no upstream configured") {
		t.Errorf("body 应说明原因: %s", rec.Body.String())
	}

	out := buf.String()
	if out == "" {
		t.Fatal("没有写入任何记录")
	}
	for _, leak := range []string{"SECRET-TOKEN-0123456789"} {
		if strings.Contains(out, leak) {
			t.Errorf("日志泄露了 token: %s", out)
		}
	}
	if !strings.Contains(out, `"path":"/v1:streamGenerateContent"`) {
		t.Errorf("没记下 path: %s", out)
	}
	if !strings.Contains(out, `"query":"alt=sse"`) {
		t.Errorf("没记下 query: %s", out)
	}
	if !strings.Contains(out, `"Connect-Protocol-Version"`) {
		t.Errorf("没记下 Connect 协议头: %s", out)
	}
	// body 是 {"model":"gemini-3.1","prompt":"hi"}，36 字节，未触发截断。
	if !strings.Contains(out, `"body_len":36`) {
		t.Errorf("body_len 不对（应为截断前长度）: %s", out)
	}
}

// TestHandleForwardsWhenUpstreamSet 确认配了上游才转发，且响应透传回来。
func TestHandleForwardsWhenUpstreamSet(t *testing.T) {
	var gotAuth, gotPath, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	var buf safeBuffer
	s := &HTTPServer{
		Listen:   "127.0.0.1:0",
		Upstream: up.URL,
		Recorder: New(&buf, 4096),
		Log:      discardLogger(),
	}

	req := httptest.NewRequest("POST", "http://gw/predict", strings.NewReader(`{"a":1}`))
	req.Header.Set("Authorization", "Bearer FROM-CLIENT")

	rec := httptest.NewRecorder()
	s.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d", rec.Code)
	}
	if gotPath != "/predict" {
		t.Errorf("上游收到 path = %q", gotPath)
	}
	if gotHost == "gw" {
		t.Errorf("上游 Host 没被改写，仍是 %q", gotHost)
	}
	if gotAuth != "Bearer FROM-CLIENT" {
		t.Errorf("透传阶段 Authorization 应原样保留, 得到 %q", gotAuth)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("上游响应没透传: %s", rec.Body.String())
	}

	out := buf.String()
	if !strings.Contains(out, `"kind":"upstream"`) {
		t.Errorf("没记下 upstream 记录: %s", out)
	}
	if !strings.Contains(out, `"status":200`) {
		t.Errorf("没记下状态码: %s", out)
	}
}

// TestHandleReadBodyFailure 返回 400 而不是 panic。
func TestHandleReadBodyFailure(t *testing.T) {
	var buf safeBuffer
	s := &HTTPServer{Recorder: New(&buf, 1024), Log: discardLogger()}

	req := httptest.NewRequest("POST", "http://gw/x", nil)
	req.Body = errReader{}
	rec := httptest.NewRecorder()
	s.handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 想要 400", rec.Code)
	}
}

func TestHandleNilBodyDoesNotPanic(t *testing.T) {
	var buf safeBuffer
	s := &HTTPServer{Recorder: New(&buf, 1024), Log: discardLogger()}
	req := httptest.NewRequest("GET", "http://gw/x", nil)
	rec := httptest.NewRecorder()
	s.handle(rec, req) // 只要不 panic 即可
}

// TestRunStopsOnContextCancel 确认探针能被信号干净停掉。
func TestRunStopsOnContextCancel(t *testing.T) {
	var buf safeBuffer
	s := &HTTPServer{Listen: "127.0.0.1:0", Recorder: New(&buf, 1024), Log: discardLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("取消后应返回 nil, 得到 %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Run 没有在取消后退出")
	}
}

func TestRunRejectsInvalidListen(t *testing.T) {
	var buf safeBuffer
	s := &HTTPServer{Listen: "127.0.0.1:99999", Recorder: New(&buf, 1024), Log: discardLogger()}
	if err := s.Run(context.Background()); err == nil {
		t.Error("非法监听地址应当报错")
	}
}

// TestStatusRecorderCapturesStatusAndFlushes 保证流式转发可用。
func TestStatusRecorderCapturesStatusAndFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	sr.WriteHeader(http.StatusTeapot)
	if sr.status != http.StatusTeapot || rec.Code != http.StatusTeapot {
		t.Errorf("状态码未透传: sr=%d rec=%d", sr.status, rec.Code)
	}
	sr.Flush() // 应当安全，不 panic
}

func TestOrUnset(t *testing.T) {
	if orUnset("") == "" {
		t.Error("空串应给出可读占位")
	}
	if orUnset("http://x") != "http://x" {
		t.Error("非空串应原样返回")
	}
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReader) Close() error               { return nil }
