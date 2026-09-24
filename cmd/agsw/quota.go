package main

import (
	"context"
	"log"
	"time"

	"github.com/kylesean/agsw/internal/pool"
	"github.com/kylesean/agsw/internal/quota"
)

// quotaWatcher 定期查池中每个账号的 GEMINI 额度，耗尽就打冷却、恢复就解冻。
//
// 为什么放在后台轮询，而不是每次选号时现查：
// 查一次额度是一次网络往返，挂在请求路径上会让每个请求都慢一拍，
// 而额度一分钟才变一次。冷却一旦写进 Selector，
// 下一次 Pick 就自然跳过那个号 —— 换号逻辑不需要碰转发路径一行代码。
//
// 判据是只读的 retrieveUserQuotaSummary，不消耗生成额度，
// 所以「拿现有没额度的账号反复测」是安全的。
type quotaWatcher struct {
	sel       *pool.Selector
	upstream  string
	userAgent string
	threshold float64
	log       *log.Logger

	// last 记录各账号上一次的耗尽状态，用于去重日志
	last    map[string]bool
	trigger chan struct{}
}

func newQuotaWatcher(sel *pool.Selector, upstream, userAgent string,
	threshold float64, lg *log.Logger) *quotaWatcher {
	return &quotaWatcher{
		sel:       sel,
		upstream:  upstream,
		userAgent: userAgent,
		threshold: threshold,
		log:       lg,
		last:      make(map[string]bool),
		trigger:   make(chan struct{}, 1),
	}
}

// triggerCheck 异步触发一轮即时额度检测（非阻塞）。
func (q *quotaWatcher) triggerCheck() {
	select {
	case q.trigger <- struct{}{}:
	default:
	}
}

// check 跑一轮额度检测。first 为真时无论状态如何都打一行 ——
// 启动那一轮要看到每个账号的全量额度，而不是只看到翻转。
//
// 单个账号查失败只告警并保持原状态：额度接口抖一下不该把号锁死，
// 而 cooldown 本来就有 resetTime 兜底，下一轮会纠正。
func (q *quotaWatcher) check(ctx context.Context, first bool) {
	now := time.Now()
	for _, a := range q.sel.Candidates() {
		tok, err := q.sel.FreshToken(ctx, a.Name)
		if err != nil {
			q.log.Printf("查额度 %s 获取 token 失败（保持原状态）: %v", a.Name, err)
			continue
		}
		ck, cancel := context.WithTimeout(ctx, 15*time.Second)
		sum, err := quota.Fetch(ck, q.upstream, tok, q.userAgent)
		cancel()
		if err != nil {
			q.log.Printf("查额度 %s 失败（保持原状态）: %v", a.Name, err)
			continue
		}

		g := sum.Gemini()
		until, exhausted := g.Exhausted(q.threshold, now)
		changed := first || q.last[a.Name] != exhausted
		q.last[a.Name] = exhausted
		q.sel.SetQuotaState(a.Name, exhausted)

		if exhausted {
			q.sel.SetCooldown(a.Name, until)
			if changed {
				q.log.Printf("额度耗尽 %s: %s → 冷却至 %s",
					a.Name, g.Describe(), pool.FormatCooldown(until))
			}
			continue
		}
		q.sel.SetCooldown(a.Name, time.Time{})
		if changed {
			q.log.Printf("额度 %s: %s", a.Name, g.Describe())
		}
	}
}

// run 按 interval 循环 check，ctx 取消即退。首轮由调用方同步执行。
func (q *quotaWatcher) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.check(ctx, false)
		case <-q.trigger:
			q.check(ctx, false)
		}
	}
}
