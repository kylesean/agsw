package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testUpstream 记录收到的 Authorization，用来验证注入确实生效。
func testUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newTestServer(t *testing.T, upstream string, p Picker) *Server {
	t.Helper()
	s, err := New(upstream, p, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestInjectsAuthorizationFromPickedAccount(t *testing.T) {
	up, got := testUpstream(t)
	s := newTestServer(t, up.URL, Static(&Account{
		Name: "work", Email: "w@x", AccessToken: "TOKEN-AAA",
	}))

	probe := httptest.NewRecorder()
	s.ServeHTTP(probe, httptest.NewRequest("POST", "http://proxy.local/v1:predict", nil))

	if probe.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", probe.Code, probe.Body.String())
	}
	if len(*got) != 1 || (*got)[0] != "Bearer TOKEN-AAA" {
		t.Errorf("上游收到 Authorization = %q, want %q", *got, "Bearer TOKEN-AAA")
	}
	if !strings.Contains(probe.Body.String(), `"ok":true`) {
		t.Errorf("上游响应没透传: %s", probe.Body.String())
	}
}

func TestRewritesHostToUpstream(t *testing.T) {
	var seenHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	// 上游自身就是 127.0.0.1:port，所以断言必须比对上游 URL 的 host，
	// 而不是笼统地排除 127.0.0.1（那样测不出任何东西）。
	wantHost := mustHost(t, up.URL)

	s := newTestServer(t, up.URL, Static(&Account{AccessToken: "t"}))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "http://proxy.local:7897/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if seenHost != wantHost {
		t.Errorf("上游看到 Host = %q, 想要 %q（不能透传 127.0.0.1:7897）", seenHost, wantHost)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 %q: %v", raw, err)
	}
	return u.Host
}

func TestPathAndQueryArePreserved(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{AccessToken: "t"}))
	s.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "http://proxy.local/a/b?k=v&x=1", nil))

	if seen != "/a/b?k=v&x=1" {
		t.Errorf("上游收到 URI = %q", seen)
	}
}

func TestReturns503WhenPickerFails(t *testing.T) {
	up, got := testUpstream(t)
	s := newTestServer(t, up.URL, failingPicker{})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "http://proxy.local/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d, want 503", rec.Code)
	}
	if len(*got) != 0 {
		t.Errorf("picker 失败时不该碰上游")
	}
	if !strings.Contains(rec.Body.String(), "no account") {
		t.Errorf("响应应带上原因: %s", rec.Body.String())
	}
}

func TestStaticPickerRejectsEmptyToken(t *testing.T) {
	ctx := context.Background()
	if _, err := (&staticPicker{a: &Account{Name: "x"}}).Pick(ctx); err == nil {
		t.Error("空 access_token 应当报错")
	}
	if _, err := (&staticPicker{}).Pick(ctx); err == nil {
		t.Error("nil 账号应当报错")
	}
}

func TestNewRejectsBadUpstream(t *testing.T) {
	for _, bad := range []string{"", "no-scheme", "://x", "http://"} {
		if _, err := New(bad, Static(&Account{AccessToken: "t"}), nil); err == nil {
			t.Errorf("New(%q) 应当报错", bad)
		}
	}
}

func TestNewRejectsNilPicker(t *testing.T) {
	if _, err := New("http://example.com", nil, nil); err == nil {
		t.Error("nil picker 应当报错")
	}
}

func TestUpstreamIsExposed(t *testing.T) {
	s := newTestServer(t, "https://aicode.googleapis.com", Static(&Account{AccessToken: "t"}))
	if got := s.Upstream().Host; got != "aicode.googleapis.com" {
		t.Errorf("Upstream().Host = %q", got)
	}
}

func TestErrorHandlerReturnsJSON(t *testing.T) {
	// 指一个必然连不上的上游，逼出 ErrorHandler。
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "t"}))

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "http://proxy.local/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "agsw proxy") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestAccountContextDoesNotLeakAcrossRequests(t *testing.T) {
	// 确认账号是按请求注入的，不是全局状态。
	up, got := testUpstream(t)

	s := newTestServer(t, up.URL, &rotatingPicker{tokens: []string{"ONE", "TWO"}})

	for i := 0; i < 2; i++ {
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://proxy.local/", nil))
	}
	if len(*got) != 2 {
		t.Fatalf("请求数 = %d", len(*got))
	}
	if (*got)[0] != "Bearer ONE" || (*got)[1] != "Bearer TWO" {
		t.Errorf("轮换没生效: %v", *got)
	}
}

// TestStreamsSSEWithoutBuffering 验证 SSE 流式响应被实时刷写透传，无额外缓冲延迟。
func TestStreamsSSEWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "data: {\"n\":1}\n\n"); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release // 上游故意停住，等测试确认第一条已经穿过代理
		_, _ = io.WriteString(w, "data: {\"n\":2}\n\n")
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{Name: "sse", AccessToken: "T"}))
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, 想要 text/event-stream", got)
	}

	lines := make(chan string, 8)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()

	// 一个 SSE 事件是 "data: ...\n\n"（两行：数据行 + 空行），
	// 所以必须循环读到命中目标子串，不能只取一行。
	awaitLine := func(want, phase string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ln, ok := <-lines:
				if !ok {
					t.Fatalf("%s: 连接在收到 %s 前就断了", phase, want)
				}
				if strings.Contains(ln, want) {
					return
				}
			case <-deadline:
				t.Fatalf("%s: 3 秒内没读到含 %s 的行 —— 响应被缓冲或流断了", phase, want)
			}
		}
	}

	// 上游此刻还阻塞着，第一条必须已经穿过来，否则就是整段缓冲。
	awaitLine(`"n":1`, "第一条")

	closeRelease()

	awaitLine(`"n":2`, "第二条")
}

// TestPickReceivesRequestContext 钉住「选号用请求 ctx」这条约定：
// 选号可能要现刷 access_token（一次网络往返），
// 必须能随客户端断开而取消，否则会挂着刷一个没人要的 token。
func TestPickReceivesRequestContext(t *testing.T) {
	got := make(chan context.Context, 1)
	s := &Server{
		upstream: mustParse(t, "http://127.0.0.1:1"),
		picker:   &capturePicker{ch: got, err: errNoAccount},
		log:      log.New(io.Discard, "", 0),
	}

	type markerKey struct{}
	req := httptest.NewRequest("GET", "http://x/", nil)
	req = req.WithContext(context.WithValue(req.Context(), markerKey{}, "from-request"))

	// picker 返回错误，ServeHTTP 会 503 而不去连上游。
	s.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case seen := <-got:
		if seen.Value(markerKey{}) != "from-request" {
			t.Error("选号拿到的不是请求 context，刷新将无法随客户端断开取消")
		}
	default:
		t.Fatal("picker 没被调用")
	}
}

type capturePicker struct {
	ch  chan context.Context
	err error
}

func (c *capturePicker) Pick(ctx context.Context) (*Account, error) {
	c.ch <- ctx
	if c.err != nil {
		return nil, c.err
	}
	return &Account{Name: "c", AccessToken: "T"}, nil
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// TestTransportInheritsProxyFromEnvironment 钉住一个致命的隐含假设。
//
// 本机到 Google 的 IPv4 直连是不通的（实测 net.DialTimeout 对
// 172.217.x.x:443 i/o timeout），一切出网必须走环境变量里的本地代理
// （实测 https_proxy=http://127.0.0.1:10808）。
//
// 我们刻意把 ReverseProxy.Transport 留 nil，好让它落到
// http.DefaultTransport —— 而 DefaultTransport 正是带着
// Proxy: http.ProxyFromEnvironment。一旦将来有人给 Transport 赋值
// 却忘了配 Proxy，serve 会静默出不了网（连上游直接 502），
// 而所有单测因为上游是 127.0.0.1（在 NO_PROXY 里）依然全绿。
// 这个测试就是那道防线。
// TestStripTopLevelRemovesOnlyNamedField 验证从请求体顶层剔除指定字段，保留其余字段及原始值。
func TestStripTopLevelRemovesOnlyNamedField(t *testing.T) {
	// 用与探针同构的字段顺序。
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"systemInstruction":{"role":"user","parts":[{"text":"sys"}]},` +
		`"labels":{"client":"antigravity-cli"},` +
		`"generationConfig":{"temperature":0.7,"maxOutputTokens":8192,` +
		`"thinkingConfig":{"thinkingBudget":0}},` +
		`"sessionId":"-3750763034362895579"}`

	out, err := stripTopLevel([]byte(body), []string{"sessionId"})
	if err != nil {
		t.Fatalf("stripTopLevel: %v", err)
	}
	if bytes.Contains(out, []byte("sessionId")) {
		t.Errorf("sessionId 没删掉: %s", out)
	}
	for _, keep := range []string{"contents", "systemInstruction", "labels", "generationConfig"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Errorf("不该被删的字段 %q 丢了: %s", keep, out)
		}
	}

	// 数值必须保持原字面量，不能被重新编码改写。
	if !bytes.Contains(out, []byte("0.7")) {
		t.Errorf("temperature 被改写了: %s", out)
	}
	if !bytes.Contains(out, []byte("8192")) {
		t.Errorf("maxOutputTokens 被改写了: %s", out)
	}
}

// TestStripTopLevelPreservesRawFieldValue 钉住 RawMessage 语义：
// 用 interface{} 解析会把 0.7 变成 0.6999999999999999，
// 那会让上游收到一个与 agy 原意不同的请求。
func TestStripTopLevelPreservesRawFieldValue(t *testing.T) {
	body := `{"generationConfig":{"temperature":0.7},"sessionId":"x"}`
	out, err := stripTopLevel([]byte(body), []string{"sessionId"})
	if err != nil {
		t.Fatalf("stripTopLevel: %v", err)
	}
	var m map[string]map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := string(m["generationConfig"]["temperature"]); got != "0.7" {
		t.Errorf("temperature = %s, 想要原字面量 0.7", got)
	}
}

func TestStripTopLevelNoopWhenFieldAbsent(t *testing.T) {
	body := `{"contents":[]}`
	out, err := stripTopLevel([]byte(body), []string{"sessionId"})
	if err != nil {
		t.Fatalf("stripTopLevel: %v", err)
	}
	if string(out) != body {
		t.Errorf("没有可删字段时应原样返回, got %s", out)
	}
}

func TestStripTopLevelRejectsNonObject(t *testing.T) {
	if _, err := stripTopLevel([]byte(`[1,2,3]`), []string{"sessionId"}); err == nil {
		t.Error("顶层数组应报错而不是静默通过")
	}
	if _, err := stripTopLevel([]byte(`not json`), []string{"sessionId"}); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// TestStripFieldsThroughProxy 是端到端的：请求穿过代理后，
// sessionId 必须消失、其余字段必须完好、Content-Length 必须同步更新。
func TestStripFieldsThroughProxy(t *testing.T) {
	const body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"labels":{"client":"antigravity-cli"},` +
		`"generationConfig":{"temperature":0.7},"sessionId":"SID-123"}`

	var gotBody, gotCT string
	var gotCL int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		gotCL = r.ContentLength
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{Name: "a", AccessToken: "T"}))
	s.StripFields("sessionId")

	req := httptest.NewRequest("POST", "http://proxy/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(gotBody, "sessionId") {
		t.Errorf("sessionId 没被剥离: %s", gotBody)
	}
	for _, keep := range []string{"contents", "labels", "generationConfig"} {
		if !strings.Contains(gotBody, keep) {
			t.Errorf("字段 %q 不该丢: %s", keep, gotBody)
		}
	}
	if !strings.Contains(gotBody, `"temperature":0.7`) {
		t.Errorf("temperature 被改写: %s", gotBody)
	}
	if want := int64(len(gotBody)); gotCL != want {
		t.Errorf("Content-Length = %d, 实际 body %d —— 长度没同步，上游会读到截断或挂起", gotCL, want)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if code := rec.Code; code != http.StatusOK {
		t.Errorf("status = %d, body=%s", code, rec.Body.String())
	}
}

// TestStripFieldsRejectsNonJSONBody：body 不是 JSON 时必须 400，
// 而不是把半截数据转发给上游（那会变成一个难查的上游 500）。
func TestStripFieldsRejectsNonJSONBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("body 非法时不该转发到上游")
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{Name: "a", AccessToken: "T"}))
	s.StripFields("sessionId")

	req := httptest.NewRequest("POST", "http://proxy/x", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, 想要 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "JSON") {
		t.Errorf("错误信息应说明原因: %s", rec.Body.String())
	}
}

// TestStripFieldsSkipsChunkedBody：分块请求体等不到终点，
// 原样放行（宁可上游报错，也不能半途截断）。
func TestStripFieldsSkipsChunkedBody(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{Name: "a", AccessToken: "T"}))
	s.StripFields("sessionId")

	req := httptest.NewRequest("POST", "http://proxy/x", strings.NewReader(`{"sessionId":"x"}`))
	req.ContentLength = -1 // 模拟分块，无 Content-Length
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(gotBody, "sessionId") {
		t.Errorf("分块请求体不该被改写: %s", gotBody)
	}
}

func TestStrippedReturnsCopy(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	s.StripFields("a", "b")
	got := s.Stripped()
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	got[0] = "MUTATED"
	if s.Stripped()[0] != "a" {
		t.Error("Stripped 应返回副本")
	}
}

func TestTransportInheritsProxyFromEnvironment(t *testing.T) {
	s := newTestServer(t, "https://aicode.googleapis.com", Static(&Account{AccessToken: "T"}))

	// 若未显式覆盖 Transport，ReverseProxy 将默认使用 http.DefaultTransport，
	// 它默认挂载了 http.ProxyFromEnvironment，自动支持 HTTP_PROXY/HTTPS_PROXY。
	if s.rp.Transport == nil {
		dt, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			t.Fatalf("http.DefaultTransport 类型变了: %T", http.DefaultTransport)
		}
		if dt.Proxy == nil {
			t.Fatal("DefaultTransport.Proxy 为 nil，将无法继承环境变量代理")
		}
		return
	}

	// 若显式指定了 Transport，必须确保配置了 Proxy 函数。
	rt, ok := s.rp.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("自定义 Transport 是 %T，期望 *http.Transport 以便校验 Proxy", s.rp.Transport)
	}
	if rt.Proxy == nil {
		t.Fatal("自定义 Transport 缺少 Proxy 配置")
	}
}

func TestContextValueKeyIsUnexportedAndUnique(t *testing.T) {
	// ctxAccount{} 是未导出类型，外部无法伪造我们的 context key。
	var a, b interface{} = ctxAccount{}, ctxAccount{}
	if a != b {
		t.Error("同类型零值应当相等")
	}
	if _, ok := context.Background().Value(ctxAccount{}).(*Account); ok {
		t.Error("空 context 不该带出账号")
	}
}

type failingPicker struct{}

func (failingPicker) Pick(context.Context) (*Account, error) {
	return nil, errNoAccount
}

// errNoAccount 的文案必须含 "no account"，TestReturns503WhenPickerFails 断言它。
var errNoAccount = errors.New("no account available")

type rotatingPicker struct {
	tokens []string
	i      int
}

func (p *rotatingPicker) Pick(context.Context) (*Account, error) {
	tok := p.tokens[p.i%len(p.tokens)]
	p.i++
	return &Account{Name: "r", AccessToken: tok}, nil
}

type mockStatusReporterPicker struct {
	picker       Picker
	reportedName string
	reportedCode int
	mu           sync.Mutex
}

func (m *mockStatusReporterPicker) Pick(ctx context.Context) (*Account, error) {
	return m.picker.Pick(ctx)
}

func (m *mockStatusReporterPicker) ReportStatus(name string, code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reportedName = name
	m.reportedCode = code
}

func TestProxyReports429ToStatusReporter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"quota exceeded"}`)
	}))
	defer up.Close()

	reporter := &mockStatusReporterPicker{
		picker: Static(&Account{Name: "test-acc", AccessToken: "TOK"}),
	}
	s := newTestServer(t, up.URL, reporter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://proxy.local/v1:predict", strings.NewReader(`{}`))
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("预期 429，实际得到 %d", rec.Code)
	}

	reporter.mu.Lock()
	name, code := reporter.reportedName, reporter.reportedCode
	reporter.mu.Unlock()

	if name != "test-acc" || code != http.StatusTooManyRequests {
		t.Errorf("预期上报 (test-acc, 429)，实际得到 (%s, %d)", name, code)
	}
}

func TestProxyRetries429BeforeResponseOnNextAccount(t *testing.T) {
	var gotTokens []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"prompt":"hello"}` {
			t.Errorf("重试请求体 = %q", body)
		}
		token := r.Header.Get("Authorization")
		gotTokens = append(gotTokens, token)
		if len(gotTokens) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	picker := &rotatingPicker{tokens: []string{"A", "B"}}
	s := newTestServer(t, up.URL, picker)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("POST", "http://proxy.local/v1:predict", strings.NewReader(`{"prompt":"hello"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("最终状态码 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(gotTokens) != 2 || gotTokens[0] != "Bearer A" || gotTokens[1] != "Bearer B" {
		t.Fatalf("上游收到 token = %v, want [Bearer A Bearer B]", gotTokens)
	}
}
