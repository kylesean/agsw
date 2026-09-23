package quota

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// realSummary 是 retrieveUserQuotaSummary 的实测响应原文（截掉与 GEMINI 无关的长文案）。
// 关键点都保留了：两组并存、3p 组没有 gemini bucketId、5h 归零、weekly 接近满。
const realSummary = `{
  "description": "Within each group, models share a weekly limit and a 5-hour limit.",
  "groups": [
    {
      "buckets": [
        {
          "bucketId": "gemini-weekly",
          "description": "You have hit your 5-hour limit, so the weekly limit does not currently apply.",
          "displayName": "Weekly Limit Remaining",
          "remainingFraction": 0.9999833,
          "resetTime": "2026-09-30T07:52:27Z",
          "window": "weekly"
        },
        {
          "bucketId": "gemini-5h",
          "description": "You have hit your 5-hour limit, it will refresh in 22 minutes.",
          "displayName": "Five Hour Limit Remaining",
          "remainingFraction": 0,
          "resetTime": "2026-09-23T11:24:50Z",
          "window": "5h"
        }
      ],
      "description": "Models within this group: Gemini Flash, Gemini Pro",
      "displayName": "Gemini Models"
    },
    {
      "buckets": [
        {
          "bucketId": "3p-weekly",
          "displayName": "Weekly Limit Remaining",
          "remainingFraction": 1,
          "resetTime": "2026-09-30T11:02:32Z",
          "window": "weekly"
        },
        {
          "bucketId": "3p-5h",
          "displayName": "Five Hour Limit Remaining",
          "remainingFraction": 1,
          "resetTime": "2026-09-23T16:02:32Z",
          "window": "5h"
        }
      ],
      "description": "Models within this group: Claude, GPT",
      "displayName": "Third Party Models"
    }
  ]
}`

func mustParse(t *testing.T, raw string) *Summary {
	t.Helper()
	var s Summary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	return &s
}

func f64(v float64) *float64 { return &v }

// 真实响应必须能取到两个 bucket，且数值、时刻都对得上。
func TestGeminiFromRealResponse(t *testing.T) {
	g := mustParse(t, realSummary).Gemini()
	if g == nil {
		t.Fatal("Gemini() 返回 nil，本该认出 GEMINI 组")
	}
	if g.FiveHour == nil || g.Weekly == nil {
		t.Fatalf("两个窗口都要取到: 5h=%v weekly=%v", g.FiveHour, g.Weekly)
	}

	if rem, ok := g.FiveHour.Remaining(); !ok || rem != 0 {
		t.Errorf("5h 剩余 = (%v,%v)，want (0,true)", rem, ok)
	}
	if rem, ok := g.Weekly.Remaining(); !ok || rem < 0.99 || rem > 1 {
		t.Errorf("weekly 剩余 = (%v,%v)，want ≈1", rem, ok)
	}

	want := time.Date(2026, 9, 23, 11, 24, 50, 0, time.UTC)
	if got := g.FiveHour.Reset(); !got.Equal(want) {
		t.Errorf("5h resetTime = %v，want %v", got, want)
	}
}

// 3p 组（Claude/GPT）绝不能被当成 GEMINI 组：它同样有 5h 和 weekly 窗口。
func TestGeminiIgnoresThirdPartyGroup(t *testing.T) {
	sum := mustParse(t, realSummary)
	for i, g := range sum.Groups {
		t.Logf("组[%d] displayName=%q buckets=%d", i, g.DisplayName, len(g.Buckets))
	}
	g := sum.Gemini()
	if g == nil || g.FiveHour == nil {
		t.Fatal("应认出 GEMINI 组")
	}
	if g.FiveHour.BucketID != geminiBucket5h {
		t.Errorf("取到了 %q，不是 %q —— 很可能抓成了 3p 组",
			g.FiveHour.BucketID, geminiBucket5h)
	}
	// 3p 组两个窗口都是 1；若误取会得到 1 而不是 0。
	if rem, _ := g.FiveHour.Remaining(); rem != 0 {
		t.Errorf("5h 剩余 = %v，want 0（取错组了）", rem)
	}
}

// 组名被上游改掉（不含 gemini 字样）时，靠 bucketId 仍要认得出来。
func TestGeminiFindsGroupByBucketID(t *testing.T) {
	raw := `{"groups":[{"displayName":"AI Models","buckets":[
	  {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5},
	  {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":1}]}]}`
	g := mustParse(t, raw).Gemini()
	if g == nil || g.FiveHour == nil {
		t.Fatal("bucketId 匹配失败")
	}
	if rem, _ := g.FiveHour.Remaining(); rem != 0.5 {
		t.Errorf("5h 剩余 = %v，want 0.5", rem)
	}
}

// 找不到 GEMINI 组时必须返回 nil，且不能被误判为「已耗尽」。
func TestGeminiAbsentIsNotExhausted(t *testing.T) {
	cases := map[string]string{
		"空 groups": `{"groups":[]}`,
		"只有 3p 组": `{"groups":[{"displayName":"Third Party Models","buckets":[
		                  {"bucketId":"3p-5h","window":"5h","remainingFraction":0}]}]}`,
		"组在但没窗口": `{"groups":[{"displayName":"Gemini Models","buckets":[]}]}`,
		"顶层不是对象": `[]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var s Summary
			_ = json.Unmarshal([]byte(raw), &s) // 顶层不是对象时静默得到零值
			g := s.Gemini()
			if _, ex := g.Exhausted(0, time.Now()); ex {
				t.Errorf("Gemini()=%v 却判成耗尽", g)
			}
		})
	}
}

func TestExhausted(t *testing.T) {
	now := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	reset5h := now.Add(25 * time.Minute) // 11:25
	resetWeek := now.Add(7 * 24 * time.Hour)

	cases := []struct {
		name      string
		five, wk  *Bucket
		threshold float64
		want      bool
		wantUntil time.Time // 零值表示不校验
	}{
		{
			name: "5h 归零 → 冷却到 5h reset",
			five: &Bucket{BucketID: geminiBucket5h, RemainingFraction: f64(0),
				ResetTime: reset5h.Format(time.RFC3339)},
			wk:        &Bucket{RemainingFraction: f64(1), ResetTime: resetWeek.Format(time.RFC3339)},
			want:      true,
			wantUntil: reset5h.Add(resetGrace),
		},
		{
			name:      "都归零 → 取最晚的那个（周）",
			five:      &Bucket{RemainingFraction: f64(0), ResetTime: reset5h.Format(time.RFC3339)},
			wk:        &Bucket{RemainingFraction: f64(0), ResetTime: resetWeek.Format(time.RFC3339)},
			want:      true,
			wantUntil: resetWeek.Add(resetGrace),
		},
		{
			name: "都有余量 → 不冷却",
			five: &Bucket{RemainingFraction: f64(0.3), ResetTime: reset5h.Format(time.RFC3339)},
			wk:   &Bucket{RemainingFraction: f64(1)},
			want: false,
		},
		{
			name:      "刚好等于阈值 → 判耗尽（<= 语义）",
			five:      &Bucket{RemainingFraction: f64(0.1)},
			wk:        &Bucket{RemainingFraction: f64(1)},
			threshold: 0.1,
			want:      true,
		},
		{
			name: "remainingFraction 字段缺失 → 当未知，不冷却",
			five: &Bucket{}, // RemainingFraction == nil
			wk:   &Bucket{},
			want: false,
		},
		{
			name: "resetTime 已过去（上游滞后）→ 至少退避 minCooldown",
			five: &Bucket{RemainingFraction: f64(0),
				ResetTime: now.Add(-time.Hour).Format(time.RFC3339)},
			wk:        &Bucket{RemainingFraction: f64(1)},
			want:      true,
			wantUntil: now.Add(minCooldown + resetGrace),
		},
		{
			name:      "resetTime 格式解析不出 → 至少退避 minCooldown",
			five:      &Bucket{RemainingFraction: f64(0), ResetTime: "not-a-time"},
			wk:        &Bucket{RemainingFraction: f64(1)},
			want:      true,
			wantUntil: now.Add(minCooldown + resetGrace),
		},
		{
			name:      "只有 5h 有值",
			five:      &Bucket{RemainingFraction: f64(0), ResetTime: reset5h.Format(time.RFC3339)},
			want:      true,
			wantUntil: reset5h.Add(resetGrace),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &Gemini{FiveHour: c.five, Weekly: c.wk}
			until, got := g.Exhausted(c.threshold, now)
			if got != c.want {
				t.Fatalf("Exhausted(%v) = (%v,%v)，want %v", c.threshold, until, got, c.want)
			}
			if c.want && !c.wantUntil.IsZero() && !until.Equal(c.wantUntil) {
				t.Errorf("冷却至 %v，want %v", until, c.wantUntil)
			}
		})
	}
}

// nil 接收者（找不到 GEMINI 组）不能 panic。
func TestGeminiNilReceiver(t *testing.T) {
	var g *Gemini
	if _, ex := g.Exhausted(0, time.Now()); ex {
		t.Error("nil 不应判成耗尽")
	}
	if s := g.Describe(); !strings.Contains(s, "未找到") {
		t.Errorf("Describe() = %q", s)
	}
}

func TestDescribe(t *testing.T) {
	g := mustParse(t, realSummary).Gemini()
	s := g.Describe()
	t.Logf("摘要: %s", s)
	wantTime := g.FiveHour.Reset().Local().Format("15:04")
	for _, want := range []string{"5h", "0.00%", "周", "100.00%", wantTime} {
		if !strings.Contains(s, want) {
			t.Errorf("Describe()=%q 缺 %q", s, want)
		}
	}
}

// Fetch 必须带许可证闸门 UA，否则上游 403 —— 这是整个额度检测的前提。
func TestFetchSendsRequiredHeaders(t *testing.T) {
	const (
		wantPath = "/v1internal:retrieveUserQuotaSummary"
		wantUA   = "antigravity-cli/1.2.9"
		wantTok  = "tok-abc"
	)
	var got struct {
		method, path, ua, auth, ctype string
		body                          []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.ua = r.Header.Get("User-Agent")
		got.auth = r.Header.Get("Authorization")
		got.ctype = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, realSummary)
	}))
	defer srv.Close()

	// 末尾斜杠必须被吃掉，否则会拼出 //v1internal:...
	sum, err := Fetch(context.Background(), srv.URL+"/", wantTok, wantUA)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sum.Gemini() == nil {
		t.Error("取回的汇总里没有 GEMINI 组")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s", got.method)
	}
	if got.path != wantPath {
		t.Errorf("path = %q，want %q", got.path, wantPath)
	}
	if got.ua != wantUA {
		t.Errorf("User-Agent = %q，want %q —— 缺了会被上游 403", got.ua, wantUA)
	}
	if got.auth != "Bearer "+wantTok {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q", got.ctype)
	}
	if len(got.body) == 0 {
		t.Error("请求体为空，上游期望 {}")
	}
}

// 非 200 必须报错而不是返回空汇总 —— 空汇总会被静默读成「无 GEMINI 组」。
func TestFetchNon200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"status":"SUBSCRIPTION_REQUIRED"}}`)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, "tok", "Go-http-client/1.1")
	if err == nil {
		t.Fatal("403 必须返回 error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息没带上状态码: %v", err)
	}
}

func TestFetchRejectsEmptyTokenAndBadJSON(t *testing.T) {
	if _, err := Fetch(context.Background(), "http://x", "", "ua"); err == nil {
		t.Error("空 access_token 应报错")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>oops</html>")
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.URL, "tok", "ua"); err == nil {
		t.Error("非 JSON 响应应报错")
	}
}
