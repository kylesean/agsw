package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kylesean/agsw/internal/token"
)

// ---------- 配置 ----------

func TestDefaultsAreTheRealConsumerClient(t *testing.T) {
	c := Config{}.withDefaults()
	if c.ClientID != DefaultClientID() || c.ClientSecret == "" {
		t.Fatalf("默认 client 配对丢失: id=%q secretLen=%d", c.ClientID, len(c.ClientSecret))
	}
	if c.RedirectPath != "/auth/callback" {
		t.Errorf("回调路径 = %q", c.RedirectPath)
	}
	if len(c.Scopes) != len(DefaultScopes) {
		t.Errorf("scope 数 = %d，want %d", len(c.Scopes), len(DefaultScopes))
	}
	// 确保包含核心授权 Scope
	missing := []string{
		"https://www.googleapis.com/auth/aicode",
		"https://www.googleapis.com/auth/cclog",
		"https://www.googleapis.com/auth/cloud-platform",
		"https://www.googleapis.com/auth/userinfo.email",
		"https://www.googleapis.com/auth/userinfo.profile",
	}
	for _, s := range missing {
		found := false
		for _, got := range DefaultScopes {
			if got == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scope 集合缺 %s —— 上游会 403", s)
		}
	}
}

func TestValidateCatchesMissingPieces(t *testing.T) {
	c := Config{}.withDefaults()
	c.ClientSecret = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "client_secret") {
		t.Errorf("缺 secret 应报错，got %v", err)
	}

	c = Config{}.withDefaults()
	c.RedirectPath = "no-slash"
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "回调路径") {
		t.Errorf("回调路径缺 / 应报错，got %v", err)
	}

	c = Config{}.withDefaults()
	c.Scopes = nil
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("空 scope 应报错，got %v", err)
	}

	c = Config{}.withDefaults()
	c.ClientID = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Errorf("缺 client_id 应报错，got %v", err)
	}
}

// ---------- PKCE ----------

// RFC 7636 Appendix B 官方测试向量。
func TestPKCEChallengeMatchesRFC7636Vector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := pkceChallenge(verifier); got != want {
		t.Errorf("challenge = %q\n     want %q", got, want)
	}
}

func TestRandomTokenIsURLSafeAndLongEnough(t *testing.T) {
	a, err := randomToken(64)
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomToken(64)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("两次随机结果相同 —— 熵源有问题")
	}
	if len(a) < 80 { // 64 字节 → base64url ≈ 86 字符
		t.Errorf("随机串过短: %d", len(a))
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("必须是 URL-safe base64（无 + / =）: %q", a)
	}
}

// ---------- 授权链接 ----------

func TestBuildAuthURLCarriesEverythingGoogleNeeds(t *testing.T) {
	cfg := Config{}.withDefaults()
	u := buildAuthURL(cfg, "http://127.0.0.1:8791/auth/callback", "st4te", "ch4llenge", "")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             DefaultClientID(),
		"redirect_uri":          "http://127.0.0.1:8791/auth/callback",
		"state":                 "st4te",
		"code_challenge":        "ch4llenge",
		"code_challenge_method": "S256",
		"access_type":           "offline", // 没有它拿不到 refresh_token
		"prompt":                "consent", // 没有它第二次登录不下发 refresh_token
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	scopes := strings.Fields(q.Get("scope"))
	if len(scopes) != len(DefaultScopes) {
		t.Errorf("scope 有 %d 项，want %d: %q", len(scopes), len(DefaultScopes), q.Get("scope"))
	}
	if q.Get("login_hint") != "" {
		t.Error("未传 hint 时不该带 login_hint")
	}
	if parsed.Host != "accounts.google.com" {
		t.Errorf("授权端点 host = %q", parsed.Host)
	}
}

func TestBuildAuthURLOmitsEmptyLoginHintAndSetsNonEmpty(t *testing.T) {
	cfg := Config{}.withDefaults()
	u := buildAuthURL(cfg, "http://127.0.0.1:1/x", "s", "c", "")
	if strings.Contains(u, "login_hint") {
		t.Error("hint 为空时不该出现 login_hint")
	}
	u = buildAuthURL(cfg, "http://127.0.0.1:1/x", "s", "c", "me@example.com")
	parsed, _ := url.Parse(u)
	if got := parsed.Query().Get("login_hint"); got != "me@example.com" {
		t.Errorf("login_hint = %q", got)
	}
}

// ---------- 回调服务：CSRF / 错误 / 超时 ----------

// serveOnce 起一个本地回调服务并让 waitCode 在后台等，返回地址与结果通道。
func serveOnce(t *testing.T, path, wantState string, timeout time.Duration) (addr string, done chan waitOutcome) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	done = make(chan waitOutcome, 1)
	go func() {
		code, err := waitCode(ctx, ln, path, wantState, timeout)
		done <- waitOutcome{code, err}
	}()
	// 给 goroutine 一点时间接管 listener。
	time.Sleep(50 * time.Millisecond)
	return ln.Addr().String(), done
}

type waitOutcome struct {
	code string
	err  error
}

func awaitOutcome(t *testing.T, done chan waitOutcome) waitOutcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("waitCode 没有返回")
		return waitOutcome{}
	}
}

func TestWaitCodeAcceptsValidState(t *testing.T) {
	addr, done := serveOnce(t, "/cb", "good-state", 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://%s/cb?code=THE_CODE&state=good-state", addr))
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("回调状态码 = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body[:n]), "登录完成") {
		t.Errorf("成功页内容不对: %q", string(body[:n]))
	}

	o := awaitOutcome(t, done)
	if o.err != nil {
		t.Fatalf("不该报错: %v", o.err)
	}
	if o.code != "THE_CODE" {
		t.Errorf("code = %q, want THE_CODE", o.code)
	}
}

// 伪造回调（state 对不上）必须被拒 —— 这是本流程唯一的 CSRF 防线。
func TestWaitCodeRejectsForgedState(t *testing.T) {
	addr, done := serveOnce(t, "/cb", "real-state", 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://%s/cb?code=EVIL&state=forged", addr))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("伪造 state 状态码 = %d, want 400", resp.StatusCode)
	}

	o := awaitOutcome(t, done)
	if o.err == nil || !strings.Contains(o.err.Error(), "state 不匹配") {
		t.Errorf("应报 state 不匹配，got %v", o.err)
	}
	if o.code != "" {
		t.Errorf("伪造回调不该产出 code，got %q", o.code)
	}
}

// 用户在同意页点「取消」→ Google 回 error=access_denied（此时 state 仍是好的）。
func TestWaitCodeSurfacesGoogleDenial(t *testing.T) {
	addr, done := serveOnce(t, "/cb", "st", 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://%s/cb?error=access_denied&state=st", addr))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("拒绝状态码 = %d", resp.StatusCode)
	}

	o := awaitOutcome(t, done)
	if o.err == nil || !strings.Contains(o.err.Error(), "access_denied") {
		t.Errorf("应透出 access_denied，got %v", o.err)
	}
}

func TestWaitCodeRejectsMissingCode(t *testing.T) {
	addr, done := serveOnce(t, "/cb", "st", 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://%s/cb?state=st", addr))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 code 状态码 = %d", resp.StatusCode)
	}
	o := awaitOutcome(t, done)
	if o.err == nil || !strings.Contains(o.err.Error(), "code") {
		t.Errorf("应报缺 code，got %v", o.err)
	}
}

// 第一次回调定胜负：拿到 code 后监听立刻关闭，后续重放连不上、也改写不了结果。
func TestWaitCodeIgnoresReplayedCallback(t *testing.T) {
	addr, done := serveOnce(t, "/cb", "st", 5*time.Second)

	first, err := http.Get(fmt.Sprintf("http://%s/cb?code=FIRST&state=st", addr))
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()

	o := awaitOutcome(t, done)
	if o.err != nil {
		t.Fatalf("第一次回调不该报错: %v", o.err)
	}
	if o.code != "FIRST" {
		t.Errorf("应取第一次的 code，got %q", o.code)
	}

	// 服务已随第一次回调关闭 —— 重放要么连不上，要么拿不到第二次的 code。
	second, err := http.Get(fmt.Sprintf("http://%s/cb?code=SECOND&state=st", addr))
	if err == nil {
		second.Body.Close()
		t.Error("第一次回调后监听应已关闭，重放不该连得上")
	}
}

func TestWaitCodeTimesOutWithActionableMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = waitCode(ctx, ln, "/cb", "st", 200*time.Millisecond)
	if err == nil {
		t.Fatal("无人回调必须超时报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("超时信息不可读: %v", err)
	}
}

// ---------- 换 token ----------

func jsonHandler(t *testing.T, status int, payload any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		for _, k := range []string{"grant_type", "code", "redirect_uri", "client_id", "client_secret", "code_verifier"} {
			if r.PostFormValue(k) == "" {
				t.Errorf("令牌请求缺字段 %s", k)
			}
		}
		if g := r.PostFormValue("grant_type"); g != "authorization_code" {
			t.Errorf("grant_type = %q", g)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func TestExchangeCodeHappyPath(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, map[string]any{
		"access_token":  "at-123",
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": "rt-456",
		"id_token":      makeIDToken(t, DefaultClientID(), "a@b.com", true),
		"scope":         "openid aicode",
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	res, err := exchangeCode(context.Background(), cfg, "CODE", "http://127.0.0.1:1/auth/callback", "ver")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessToken != "at-123" || res.RefreshToken != "rt-456" {
		t.Errorf("token 对不上: %+v", res)
	}
	if !res.Expiry().After(time.Now()) {
		t.Errorf("Expiry 没算对: %v", res.Expiry())
	}
}

// 没有 refresh_token 就做不到「登一次永久免登」—— 必须报错，不能静默入库。
func TestExchangeCodeFailsWhenGoogleOmitsRefreshToken(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, map[string]any{
		"access_token": "at-only",
		"expires_in":   3600,
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("应明确报缺 refresh_token，got %v", err)
	}
}

func TestExchangeCodeFailsWhenAccessTokenMissing(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusOK, map[string]any{
		"refresh_token": "rt",
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("缺 access_token 应报错，got %v", err)
	}
}

func TestExchangeCodeSurfacesGoogleErrorBody(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, http.StatusBadRequest, map[string]any{
		"error":             "invalid_grant",
		"error_description": "Malformed auth code.",
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "Malformed") {
		t.Errorf("错误应透出 Google 原文，got %v", err)
	}
}

func TestExchangeCodeHandlesUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil || !strings.Contains(err.Error(), "解析") {
		t.Errorf("应报解析失败，got %v", err)
	}
}

func TestExchangeCodeRejectsNonOKWithoutJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>502</html>", http.StatusBadGateway)
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = srv.URL
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("应报 HTTP 502 并带上原始 body，got %v", err)
	}
}

func TestExchangeCodeUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close() // 立刻关掉，模拟连不上

	cfg := Config{}.withDefaults()
	cfg.TokenEndpoint = url
	_, err := exchangeCode(context.Background(), cfg, "C", "R", "V")
	if err == nil || !strings.Contains(err.Error(), "令牌端点") {
		t.Errorf("连不上应报得清，got %v", err)
	}
}

func TestDescribeErrorBody(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`<html>oops</html>`, "<html>oops</html>"},
		{`{"error":"x","error_description":"y"}`, "x y"},
		{`{"error":"only"}`, "only"},
		{"  \n \t ", ""},
	}
	for _, c := range cases {
		if got := describeErrorBody([]byte(c.in)); got != c.want {
			t.Errorf("describeErrorBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- id_token → 邮箱 ----------

func makeIDToken(t *testing.T, aud, email string, verified bool) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"aud":            aud,
		"email":          email,
		"email_verified": verified,
	})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func TestEmailFromIDTokenAcceptsMatchingAud(t *testing.T) {
	got, err, fallback := emailFromIDToken(makeIDToken(t, DefaultClientID(), "me@gmail.com", true), DefaultClientID())
	if err != nil || fallback {
		t.Fatalf("err=%v fallback=%v", err, fallback)
	}
	if got != "me@gmail.com" {
		t.Errorf("email = %q", got)
	}
}

// aud 不符 = 拿到别家 client 的 id_token，绝不能当真。
func TestEmailFromIDTokenRejectsForeignAud(t *testing.T) {
	_, err, fallback := emailFromIDToken(
		makeIDToken(t, "someone-elses.apps.googleusercontent.com", "x@y.com", true), DefaultClientID())
	if fallback {
		t.Error("aud 不符不该走 fallback，应直接拒绝")
	}
	if err == nil || !strings.Contains(err.Error(), "aud") {
		t.Errorf("应报 aud 不符，got %v", err)
	}
}

func TestEmailFromIDTokenAcceptsAudArray(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"aud":            []string{"other", DefaultClientID()},
		"email":          "arr@gmail.com",
		"email_verified": true,
	})
	tok := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	got, err, fallback := emailFromIDToken(tok, DefaultClientID())
	if err != nil || fallback || got != "arr@gmail.com" {
		t.Errorf("aud 数组应识别，got %q err=%v fallback=%v", got, err, fallback)
	}
}

func TestEmailFromIDTokenRejectsUnverifiedEmail(t *testing.T) {
	_, err, _ := emailFromIDToken(makeIDToken(t, DefaultClientID(), "u@y.com", false), DefaultClientID())
	if err == nil || !strings.Contains(err.Error(), "未验证") {
		t.Errorf("未验证邮箱应拒绝，got %v", err)
	}
}

func TestEmailFromIDTokenFallsBackWhenNoEmailClaim(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"aud": DefaultClientID()})
	tok := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	_, err, fallback := emailFromIDToken(tok, DefaultClientID())
	if err != nil {
		t.Errorf("缺 email 应 fallback 而非报错，got %v", err)
	}
	if !fallback {
		t.Error("fallback 应为 true")
	}
}

func TestEmailFromIDTokenRejectsGarbage(t *testing.T) {
	for _, tok := range []string{"", "noDots", "a.b", "h." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".s", "h.!!!notb64!!!.s"} {
		_, err, fallback := emailFromIDToken(tok, DefaultClientID())
		if fallback {
			t.Errorf("%q 该直接报错，不该 fallback", tok)
		}
		if err == nil {
			t.Errorf("%q 应报错", tok)
		}
	}
	// aud 缺失 → 拒绝（不能确认来源）。
	claims, _ := json.Marshal(map[string]any{"email": "x@y.com"})
	_, err, _ := emailFromIDToken("h."+base64.RawURLEncoding.EncodeToString(claims)+".s", DefaultClientID())
	if err == nil {
		t.Error("缺 aud 应拒绝")
	}
	// aud 类型不对（数字）→ 拒绝。
	claims, _ = json.Marshal(map[string]any{"aud": 12345, "email": "x@y.com"})
	_, err, _ = emailFromIDToken("h."+base64.RawURLEncoding.EncodeToString(claims)+".s", DefaultClientID())
	if err == nil {
		t.Error("非字符串 aud 应拒绝")
	}
}

func TestAudMatchesHandlesEveryShape(t *testing.T) {
	if !audMatches(DefaultClientID(), DefaultClientID()) {
		t.Error("字符串 aud 应匹配")
	}
	if audMatches("other", DefaultClientID()) {
		t.Error("别家 aud 不该匹配")
	}
	if !audMatches([]any{"x", DefaultClientID()}, DefaultClientID()) {
		t.Error("数组含目标应匹配")
	}
	if audMatches([]any{"x", "y"}, DefaultClientID()) {
		t.Error("数组不含目标不该匹配")
	}
	if !audMatches([]any{"only-one"}, "only-one") {
		t.Error("单元素数组应匹配")
	}
	if audMatches(nil, DefaultClientID()) {
		t.Error("nil aud 不该匹配")
	}
	if audMatches(12345, DefaultClientID()) {
		t.Error("非字符串 aud 不该匹配")
	}
	if audMatches([]any{1, 2}, DefaultClientID()) {
		t.Error("非字符串元素数组不该匹配")
	}
}

// ---------- resolveEmail 的 userinfo 兜底 ----------

func TestResolveEmailFallsBackToUserinfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("userinfo 缺 Bearer 头: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "fallback@x.com"})
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	res := &token.Result{AccessToken: "AT"}
	got, err := resolveEmail(context.Background(), cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fallback@x.com" {
		t.Errorf("email = %q", got)
	}
}

func TestResolveEmailPrefersIDToken(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "from-userinfo@x.com"})
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	res := &token.Result{
		AccessToken: "AT",
		IDToken:     makeIDToken(t, DefaultClientID(), "from-idtoken@x.com", true),
	}
	got, err := resolveEmail(context.Background(), cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-idtoken@x.com" {
		t.Errorf("email = %q", got)
	}
	if called {
		t.Error("id_token 已给出邮箱，不该再打 userinfo")
	}
}

// id_token 是别家 aud → emailFromIDToken 拒绝，此时不该把 userinfo 也当免检通道，
// 而是要让错误浮出来。
func TestResolveEmailPropagatesAudMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "sneaky@x.com"})
	}))
	defer srv.Close()

	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	res := &token.Result{AccessToken: "AT", IDToken: makeIDToken(t, "foreign.client/x", "x@y.com", true)}
	_, err := resolveEmail(context.Background(), cfg, res)
	if err == nil || !strings.Contains(err.Error(), "aud") {
		t.Errorf("aud 不符必须终止，got %v", err)
	}
}

func TestResolveEmailFailsWhenUserinfoReturnsNoEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()
	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	_, err := resolveEmail(context.Background(), cfg, &token.Result{})
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Errorf("应报缺 email，got %v", err)
	}
}

func TestResolveEmailFailsWhenUserinfoHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	_, err := resolveEmail(context.Background(), cfg, &token.Result{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("应报 HTTP 401，got %v", err)
	}
}

func TestResolveEmailFailsWhenUserinfoGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	cfg := Config{}.withDefaults()
	cfg.UserinfoEndpoint = srv.URL
	_, err := resolveEmail(context.Background(), cfg, &token.Result{})
	if err == nil || !strings.Contains(err.Error(), "解析") {
		t.Errorf("应报解析失败，got %v", err)
	}
}

// ---------- Login 的前置校验 ----------

// 注意：Login 会先 withDefaults 补齐零值再 validate，所以「缺 client_id」
// 这类错误只能从 validate 本身触发 —— 直接构造残缺 Config 来测。
func TestValidateRejectsConfigWithNoClientID(t *testing.T) {
	c := Config{ClientSecret: "s", Scopes: []string{"scope"}, RedirectPath: "/cb"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Errorf("缺 client_id 应报错，got %v", err)
	}
}

// withDefaults 之后配置必然合法，Login 不该在起端口前就失败。
func TestLoginWithDefaultsPassesValidation(t *testing.T) {
	c := Config{}.withDefaults()
	if err := c.validate(); err != nil {
		t.Errorf("默认配置应通过校验，got %v", err)
	}
}

// quiet 是不打印授权链接的 Options，避免污染测试输出。
func quiet(timeout time.Duration) Options {
	return Options{
		NoBrowser:    true,
		Timeout:      timeout,
		PrintAuthURL: func(string) {},
	}
}

func TestLoginRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Login(ctx, Config{}, quiet(5*time.Second))
	if err == nil {
		t.Fatal("已取消的 ctx 应立刻失败")
	}
}

func TestLoginTimesOutWhenNobodyCompletesLogin(t *testing.T) {
	start := time.Now()
	_, err := Login(context.Background(), Config{}, quiet(200*time.Millisecond))
	if err == nil {
		t.Fatal("无人完成登录必须超时报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("超时信息不可读: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("超时没生效，等了 %s", time.Since(start))
	}
}

// ---------- 打印授权链接 ----------

func TestPrintAuthURLDispatchesToCustomPrinter(t *testing.T) {
	var got string
	printAuthURL("http://example/auth", func(u string) { got = u })
	if got != "http://example/auth" {
		t.Errorf("自定义 printer 没被调用: %q", got)
	}
	// nil printer 走默认 stdout 分支，不应 panic。
	printAuthURL("http://example/auth", nil)
}

// Login 必须把授权链接交给调用方指定的 printer（否则 SSH 场景看不到链接）。
func TestLoginEmitsAuthURLThroughCustomPrinter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 1)
	go func() {
		_, _ = Login(ctx, Config{}, Options{
			NoBrowser:    true,
			Timeout:      3 * time.Second,
			PrintAuthURL: func(u string) { got <- u },
		})
	}()

	select {
	case u := <-got:
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("链接不可解析: %v", err)
		}
		if parsed.Host != "accounts.google.com" {
			t.Errorf("授权链接 host = %q", parsed.Host)
		}
		q := parsed.Query()
		if q.Get("prompt") != "consent" || q.Get("access_type") != "offline" {
			t.Errorf("缺关键参数: prompt=%q access_type=%q", q.Get("prompt"), q.Get("access_type"))
		}
		if !strings.Contains(q.Get("redirect_uri"), "127.0.0.1") {
			t.Errorf("回调必须走 127.0.0.1，got %q", q.Get("redirect_uri"))
		}
		cancel() // 拿到链接就够了，别等浏览器
	case <-time.After(3 * time.Second):
		t.Fatal("没等到授权链接")
	}
}

func TestBrowserCommandForOS(t *testing.T) {
	cases := []struct {
		goos     string
		wantPath string
	}{
		{"darwin", "open"},
		{"windows", "rundll32"},
		{"linux", "xdg-open"},
	}
	for _, c := range cases {
		cmd := browserCommandForOS(c.goos, "https://example.com")
		if !strings.Contains(cmd.Path, c.wantPath) && cmd.Args[0] != c.wantPath {
			t.Errorf("GOOS=%s: got %v, want command starting with %s", c.goos, cmd.Args, c.wantPath)
		}
	}
}

