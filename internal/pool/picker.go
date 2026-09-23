package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultRefreshSkew 是过期前提前续期的宽限期。
// 留 5 分钟余量避免请求落在过期临界点引发鉴权失败。
const DefaultRefreshSkew = 5 * time.Minute

// Refresher 用 refresh_token 换取新的 access_token 并更新账号对象。
type Refresher func(ctx context.Context, a *Account) error

// Selector 从候选账号中挑选当前可用账号。
// 采用账号级别的细粒度互斥锁同步续期状态，避免刷新风暴，返回调用方快照副本。
// 当某个账号设置了 CooldownUntil 时，Pick 会自动跳过并顺延到下一可用账号。
type Selector struct {
	mu       sync.Mutex
	cands    []*Account
	refresh  Refresher
	skew     time.Duration
	now      func() time.Time
	lastErrs error

	accountMu sync.Mutex
	locks     map[string]*sync.Mutex
}

// NewSelector 构造选择器。refresh 为 nil 时不刷新，仅使用现存 token。
func NewSelector(cands []*Account, refresh Refresher) *Selector {
	return &Selector{
		cands:   cands,
		refresh: refresh,
		skew:    DefaultRefreshSkew,
		now:     time.Now,
		locks:   make(map[string]*sync.Mutex),
	}
}

func (s *Selector) accountLock(name string) *sync.Mutex {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	l, ok := s.locks[name]
	if !ok {
		l = new(sync.Mutex)
		s.locks[name] = l
	}
	return l
}

// Candidates 返回候选账号的快照副本，仅供状态展示。
func (s *Selector) Candidates() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.cands))
	for _, a := range s.cands {
		if a == nil {
			continue
		}
		l := s.accountLock(a.Name)
		l.Lock()
		cp := *a
		l.Unlock()
		out = append(out, &cp)
	}
	return out
}

// LastErrors 返回最近一次 Pick 失败的原因。
func (s *Selector) LastErrors() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErrs
}

// Pick 挑选可用账号快照。账号需要续期时在账号粒度进行并发同步，不阻塞全局锁。
func (s *Selector) Pick(ctx context.Context) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var errs []error

	for i := 0; i < len(s.cands); i++ {
		a := s.cands[i]
		if a == nil {
			continue
		}
		if cooling(a, now) {
			errs = append(errs, fmt.Errorf("%s: 冷却至 %s",
				a.Name, FormatCooldown(a.CooldownUntil)))
			continue
		}

		l := s.accountLock(a.Name)

		l.Lock()
		needsRefresh := a.AccessToken == "" || withinSkew(a, now, s.skew)
		l.Unlock()

		if needsRefresh {
			if s.refresh != nil && a.RefreshToken != "" {
				s.mu.Unlock()

				l.Lock()
				now = s.now()
				var err error
				if a.AccessToken == "" || withinSkew(a, now, s.skew) {
					err = s.refresh(ctx, a)
				}
				l.Unlock()

				s.mu.Lock()
				now = s.now()

				if err != nil {
					l.Lock()
					expired := a.AccessToken == "" || hardExpired(a, now)
					l.Unlock()
					if expired {
						errs = append(errs, fmt.Errorf("%s 刷新失败: %w", a.Name, err))
						continue
					}
				}
			} else {
				l.Lock()
				expired := a.AccessToken == "" || hardExpired(a, now)
				l.Unlock()
				if expired {
					errs = append(errs, fmt.Errorf("%s: 无可用 token 且无法刷新", a.Name))
					continue
				}
			}
		}

		l.Lock()
		if a.AccessToken == "" {
			l.Unlock()
			errs = append(errs, fmt.Errorf("%s: access_token 为空", a.Name))
			continue
		}
		cp := *a
		l.Unlock()

		s.lastErrs = nil
		return &cp, nil
	}

	s.lastErrs = errors.Join(errs...)
	if len(errs) == 0 {
		return nil, errors.New("账号池为空（先用 agsw add <name> 捕获一个）")
	}
	return nil, fmt.Errorf("没有可用账号: %w", s.lastErrs)
}

// CooldownOf 返回账号当前的冷却截止时刻，账号不存在返回零值。只读，供日志展示。
func (s *Selector) CooldownOf(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.cands {
		if a != nil && a.Name == name {
			return a.CooldownUntil
		}
	}
	return time.Time{}
}

// SetCooldown 设置账号冷却截止时刻。处于冷却期的账号在 Pick 时会被自动跳过。
// until 为零值或过去时间表示解除冷却。
func (s *Selector) SetCooldown(name string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.cands {
		if a == nil || a.Name != name {
			continue
		}
		a.CooldownUntil = until
		return
	}
}

// SetQuotaState 记录账号的额度耗尽标记（内存状态）。
func (s *Selector) SetQuotaState(name string, exhausted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.cands {
		if a == nil || a.Name != name {
			continue
		}
		a.QuotaExhausted = exhausted
		return
	}
}

// RefreshAll 强制刷新全部候选账号，常用于启动预热。
func (s *Selector) RefreshAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.refresh == nil {
		return errors.New("未配置 Refresher，无法刷新")
	}

	var errs []error
	tried := 0
	for _, a := range s.cands {
		if a == nil || a.RefreshToken == "" {
			continue
		}
		tried++
		if err := s.refresh(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
		}
	}
	if tried == 0 {
		return errors.New("候选账号都没有 refresh_token，无法刷新")
	}
	return errors.Join(errs...)
}

// FormatCooldown 将冷却截止时刻格式化为易读时间文本。
func FormatCooldown(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	l := t.Local()
	now := time.Now()
	if l.Year() == now.Year() && l.YearDay() == now.YearDay() {
		return l.Format("15:04:05")
	}
	return l.Format("01-02 15:04")
}

// cooling 判断账号当前是否处于冷却期。
func cooling(a *Account, now time.Time) bool {
	return !a.CooldownUntil.IsZero() && now.Before(a.CooldownUntil)
}

// withinSkew 判断是否已进入「过期前 skew 就该刷新」的窗口。
// Expiry 为零值表示不知道过期时间，此时不主动刷新（避免每次请求都刷）。
func withinSkew(a *Account, now time.Time, skew time.Duration) bool {
	if a.Expiry.IsZero() {
		return false
	}
	return !now.Before(a.Expiry.Add(-skew))
}

// hardExpired 判断 token 是否已真正过期（而非仅进入宽限期）。
func hardExpired(a *Account, now time.Time) bool {
	return !a.Expiry.IsZero() && now.After(a.Expiry)
}
