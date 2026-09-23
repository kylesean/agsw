// Package oauth 实现独立的 OAuth2 授权码流程（带 PKCE 与本地回环回调）。
// 登录成功后直接产出包含 refresh_token 的完整凭据，存入账号池。
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kylesean/agsw/internal/token"
)

// DefaultClientID 返回内置的客户端 ID。
func DefaultClientID() string {
	return token.DefaultClientID()
}

// DefaultClientSecret 返回内置的客户端密钥。
func DefaultClientSecret() string {
	return token.DefaultClientSecret()
}

const (
	// 授权端点 / 令牌端点 / userinfo（id_token 缺 email 时的兜底）。
	AuthEndpoint        = "https://accounts.google.com/o/oauth2/auth"
	TokenEndpoint       = "https://oauth2.googleapis.com/token"
	UserinfoEndpoint    = "https://www.googleapis.com/oauth2/v2/userinfo"
	DefaultRedirectPath = "/auth/callback"

	// AuthMethod 与 agy 写进 keyring 的取值一致（同 client、同 scope，故同 auth 方法）。
	AuthMethod = "consumer"
)

// DefaultScopes 是 Google OAuth 授权所需完整的 Scope 列表。
var DefaultScopes = []string{
	"openid",
	"email",
	"profile",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/aicode",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// Config 是一次登录涉及的全部端点与身份。零值字段会被 withDefaults 补齐。
type Config struct {
	ClientID         string
	ClientSecret     string
	AuthEndpoint     string
	TokenEndpoint    string
	UserinfoEndpoint string
	RedirectPath     string
	Scopes           []string
}

func (c Config) withDefaults() Config {
	if c.ClientID == "" {
		c.ClientID = DefaultClientID()
	}
	if c.ClientSecret == "" {
		c.ClientSecret = DefaultClientSecret()
	}
	if c.AuthEndpoint == "" {
		c.AuthEndpoint = AuthEndpoint
	}
	if c.TokenEndpoint == "" {
		c.TokenEndpoint = TokenEndpoint
	}
	if c.UserinfoEndpoint == "" {
		c.UserinfoEndpoint = UserinfoEndpoint
	}
	if c.RedirectPath == "" {
		c.RedirectPath = DefaultRedirectPath
	}
	if len(c.Scopes) == 0 {
		c.Scopes = DefaultScopes
	}
	return c
}

func (c Config) validate() error {
	var bad []string
	if c.ClientID == "" {
		bad = append(bad, "client_id")
	}
	if c.ClientSecret == "" {
		bad = append(bad, "client_secret")
	}
	if c.AuthEndpoint == "" {
		bad = append(bad, "授权端点")
	}
	if c.TokenEndpoint == "" {
		bad = append(bad, "令牌端点")
	}
	if !strings.HasPrefix(c.RedirectPath, "/") {
		bad = append(bad, "回调路径（须以 / 开头）")
	}
	if len(c.Scopes) == 0 {
		bad = append(bad, "scope")
	}
	if len(bad) > 0 {
		return fmt.Errorf("OAuth 配置缺项: %s", strings.Join(bad, "、"))
	}
	return nil
}

// Options 控制单次登录的交互行为。
type Options struct {
	// Timeout 等待浏览器完成登录的上限，0 表示用 DefaultTimeout。
	Timeout time.Duration
	// NoBrowser 只打印授权链接、不拉起浏览器（SSH / 无头场景）。
	NoBrowser bool
	// LoginHint 预填的 Google 邮箱，可空。
	LoginHint string
	// PrintAuthURL 接收完整授权链接，默认打到 stdout。测试用它把链接截下来。
	PrintAuthURL func(string)
}

// DefaultTimeout 覆盖「输密码 + 二次验证 + 同意页」的正常耗时；
// 卡在某个页没点完时按 Ctrl-C 即可，不必干等。
const DefaultTimeout = 5 * time.Minute

// Result 是登录成功后的完整凭据，字段与池文件对齐。
type Result struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Scope        string
	Email        string
	Expiry       time.Time
}

// Login 走完「起本地端口 → 拉浏览器 → 收 code → 换 token → 取邮箱」全流程。
// 任一步失败都返回 error，绝不返回半成品凭据。
func Login(ctx context.Context, cfg Config, opts Options) (*Result, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 绑 127.0.0.1 而非 :0 —— 回调带 code，绝不能开给别的网卡；
	// 用 127.0.0.1 而非 localhost 免去浏览器先试 ::1 连不上的坑。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("监听本地回调端口失败: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, cfg.RedirectPath)

	state, err := randomToken(32)
	if err != nil {
		ln.Close()
		return nil, err
	}
	verifier, err := randomToken(64)
	if err != nil {
		ln.Close()
		return nil, err
	}

	authURL := buildAuthURL(cfg, redirectURI, state, pkceChallenge(verifier), opts.LoginHint)
	printAuthURL(authURL, opts.PrintAuthURL)
	if !opts.NoBrowser {
		openBrowser(authURL) // 打不开就拉倒，链接已经打印了
	}

	code, err := waitCode(ctx, ln, cfg.RedirectPath, state, timeout)
	if err != nil {
		return nil, err
	}

	tres, err := exchangeCode(ctx, cfg, code, redirectURI, verifier)
	if err != nil {
		return nil, err
	}

	email, err := resolveEmail(ctx, cfg, tres)
	if err != nil {
		return nil, err
	}

	return &Result{
		AccessToken:  tres.AccessToken,
		RefreshToken: tres.RefreshToken,
		IDToken:      tres.IDToken,
		TokenType:    tres.TokenType,
		Scope:        tres.Scope,
		Email:        email,
		Expiry:       tres.Expiry(),
	}, nil
}

// buildAuthURL 拼授权链接。
// access_type=offline 是拿 refresh_token 的前提，prompt=consent 保证每次重新下发 ——
// 没有 consent 时 Google 只在首次授权给 refresh_token，第二次登录就等于白登。
func buildAuthURL(cfg Config, redirectURI, state, challenge, loginHint string) string {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(cfg.Scopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if loginHint != "" {
		v.Set("login_hint", loginHint)
	}
	return cfg.AuthEndpoint + "?" + v.Encode()
}

func printAuthURL(u string, fn func(string)) {
	if fn != nil {
		fn(u)
		return
	}
	fmt.Println("请在浏览器中打开并完成登录：")
	fmt.Println(u)
}

// browserCommandForOS 根据操作系统返回打开 URL 的命令对象。
func browserCommandForOS(goos, u string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", u)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		return exec.Command("xdg-open", u)
	}
}

// openBrowser 尽力拉起默认浏览器。URL 仅作为命令参数传递，避免 Shell 注入风险。
func openBrowser(u string) {
	cmd := browserCommandForOS(runtime.GOOS, u)
	if err := cmd.Start(); err != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}

// waitCode 等浏览器把 code 送回本地端口。
func waitCode(ctx context.Context, ln net.Listener, path, wantState string, timeout time.Duration) (string, error) {
	ch := make(chan authResult, 1) // 缓冲 1：第一次回调定胜负，后续重放进不来
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		q := r.URL.Query()

		// Google 把拒绝理由放在 error 参数里（用户点「取消」= access_denied）。
		if e := q.Get("error"); e != "" {
			msg := fmt.Sprintf("Google 拒绝了登录: %s", e)
			if d := q.Get("error_description"); d != "" {
				msg += " " + d
			}
			http.Error(w, msg, http.StatusBadRequest)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case ch <- authResult{err: errors.New(msg)}:
			default:
			}
			return
		}

		// state 是这次登录唯一的 CSRF 凭据，对不上一律不认。
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(wantState)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case ch <- authResult{err: errors.New("回调 state 不匹配（伪造回调或页面重放），已拒绝")}:
			default:
			}
			return
		}

		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case ch <- authResult{err: errors.New("回调里没有 code")}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, callbackOKHTML)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case ch <- authResult{code: code}:
		default:
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return r.code, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("等待浏览器登录超时（%s），可加大 -timeout 或按 Ctrl-C 中止", timeout)
		}
		return "", fmt.Errorf("登录已中断: %w", ctx.Err())
	}
}

type authResult struct {
	code string
	err  error
}

const callbackOKHTML = `<!doctype html>
<meta charset="utf-8">
<title>agsw 登录完成</title>
<style>body{font:16px/1.6 system-ui,sans-serif;max-width:32em;margin:4em auto;padding:0 1em;color:#222}
.ok{color:#0a7a3d;font-size:2em}</style>
<p class="ok">✓ 登录完成</p>
<p>凭据已交给 agsw，可以关掉这个标签页、回到终端了。</p>
`

// exchangeCode 用授权码换 token。
// redirect_uri 必须与授权请求里那个逐字相同，否则 Google 回 redirect_uri_mismatch。
func exchangeCode(ctx context.Context, cfg Config, code, redirectURI, verifier string) (*token.Result, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造令牌请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req) // DefaultClient 走 ProxyFromEnvironment，HTTPS_PROXY 生效
	if err != nil {
		return nil, fmt.Errorf("请求令牌端点失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取令牌响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("换 token 被拒（HTTP %d）: %s", resp.StatusCode, describeErrorBody(body))
	}

	var res token.Result
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("解析令牌响应失败: %w", err)
	}
	if res.AccessToken == "" {
		return nil, errors.New("令牌响应里没有 access_token")
	}
	if res.RefreshToken == "" {
		// 这是致命的：没有 refresh_token 就做不到「登一次永久免登」。
		return nil, errors.New("Google 没下发 refresh_token —— 授权未带 prompt=consent 或账号策略禁用了离线访问")
	}
	return &res, nil
}

func describeErrorBody(body []byte) string {
	var eb struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
		if eb.ErrorDescription != "" {
			return eb.Error + " " + eb.ErrorDescription
		}
		return eb.Error
	}
	return strings.TrimSpace(string(body))
}

// resolveEmail 优先取 id_token 的 email claim，缺了再回落 userinfo。
func resolveEmail(ctx context.Context, cfg Config, res *token.Result) (string, error) {
	if res.IDToken != "" {
		email, err, fallback := emailFromIDToken(res.IDToken, cfg.ClientID)
		if !fallback {
			if err != nil {
				return "", err
			}
			return email, nil
		}
		// id_token 里没有 email claim —— 落到 userinfo。
	}
	return emailFromUserinfo(ctx, cfg, res.AccessToken)
}

// emailFromIDToken 解出邮箱并核对 aud。
//
// 不验签名是刻意的：id_token 是我们自己刚从 Google 的 TLS 通道换来的，不是外来输入；
// 但 aud 必须核对，否则拿到别人 client 的 id_token 也会当真。
// fallback=true 表示「可用但缺 email」，调用方走 userinfo 兜底。
func emailFromIDToken(idToken, wantAud string) (email string, err error, fallback bool) {
	// 标准 JWT 是 三段（header.payload.signature）；我们只要 payload 里的 claim。
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("id_token 格式异常"), false
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
	if err != nil {
		return "", fmt.Errorf("解析 id_token 失败: %w", err), false
	}
	var claims struct {
		Aud           any    `json:"aud"`
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("解析 id_token claim 失败: %w", err), false
	}
	if !audMatches(claims.Aud, wantAud) {
		return "", fmt.Errorf("id_token 的 aud 与预期 client 不符，已拒绝"), false
	}
	if claims.Email == "" {
		return "", nil, true
	}
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		return "", fmt.Errorf("Google 标记该邮箱未验证，已拒绝入池"), false
	}
	return claims.Email, nil, false
}

// aud 在 id_token 里既可能是字符串也可能是字符串数组（RFC 7519 都合法）。
func audMatches(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return subtle.ConstantTimeCompare([]byte(v), []byte(want)) == 1
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && subtle.ConstantTimeCompare([]byte(s), []byte(want)) == 1 {
				return true
			}
		}
	}
	return false
}

func emailFromUserinfo(ctx context.Context, cfg Config, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.UserinfoEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("构造 userinfo 请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 userinfo 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取 userinfo 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo 被拒（HTTP %d）: %s", resp.StatusCode, describeErrorBody(body))
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("解析 userinfo 响应失败: %w", err)
	}
	if info.Email == "" {
		return "", errors.New("userinfo 里没有 email，无法确定账号身份")
	}
	return info.Email, nil
}

// randomToken 生成密码学随机的 base64url 串（无填充）。
func randomToken(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机数失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge = BASE64URL-ENCODE(SHA256(ASCII(verifier)))，RFC 7636 §4.2。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
