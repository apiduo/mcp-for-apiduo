package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/config"
)

const testServiceKey = "sk-service-test-key"

// newTestHandler 构造纯内存模式的管理端：不落盘、不轮询。
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	t.Setenv("API_KEY", testServiceKey)
	store, err := config.NewStore("", 0)
	if err != nil {
		t.Fatalf("构造 config.Store 失败: %v", err)
	}
	return New(store)
}

// newFileHandler 构造落盘模式的管理端，返回 handler 与自增配置路径。
func newFileHandler(t *testing.T, seed string) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if seed == "" {
		seed = "base_url: http://127.0.0.1:3000/v1\napi_key: " + testServiceKey + "\n"
	}
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("API_KEY", testServiceKey)
	store, err := config.NewStore(path, 0)
	if err != nil {
		t.Fatalf("构造 config.Store 失败: %v", err)
	}
	return New(store), path
}

func testMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func do(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testServiceKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func driftRequest(mux http.Handler, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/drift", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testServiceKey)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

type driftResponse struct {
	UpstreamCount int            `json:"upstream_count"`
	Configured    []string       `json:"configured"`
	Unconfigured  []string       `json:"unconfigured"`
	Orphaned      []string       `json:"orphaned"`
	Disabled      []string       `json:"disabled"`
	Counts        map[string]int `json:"counts"`
}

// 漂移端点必须走服务身份鉴权，且输出与运行时归一化结果一致。
func TestDriftEndpoint(t *testing.T) {
	h := newTestHandler(t)
	if _, err := h.store.ReplaceOverlays(catalog.Overlays{
		Models: map[string]map[string]any{
			"img-a":           {"kind": "image"},
			"gone-old-260915": {"kind": "image"}, // 上游换版后遗留的孤儿条目
		},
		Settings: catalog.Settings{Defaults: map[string]string{}},
		Disabled: []string{"vid-b", "gone-disabled"},
	}); err != nil {
		t.Fatal(err)
	}
	h.Models = func(_ context.Context, key string) ([]catalog.Model, error) {
		if key != testServiceKey {
			t.Errorf("传给上游的 key 不对: %q", key)
		}
		return []catalog.Model{
			{"id": "img-a", "kind": "image"},
			{"id": "vid-b", "kind": "video"},
			{"id": "llm-c", "domain": "LLM"},
		}, nil
	}
	mux := testMux(h)

	if w := driftRequest(mux, false); w.Code != http.StatusUnauthorized {
		t.Errorf("无鉴权应 401，实际 %d", w.Code)
	}

	w := driftRequest(mux, true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got driftResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.UpstreamCount != 3 {
		t.Errorf("upstream_count = %d", got.UpstreamCount)
	}
	if !slices.Equal(got.Configured, []string{"img-a"}) {
		t.Errorf("configured = %v", got.Configured)
	}
	if !slices.Equal(got.Unconfigured, []string{"vid-b"}) {
		t.Errorf("unconfigured = %v", got.Unconfigured)
	}
	// 未配置与已下架是独立维度，vid-b 可以同时出现在两栏。
	if !slices.Equal(got.Orphaned, []string{"gone-disabled", "gone-old-260915"}) {
		t.Errorf("orphaned = %v", got.Orphaned)
	}
	if !slices.Equal(got.Disabled, []string{"vid-b"}) {
		t.Errorf("disabled = %v", got.Disabled)
	}
	if got.Counts["orphaned"] != 2 || got.Counts["unconfigured"] != 1 || got.Counts["disabled"] != 1 {
		t.Errorf("counts = %v", got.Counts)
	}
}

// 上游不可用时如实报错，不拿缓存冒充事实；错误里不能泄露服务 key。
func TestDriftEndpointUpstreamError(t *testing.T) {
	h := newTestHandler(t)
	h.Models = func(_ context.Context, _ string) ([]catalog.Model, error) {
		return nil, errors.New("upstream 401: echoed " + testServiceKey)
	}
	w := driftRequest(testMux(h), true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("期望 502，实际 %d", w.Code)
	}
	if strings.Contains(w.Body.String(), testServiceKey) {
		t.Error("错误信息泄露了服务 key")
	}
	if !strings.Contains(w.Body.String(), "***") {
		t.Error("错误信息没有脱敏标记")
	}
}

// 未注入上游查询时明确 503，不静默返回空报告。
func TestDriftEndpointNotWired(t *testing.T) {
	h := newTestHandler(t)
	w := driftRequest(testMux(h), true)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("期望 503，实际 %d", w.Code)
	}
}

func putOverlays(mux http.Handler, body string) *httptest.ResponseRecorder {
	return do(mux, http.MethodPut, "/admin/overlays", body)
}

type saveResponse struct {
	Saved    bool              `json:"saved"`
	Revision string            `json:"revision"`
	Warnings []catalog.Problem `json:"warnings"`
	Issues   []catalog.Problem `json:"issues"`
}

// 漂移告警不阻断保存：前期人工配置频度高，一个孤儿条目不该锁死整个面板。
func TestPutOverlaysWarningsDoNotBlock(t *testing.T) {
	h := newTestHandler(t)
	h.Validate = func(_ catalog.Overlays) Validation {
		return Validation{Warnings: []catalog.Problem{{Path: "/models/gone-260915",
			Message: "覆盖层包含上游不存在的模型: gone-260915"}}}
	}
	w := putOverlays(testMux(h), `{"models":{"img-a":{"kind":"image"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got saveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Saved || len(got.Warnings) != 1 {
		t.Errorf("响应 = %+v", got)
	}
	if got.Warnings[0].Path != "/models/gone-260915" {
		t.Errorf("告警丢了路径: %+v", got.Warnings[0])
	}
	if _, ok := h.Overlays().Models["img-a"]; !ok {
		t.Error("有告警却没落盘")
	}
}

// 会让运行时做错事的校验项仍然硬拦，且不落盘。
func TestPutOverlaysIssuesBlock(t *testing.T) {
	h := newTestHandler(t)
	h.Validate = func(_ catalog.Overlays) Validation {
		return Validation{Issues: []catalog.Problem{{Path: "/models/img-a/kind",
			Message: "kind 取值非法"}}}
	}
	w := putOverlays(testMux(h), `{"models":{"img-a":{"kind":"audio"}}}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("期望 422，实际 %d: %s", w.Code, w.Body.String())
	}
	var got saveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Issues) != 1 || got.Saved {
		t.Errorf("响应 = %+v", got)
	}
	if len(h.Overlays().Models) != 0 {
		t.Error("校验失败却落盘了")
	}
}

// 下架列表必须真正写进配置文件并能读回，否则重启即丢失。
func TestPutOverlaysPersistsDisabled(t *testing.T) {
	h, path := newFileHandler(t, "")
	body := `{"models":{},"disabled":["doubao-old-260915"],"settings":{"defaults":{}}}`
	if w := putOverlays(testMux(h), body); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "doubao-old-260915") {
		t.Errorf("disabled 未写入文件:\n%s", data)
	}
	if !strings.Contains(string(data), "base_url") {
		t.Error("保存时把部署配置冲掉了")
	}

	// 重启 = 用同一个文件重新构造一份 Store，覆盖层必须能读回。
	t.Setenv("API_KEY", testServiceKey)
	store2, err := config.NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store2.Overlays().Disabled, []string{"doubao-old-260915"}) {
		t.Errorf("重启后 disabled = %v", store2.Overlays().Disabled)
	}
}

// 旧管理端不知道 disabled 字段，PUT 里不带这个键时不能把下架列表清掉。
func TestPutOverlaysLegacyClientKeepsDisabled(t *testing.T) {
	h := newTestHandler(t)
	if _, err := h.store.ReplaceOverlays(catalog.Overlays{
		Models:   map[string]map[string]any{},
		Settings: catalog.Settings{Defaults: map[string]string{}},
		Disabled: []string{"old-model-260915"},
	}); err != nil {
		t.Fatal(err)
	}
	mux := testMux(h)

	if w := putOverlays(mux, `{"models":{"img-a":{"kind":"image"}}}`); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if !slices.Equal(h.Overlays().Disabled, []string{"old-model-260915"}) {
		t.Errorf("旧客户端保存后下架列表被清空: %v", h.Overlays().Disabled)
	}

	// 显式传空数组才是真清空。
	if w := putOverlays(mux, `{"models":{},"disabled":[]}`); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(h.Overlays().Disabled) != 0 {
		t.Errorf("显式清空未生效: %v", h.Overlays().Disabled)
	}
}

// 协议模板必须写进配置文件并能读回，且不被旧客户端的保存清空。
func TestPutOverlaysProtocols(t *testing.T) {
	h, path := newFileHandler(t, "")
	body := `{"models":{},"protocols":{"video":{"execution":{"task":"async","transport":"video_generations"}}}}`
	if w := putOverlays(testMux(h), body); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "video_generations") {
		t.Errorf("protocols 未写入文件:\n%s", data)
	}

	t.Setenv("API_KEY", testServiceKey)
	store2, err := config.NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := store2.Overlays().Protocols["video"]["execution"].(map[string]any)
	if !ok || ex["transport"] != "video_generations" {
		t.Errorf("重启后 protocols = %v", store2.Overlays().Protocols)
	}

	// 旧客户端 PUT 不带 protocols 键时保留；显式 {} 才清空。
	if w := putOverlays(testMux(h), `{"models":{}}`); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if _, ok := h.Overlays().Protocols["video"]; !ok {
		t.Error("旧客户端保存把协议模板清空了")
	}
	if w := putOverlays(testMux(h), `{"models":{},"protocols":{}}`); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if len(h.Overlays().Protocols) != 0 {
		t.Error("显式清空未生效")
	}
}
