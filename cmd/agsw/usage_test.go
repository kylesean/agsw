package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kylesean/agsw/internal/pool"
	"github.com/kylesean/agsw/internal/quota"
)

func TestFormatUsageLineIncludesQuotaWindows(t *testing.T) {
	reset := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	five := 0.25
	week := 0.8
	line := formatUsageLine(&pool.Account{Name: "A", Email: "a@example.com"}, &quota.Summary{
		Groups: []quota.Group{{DisplayName: "Gemini Models", Buckets: []quota.Bucket{
			{BucketID: "gemini-5h", Window: "5h", RemainingFraction: &five, ResetTime: reset.Format(time.RFC3339)},
			{BucketID: "gemini-weekly", Window: "weekly", RemainingFraction: &week, ResetTime: reset.Format(time.RFC3339)},
		}}},
	})
	if !strings.Contains(line, "A") || !strings.Contains(line, "a@example.com") ||
		!strings.Contains(line, "25.00%") || !strings.Contains(line, "80.00%") {
		t.Fatalf("usage line = %q", line)
	}
}
