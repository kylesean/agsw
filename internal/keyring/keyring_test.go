package keyring

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// makeJWT 造一个只有 header+payload+假签名的 JWT，payload 是合法 claims。
func makeJWT(payload string) string {
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return strings.Join([]string{
		enc(`{"alg":"RS256","typ":"JWT"}`),
		enc(payload),
		"signature",
	}, ".")
}

func TestDecodeJWTEmailAndAudience(t *testing.T) {
	payload := `{"email":"me@example.com","sub":"123","aud":"999.apps.googleusercontent.com"}`
	c, err := decodeJWT(makeJWT(payload))
	if err != nil {
		t.Fatalf("decodeJWT: %v", err)
	}
	if c.Email != "me@example.com" {
		t.Errorf("Email = %q", c.Email)
	}
	if got := c.Audience(); got != "999.apps.googleusercontent.com" {
		t.Errorf("Audience = %q", got)
	}
}

func TestAudienceAcceptsArrayForm(t *testing.T) {
	// Google 有时把 aud 下发成数组。
	c := &Claims{Aud: []byte(`["first.apps.googleusercontent.com","second"]`)}
	if got := c.Audience(); got != "first.apps.googleusercontent.com" {
		t.Errorf("Audience = %q", got)
	}
}

func TestDecodeJWTRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"onlyonesegment",
		"a.b",                 // payload 不是 base64
		"a.!!!notbase64!!!.c", // 非法字符
		"a." + base64.RawURLEncoding.EncodeToString([]byte("{bad json")) + ".c",
	}
	for _, tok := range bad {
		if _, err := decodeJWT(tok); err == nil {
			t.Errorf("decodeJWT(%q) 应当报错", tok)
		}
	}
}

func TestParseRejectsEmptyTokens(t *testing.T) {
	if _, err := Parse(`{"token":{},"id_token":"x"}`); err == nil {
		t.Error("access_token 与 refresh_token 均为空时应当报错")
	}
	if _, err := Parse(`not json`); err == nil {
		t.Error("非法 JSON 应当报错")
	}
}

func TestParseAcceptsFullSecret(t *testing.T) {
	raw := `{
		"token": {
			"access_token": "at",
			"token_type": "Bearer",
			"refresh_token": "rt",
			"expiry": "2026-09-24T12:00:00Z"
		},
		"auth_method": "consumer",
		"id_token": "h.p.s"
	}`
	sec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sec.Token.RefreshToken != "rt" {
		t.Errorf("RefreshToken = %q", sec.Token.RefreshToken)
	}
	if sec.AuthMethod != "consumer" {
		t.Errorf("AuthMethod = %q", sec.AuthMethod)
	}
	want := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if !sec.Token.Expiry.Equal(want) {
		t.Errorf("Expiry = %v, want %v", sec.Token.Expiry, want)
	}
}

func TestExpiryTimeAcceptsUnixSeconds(t *testing.T) {
	// 有些实现把 expiry 下发成数字。
	sec, err := Parse(`{"token":{"access_token":"at","refresh_token":"rt","expiry":1790000000}}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := sec.Token.Expiry.Unix()
	if got != 1790000000 {
		t.Errorf("Expiry.Unix() = %d, want 1790000000", got)
	}
}

func TestExpiryTimeToleratesNullAndMissing(t *testing.T) {
	for _, raw := range []string{
		`{"token":{"access_token":"a","refresh_token":"r","expiry":null}}`,
		`{"token":{"access_token":"a","refresh_token":"r"}}`,
		`{"token":{"access_token":"a","refresh_token":"r","expiry":""}}`,
	} {
		sec, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%s): %v", raw, err)
		}
		if !sec.Token.Expiry.IsZero() {
			t.Errorf("expiry 应当为零值, 得到 %v", sec.Token.Expiry)
		}
	}
}

func TestExpiryTimeRoundTrip(t *testing.T) {
	in := ExpiryTime{Time: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	b, err := in.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var out ExpiryTime
	if err := out.UnmarshalJSON(b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !out.Equal(in.Time) {
		t.Errorf("往返不一致: %v vs %v", out, in)
	}
}

func TestClaimsEmailMissingIsAnError(t *testing.T) {
	sec := &Secret{IDToken: makeJWT(`{"sub":"123"}`)}
	if _, err := sec.Email(); err == nil {
		t.Error("缺 email 时应当报错")
	}
}

func TestClaimsRequiresIDToken(t *testing.T) {
	sec := &Secret{}
	if _, err := sec.Claims(); err == nil {
		t.Error("缺 id_token 时应当报错")
	}
	if _, err := sec.Email(); err == nil {
		t.Error("缺 id_token 时 Email 应当报错")
	}
}

// TestServiceConstantsDocumentsTheContract 把逆向出来的定位键钉死。
// agy 升级若改动这些值，本测试会在 status/add 上先炸，而不是静默读错条目。
func TestServiceConstantsDocumentsTheContract(t *testing.T) {
	if Service != "gemini" {
		t.Errorf("Service = %q, 想要 gemini", Service)
	}
	if Username != "antigravity" {
		t.Errorf("Username = %q, 想要 antigravity", Username)
	}
}

func TestStoreSerializesSecretForAgyKeyring(t *testing.T) {
	oldSet := setSecret
	t.Cleanup(func() { setSecret = oldSet })
	var gotService, gotUser, gotRaw string
	setSecret = func(service, user, raw string) error {
		gotService, gotUser, gotRaw = service, user, raw
		return nil
	}

	sec := &Secret{
		Token: Token{
			AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer",
			Expiry: ExpiryTime{Time: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)},
		},
		AuthMethod: "consumer", IDToken: "h.p.s",
	}
	if err := Store(sec); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if gotService != Service || gotUser != Username {
		t.Fatalf("keyring location = %q/%q", gotService, gotUser)
	}
	got, err := Parse(gotRaw)
	if err != nil {
		t.Fatalf("stored JSON invalid: %v\n%s", err, gotRaw)
	}
	if got.Token.AccessToken != "at" || got.Token.RefreshToken != "rt" || got.IDToken != "h.p.s" {
		t.Errorf("stored credentials = %+v", got)
	}
}
