package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// baseTime 固定基准时刻，避免用例受真实时钟抖动影响。
var baseTime = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// fixedClock 让 Selector 的 now 可控。
func newSelector(cands []*Account, refresh Refresher, at time.Time) *Selector {
	s := NewSelector(cands, refresh)
	s.now = func() time.Time { return at }
	return s
}

// noRefresh 模拟一个「不会刷新」的环境。
func noRefresh(context.Context, *Account) error { return errors.New("不该被调用") }

func TestPickReturnsSnapshotOfValidAccount(t *testing.T) {
	a := &Account{Name: "main", Email: "m@x", AccessToken: "AT-1",
		Expiry: baseTime.Add(time.Hour)}
	s := newSelector([]*Account{a}, noRefresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "main" || got.AccessToken != "AT-1" {
		t.Errorf("got %+v", got)
	}

	// 快照语义：改返回值不能影响选择器内部状态。
	got.AccessToken = "MUTATED"
	again, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick 2: %v", err)
	}
	if again.AccessToken != "AT-1" {
		t.Errorf("返回的不是快照，内部 token 被改成了 %q", again.AccessToken)
	}
}

func TestPickRefreshesWhenInsideSkewWindow(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "OLD", RefreshToken: "RT",
		Expiry: baseTime.Add(2 * time.Minute)} // 在 5 分钟宽限期内

	calls := 0
	refresh := func(_ context.Context, ac *Account) error {
		calls++
		ac.AccessToken = "NEW"
		ac.Expiry = baseTime.Add(time.Hour)
		return nil
	}
	s := newSelector([]*Account{a}, refresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if calls != 1 {
		t.Errorf("刷新次数 = %d, 想要 1", calls)
	}
	if got.AccessToken != "NEW" {
		t.Errorf("拿到旧 token %q，刷新没生效", got.AccessToken)
	}
}

func TestPickDoesNotRefreshWhenTokenFresh(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "FRESH", RefreshToken: "RT",
		Expiry: baseTime.Add(time.Hour)}
	s := newSelector([]*Account{a}, noRefresh, baseTime) // noRefresh 一被调用就报错

	if _, err := s.Pick(context.Background()); err != nil {
		t.Errorf("token 还新鲜就不该触发刷新: %v", err)
	}
}

func TestPickSkipsCoolingAccountAndUsesNext(t *testing.T) {
	// 账号设置冷却后，Pick 自动跳过并顺延至下一可用账号
	a := &Account{Name: "exhausted", AccessToken: "A1",
		Expiry:        baseTime.Add(time.Hour),
		CooldownUntil: baseTime.Add(30 * time.Minute)}
	b := &Account{Name: "backup", AccessToken: "A2",
		Expiry: baseTime.Add(time.Hour)}

	s := newSelector([]*Account{a, b}, noRefresh, baseTime)
	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "backup" {
		t.Errorf("选到 %q，应跳过冷却中的 exhausted", got.Name)
	}
}

func TestPickFailsWhenAllCooling(t *testing.T) {
	a := &Account{Name: "only", AccessToken: "A", Expiry: baseTime.Add(time.Hour),
		CooldownUntil: baseTime.Add(30 * time.Minute)}
	s := newSelector([]*Account{a}, noRefresh, baseTime)

	_, err := s.Pick(context.Background())
	if err == nil {
		t.Fatal("全部冷却时应报错")
	}
	if !strings.Contains(err.Error(), "only") {
		t.Errorf("错误里应点名账号: %v", err)
	}
	if !strings.Contains(err.Error(), "冷却") {
		t.Errorf("错误里应说明是冷却导致: %v", err)
	}
}

func TestPickDegradesToOldTokenWhenRefreshFailsButNotExpired(t *testing.T) {
	// 宽限期内刷新失败 → 降级用旧 token，比把这次请求判死更稳。
	a := &Account{Name: "main", AccessToken: "OLD", RefreshToken: "RT",
		Expiry: baseTime.Add(2 * time.Minute)}
	refresh := func(context.Context, *Account) error {
		return errors.New("网络抖动")
	}
	s := newSelector([]*Account{a}, refresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("刷新失败但未过期时应降级，不应报错: %v", err)
	}
	if got.AccessToken != "OLD" {
		t.Errorf("应沿用旧 token, got %q", got.AccessToken)
	}
}

func TestPickFailsWhenRefreshFailsAndExpired(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "EXPIRED", RefreshToken: "RT",
		Expiry: baseTime.Add(-time.Minute)} // 已经过期了
	refresh := func(context.Context, *Account) error {
		return errors.New("invalid_grant")
	}
	s := newSelector([]*Account{a}, refresh, baseTime)

	_, err := s.Pick(context.Background())
	if err == nil {
		t.Fatal("已过期且刷新失败时应报错")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("错误应带上刷新失败的原因: %v", err)
	}
}

func TestPickSkipsToNextWhenFirstAccountRefreshFails(t *testing.T) {
	bad := &Account{Name: "bad", AccessToken: "EXPIRED", RefreshToken: "RT",
		Expiry: baseTime.Add(-time.Minute)}
	good := &Account{Name: "good", AccessToken: "OK",
		Expiry: baseTime.Add(time.Hour)}

	refresh := func(context.Context, *Account) error { return errors.New("boom") }
	s := newSelector([]*Account{bad, good}, refresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("应跳到下一个账号: %v", err)
	}
	if got.Name != "good" {
		t.Errorf("选到 %q, 想要 good", got.Name)
	}
}

func TestPickEmptyPoolExplainsWhatToDo(t *testing.T) {
	s := newSelector(nil, nil, baseTime)
	_, err := s.Pick(context.Background())
	if err == nil {
		t.Fatal("空池应报错")
	}
	if !strings.Contains(err.Error(), "agsw add") {
		t.Errorf("错误应给出下一步操作提示: %v", err)
	}
}

func TestPickTreatsEmptyAccessTokenAsUnusable(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "", RefreshToken: ""}
	s := newSelector([]*Account{a}, nil, baseTime)

	_, err := s.Pick(context.Background())
	if err == nil {
		t.Fatal("空 access_token 且无法刷新时应报错")
	}
}

func TestPickWithUnknownExpiryUsesTokenAsIs(t *testing.T) {
	// Expiry 零值 = 不知道过期时间：用现成 token，不主动刷新。
	a := &Account{Name: "main", AccessToken: "USED", RefreshToken: "RT"}
	s := newSelector([]*Account{a}, noRefresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AccessToken != "USED" {
		t.Errorf("got %q", got.AccessToken)
	}
}

func TestPickRefreshesEmptyAccessTokenFromRefreshToken(t *testing.T) {
	// 池里只有 refresh_token（access_token 被清掉）时应当能自愈。
	a := &Account{Name: "main", AccessToken: "", RefreshToken: "RT"}
	refresh := func(_ context.Context, ac *Account) error {
		ac.AccessToken = "RECOVERED"
		ac.Expiry = baseTime.Add(time.Hour)
		return nil
	}
	s := newSelector([]*Account{a}, refresh, baseTime)

	got, err := s.Pick(context.Background())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AccessToken != "RECOVERED" {
		t.Errorf("got %q", got.AccessToken)
	}
}

func TestRefreshAllSucceedsPartially(t *testing.T) {
	a := &Account{Name: "ok", RefreshToken: "RT1", AccessToken: "A1"}
	b := &Account{Name: "dead", RefreshToken: "RT2", AccessToken: "A2"}

	refresh := func(_ context.Context, ac *Account) error {
		if ac.Name == "dead" {
			return errors.New("revoked")
		}
		ac.AccessToken = "FRESH"
		return nil
	}
	s := newSelector([]*Account{a, b}, refresh, baseTime)

	err := s.RefreshAll(context.Background())
	if err == nil {
		t.Fatal("有账号刷新失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "dead") || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("应点名失败账号与原因: %v", err)
	}
	if a.AccessToken != "FRESH" {
		t.Error("成功的那个不该被失败的连累")
	}
}

func TestRefreshAllRejectsWhenNoRefreshToken(t *testing.T) {
	a := &Account{Name: "no-rt", AccessToken: "A"}
	called := false
	s := newSelector([]*Account{a}, func(context.Context, *Account) error {
		called = true
		return nil
	}, baseTime)

	err := s.RefreshAll(context.Background())
	if err == nil {
		t.Fatal("没有任何 refresh_token 时应报错")
	}
	if called {
		t.Error("不该调用 refresh")
	}
	if !strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("错误应说明原因: %v", err)
	}
}

func TestRefreshAllRejectsWhenNoRefresher(t *testing.T) {
	a := &Account{Name: "x", RefreshToken: "RT"}
	s := newSelector([]*Account{a}, nil, baseTime)
	if err := s.RefreshAll(context.Background()); err == nil {
		t.Error("没有 Refresher 时应报错")
	}
}

func TestCandidatesReturnsCopies(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "AT"}
	s := newSelector([]*Account{a}, nil, baseTime)

	got := s.Candidates()
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	got[0].AccessToken = "MUTATED"
	if a.AccessToken != "AT" {
		t.Error("Candidates 应返回副本")
	}
}

func TestCandidatesSkipsNilEntries(t *testing.T) {
	s := newSelector([]*Account{nil, {Name: "real", AccessToken: "AT"}}, nil, baseTime)
	if got := s.Candidates(); len(got) != 1 || got[0].Name != "real" {
		t.Errorf("got %+v", got)
	}
}

// TestPickIsConcurrencySafe 是 serve 的基本盘：
// 一个 agy 会话会并发打很多请求，全都可能走进 Pick 触发刷新。
// 用 -race 跑，一旦有数据竞争这里就会炸。
func TestPickIsConcurrencySafe(t *testing.T) {
	a := &Account{Name: "main", AccessToken: "OLD", RefreshToken: "RT",
		Expiry: baseTime.Add(2 * time.Minute)}

	var mu sync.Mutex
	refreshCalls := 0
	refresh := func(_ context.Context, ac *Account) error {
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		ac.AccessToken = "NEW"
		ac.Expiry = baseTime.Add(time.Hour)
		return nil
	}
	s := newSelector([]*Account{a}, refresh, baseTime)

	const n = 32
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Pick(context.Background())
			if err != nil {
				errCh <- err
				return
			}
			// 拿到快照后读它，若不是副本就会和并发刷新产生竞争。
			_ = got.AccessToken
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发 Pick 出错: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 {
		t.Errorf("刷新次数 = %d, 想要 1（互斥锁应防止刷新风暴）", refreshCalls)
	}
}

func TestCoolingWithinSkewAndHardExpiredHelpers(t *testing.T) {
	if cooling(&Account{}, baseTime) {
		t.Error("无 CooldownUntil 不应算冷却")
	}
	if !cooling(&Account{CooldownUntil: baseTime.Add(time.Minute)}, baseTime) {
		t.Error("未来时刻应算冷却")
	}
	if cooling(&Account{CooldownUntil: baseTime.Add(-time.Minute)}, baseTime) {
		t.Error("已过去的冷却应失效")
	}

	// Expiry 零值：不进宽限期、也不算硬过期。
	if withinSkew(&Account{}, baseTime, DefaultRefreshSkew) {
		t.Error("Expiry 未知不应进宽限期")
	}
	if hardExpired(&Account{}, baseTime) {
		t.Error("Expiry 未知不应算硬过期")
	}

	if !withinSkew(&Account{Expiry: baseTime.Add(time.Minute)}, baseTime, DefaultRefreshSkew) {
		t.Error("1 分钟后过期应在 5 分钟宽限期内")
	}
	if withinSkew(&Account{Expiry: baseTime.Add(time.Hour)}, baseTime, DefaultRefreshSkew) {
		t.Error("1 小时后过期不应进宽限期")
	}
	if !hardExpired(&Account{Expiry: baseTime.Add(-time.Second)}, baseTime) {
		t.Error("已过期应算硬过期")
	}
}

// 冷却时刻必须看得出日期：周窗口额度耗尽要冷却一周，
// 只打 15:04:05 会让人以为那个时刻已经过去（E2E 实测踩到过）。
func TestFormatCooldown(t *testing.T) {
	if got := FormatCooldown(time.Time{}); got != "-" {
		t.Errorf("零值 = %q，want -", got)
	}

	now := time.Now()
	sameDay := now.Add(90 * time.Second)
	want := sameDay.Local().Format("15:04:05")
	if got := FormatCooldown(sameDay); got != want {
		t.Errorf("同日 = %q，want %q", got, want)
	}

	nextWeek := now.Add(7 * 24 * time.Hour)
	got := FormatCooldown(nextWeek)
	if len(got) != len("01-02 15:04") {
		t.Errorf("跨天 = %q（长度 %d），want 带日期的 %d",
			got, len(got), len("01-02 15:04"))
	}
	if wantDate := nextWeek.Local().Format("01-02"); !strings.Contains(got, wantDate) {
		t.Errorf("跨天 = %q，缺日期 %q", got, wantDate)
	}
}

// TestPickRefreshDoesNotBlockGlobalSelectorMethods 验证单个账号刷新网络 I/O 不会阻塞全局方法
func TestPickRefreshDoesNotBlockGlobalSelectorMethods(t *testing.T) {
	block := make(chan struct{})
	enteredRefresh := make(chan struct{})

	refresh := func(_ context.Context, ac *Account) error {
		close(enteredRefresh)
		<-block
		ac.AccessToken = "REFRESHED"
		ac.Expiry = baseTime.Add(time.Hour)
		return nil
	}

	a := &Account{Name: "a", AccessToken: "OLD", RefreshToken: "RT", Expiry: baseTime.Add(time.Minute)}
	b := &Account{Name: "b", AccessToken: "FRESH", Expiry: baseTime.Add(time.Hour)}

	s := newSelector([]*Account{a, b}, refresh, baseTime)

	pickDone := make(chan struct{})
	go func() {
		defer close(pickDone)
		_, _ = s.Pick(context.Background())
	}()

	// 等待进入刷新逻辑
	select {
	case <-enteredRefresh:
	case <-time.After(time.Second):
		t.Fatal("未能在预期时间内进入 refresh")
	}

	// 在刷新依然阻塞的情况下，调用 CooldownOf / SetCooldown 必须不被阻塞
	doneMethod := make(chan struct{})
	go func() {
		defer close(doneMethod)
		_ = s.CooldownOf("b")
		s.SetCooldown("b", baseTime.Add(time.Minute))
	}()

	select {
	case <-doneMethod:
		// 成功：没有被阻塞
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Candidates/Cooldown 方法被账号刷新锁阻塞了")
	}

	close(block)
	<-pickDone
}

// TestPickDeduplicatesConcurrentRefreshes 验证同一账号的并发刷新会被单飞合并
func TestPickDeduplicatesConcurrentRefreshes(t *testing.T) {
	var refreshCalls int
	var mu sync.Mutex
	block := make(chan struct{})

	refresh := func(_ context.Context, ac *Account) error {
		mu.Lock()
		refreshCalls++
		mu.Unlock()
		<-block
		ac.AccessToken = "REFRESHED"
		ac.Expiry = baseTime.Add(time.Hour)
		return nil
	}

	a := &Account{Name: "a", AccessToken: "OLD", RefreshToken: "RT", Expiry: baseTime.Add(time.Minute)}
	s := newSelector([]*Account{a}, refresh, baseTime)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.Pick(context.Background())
		}()
	}

	// 稍微等待两个协程都进入 Pick
	time.Sleep(30 * time.Millisecond)
	close(block)
	wg.Wait()

	mu.Lock()
	calls := refreshCalls
	mu.Unlock()

	if calls != 1 {
		t.Fatalf("预期只执行 1 次刷新合并，实际执行了 %d 次", calls)
	}
}


