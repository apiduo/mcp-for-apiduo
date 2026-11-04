package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

func stateRequest(mux http.Handler, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/state", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testServiceKey)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func decodeState(t *testing.T, w *httptest.ResponseRecorder) stateResponse {
	t.Helper()
	var got stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析 /admin/state 失败: %v; body=%s", err, w.Body.String())
	}
	return got
}

// 状态聚合必须同时给出上游事实、覆盖层、后端合并结果与漂移，且走服务身份鉴权。
func TestStateEndpoint(t *testing.T) {
	h := newTestHandler(t)
	h.Version = "0.1.0-dev"
	h.cur.Store(&catalog.Overlays{
		Models: map[string]map[string]any{
			"img-a": {"input": map[string]any{"max_reference_images": 2}},
		},
		Settings: catalog.Settings{Defaults: map[string]string{"image": "img-a"}},
	})
	h.Models = func(_ context.Context, _ string) ([]catalog.Model, error) {
		return []catalog.Model{
			{"id": "img-a", "kind": "image", "input": map[string]any{"max_reference_images": 3}},
			{"id": "vid-b", "kind": "video"},
		}, nil
	}
	mux := testMux(h)

	if w := stateRequest(mux, false); w.Code != http.StatusUnauthorized {
		t.Errorf("无鉴权应 401，实际 %d", w.Code)
	}

	w := stateRequest(mux, true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	got := decodeState(t, w)
	if !got.Upstream.OK || got.Upstream.Count != 2 {
		t.Errorf("upstream = %+v", got.Upstream)
	}
	if got.Server.Version != "0.1.0-dev" || got.Server.OverlayCount != 1 {
		t.Errorf("server = %+v", got.Server)
	}
	if len(got.Merged) != 2 {
		t.Fatalf("merged 应有 2 条，实际 %d", len(got.Merged))
	}
	// 后端合并结果必须已应用覆盖层，页面才有一致性断言的基准。
	if v := got.Merged[0]["input"].(map[string]any)["max_reference_images"]; v != float64(2) {
		t.Errorf("merged 未应用覆盖层: %v", v)
	}
	if got.Merged[0]["source"] != "mcp" {
		t.Errorf("被覆盖的模型应带 source=mcp: %v", got.Merged[0]["source"])
	}
	if !got.Drift.OK || len(got.Drift.Unconfigured) != 1 || got.Drift.Unconfigured[0] != "vid-b" {
		t.Errorf("drift = %+v", got.Drift)
	}
}

// 上游不可用：如实标注，不能拿缓存/种子数据冒充事实，也不能泄露服务 key。
func TestStateEndpointUpstreamError(t *testing.T) {
	h := newTestHandler(t)
	h.Models = func(_ context.Context, _ string) ([]catalog.Model, error) {
		return nil, errors.New("upstream 401 echoed " + testServiceKey)
	}
	w := stateRequest(testMux(h), true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200（分项降级），实际 %d", w.Code)
	}
	got := decodeState(t, w)
	if got.Upstream.OK || got.Drift.OK {
		t.Error("上游失败时 ok 必须为 false")
	}
	if len(got.Upstream.Models) != 0 || len(got.Merged) != 0 {
		t.Error("上游失败时不能返回陈旧模型或伪造的合并结果")
	}
	if strings.Contains(w.Body.String(), testServiceKey) || !strings.Contains(w.Body.String(), "***") {
		t.Error("错误信息未脱敏")
	}
}

// 未注入上游查询时明确降级，不静默返回空报告。
func TestStateEndpointNotWired(t *testing.T) {
	h := newTestHandler(t)
	got := decodeState(t, stateRequest(testMux(h), true))
	if got.Upstream.OK || got.Upstream.Error == "" {
		t.Errorf("未接线时应给出错误说明: %+v", got.Upstream)
	}
}

func TestSchemaEndpoint(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(good, []byte(`{"$id":"urn:test","type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t)
	h.SchemaPath = good

	req := httptest.NewRequest(http.MethodGet, "/admin/schema", nil)
	req.Header.Set("Authorization", "Bearer "+testServiceKey)
	w := httptest.NewRecorder()
	testMux(h).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "urn:test") {
		t.Fatalf("schema 下发失败: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/schema+json") {
		t.Errorf("Content-Type = %q", ct)
	}

	// 无鉴权不可读
	noAuth := httptest.NewRequest(http.MethodGet, "/admin/schema", nil)
	w2 := httptest.NewRecorder()
	testMux(h).ServeHTTP(w2, noAuth)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("无鉴权应 401，实际 %d", w2.Code)
	}

	// 文件非法 JSON 时 500，不返回半截内容
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	h.SchemaPath = bad
	w3 := httptest.NewRecorder()
	testMux(h).ServeHTTP(w3, req)
	if w3.Code != http.StatusInternalServerError {
		t.Errorf("非法 schema 应 500，实际 %d", w3.Code)
	}
}

// 调试端点：透传、耗时、状态、脱敏、成本闸。
func TestDebugMCP(t *testing.T) {
	h := newTestHandler(t)
	h.MCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"jsonrpc"`) {
			t.Errorf("MCP handler 没收到原始报文: %s", body)
		}
		if r.Header.Get("Accept") == "" || r.Header.Get("MCP-Protocol-Version") == "" {
			t.Error("缺少 MCP 必需头")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"models.list"}]}}`)
	})
	mux := testMux(h)

	call := func(body, cost string, withAuth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/debug/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+testServiceKey)
		}
		if cost != "" {
			req.Header.Set("X-Debug-Allow-Cost", cost)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	if w := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "", false); w.Code != http.StatusUnauthorized {
		t.Errorf("无鉴权应 401，实际 %d", w.Code)
	}

	w := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		OK         bool           `json:"ok"`
		Status     int            `json:"status"`
		DurationMS int64          `json:"duration_ms"`
		Transport  string         `json:"transport"`
		Identity   string         `json:"identity"`
		Request    map[string]any `json:"request"`
		Response   map[string]any `json:"response"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Status != 200 || got.Transport != "in-process" || got.Identity != "service" {
		t.Errorf("包装字段不对: %+v", got)
	}
	if got.Request["method"] != "tools/list" || got.Response["result"] == nil {
		t.Errorf("缺少原始请求/响应: %+v", got)
	}

	// 生成类调用：缺确认头必须被硬闸拦下（不产生任何上游调用）
	costBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"image.generate","arguments":{"prompt":"x"}}}`
	if w := call(costBody, "", true); w.Code != http.StatusPreconditionRequired {
		t.Errorf("生成类调用缺确认应 428，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := call(costBody, "yes", true); w.Code != http.StatusOK {
		t.Errorf("带确认头应放行，实际 %d: %s", w.Code, w.Body.String())
	}

	// 非 JSON 请求体 400
	if w := call(`not-json`, "", true); w.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，实际 %d", w.Code)
	}
}

// 响应里若回显了服务 key，必须抹掉。
func TestDebugMCPRedactsKey(t *testing.T) {
	h := newTestHandler(t)
	h.MCP = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"message":"bad `+testServiceKey+`"}}`)
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/debug/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+testServiceKey)
	w := httptest.NewRecorder()
	testMux(h).ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), testServiceKey) {
		t.Error("调试响应泄露了服务 key")
	}
	if !strings.Contains(w.Body.String(), "***") {
		t.Error("调试响应没有脱敏标记")
	}
}

func TestCORSLocalOnly(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := CORS(next)
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:8080", "null"} {
		req := httptest.NewRequest(http.MethodOptions, "/admin/state", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("origin %q 未被允许: %d %v", origin, w.Code, w.Header())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/state", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("外部站点应 403，实际 %d", w.Code)
	}
}
