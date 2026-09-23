package probe

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactValueKeepsSchemeAndPrefixOnly(t *testing.T) {
	// 这条测试存在的原因：secret-tool 曾把完整 OAuth token 打进会话输出。
	got := RedactValue("Bearer abcdefghijklmnopqrstuvwxyz")
	if strings.Contains(got, "ijklmnop") {
		t.Errorf("泄露了完整 token: %q", got)
	}
	if !strings.Contains(got, "Bearer abcdef") {
		t.Errorf("应当保留 scheme + 前 6 字符, 得到 %q", got)
	}
	if !strings.Contains(got, "len=26") {
		t.Errorf("应当保留长度信息, 得到 %q", got)
	}
}

func TestRedactValueShortAndNoSpace(t *testing.T) {
	if got := RedactValue(""); got != "" {
		t.Errorf("空值应保持空, got %q", got)
	}
	if got := RedactValue("short"); !strings.Contains(got, "len=5") {
		t.Errorf("短值应保留长度, got %q", got)
	}
	if got := RedactValue("abcdefgXYZ"); strings.Contains(got, "XYZ") {
		t.Errorf("超过 6 字符须截断, got %q", got)
	}
}

func TestFromRequestRedactsSensitiveHeadersOnly(t *testing.T) {
	r := httptest.NewRequest("POST", "http://example.com/v1:predict?x=1", nil)
	r.Host = "example.com"
	r.Header.Set("Authorization", "Bearer supersecrettoken-0123456789")
	r.Header.Set("Cookie", "session=topsecret")
	r.Header.Set("X-Goog-Api-Key", "AIza-something-secret")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connect-Protocol-Version", "1")

	rec := FromRequest("request", r, []byte(`{"a":1}`), 8192)

	// 脱敏
	for _, k := range []string{"Authorization", "Cookie", "X-Goog-Api-Key"} {
		v := strings.Join(rec.Headers[k], " ")
		if strings.Contains(v, "supersecrettoken") ||
			strings.Contains(v, "topsecret") ||
			strings.Contains(v, "AIza-something-secret") {
			t.Errorf("%s 泄露了原值: %q", k, v)
		}
		if v == "" {
			t.Errorf("%s 不该被丢掉", k)
		}
	}

	// 非敏感头原样保留
	if got := strings.Join(rec.Headers["Content-Type"], ""); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := strings.Join(rec.Headers["Connect-Protocol-Version"], ""); got != "1" {
		t.Errorf("Connect-Protocol-Version = %q", got)
	}

	// 字段
	if rec.Method != "POST" || rec.Host != "example.com" || rec.Path != "/v1:predict" {
		t.Errorf("字段不对: %+v", rec)
	}
	if rec.Query != "x=1" {
		t.Errorf("Query = %q", rec.Query)
	}
	if rec.Body != `{"a":1}` || rec.BodyEncoding != "text" || rec.BodyLen != 7 {
		t.Errorf("Body = %q/%s/%d", rec.Body, rec.BodyEncoding, rec.BodyLen)
	}
}

func TestBodyFieldTruncatesAndKeepsFullLength(t *testing.T) {
	long := []byte(strings.Repeat("z", 100))
	val, enc, full := BodyField(long, 10)
	if full != 100 {
		t.Errorf("full = %d, want 100", full)
	}
	if len(val) != 10 {
		t.Errorf("val 长度 = %d, want 10", len(val))
	}
	if enc != "text" {
		t.Errorf("enc = %q", enc)
	}
}

func TestBodyFieldBase64ForBinary(t *testing.T) {
	val, enc, full := BodyField([]byte{0x00, 0x01, 0xff, 0xfe}, 8192)
	if enc != "base64" {
		t.Errorf("enc = %q, want base64", enc)
	}
	if full != 4 {
		t.Errorf("full = %d", full)
	}
	if strings.Contains(val, "\x00") {
		t.Error("base64 字段不该含原始 NUL")
	}
}

func TestBodyFieldEmpty(t *testing.T) {
	val, enc, full := BodyField(nil, 100)
	if val != "" || enc != "" || full != 0 {
		t.Errorf("空体应全空, got %q %q %d", val, enc, full)
	}
}

func TestRecorderWritesJSONLAndIsThreadSafe(t *testing.T) {
	var buf bytes.Buffer
	rec := New(&buf, 1024)

	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			_ = rec.Add(Record{Kind: "request", Path: "/p"})
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("JSONL 行数 = %d, want %d", len(lines), n)
	}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "{") || !strings.HasSuffix(ln, "}") {
			t.Errorf("不是合法 JSON 行: %q", ln)
		}
		if !strings.Contains(ln, `"time"`) {
			t.Errorf("缺 time 字段: %q", ln)
		}
	}
}

func TestRecorderBodyMaxIsExposed(t *testing.T) {
	r := New(&bytes.Buffer{}, 42)
	if r.BodyMax() != 42 {
		t.Errorf("BodyMax = %d", r.BodyMax())
	}
}
