package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kylesean/agsw/internal/cloudcode"
	"github.com/kylesean/agsw/internal/pool"
	"github.com/kylesean/agsw/internal/proxy"
	"github.com/kylesean/agsw/internal/token"
)

// stringList 让 -strip-field 可以重复出现，例如
//
//	-strip-field sessionId -strip-field somethingElse
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("-strip-field 不能是空字符串")
	}
	*s = append(*s, v)
	return nil
}

// DefaultUpstream 是 Antigravity CloudCode 服务端点。
// 方法路径为 v1internal:streamGenerateContent，顶层协议为 {model, request}。
const DefaultUpstream = "https://daily-cloudcode-pa.googleapis.com"

// defaultUserAgent 是上游服务要求的客户端标识头，用于通行认证闸门。
const defaultUserAgent = "antigravity-cli/1.2.9"

// cmdServe 启动反向代理服务。
// 职责：挑选有效账号 → 注入 Authorization 与专用 User-Agent → 双向改写请求/响应信封
// → 转发上游 → 后台监听配额并在耗尽或收到 429 时自动切号。
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:7897", "监听地址")
	upstream := fs.String("upstream", DefaultUpstream, "上游地址")
	account := fs.String("account", "", "指定用池中哪个账号；留空则用池里全部，池空则回落 keyring 当前账号")
	refresh := fs.Bool("refresh", false, "启动时先强制刷新一次 access_token")
	verbose := fs.Bool("v", false, "打印每个请求的选号与转发结果")
	ua := fs.String("user-agent", defaultUserAgent, "发给上游的 User-Agent（许可证闸门，一般别改）")
	passthrough := fs.Bool("passthrough", false,
		"关闭信封改写，纯透传（调试用；此时上游不会接受 agy 的 Gemini 路径）")
	noModelAlias := fs.Bool("no-model-alias", false,
		"不拉模型表、不做模型名映射（调试用；gemini-3.8-flash 之类会 404）")
	quotaInterval := fs.Duration("quota-interval", time.Minute,
		"GEMINI 额度轮询间隔；设为 0 关闭额度检测（关了就不会自动换号）")
	quotaThreshold := fs.Float64("quota-threshold", 0,
		"GEMINI 剩余比例 ≤ 该值即判耗尽并换号（0 = 归零才切，不浪费残量）")
	var stripFields stringList
	fs.Var(&stripFields, "strip-field",
		"转发前从请求体顶层删掉这个 JSON 字段，可重复（云封信封下 agy 的 body 原样可用，一般用不到）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve 不接受位置参数")
	}
	if *quotaThreshold < 0 || *quotaThreshold > 1 {
		return fmt.Errorf("-quota-threshold 需在 0~1 之间（收到 %v）", *quotaThreshold)
	}
	if *quotaInterval < 0 {
		return fmt.Errorf("-quota-interval 不能为负（收到 %s）", *quotaInterval)
	}

	lg := log.New(os.Stderr, "[serve] ", log.LstdFlags|log.Lmsgprefix)
	if !*verbose {
		// 逐请求日志只在 -v 下开；否则 agy 每次重试都刷一行太吵。
		lg.SetOutput(os.Stderr)
	}

	cands, err := resolveCandidates(*account)
	if err != nil {
		return err
	}

	// 刷新逻辑交给 Selector，它在锁内做，防止并发请求触发刷新风暴。
	sel := pool.NewSelector(cands, refreshAccount)
	adapter := &selectorPicker{sel: sel, verbose: *verbose, log: lg}

	if *refresh {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := sel.RefreshAll(refreshCtx)
		cancel()
		if err != nil {
			// 一个账号失败不该阻止整个 serve 启动，
			// 但如果连一个可用账号都挑不出来，那就必须报错。
			lg.Printf("强制刷新部分失败: %v", err)
			if _, pickErr := sel.Pick(ctx); pickErr != nil {
				return fmt.Errorf("刷新后仍无可用账号: %w", pickErr)
			}
		} else {
			lg.Printf("已强制刷新全部账号")
		}
	}

	srv, err := proxy.New(*upstream, adapter, lg)
	if err != nil {
		return err
	}
	srv.SetUserAgent(*ua)
	if !*passthrough {
		srv.EnableEnvelope()
	}
	srv.StripFields(stripFields...)
	if n := len(stripFields); n > 0 {
		lg.Printf("剥离请求体字段: %s", strings.Join(stripFields, ", "))
	}

	// 启动时先挑一次，让错误在监听前就暴露出来，而不是等第一个请求 503。
	first, err := sel.Pick(ctx)
	if err != nil {
		return err
	}

	// 模型别名：agy 网关模式发的 gemini-3.8-flash 上游并不认，
	// 真实键是 gemini-3.8-flash-tiered。拉一次模型表推导出来。
	// 失败只告警不退出：模型表拿不到时仍可能有一部分模型是直接命中的
	//（实测 gemini-3.1-flash-lite 就是），不该因此把整个 serve 判死。
	if srv.Envelope() && !*noModelAlias {
		aliasCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		models, aliasErr := cloudcode.FetchModels(aliasCtx, *upstream,
			first.AccessToken, srv.UserAgentForUpstream())
		cancel()
		if aliasErr != nil {
			lg.Printf("获取模型表失败，不做模型名映射（部分模型可能 404）: %v", aliasErr)
		} else {
			srv.SetModelAliases(models.Aliases)
			lg.Printf("模型表 %d 个键，别名 %d 条：%s",
				models.Keys, len(models.Aliases), formatAliases(models.Aliases))
			if models.DefaultAgentModelID != "" {
				lg.Printf("上游默认模型 %s", models.DefaultAgentModelID)
			}
		}
	}
	lg.Printf("监听 %s → %s", *listen, srv.Upstream())
	lg.Printf("信封改写 %s，User-Agent %s",
		map[bool]string{true: "开", false: "关（纯透传）"}[srv.Envelope()], *ua)
	lg.Printf("账号 %s (%s)，token 过期 %s", first.Name, first.Email, fmtExpiry(first.Expiry))
	if n := len(cands); n > 1 {
		lg.Printf("池中候选 %d 个：%s", n, joinNames(sel.Candidates()))
	}
	lg.Printf("接入: AGY_GATEWAY_URL=http://%s agy --print 'hi'", *listen)

	// 额度检测：启动先同步跑一轮，让第一个请求就已经知道哪些号没额度；
	// 之后交给后台按 interval 轮询。首轮失败只告警，不拦启动 ——
	// 额度接口抖动不该让代理起不来。
	if *quotaInterval > 0 {
		qw := newQuotaWatcher(sel, *upstream, srv.UserAgentForUpstream(), *quotaThreshold, lg)
		adapter.on429 = func(name string) {
			qw.triggerCheck()
		}
		qw.check(ctx, true)
		go qw.run(ctx, *quotaInterval)
		lg.Printf("额度检测 每 %s 查一次 GEMINI 组（阈值 %.3f）", *quotaInterval, *quotaThreshold)
	} else {
		lg.Printf("额度检测 已关闭（-quota-interval=0），不会自动换号")
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout / ReadTimeout：SSE 长流可能持续数分钟，
		// 任何全局超时都会把它拦腰砍断。
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		lg.Printf("收到退出信号，优雅关闭…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("关闭失败: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// selectorPicker 把 pool.Selector 适配成 proxy.Picker 与 proxy.StatusReporter。
type selectorPicker struct {
	sel     *pool.Selector
	verbose bool
	log     *log.Logger
	on429   func(name string)
}

func (p *selectorPicker) ReportStatus(name string, statusCode int) {
	if statusCode == http.StatusTooManyRequests {
		if p.log != nil {
			p.log.Printf("上游返回 429，账号 %s 进入临时冷却 (1 分钟)", name)
		}
		p.sel.SetCooldown(name, time.Now().Add(time.Minute))
		if p.on429 != nil {
			p.on429(name)
		}
	}
}

func (p *selectorPicker) Pick(ctx context.Context) (*proxy.Account, error) {
	a, err := p.sel.Pick(ctx)
	if err != nil {
		return nil, err
	}
	if p.verbose && p.log != nil {
		p.log.Printf("选号 %s (%s) 过期 %s", a.Name, a.Email, fmtExpiry(a.Expiry))
	}
	return &proxy.Account{Name: a.Name, Email: a.Email, AccessToken: a.AccessToken}, nil
}

// resolveCandidates 决定反向代理的候选账号集合：
//   - name 非空：显式使用指定账号
//   - name 为空：使用账号池中全部可用账号（支持自动轮转），若池为空则回落至当前系统 Keyring 凭据
func resolveCandidates(name string) ([]*pool.Account, error) {
	if name != "" {
		a, err := pool.Load(name)
		if err != nil {
			return nil, fmt.Errorf("加载账号 %q 失败: %w", name, err)
		}
		return []*pool.Account{a}, nil
	}

	accounts, err := pool.List()
	if err != nil {
		return nil, err
	}
	if len(accounts) > 0 {
		return accounts, nil
	}

	// 池为空：直接用 keyring 里那一份，让链路先跑通（只读，不写 keyring）。
	sec, email, err := keyringCurrentAdapter()
	if err != nil {
		return nil, fmt.Errorf("账号池为空，且读取 keyring 失败: %w", err)
	}
	claims, claimsErr := sec.Claims()
	if claimsErr != nil {
		return nil, fmt.Errorf("解析身份失败: %w", claimsErr)
	}
	return []*pool.Account{{
		Name:         "keyring",
		Email:        email,
		ClientID:     claims.Audience(),
		AccessToken:  sec.Token.AccessToken,
		RefreshToken: sec.Token.RefreshToken,
		Expiry:       sec.Token.Expiry.Time,
		AuthMethod:   sec.AuthMethod,
	}}, nil
}

// resolverMap 是给日志用的：别名表可能有几十条，全打出来会把启动日志淹掉。
func formatAliases(m map[string]string) string {
	if len(m) == 0 {
		return "(空)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("→")
		b.WriteString(m[k])
		if i >= 7 && len(keys) > 9 {
			b.WriteString(fmt.Sprintf(" …(共 %d 条)", len(keys)))
			break
		}
	}
	return b.String()
}

// refreshAccount 刷新账号的 access_token 并落盘（keyring 那份是临时态，不落盘）。
// 这是「每号只登一次，之后永久免登」的兑现点。
func refreshAccount(ctx context.Context, a *pool.Account) error {
	if a.RefreshToken == "" {
		return errors.New("该账号没有 refresh_token")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}

	res, err := token.Refresh(ctx, a.ClientID, a.RefreshToken)
	if err != nil {
		return err
	}
	a.AccessToken = res.AccessToken
	a.Expiry = res.Expiry().UTC()
	if res.RefreshToken != "" {
		a.RefreshToken = res.RefreshToken
	}
	if a.Name == "keyring" {
		return nil
	}
	return pool.Save(a)
}

func joinNames(cands []*pool.Account) string {
	names := make([]string, 0, len(cands))
	for _, a := range cands {
		names = append(names, a.Name)
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
