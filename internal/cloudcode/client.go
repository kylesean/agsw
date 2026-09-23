// cloudcode 是 daily-cloudcode-pa.googleapis.com 的最小客户端。
//
// 目前只做一件事：拉 fetchAvailableModels 并推导出「agy 网关模式的模型名 →
// 上游真实模型键」的别名表。
//
// 为什么需要它（实测）：
//
//	agy 网关模式发  /v1beta/models/gemini-3.8-flash:streamGenerateContent
//	上游只有键     gemini-3.8-flash-tiered
//	直接转过去      → 404 NOT_FOUND「Requested entity was not found.」
//
// 上游这份响应里没有现成的「plain 名 → tiered 名」映射，但两条规则都能推：
//
//  1. deprecatedModelIds 是官方重命名表（gemini-3.1-pro-high → gemini-pro-agent）
//  2. 以 -tiered 结尾的键，去掉后缀若不与已有键冲突，就是那个 plain 名的别名
//
// 这与 native agy 的做法同源：二进制里存在
// FetchAvailableModelsResponse 的 cache，以及 ResolveModel / resolveModelName。
package cloudcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MethodFetchAvailableModels 是取模型表的方法路径。
const MethodFetchAvailableModels = "/v1internal:fetchAvailableModels"

// maxBody 限制读入大小。实测完整响应 164962 字节，留足余量，
// 但上游真要回一整个页面也不会把内存吃光。
const maxBody = 8 << 20

// Models 是 fetchAvailableModels 的解析结果里我们关心的部分。
type Models struct {
	// Aliases 是「旧名/别名 → 上游真实键」，例如
	// gemini-3.8-flash → gemini-3.8-flash-tiered。
	Aliases map[string]string
	// DefaultAgentModelID 是上游声明的默认模型键（实测 23 字节 =
	// "gemini-3.8-flash-tiered"），用来在启动日志里交叉验证别名推得对不对。
	DefaultAgentModelID string
	// Keys 是上游模型键总数，用于启动日志。
	Keys int
}

type fetchAvailableModelsResponse struct {
	DefaultAgentModelID string                     `json:"defaultAgentModelId"`
	Models              map[string]json.RawMessage `json:"models"`
	DeprecatedModelIDs  map[string]struct {
		NewModelID string `json:"newModelId"`
	} `json:"deprecatedModelIds"`
}

// FetchModels 取回模型表并推导别名。upstream 必须带 scheme 与 host。
//
// 失败时返回 error 而不是空表：调用方（serve）把它当启动健康检查用 ——
// 拉不到模型表通常意味着 token、User-Agent 或上游三者有一个不对，
// 那时与其监听一个注定 404 的端口，不如立刻报出来。
func FetchModels(ctx context.Context, upstream, accessToken, userAgent string) (*Models, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("access_token 为空")
	}
	if strings.HasSuffix(upstream, "/") {
		upstream = strings.TrimRight(upstream, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		upstream+MethodFetchAvailableModels, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	// 许可证闸门：不带这个 UA 上游回 403，见 internal/proxy/upstreamUserAgent。
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", MethodFetchAvailableModels, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("读响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游 %d: %s", resp.StatusCode, clip(body, 512))
	}

	var parsed fetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型表失败: %w", err)
	}
	return buildModels(&parsed), nil
}

// buildModels 从解析结果推导别名表。拆出来是为了能用纯数据做单测。
//
// 两条规则，作用域不同：
//
//  1. 官方重命名表 deprecatedModelIds：old → newModelId，**无条件**生效。
//     实测旧键往往仍列在 models 里（如 gemini-3.1-pro-high），那正是
//     「已弃用但还在列表」的形状，按「old 存在就跳过」会把整张表废掉。
//  2. -tiered 推导：gemini-3.8-flash-tiered 的 plain 名 gemini-3.8-flash
//     只有在上游**没有**同名键时才认作别名。这条是我们自己推的，标准要更严：
//     覆盖一个真实键会把请求悄悄换到别的模型，那比报 404 更糟。
//
// 先跑官方表，再跑推导；推导不覆盖已有别名（先到先得）。
func buildModels(r *fetchAvailableModelsResponse) *Models {
	aliases := make(map[string]string)

	// 规则 1：官方重命名，无条件。
	for old, entry := range r.DeprecatedModelIDs {
		if entry.NewModelID == "" || old == "" {
			continue
		}
		aliases[old] = entry.NewModelID
	}

	// 规则 2：-tiered 推导，且不得覆盖真实键或已有别名。
	for key := range r.Models {
		if !strings.HasSuffix(key, "-tiered") {
			continue
		}
		plain := strings.TrimSuffix(key, "-tiered")
		if plain == "" {
			continue
		}
		if _, exists := r.Models[plain]; exists {
			continue // plain 是真实键，不能覆盖
		}
		if _, taken := aliases[plain]; taken {
			continue // 官方表已接管
		}
		aliases[plain] = key
	}

	return &Models{
		Aliases:             aliases,
		DefaultAgentModelID: r.DefaultAgentModelID,
		Keys:                len(r.Models),
	}
}

// clip 截断并压平超长文本，避免把上游整页错误写进日志。
func clip(b []byte, limit int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
