package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseGeminiPath(t *testing.T) {
	cases := []struct {
		in         string
		model      string
		method     string
		ok         bool
		wantErrMsg string
	}{
		{in: "/v1beta/models/gemini-3.1-flash-lite:streamGenerateContent",
			model: "gemini-3.1-flash-lite", method: "streamGenerateContent", ok: true},
		{in: "/v1beta/models/gemini-3.8-flash:generateContent",
			model: "gemini-3.8-flash", method: "generateContent", ok: true},
		// 不是生成路径：原样放行而不是报错，否则健康检查会被我们自己挡掉。
		{in: "/", ok: false},
		{in: "/v1beta/models", ok: false},
		{in: "/v1beta/models/only-model", ok: false},
		{in: "/v1beta/models/:streamGenerateContent", ok: false},
		{in: "/v1beta/models/x:", ok: false},
		// 模型名里带斜杠 / 方法里带斜杠都不是我们要改写的形态。
		{in: "/v1beta/models/a/b:streamGenerateContent", ok: false},
	}
	for _, c := range cases {
		model, method, ok := parseGeminiPath(c.in)
		if ok != c.ok {
			t.Errorf("parseGeminiPath(%q) ok = %v, 想要 %v", c.in, ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if model != c.model || method != c.method {
			t.Errorf("parseGeminiPath(%q) = (%q,%q), 想要 (%q,%q)",
				c.in, model, method, c.model, c.method)
		}
	}
}

// TestBuildEnvelopePreservesInnerBytes 钉住「内层不重编码」这条约定：
// 温度 0.7、thinkingBudget=-1 这类值一旦被 Unmarshal→Marshal，
// 可能变成另一个字面量，上游收到的就不是 agy 原意了。
func TestBuildEnvelopePreservesInnerBytes(t *testing.T) {
	inner := []byte(`{"generationConfig":{"temperature":0.7,"thinkingConfig":{"thinkingBudget":-1}},` +
		`"sessionId":"-3750763034362895579","labels":{"client":"antigravity-cli"}}`)

	out, err := buildEnvelope("gemini-3.1-flash-lite", inner)
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	if !bytes.Contains(out, inner) {
		t.Errorf("内层字节被改动了:\n got=%s", out)
	}
	if !strings.HasPrefix(string(out), `{"model":"gemini-3.1-flash-lite","request":`) {
		t.Errorf("信封前缀不对: %s", out)
	}
	if !bytes.HasSuffix(out, []byte("}")) {
		t.Errorf("信封没闭合: %s", out)
	}
	// 反证：重编码会把 -1 变成别的吗？这里只保证原字面量还在。
	if !bytes.Contains(out, []byte(`"thinkingBudget":-1`)) {
		t.Errorf("thinkingBudget 原字面量丢了: %s", out)
	}
}

func TestBuildEnvelopeRejectsNonObject(t *testing.T) {
	for _, bad := range []string{`[1,2,3]`, `"str"`, `123`, `not json`} {
		if _, err := buildEnvelope("m", []byte(bad)); err == nil {
			t.Errorf("buildEnvelope(%q) 应当报错", bad)
		}
	}
}

func TestBuildEnvelopeEmptyInnerBecomesObject(t *testing.T) {
	out, err := buildEnvelope("m", nil)
	if err != nil {
		t.Fatalf("buildEnvelope(nil): %v", err)
	}
	if string(out) != `{"model":"m","request":{}}` {
		t.Errorf("空内层应当变 {}: %s", out)
	}
}

// TestUnwrapSSELine 是「agy 能不能读到候选」的判据：
// 上游包了 {"response":{...}}，不解开 agy 就拿不到 candidates。
func TestUnwrapSSELine(t *testing.T) {
	// 与实测上游逐字节同构。
	raw := `data: {"response": {"candidates": [{"content": {"role": "model","parts": [{"text": "Hello!"}]}}],` +
		`"usageMetadata": {"promptTokenCount": 2},"modelVersion": "gemini-3.1-flash-lite"},` +
		`"traceId": "9b5c9d95819d7780","metadata": {}}`

	got, ok := unwrapSSELine(raw)
	if !ok {
		t.Fatal("没能解开信封")
	}
	if !strings.HasPrefix(got, "data: ") {
		t.Errorf("改写后仍应是 data: 行: %q", got)
	}
	if !strings.Contains(got, `"candidates"`) {
		t.Errorf("candidates 应当被提到顶层: %s", got)
	}
	if strings.Contains(got, `"response"`) {
		t.Errorf("response 包装没拆掉: %s", got)
	}
	if strings.Contains(got, "traceId") {
		t.Errorf("traceId 该丢弃: %s", got)
	}
}

// TestUnwrapSSELinePassesThroughUnknown 保证「解不开就原样放行」——
// 改坏一个正常响应比不解更糟。
func TestUnwrapSSELinePassesThroughUnknown(t *testing.T) {
	cases := []string{
		"",                               // 空行
		"\n",                             // 只有换行
		"event: ping\n",                  // 非 data 行
		": keep-alive comment\n",         // SSE 注释
		"data: [DONE]\n",                 // 终止哨兵
		"data: \n",                       // 空 payload
		"data: not json\n",               // 非 JSON
		"data: {\"candidates\":[]}\n",    // 已经是解好的形状
		"data: {\"response\":\"str\"}\n", // response 不是对象
	}
	for _, in := range cases {
		if out, ok := unwrapSSELine(strings.TrimRight(in, "\n")); ok {
			t.Errorf("unwrapSSELine(%q) 不该改写, got %q", in, out)
		}
	}
}

func TestUnwrapJSONBody(t *testing.T) {
	in := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]},"traceId":"x"}`)
	out, ok := unwrapJSONBody(in, 1<<20)
	if !ok {
		t.Fatal("没解开非流式信封")
	}
	if !bytes.Contains(out, []byte(`"candidates"`)) {
		t.Errorf("candidates 没提到顶层: %s", out)
	}
	if bytes.Contains(out, []byte(`"response"`)) {
		t.Errorf("response 没拆掉: %s", out)
	}
	// 超限必须拒绝，而不是硬读。
	if _, ok := unwrapJSONBody(in, 4); ok {
		t.Error("超过 limit 时应当拒绝")
	}
}

// TestSSEUnwrapperYieldsPerLine 钉住流式语义：改写器不能把整段攒着返回，
// 否则 59KB 上下文的流式回答会被卡在缓冲里，agy 表现为卡住不吐字。
func TestSSEUnwrapperYieldsPerLine(t *testing.T) {
	upstream := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"a"}]}}]}}` + "\n\n" +
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}}` + "\n\n"

	u := newSSEUnwrapper(io.NopCloser(strings.NewReader(upstream)))

	// 用很小的缓冲读，逼出分片逻辑（pending 余量必须正确续上）。
	var got []byte
	buf := make([]byte, 7)
	for {
		n, err := u.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			if err != io.EOF {
				t.Fatalf("Read: %v", err)
			}
			break
		}
	}
	if u.Changed() != 2 {
		t.Errorf("改写了 %d 行, 想要 2", u.Changed())
	}
	if strings.Contains(string(got), `"response"`) {
		t.Errorf("信封没拆干净: %s", got)
	}
	if strings.Count(string(got), `"text":"a"`) != 1 ||
		strings.Count(string(got), `"text":"b"`) != 1 {
		t.Errorf("两条事件没都透传: %s", got)
	}
	// 换行结构要保住，否则 SSE 事件会被粘成一条。
	if strings.Count(string(got), "data: ") != 2 {
		t.Errorf("data: 行数不对: %q", got)
	}
}

// TestSSEUnwrapperKeepsFinalLineWithoutNewline：
// EOF 处最后一行没有换行，绝不能凭空给它补一个 —— 那会多出一个空行。
func TestSSEUnwrapperKeepsFinalLineWithoutNewline(t *testing.T) {
	const in = `data: {"response":{"candidates":[]}}`
	u := newSSEUnwrapper(io.NopCloser(strings.NewReader(in)))
	got, err := io.ReadAll(u)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Errorf("不该给 EOF 行补换行: %q", got)
	}
	if u.Changed() != 1 {
		t.Errorf("改写了 %d 行, 想要 1", u.Changed())
	}
}

// TestSSEUnwrapperNormalizesCRLF 验证将上游 CRLF 统一归一化为 LF 换行。
func TestSSEUnwrapperNormalizesCRLF(t *testing.T) {
	upstream := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"a\"}]}}]}}\r\n" +
		"\r\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"b\"}]}}]}}\r\n" +
		"\r\n"

	u := newSSEUnwrapper(io.NopCloser(strings.NewReader(upstream)))
	got, err := io.ReadAll(u)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if strings.ContainsAny(string(got), "\r") {
		t.Errorf("回吐里还有 CR，会造成混合换行: %q", got)
	}
	if u.Changed() != 2 {
		t.Errorf("改写了 %d 行, 想要 2", u.Changed())
	}
	// 空分隔行必须是纯 LF，否则事件不会被 dispatch。
	if strings.Count(string(got), "\n\n") != 2 {
		t.Errorf("空分隔行结构不对（要 2 个纯 LF 空行）: %q", got)
	}
	if strings.Count(string(got), "data: ") != 2 {
		t.Errorf("data: 行数不对: %q", got)
	}
}

// newEnvelopeServer 造一个开了信封模式的代理，指向给定上游。
func newEnvelopeServer(t *testing.T, upstream string, p Picker) *Server {
	t.Helper()
	s := newTestServer(t, upstream, p)
	s.EnableEnvelope()
	return s
}

// TestEnvelopeEndToEndThroughProxy 验证客户端请求经代理改写为 CloudCode 信封并注入鉴权与客户端 UA。
func TestEnvelopeEndToEndThroughProxy(t *testing.T) {
	var gotPath, gotUA, gotAuth, gotQuery, gotBody string

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)

		w.Header().Set("Content-Type", "text/event-stream")
		// 与实测上游逐字节同构：traceId / metadata 在 response 外层。
		_, _ = io.WriteString(w,
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello!"}]}}],`+
				`"usageMetadata":{"promptTokenCount":2}},`+
				`"traceId":"t","metadata":{}}`+"\n\n")
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{Name: "a", AccessToken: "TOK"}))

	// 与实测 agy 请求逐字节同构。
	const agyBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"labels":{"client":"antigravity-cli"},` +
		`"generationConfig":{"maxOutputTokens":1024,"thinkingConfig":{"thinkingBudget":-1}},` +
		`"sessionId":"-3750763034362895579"}`

	req := httptest.NewRequest("POST",
		"http://proxy.local:7897/v1beta/models/gemini-3.1-flash-lite:streamGenerateContent?alt=sse",
		strings.NewReader(agyBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/1.1") // agy 网关模式真实发的就是它
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	// 1) 路径与 query 改写
	if gotPath != "/v1internal:streamGenerateContent" {
		t.Errorf("上游收到 Path = %q, 想要 /v1internal:streamGenerateContent", gotPath)
	}
	if gotQuery != "alt=sse" {
		t.Errorf("上游收到 Query = %q, 想要保留 alt=sse", gotQuery)
	}

	// 2) 许可证闸门：UA 必须被盖掉，不能沿用 agy 的 Go 默认 UA
	if gotUA != "antigravity-cli/1.2.9" {
		t.Errorf("上游收到 User-Agent = %q, 想要 antigravity-cli/1.2.9（否则 403）", gotUA)
	}
	if gotAuth != "Bearer TOK" {
		t.Errorf("上游收到 Authorization = %q, 想要 Bearer TOK", gotAuth)
	}

	// 3) body 必须是信封，且内层是 agy 原字节
	if !strings.HasPrefix(gotBody, `{"model":"gemini-3.1-flash-lite","request":`) {
		t.Errorf("请求体没包信封: %s", gotBody)
	}
	if !strings.Contains(gotBody, agyBody) {
		t.Errorf("内层不是 agy 原字节:\n got=%s\nwant 包含=%s", gotBody, agyBody)
	}

	// 4) 响应信封必须解开，agy 才读得到 candidates
	body := rec.Body.String()
	if !strings.Contains(body, `"candidates"`) {
		t.Errorf("响应没解信封: %s", body)
	}
	if strings.Contains(body, `"response"`) {
		t.Errorf("响应还包着 response: %s", body)
	}
	if !strings.Contains(body, `data: `) {
		t.Errorf("SSE 的 data: 前缀丢了: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, 应当是 text/event-stream", ct)
	}
}

// TestEnvelopePassesThroughUnrecognizedPath：
// 认不出的路径原样透传，不能因为我们改写失败就把请求挡在门外。
func TestEnvelopePassesThroughUnrecognizedPath(t *testing.T) {
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	req := httptest.NewRequest("POST", "http://proxy/v1beta/somethingElse",
		strings.NewReader(`{"a":1}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if gotPath != "/v1beta/somethingElse" {
		t.Errorf("认不出的路径被改写了: %q", gotPath)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("body 被改动了: %s", gotBody)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestEnvelopeRejectsNonJSONInnerBody：内层不是 JSON 就该 400，
// 而不是把一个我们自己造出来的坏信封发给上游。
func TestEnvelopeRejectsNonJSONInnerBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("body 非法时不该转发到上游")
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	req := httptest.NewRequest("POST",
		"http://proxy/v1beta/models/m:streamGenerateContent", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, 想要 400；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "JSON") {
		t.Errorf("错误信息应说明原因: %s", rec.Body.String())
	}
}

// TestEnvelopeErrorResponsesPassThroughUntouched 验证上游错误响应不执行信封解包，原样放行供日志与状态判定使用。
func TestEnvelopeErrorResponsesPassThroughUntouched(t *testing.T) {
	const errBody = `{"error":{"code":403,"message":"You do not have a valid license ` +
		`of this product. (#3501)","status":"PERMISSION_DENIED"}}`

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, errBody)
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	req := httptest.NewRequest("POST",
		"http://proxy/v1beta/models/m:streamGenerateContent", strings.NewReader(`{"a":1}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, 想要 403 原样透传", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SUBSCRIPTION_REQUIRED") &&
		!strings.Contains(rec.Body.String(), "#3501") {
		t.Errorf("错误原因被改写/吞掉了: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("错误信封不该被解包: %s", rec.Body.String())
	}
}

// TestUserAgentIsOverriddenNotJustSet 钉住「盖掉」而非「缺失时补上」：
// agy 明确发了 Go-http-client/1.1，只做补空会原样透传出去 → 403。
func TestUserAgentIsOverriddenNotJustSet(t *testing.T) {
	var gotUA []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = append(gotUA, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	req := httptest.NewRequest("GET", "http://proxy/x", nil)
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	s.ServeHTTP(httptest.NewRecorder(), req)

	if len(gotUA) != 1 || gotUA[0] != "antigravity-cli/1.2.9" {
		t.Errorf("上游收到 UA = %v, 想要 [antigravity-cli/1.2.9]", gotUA)
	}
}

func TestSetUserAgentOverridesDefault(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	if s.UserAgentForUpstream() != "antigravity-cli/1.2.9" {
		t.Errorf("默认 UA = %q", s.UserAgentForUpstream())
	}
	s.SetUserAgent("custom/1.0")
	if s.UserAgentForUpstream() != "custom/1.0" {
		t.Errorf("SetUserAgent 没生效: %q", s.UserAgentForUpstream())
	}
}

// TestModelAliasesRewriteEnvelope 是「404 变 429/200」的关键：
// agy 发 gemini-3.8-flash，上游只有 gemini-3.8-flash-tiered。
func TestModelAliasesRewriteEnvelope(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[]}}`+"\n\n")
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	s.SetModelAliases(map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-tiered"})

	req := httptest.NewRequest("POST",
		"http://proxy/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[]}`))
	s.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(gotBody, `"model":"gemini-3.8-flash-tiered"`) {
		t.Errorf("模型名没被映射: %s", gotBody)
	}
}

// TestModelAliasesLeaveMappedOutModelsAlone：没有别名的模型必须原样透传，
// 否则所有请求都会被塞进同一个模型。
func TestModelAliasesLeaveMappedOutModelsAlone(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[]}}`+"\n\n")
	}))
	t.Cleanup(up.Close)

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	s.SetModelAliases(map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-tiered"})

	req := httptest.NewRequest("POST",
		"http://proxy/v1beta/models/gemini-3.1-flash-lite:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[]}`))
	s.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(gotBody, `"model":"gemini-3.1-flash-lite"`) {
		t.Errorf("无别名的模型不该被改写: %s", gotBody)
	}
	if strings.Contains(gotBody, "-tiered") {
		t.Errorf("无别名的模型不该出现 -tiered: %s", gotBody)
	}
}

func TestSetModelAliasesCopiesInput(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	in := map[string]string{"a": "b"}
	s.SetModelAliases(in)
	in["a"] = "MUTATED"
	if s.ModelAliases()["a"] != "b" {
		t.Error("SetModelAliases 应当拷贝入参，否则外部改动会绕过我们")
	}

	// 返回的也必须是副本
	out := s.ModelAliases()
	out["a"] = "MUTATED2"
	if s.ModelAliases()["a"] != "b" {
		t.Error("ModelAliases 应当返回副本")
	}

	// 空表要能清空
	s.SetModelAliases(nil)
	if s.ModelAliases() != nil {
		t.Error("传 nil 应当清空别名表")
	}
}

func TestSetModelAliasesConcurrentSafety(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	s.EnableEnvelope()

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					s.SetModelAliases(map[string]string{
						"model-a": "model-a-tiered",
						"model-b": "model-b-tiered",
					})
				}
			}
		}()
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_ = s.ModelAliases()
					req, _ := http.NewRequest("POST", "http://gw/v1beta/models/model-a:generateContent", strings.NewReader(`{}`))
					_ = s.applyEnvelope(req)
				}
			}
		}()
	}

	wg.Wait()
}


func TestEnvelopeDefaultsToOff(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	if s.Envelope() {
		t.Error("信封改写应当默认关闭，保持纯透传语义")
	}
	s.EnableEnvelope()
	if !s.Envelope() {
		t.Error("EnableEnvelope 没生效")
	}
}

// TestStreamThroughEnvelopeNotBuffered：解信封不能牺牲实时性。
// 上游发完第一条就阻塞，客户端必须立刻读到它。
func TestStreamThroughEnvelopeNotBuffered(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":"first"}]}}]}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release // 卡住，等测试确认第一条已经穿过去
		_, _ = io.WriteString(w,
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":"second"}]}}]}}`+"\n\n")
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/v1beta/models/m:streamGenerateContent?alt=sse",
		"application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// 上游此刻还阻塞着，第一条必须已经穿过来。
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("读流失败（被缓冲或断流）: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "first") {
		t.Errorf("没读到第一条: %q", got)
	}
	if strings.Contains(got, `"response"`) {
		t.Errorf("读到的还是没解的信封: %q", got)
	}
}

// TestSSEUnwrapperClosesUpstreamBody 钉住连接释放：
// 不透传 Close 就会攒出一堆泄漏的连接。
func TestSSEUnwrapperClosesUpstreamBody(t *testing.T) {
	c := &trackingCloser{r: strings.NewReader("")}
	u := newSSEUnwrapper(c)
	if err := u.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.closed {
		t.Error("Close 没透传到底下的上游 body")
	}
}

type trackingCloser struct {
	r      io.Reader
	closed bool
}

func (c *trackingCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *trackingCloser) Close() error               { c.closed = true; return nil }

// TestDefaultUpstreamIsCloudcode 验证默认上游配置。
func TestDefaultUpstreamIsCloudcode(t *testing.T) {
	s := newTestServer(t, "https://daily-cloudcode-pa.googleapis.com",
		Static(&Account{AccessToken: "T"}))
	if s.Upstream().Host != "daily-cloudcode-pa.googleapis.com" {
		t.Errorf("Upstream().Host = %q", s.Upstream().Host)
	}
}

// TestUnwrapResponseJSONExceededPreservesReadableBody 验证响应超过解包上限时仍能完整读取原内容
func TestUnwrapResponseJSONExceededPreservesReadableBody(t *testing.T) {
	// 构造超出 1MB 的超大 JSON 响应
	bigData := strings.Repeat("x", maxErrBody*16+1024)
	rawJSON := `{"huge":"` + bigData + `"}`

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rawJSON)
	}))
	defer up.Close()

	s := newEnvelopeServer(t, up.URL, Static(&Account{AccessToken: "T"}))
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1beta/models/m:generateContent",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败 (Body 可能被提前关闭): %v", err)
	}
	if len(got) != len(rawJSON) {
		t.Fatalf("收到内容长度 %d, 预期 %d", len(got), len(rawJSON))
	}
}

func TestUnwrapResponseJSONExceededClosesUnderlyingBody(t *testing.T) {
	s := newEnvelopeServer(t, "http://127.0.0.1:1", Static(&Account{AccessToken: "T"}))
	bigData := strings.Repeat("x", maxErrBody*16+1024)
	tc := &trackingCloser{r: strings.NewReader(`{"huge":"` + bigData + `"}`)}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       tc,
	}
	if err := s.unwrapResponse(resp); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !tc.closed {
		t.Error("超限响应的底层 Body 未被关闭，存在连接泄漏")
	}
}

