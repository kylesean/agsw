package token

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stubTokenServer 模拟 Google 的令牌端点。
func stubTokenServer(t *testing.T, status int, body any) (*httptest.Server, *url.Values) {
	t.Helper()
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(raw))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// withEndpoint 把刷新端点临时指到桩服务器。
func withEndpoint(t *testing.T, u string) {
	t.Helper()
	orig := Endpoint
	// Endpoint 是常量，这里改用可变副本的机制。
	setEndpoint(u)
	t.Cleanup(func() { setEndpoint(orig) })
}

func TestRefreshSendsExpectedForm(t *testing.T) {
	srv, got := stubTokenServer(t, http.StatusOK, map[string]any{
		"access_token": "NEW", "expires_in": 3600, "token_type": "Bearer",
	})
	withEndpoint(t, srv.URL)

	res, err := Refresh(context.Background(), "CID", "RT")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != "NEW" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}

	q := *got
	if q.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q", q.Get("grant_type"))
	}
	if q.Get("refresh_token") != "RT" {
		t.Errorf("refresh_token = %q", q.Get("refresh_token"))
	}
	if q.Get("client_id") != "CID" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
}

func TestRefreshWithDefaultClientIncludesClientSecret(t *testing.T) {
	srv, got := stubTokenServer(t, http.StatusOK, map[string]any{
		"access_token": "NEW", "expires_in": 3600,
	})
	withEndpoint(t, srv.URL)

	_, err := Refresh(context.Background(), DefaultClientID(), "RT")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	q := *got
	if q.Get("client_id") != DefaultClientID() {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), DefaultClientID())
	}
	if q.Get("client_secret") != DefaultClientSecret() {
		t.Errorf("client_secret = %q, want %q", q.Get("client_secret"), DefaultClientSecret())
	}
}

func TestDefaultCredentialsEnvOverride(t *testing.T) {
	t.Setenv("AGSW_CLIENT_ID", "env-cid")
	t.Setenv("AGSW_CLIENT_SECRET", "env-secret")

	if got := DefaultClientID(); got != "env-cid" {
		t.Errorf("DefaultClientID() = %q, want env-cid", got)
	}
	if got := DefaultClientSecret(); got != "env-secret" {
		t.Errorf("DefaultClientSecret() = %q, want env-secret", got)
	}
}

func TestRefreshKeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	// Google 通常不在刷新响应里重复下发 refresh_token。
	srv, _ := stubTokenServer(t, http.StatusOK, map[string]any{
		"access_token": "NEW", "expires_in": 3600,
	})
	withEndpoint(t, srv.URL)

	res, err := Refresh(context.Background(), "CID", "RT-ORIG")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.RefreshToken != "RT-ORIG" {
		t.Errorf("RefreshToken = %q, 想要沿用 %q", res.RefreshToken, "RT-ORIG")
	}
}

func TestRefreshHonorsNewRefreshToken(t *testing.T) {
	srv, _ := stubTokenServer(t, http.StatusOK, map[string]any{
		"access_token": "NEW", "refresh_token": "RT-ROTATED", "expires_in": 60,
	})
	withEndpoint(t, srv.URL)

	res, err := Refresh(context.Background(), "CID", "RT-OLD")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.RefreshToken != "RT-ROTATED" {
		t.Errorf("RefreshToken = %q, 想要 %q", res.RefreshToken, "RT-ROTATED")
	}
}

func TestRefreshSurfacesGoogleErrorBody(t *testing.T) {
	srv, _ := stubTokenServer(t, http.StatusBadRequest, map[string]any{
		"error": "invalid_grant", "error_description": "Token has been expired or revoked.",
	})
	withEndpoint(t, srv.URL)

	_, err := Refresh(context.Background(), "CID", "RT")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("错误应带上 error 码: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("错误应带上状态码: %v", err)
	}
	// 错误里绝不能出现我们传进去的 refresh_token。
	if strings.Contains(err.Error(), "RT") {
		t.Errorf("错误信息泄露了 refresh_token: %v", err)
	}
}

func TestRefreshHandlesNonJSONErrorBody(t *testing.T) {
	srv, _ := stubTokenServer(t, http.StatusBadGateway, "upstream exploded")
	withEndpoint(t, srv.URL)

	_, err := Refresh(context.Background(), "CID", "RT")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("错误应带上状态码: %v", err)
	}
}

func TestRefreshRejectsEmptyInputs(t *testing.T) {
	if _, err := Refresh(context.Background(), "CID", ""); err == nil {
		t.Error("空 refresh_token 应当报错")
	}
	if _, err := Refresh(context.Background(), "CID", "   "); err == nil {
		t.Error("空白 refresh_token 应当报错")
	}
	// clientID 为空且默认值也为空时应当报错，而不是发出无 client_id 的请求。
	if _, err := Refresh(context.Background(), "", "RT"); err == nil {
		t.Error("空 client_id 应当报错")
	}
}

func TestRefreshRejectsResponseWithoutAccessToken(t *testing.T) {
	srv, _ := stubTokenServer(t, http.StatusOK, map[string]any{"expires_in": 3600})
	withEndpoint(t, srv.URL)

	if _, err := Refresh(context.Background(), "CID", "RT"); err == nil {
		t.Error("缺 access_token 应当报错")
	}
}

func TestRefreshAppliesTimeoutWhenContextHasNone(t *testing.T) {
	// 缩短真实超时，让用例从 20s 降到亚秒级。
	orig := requestTimeout
	requestTimeout = 800 * time.Millisecond
	t.Cleanup(func() { requestTimeout = orig })

	block := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-block:
		}
	}))
	t.Cleanup(srv.Close)
	// 必须注册在 srv.Close() 之后：cleanup 是 LIFO，这样 close(block)
	// 会先于 Close 执行，否则 Close 会永远等这个卡住的 handler。
	t.Cleanup(func() { close(block) })
	withEndpoint(t, srv.URL)

	start := time.Now()
	ctx := context.Background() // 无 deadline
	_, err := Refresh(ctx, "CID", "RT")
	if err == nil {
		t.Fatal("超时应当报错")
	}
	if elapsed := time.Since(start); elapsed > requestTimeout+5*time.Second {
		t.Errorf("耗时 %v，超过预期超时 %v", elapsed, requestTimeout)
	}
}

func TestExpiryComputation(t *testing.T) {
	r := &Result{ExpiresIn: 3600}
	got := r.Expiry()
	if until := time.Until(got); until < 3590*time.Second || until > 3600*time.Second {
		t.Errorf("ExpiresIn=3600 时 Expiry 距今 %v", until)
	}

	// ExpiresIn 缺省时退回到 1 小时，而不是零值。
	z := &Result{}
	if z.Expiry().IsZero() {
		t.Error("ExpiresIn=0 时不应返回零值时间")
	}
}
