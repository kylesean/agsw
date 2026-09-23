package cloudcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sampleResponse 与实测上游同构（字段取自真实 164962 字节响应的顶层）。
//
// 特意保留 gemini-3.6-flash 与 gemini-3.6-flash-tiered 并存：
// 这是「plain 已是真实键」的反例，用来钉住不许覆盖的规则。
const sampleResponse = `{
  "defaultAgentModelId": "gemini-3.8-flash-tiered",
  "deprecatedModelIds": {
    "gemini-3.1-pro-high": {"newModelId": "gemini-pro-agent",
                             "oldModelEnum": "MODEL_PLACEHOLDER_M37",
                             "newModelEnum": "MODEL_PLACEHOLDER_M16"}
  },
  "models": {
    "gemini-3.8-flash-tiered": {"model": "MODEL_PLACEHOLDER_M322"},
    "gemini-3.7-flash-tiered": {"model": "MODEL_PLACEHOLDER_M301"},
    "gemini-3.6-flash-tiered": {"model": "MODEL_PLACEHOLDER_M196"},
    "gemini-3.1-flash-lite":   {"model": "MODEL_PLACEHOLDER_M50"},
    "gemini-3.6-flash":        {"model": "MODEL_EXISTS_AS_PLAIN"}
  },
  "experimentIds": [1, 2]
}`

func mustParseSample(t *testing.T) *fetchAvailableModelsResponse {
	t.Helper()
	var r fetchAvailableModelsResponse
	if err := json.Unmarshal([]byte(sampleResponse), &r); err != nil {
		t.Fatalf("解析样例失败: %v", err)
	}
	return &r
}

// TestBuildModelsDerivesTieredAliases 是这层存在的全部理由：
// agy 发 gemini-3.8-flash，上游只有 gemini-3.8-flash-tiered，不映射就是 404。
func TestBuildModelsDerivesTieredAliases(t *testing.T) {
	m := buildModels(mustParseSample(t))

	want := map[string]string{
		"gemini-3.8-flash":    "gemini-3.8-flash-tiered",
		"gemini-3.7-flash":    "gemini-3.7-flash-tiered",
		"gemini-3.1-pro-high": "gemini-pro-agent", // 官方重命名表
	}
	if len(m.Aliases) != len(want) {
		t.Errorf("别名数 = %d, 想要 %d；got=%v", len(m.Aliases), len(want), m.Aliases)
	}
	for from, to := range want {
		if got := m.Aliases[from]; got != to {
			t.Errorf("别名 %s = %q, 想要 %q", from, got, to)
		}
	}
	if m.Keys != 5 {
		t.Errorf("Keys = %d, 想要 5", m.Keys)
	}
	if m.DefaultAgentModelID != "gemini-3.8-flash-tiered" {
		t.Errorf("DefaultAgentModelID = %q", m.DefaultAgentModelID)
	}
}

// TestBuildModelsDoesNotShadowRealKeys 是最关键的防线：
// 别名覆盖一个真实存在的键，等于悄悄把请求换到别的模型 ——
// 那比报 404 更糟，因为用户不会察觉自己用的不是指定的模型。
func TestBuildModelsDoesNotShadowRealKeys(t *testing.T) {
	m := buildModels(mustParseSample(t))

	if _, ok := m.Aliases["gemini-3.6-flash"]; ok {
		t.Errorf("gemini-3.6-flash 是上游真实键，不该被改写成 %q",
			m.Aliases["gemini-3.6-flash"])
	}
	if _, ok := m.Aliases["gemini-3.1-flash-lite"]; ok {
		t.Error("普通键不该出现在别名表里")
	}
}

func TestBuildModelsEmptyInputs(t *testing.T) {
	m := buildModels(&fetchAvailableModelsResponse{})
	if len(m.Aliases) != 0 {
		t.Errorf("空响应不该有别名: %v", m.Aliases)
	}
	if m.Keys != 0 {
		t.Errorf("Keys = %d", m.Keys)
	}
}

func TestBuildModelsSkipsDegenerateEntries(t *testing.T) {
	r := &fetchAvailableModelsResponse{
		DeprecatedModelIDs: map[string]struct {
			NewModelID string `json:"newModelId"`
		}{
			"":      {NewModelID: "x"}, // 空 old：没有可替换的来源
			"keep":  {NewModelID: ""},  // 空 new：改写成空等于删模型
			"valid": {NewModelID: "real"},
		},
		Models: map[string]json.RawMessage{},
	}
	m := buildModels(r)
	if len(m.Aliases) != 1 || m.Aliases["valid"] != "real" {
		t.Errorf("别名 = %v, 只该留 valid→real", m.Aliases)
	}
}

// TestBuildModelsDeprecatedWinsOverTiered：两条规则撞车时，
// 官方重命名表优先 —— 而且它必须无条件生效。
//
// 实测上游正是这种形状：gemini-3.1-pro-high 同时出现在 models
// 与 deprecatedModelIds 里（旧键仍列着但已被标记重命名），
// 所以「old 是真实键就跳过」这条保护**不能**套在官方规则上，
// 否则整张重命名表会被自己的保护废掉。
// 「不许覆盖真实键」只用于我们自己推的 -tiered 规则。
func TestBuildModelsDeprecatedWinsOverTiered(t *testing.T) {
	r := &fetchAvailableModelsResponse{
		DeprecatedModelIDs: map[string]struct {
			NewModelID string `json:"newModelId"`
		}{"x": {NewModelID: "official"}},
		Models: map[string]json.RawMessage{
			"x-tiered": []byte(`{}`),
			"official": []byte(`{}`),
			"x":        []byte(`{}`), // 旧键仍在列表里 —— 实测就是如此
		},
	}
	m := buildModels(r)
	if got := m.Aliases["x"]; got != "official" {
		t.Errorf("官方重命名没生效: x = %q, 想要 official", got)
	}
	// x 已由官方表接管，-tiered 推导不该再插一脚。
	if len(m.Aliases) != 1 {
		t.Errorf("别名 = %v, 只该有 x→official", m.Aliases)
	}
}

// TestFetchModelsSendsGateHeaders：缺了这两个头上游就 403，
// 客户端必须自己带上，不能指望调用方记得。
func TestFetchModelsSendsGateHeaders(t *testing.T) {
	var gotUA, gotAuth, gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleResponse)
	}))
	t.Cleanup(up.Close)

	m, err := FetchModels(context.Background(), up.URL, "TOKEN", "antigravity-cli/1.2.9")
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotUA != "antigravity-cli/1.2.9" {
		t.Errorf("User-Agent = %q，缺了许可证闸门", gotUA)
	}
	if gotAuth != "Bearer TOKEN" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPath != MethodFetchAvailableModels {
		t.Errorf("Path = %q, 想要 %q", gotPath, MethodFetchAvailableModels)
	}
	if gotBody != "{}" {
		t.Errorf("body = %q, 想要 {}", gotBody)
	}
	if m.Aliases["gemini-3.8-flash"] != "gemini-3.8-flash-tiered" {
		t.Errorf("别名没推出来: %v", m.Aliases)
	}
}

func TestFetchModelsTrailingSlashIsTolerated(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, sampleResponse)
	}))
	t.Cleanup(up.Close)

	if _, err := FetchModels(context.Background(), up.URL+"/", "T", "ua"); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotPath != MethodFetchAvailableModels {
		t.Errorf("尾斜杠导致路径拼错: %q", gotPath)
	}
}

func TestFetchModelsSurfacesUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// 故意写得像被截断的长页面，考验 clip 不把日志冲爆。
		_, _ = io.WriteString(w, `{"error":{"message":"`+strings.Repeat("x", 5000)+`"}}`)
	}))
	t.Cleanup(up.Close)

	_, err := FetchModels(context.Background(), up.URL, "T", "ua")
	if err == nil {
		t.Fatal("上游 403 应当报错，而不是返回空别名表")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误里该有状态码: %v", err)
	}
	if len(err.Error()) > 900 {
		t.Errorf("错误信息没被截断，长度 %d", len(err.Error()))
	}
}

func TestFetchModelsRejectsEmptyToken(t *testing.T) {
	if _, err := FetchModels(context.Background(), "http://127.0.0.1:1", "", "ua"); err == nil {
		t.Error("空 token 应当立刻报错，别去发一次注定失败的请求")
	}
}
