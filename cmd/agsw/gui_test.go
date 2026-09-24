package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kylesean/agsw/internal/keyring"
	"github.com/kylesean/agsw/internal/pool"
)

func TestGatewayURLUsesLoopbackForWildcardListen(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:7897", ":7897", "0.0.0.0:7897", "[::]:7897"} {
		if got, want := gatewayURL(listen), "http://127.0.0.1:7897"; got != want {
			t.Errorf("gatewayURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestEnsureNoProxyAddsGatewayHost(t *testing.T) {
	env := []string{"NO_PROXY=example.com", "no_proxy=example.com"}
	got := ensureNoProxy(env, "127.0.0.1")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "NO_PROXY=example.com,127.0.0.1") ||
		!strings.Contains(joined, "no_proxy=example.com,127.0.0.1") {
		t.Fatalf("NO_PROXY not updated: %v", got)
	}
	if reflect.DeepEqual(env, got) {
		t.Fatal("ensureNoProxy should return a new environment slice")
	}
}

func TestSecretFromAccountPreservesCredentials(t *testing.T) {
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	sec := secretFromAccount(&pool.Account{
		AccessToken: "at", RefreshToken: "rt", IDToken: "id", AuthMethod: "consumer", Expiry: expiry,
	})
	if sec.Token.AccessToken != "at" || sec.Token.RefreshToken != "rt" ||
		sec.Token.TokenType != "Bearer" || sec.IDToken != "id" || sec.AuthMethod != "consumer" {
		t.Fatalf("secret = %+v", sec)
	}
	if !sec.Token.Expiry.Equal(expiry) {
		t.Errorf("expiry = %v, want %v", sec.Token.Expiry, expiry)
	}
}

func TestIsOneShotAgy(t *testing.T) {
	for _, args := range [][]string{{"--print", "hi"}, {"-p"}, {"--prompt"}} {
		if !isOneShotAgy(args) {
			t.Errorf("isOneShotAgy(%v) = false", args)
		}
	}
	if isOneShotAgy([]string{"--dangerously-skip-permissions"}) {
		t.Error("interactive agy should not be one-shot")
	}
}

func TestSyncKeyringAccountUsesPoolCredentials(t *testing.T) {
	t.Setenv("AGSW_DATA_DIR", t.TempDir())
	a := &pool.Account{
		Name: "B", Email: "b@example.com", AccessToken: "at", RefreshToken: "rt",
		IDToken: "id", AuthMethod: "consumer", Expiry: time.Now().Add(time.Hour),
	}
	if err := pool.Save(a); err != nil {
		t.Fatal(err)
	}
	oldStore := storeKeyring
	t.Cleanup(func() { storeKeyring = oldStore })
	var got *keyring.Secret
	storeKeyring = func(sec *keyring.Secret) error {
		got = sec
		return nil
	}
	if err := syncKeyringAccount(accountSwitch{name: "B", email: "B@Example.com"}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Token.AccessToken != "at" || got.Token.RefreshToken != "rt" || got.IDToken != "id" {
		t.Fatalf("stored secret = %+v", got)
	}
}

func TestWaitGatewayReturnsErrorWhenServerExitsNilEarly(t *testing.T) {
	serverErr := make(chan error, 1)
	serverErr <- nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := waitGateway(ctx, "127.0.0.1:0", serverErr)
	if err == nil || !strings.Contains(err.Error(), "serve 在 Gateway ready 前退出") {
		t.Fatalf("err = %v, want error mentioning 'serve 在 Gateway ready 前退出'", err)
	}
}

