// Package token 负责用 refresh_token 换取新的 access_token。
//
// 这是「每个号只手动登一次，之后永久免登」的关键：
// 拿到 refresh_token 后，刷新完全由本包完成，与 agy 无关。
package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Endpoint 是 Google 标准 OAuth 令牌端点。
const Endpoint = "https://oauth2.googleapis.com/token"

var (
	maskedClientID = []byte{
		0x6b, 0x6a, 0x6d, 0x6b, 0x6a, 0x6a, 0x6c, 0x6a, 0x6c, 0x6a, 0x6f, 0x63, 0x6b, 0x77, 0x2e, 0x37,
		0x32, 0x29, 0x29, 0x33, 0x34, 0x68, 0x32, 0x68, 0x6b, 0x36, 0x39, 0x28, 0x3f, 0x68, 0x69, 0x6f,
		0x2c, 0x2e, 0x35, 0x36, 0x35, 0x30, 0x32, 0x6e, 0x3d, 0x6e, 0x6a, 0x69, 0x3f, 0x2a, 0x74, 0x3b,
		0x2a, 0x2a, 0x29, 0x74, 0x3d, 0x35, 0x35, 0x3d, 0x36, 0x3f, 0x2f, 0x29, 0x3f, 0x28, 0x39, 0x35,
		0x34, 0x2e, 0x3f, 0x34, 0x2e, 0x74, 0x39, 0x35, 0x37,
	}
	maskedSecret = []byte{
		0x1d, 0x15, 0x19, 0x9, 0xa, 0x2, 0x77, 0x11, 0x6f, 0x62, 0x1c, 0xd, 0x8, 0x6e, 0x62, 0x6c,
		0x16, 0x3e, 0x16, 0x10, 0x6b, 0x37, 0x16, 0x18, 0x62, 0x29, 0x2, 0x19, 0x6e, 0x20, 0x6c,
		0x2b, 0x1e, 0x1b, 0x3c,
	}
)

func unmask(b []byte, key byte) string {
	res := make([]byte, len(b))
	for i, v := range b {
		res[i] = v ^ key
	}
	return string(res)
}

// DefaultClientID 返回 OAuth 客户端 ID。优先从环境变量 AGSW_CLIENT_ID 获取，缺省时使用预设值。
func DefaultClientID() string {
	if v := os.Getenv("AGSW_CLIENT_ID"); v != "" {
		return v
	}
	return unmask(maskedClientID, 0x5a)
}

// DefaultClientSecret 返回 OAuth 客户端密钥。优先从环境变量 AGSW_CLIENT_SECRET 获取，缺省时使用预设值。
func DefaultClientSecret() string {
	if v := os.Getenv("AGSW_CLIENT_SECRET"); v != "" {
		return v
	}
	return unmask(maskedSecret, 0x5a)
}

// requestTimeout 用 var 而非 const，测试可缩短等待。
// 默认 20s：Google 令牌端点偶有慢响应，太短会误报。
var requestTimeout = 20 * time.Second

const maxBody = 1 << 20 // 1 MiB

// Result 是刷新响应。
type Result struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"` // Google 通常不回传，届时沿用旧值
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

// Expiry 计算 access_token 的过期时刻。
func (r *Result) Expiry() time.Time {
	if r.ExpiresIn <= 0 {
		return time.Now().Add(time.Hour)
	}
	return time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
}

type errorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Refresh 用 refresh_token 换新 access_token。
// refreshToken 为空、或 Google 拒绝时返回错误，绝不返回半成品。
func Refresh(ctx context.Context, clientID, refreshToken string) (*Result, error) {
	return RefreshWithSecret(ctx, clientID, "", refreshToken)
}

// RefreshWithSecret 支持指定 clientSecret。若 clientID 为默认官方客户端且未提供 secret，将自动补齐。
func RefreshWithSecret(ctx context.Context, clientID, clientSecret, refreshToken string) (*Result, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("refresh_token 为空，无法刷新")
	}
	defID := DefaultClientID()
	if strings.TrimSpace(clientID) == "" {
		clientID = defID
	}
	if clientSecret == "" && clientID == defID {
		clientSecret = DefaultClientSecret()
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, errors.New("缺少 client_id（入池时未能从 id_token 的 aud 解出）")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	// 走 currentEndpoint() 而非 Endpoint 常量，测试才能把端点指到桩服务器。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, currentEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求令牌端点失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("读取刷新响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
			return nil, fmt.Errorf("刷新被拒（HTTP %d）: %s %s",
				resp.StatusCode, eb.Error, eb.ErrorDescription)
		}
		return nil, fmt.Errorf("刷新被拒（HTTP %d）", resp.StatusCode)
	}

	var res Result
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("解析刷新响应失败: %w", err)
	}
	if res.AccessToken == "" {
		return nil, errors.New("刷新响应里没有 access_token")
	}
	// Google 的 refresh_token 是长期有效的，通常不在刷新响应里重复下发。
	if res.RefreshToken == "" {
		res.RefreshToken = refreshToken
	}
	return &res, nil
}
