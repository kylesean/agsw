package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylesean/agsw/internal/pool"
	"github.com/kylesean/agsw/internal/proxy"
)

func TestSelectorPickerReportsAccountSwitchOnce(t *testing.T) {
	now := time.Now()
	accounts := []*pool.Account{
		{Name: "A", Email: "a@example.com", AccessToken: "A", Expiry: now.Add(time.Hour)},
		{Name: "B", Email: "b@example.com", AccessToken: "B", Expiry: now.Add(time.Hour)},
	}
	sel := pool.NewSelector(accounts, nil)
	var switched []string
	picker := &selectorPicker{
		sel: sel,
		onSwitch: func(name, email string) {
			switched = append(switched, name+":"+email)
		},
	}

	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	sel.SetCooldown("A", now.Add(time.Hour))
	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{"A:a@example.com", "B:b@example.com"}
	if len(switched) != len(want) || switched[0] != want[0] || switched[1] != want[1] {
		t.Fatalf("switch events = %v, want %v", switched, want)
	}
}

func TestSelectorPickerDefersSwitchUntilRequestFinishes(t *testing.T) {
	now := time.Now()
	accounts := []*pool.Account{
		{Name: "A", Email: "a@example.com", AccessToken: "A", Expiry: now.Add(time.Hour)},
		{Name: "B", Email: "b@example.com", AccessToken: "B", Expiry: now.Add(time.Hour)},
	}
	sel := pool.NewSelector(accounts, nil)
	var switched []string
	picker := &selectorPicker{
		sel:      sel,
		lastName: "A",
		onSwitch: func(name, email string) {
			switched = append(switched, name+":"+email)
		},
	}

	// 模拟请求开始
	picker.beginRequest()

	// A 冷却，导致当前在飞请求挑选到 B
	sel.SetCooldown("A", now.Add(time.Hour))
	got, err := picker.Pick(context.Background())
	if err != nil || got.Name != "B" {
		t.Fatalf("pick = %v, %v, want B", got, err)
	}

	// 在请求尚未完成（在飞）时，绝不能触发 onSwitch 强杀客户端
	if len(switched) != 0 {
		t.Fatalf("请求仍在在飞处理中，不应提前触发切号通知: %v", switched)
	}

	// 模拟请求完成并退还响应
	picker.endRequest()

	// 请求完成后，平滑触发一次切号通知
	if len(switched) != 1 || switched[0] != "B:b@example.com" {
		t.Fatalf("请求完成后应触发切号通知，got %v, want [B:b@example.com]", switched)
	}
}

func TestRefreshAccountDoesNotResurrectDeletedAccount(t *testing.T) {
	t.Setenv("AGSW_DATA_DIR", t.TempDir())
	a := &pool.Account{
		Name:         "dropped",
		Email:        "dropped@example.com",
		AccessToken:  "old-token",
		RefreshToken: "rt",
		Expiry:       time.Now().Add(-time.Hour),
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-token",
			"expires_in":   3600,
		})
	}))
	defer up.Close()
	t.Setenv("AGSW_TOKEN_URL", up.URL)

	if err := refreshAccount(context.Background(), a); err != nil {
		t.Fatalf("refreshAccount: %v", err)
	}

	exists, err := pool.Exists("dropped")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("已被 drop 的账号在刷新后不应被重新写入磁盘复活")
	}
}

func TestRetryFetchModelAliasesSucceedsOnSubsequentAttempt(t *testing.T) {
	var callCount int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:fetchAvailableModels" {
			callCount++
			if callCount == 1 {
				http.Error(w, "temporary error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.8-flash-tiered": map[string]any{},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()

	now := time.Now()
	accounts := []*pool.Account{
		{Name: "A", Email: "a@example.com", AccessToken: "A", Expiry: now.Add(time.Hour)},
	}
	sel := pool.NewSelector(accounts, nil)
	picker := &selectorPicker{sel: sel, log: log.New(io.Discard, "", 0)}
	srv, err := proxy.New(up.URL, picker, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go retryFetchModelAliases(ctx, sel, up.URL, srv, false, log.New(io.Discard, "", 0), 10*time.Millisecond)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		aliases := srv.ModelAliases()
		if aliases != nil && aliases["gemini-3.8-flash"] == "gemini-3.8-flash-tiered" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("model aliases were not populated after retry, got: %v", srv.ModelAliases())
}



