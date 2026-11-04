package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// upstreamStub 提供一份固定的上游模型列表。
func upstreamStub(h *Handler, models ...catalog.Model) {
	h.Models = func(_ context.Context, _ string) ([]catalog.Model, error) {
		return models, nil
	}
}

type opsResp struct {
	OK             bool     `json:"ok"`
	Saved          bool     `json:"saved"`
	DryRun         bool     `json:"dry_run"`
	NoChange       bool     `json:"no_change"`
	Applied        int      `json:"applied"`
	Revision       string   `json:"revision"`
	RevisionBefore string   `json:"revision_before"`
	Changes        []Change `json:"changes"`
}

type errResp struct {
	Error struct {
		Code    string   `json:"code"`
		Message string   `json:"message"`
		Hint    string   `json:"hint"`
		Path    string   `json:"path"`
		Allowed []string `json:"allowed"`
		OpIndex *int     `json:"op_index"`
	} `json:"error"`
	Issues   []catalog.Problem `json:"issues"`
	Warnings []catalog.Problem `json:"warnings"`
	Revision string            `json:"revision"`
}

// 合并补丁必须能删键：null 删键是 RFC 7386 的语义，也是 agent 唯一能用的「撤销某个字段」手段。
func TestPatchModelMergesAndDeletesKeys(t *testing.T) {
	h := newTestHandler(t)
	mux := testMux(h)

	w := do(mux, http.MethodPatch, "/admin/models/m1",
		`{"kind":"image","input":{"max_reference_images":2}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	patch := h.Overlays().Models["m1"]
	if patch["kind"] != "image" {
		t.Fatalf("kind 未写入: %v", patch)
	}
	in, _ := patch["input"].(map[string]any)
	if in["max_reference_images"] != float64(2) {
		t.Fatalf("input 未写入: %v", patch)
	}

	// 第二次只改一个叶子，不动已有的 kind —— 这正是细粒度补丁存在的意义。
	w = do(mux, http.MethodPatch, "/admin/models/m1", `{"input":{"max_reference_images":null}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	patch = h.Overlays().Models["m1"]
	if patch["kind"] != "image" {
		t.Errorf("补丁把无关字段冲掉了: %v", patch)
	}
	if _, ok := patch["input"]; ok {
		t.Errorf("null 没有删掉键: %v", patch)
	}
}

// 批量必须原子：任意一条不合格，整批都不落盘。
func TestOpsBatchIsAtomic(t *testing.T) {
	h := newTestHandler(t)
	h.Validate = func(ov catalog.Overlays) Validation {
		if _, ok := ov.Models["bad"]; ok {
			return Validation{Issues: []catalog.Problem{{Path: "/models/bad", Message: "会让运行时做错事"}}}
		}
		return Validation{}
	}
	mux := testMux(h)
	body := `{"ops":[
		{"op":"model.patch","model":"good","patch":{"kind":"image"}},
		{"op":"model.patch","model":"bad","patch":{"kind":"audio"}}
	]}`
	w := do(mux, http.MethodPost, "/admin/ops", body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("期望 422，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(h.Overlays().Models) != 0 {
		t.Errorf("整批被拒却写入了部分数据: %v", h.Overlays().Models)
	}
	var got errResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != "validation_failed" || len(got.Issues) != 1 {
		t.Errorf("错误形态 = %+v", got)
	}
	// 落盘前的版本不能变：被拒绝的写入不该推进 revision。
	if got.Revision != h.Revision() {
		t.Errorf("422 里的 revision (%s) 与当前 (%s) 不一致", got.Revision, h.Revision())
	}
}

// 未知操作要回一份「可用的操作名」，让调用方能自己改对。
func TestUnknownOpTeachesAllowedValues(t *testing.T) {
	h := newTestHandler(t)
	w := do(testMux(h), http.MethodPost, "/admin/ops", `{"ops":[{"op":"model.upsert","model":"m1"}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
	var got errResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != "invalid_op" {
		t.Errorf("code = %q", got.Error.Code)
	}
	if len(got.Error.Allowed) != len(OpNames) {
		t.Errorf("allowed = %v", got.Error.Allowed)
	}
	if got.Error.OpIndex == nil || *got.Error.OpIndex != 0 {
		t.Errorf("op_index = %v", got.Error.OpIndex)
	}
}

// dry_run 只返回差异，不落盘、不动版本。
func TestDryRunDoesNotPersist(t *testing.T) {
	h := newTestHandler(t)
	mux := testMux(h)
	before := h.Revision()

	body := `{"dry_run":true,"ops":[{"op":"model.patch","model":"m1","patch":{"kind":"video"}}]}`
	w := do(mux, http.MethodPost, "/admin/ops", body)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var got opsResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Saved || !got.DryRun {
		t.Errorf("dry_run 却落盘了: %+v", got)
	}
	if len(got.Changes) == 0 {
		t.Error("dry_run 必须返回 differences，否则调用方没法确认")
	}
	if h.Revision() != before || len(h.Overlays().Models) != 0 {
		t.Error("dry_run 改动了实际状态")
	}
	if got.Revision == before {
		t.Error("dry_run 应返回「提交后会得到的版本」")
	}
}

// 没有变化时不写盘、不推进版本：幂等重试是常态，不该被当成一次变更。
func TestNoChangeKeepsRevision(t *testing.T) {
	h := newTestHandler(t)
	mux := testMux(h)
	if w := do(mux, http.MethodPost, "/admin/disabled/m1", ""); w.Code != http.StatusOK {
		t.Fatalf("首次下架失败: %d %s", w.Code, w.Body.String())
	}
	rev := h.Revision()

	w := do(mux, http.MethodPost, "/admin/disabled/m1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("重复下架应成功: %d %s", w.Code, w.Body.String())
	}
	var got opsResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.NoChange || got.Saved {
		t.Errorf("重复操作不是 no_change: %+v", got)
	}
	if h.Revision() != rev {
		t.Error("no_change 却推进了版本号")
	}
}

// 版本不符必须 409 且不落盘 —— 这是多客户端（面板 + agent）并发的安全网。
func TestIfMatchConflict(t *testing.T) {
	h := newTestHandler(t)
	mux := testMux(h)
	r := doWithHeaders(mux, http.MethodPost, "/admin/ops",
		`{"ops":[{"op":"model.patch","model":"m1","patch":{"kind":"image"}}]}`,
		map[string]string{"If-Match": `"stale-revision"`})
	if r.Code != http.StatusConflict {
		t.Fatalf("期望 409，实际 %d: %s", r.Code, r.Body.String())
	}
	var got errResp
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != "revision_mismatch" || got.Error.Hint == "" {
		t.Errorf("错误形态 = %+v", got)
	}
	if len(h.Overlays().Models) != 0 {
		t.Error("版本冲突却写入了")
	}

	// 带对版本就能成功。
	r = doWithHeaders(mux, http.MethodPost, "/admin/ops",
		`{"ops":[{"op":"model.patch","model":"m1","patch":{"kind":"image"}}]}`,
		map[string]string{"If-Match": h.Revision()})
	if r.Code != http.StatusOK {
		t.Fatalf("带对版本应成功，实际 %d: %s", r.Code, r.Body.String())
	}
}

// 清单必须覆盖每一条真实路由：文档与实现共用一张表，不允许漂移。
func TestManifestCoversEveryRoute(t *testing.T) {
	h := newTestHandler(t)
	h.Version = "test"
	w := do(testMux(h), http.MethodGet, "/admin/manifest", "")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var m struct {
		Endpoints []struct {
			Method  string `json:"method"`
			Path    string `json:"path"`
			Summary string `json:"summary"`
			Auth    string `json:"auth"`
		} `json:"endpoints"`
		Ops []struct {
			Op string `json:"op"`
		} `json:"ops"`
		ErrorCodes []struct {
			Code string `json:"code"`
		} `json:"error_codes"`
		Workflows []any `json:"workflows"`
		Rules     []any `json:"rules"`
		Service   struct {
			Revision string `json:"revision"`
		} `json:"service"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Endpoints) != len(h.routes()) {
		t.Errorf("manifest 端点 %d 条，路由表 %d 条", len(m.Endpoints), len(h.routes()))
	}
	for _, e := range m.Endpoints {
		if e.Summary == "" || e.Path == "" {
			t.Errorf("端点缺摘要: %+v", e)
		}
	}
	if len(m.Ops) != len(OpNames) {
		t.Errorf("ops 清单 %d 条，OpNames %d 条", len(m.Ops), len(OpNames))
	}
	if len(m.ErrorCodes) < 5 || len(m.Workflows) < 3 || len(m.Rules) < 5 {
		t.Error("错误码 / 流程 / 规则不完整：这三样是 agent 自我纠正的依据")
	}
	if m.Service.Revision != h.Revision() {
		t.Errorf("manifest 的 revision (%s) 与当前 (%s) 不一致", m.Service.Revision, h.Revision())
	}
}

// 单模型视图必须能区分「上游给的」和「配置改的」。
func TestModelDetailShowsThreeViews(t *testing.T) {
	h := newTestHandler(t)
	upstreamStub(h, catalog.Model{"id": "m1", "kind": "image", "parameters": map[string]any{
		"size": map[string]any{"enum": []any{"1K", "2K"}, "default": "1K"},
	}})
	if w := do(testMux(h), http.MethodPatch, "/admin/models/m1",
		`{"parameters":{"size":{"default":"2K"}}}`); w.Code != http.StatusOK {
		t.Fatalf("补丁失败: %d %s", w.Code, w.Body.String())
	}

	w := do(testMux(h), http.MethodGet, "/admin/models/m1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var got struct {
		Upstream map[string]any `json:"upstream"`
		Overlay  map[string]any `json:"overlay"`
		Merged   map[string]any `json:"merged"`
		Drift    string         `json:"drift"`
		Editable bool           `json:"editable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Upstream["found"] != true || got.Overlay["present"] != true {
		t.Errorf("三份视图不完整: %+v", got)
	}
	if got.Drift != "configured" || !got.Editable {
		t.Errorf("drift=%s editable=%v", got.Drift, got.Editable)
	}
	params, _ := got.Merged["parameters"].(map[string]any)
	size, _ := params["size"].(map[string]any)
	if size["default"] != "2K" {
		t.Errorf("合并结果没反映覆盖值: %v", size)
	}
	// 上游视图保持原样，不受覆盖影响。
	upModel, _ := got.Upstream["model"].(map[string]any)
	upParams, _ := upModel["parameters"].(map[string]any)
	upSize, _ := upParams["size"].(map[string]any)
	if upSize["default"] != "1K" {
		t.Errorf("上游视图被覆盖污染: %v", upSize)
	}
}

// 变更必须留痕：谁、什么时候、做了哪些操作。
func TestHistoryRecordsWrites(t *testing.T) {
	h := newTestHandler(t)
	mux := testMux(h)
	doWithHeaders(mux, http.MethodPost, "/admin/ops",
		`{"actor":"codex","ops":[{"op":"model.patch","model":"m1","patch":{"kind":"image"}}]}`,
		map[string]string{"X-Admin-Actor": "workbuddy"})

	w := do(mux, http.MethodGet, "/admin/history?limit=10", "")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var got struct {
		Entries []struct {
			Actor   string      `json:"actor"`
			Outcome string      `json:"outcome"`
			Ops     []opSummary `json:"ops"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("条目数 = %d", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Actor != "codex" {
		t.Errorf("actor = %q（body 里的 actor 应优先于请求头）", e.Actor)
	}
	if e.Outcome != "saved" || len(e.Ops) != 1 || e.Ops[0].Target != "m1" {
		t.Errorf("记录 = %+v", e)
	}
}

// 下架 / 恢复必须能回读，且恢复后客户端列表才重新可见。
func TestDisabledAddRemove(t *testing.T) {
	h := newTestHandler(t)
	upstreamStub(h, catalog.Model{"id": "m1", "kind": "image"})
	mux := testMux(h)

	if w := do(mux, http.MethodPost, "/admin/disabled/m1", ""); w.Code != http.StatusOK {
		t.Fatalf("下架失败: %d %s", w.Code, w.Body.String())
	}
	if !h.Overlays().DisabledSet()["m1"] {
		t.Fatal("下架未生效")
	}
	// 管理端视角仍要能看见它（否则无法恢复），但必须标注 disabled。
	var list struct {
		Models []map[string]any `json:"models"`
	}
	w := do(mux, http.MethodGet, "/admin/models", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Models) != 1 || list.Models[0]["disabled"] != true {
		t.Fatalf("管理端模型列表 = %v", list.Models)
	}

	if w := do(mux, http.MethodDelete, "/admin/disabled/m1", ""); w.Code != http.StatusOK {
		t.Fatalf("恢复失败: %d %s", w.Code, w.Body.String())
	}
	if h.Overlays().DisabledSet()["m1"] {
		t.Error("恢复未生效")
	}
}

// 未知路径不能落到某个宽匹配上：模型 ID 里带斜杠时也要能取到。
func TestModelIDWithSlash(t *testing.T) {
	h := newTestHandler(t)
	if w := do(testMux(h), http.MethodPatch, "/admin/models/openai/gpt-image-1",
		`{"kind":"image"}`); w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if _, ok := h.Overlays().Models["openai/gpt-image-1"]; !ok {
		t.Errorf("ID 含斜杠时路径参数被截断: %v", h.Overlays().Models)
	}
}

// doWithHeaders 与 do 相同，但可以附加请求头。
func doWithHeaders(mux http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testServiceKey)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// 差异必须是叶子级：agent 靠它核对「我到底改了什么」，
// 一条「整段 /models 被替换」等于什么都没说。
func TestDiffIsLeafLevel(t *testing.T) {
	before := catalog.Overlays{}
	after := catalog.Overlays{Models: map[string]map[string]any{
		"m1": {"kind": "image", "input": map[string]any{"max_reference_images": 3}},
	}}
	got := Diff(before, after)
	var paths []string
	for _, c := range got {
		paths = append(paths, c.Path)
	}
	want := []string{"/models/m1/input/max_reference_images", "/models/m1/kind"}
	if !slices.Equal(paths, want) {
		t.Errorf("差异路径 = %v，期望 %v", paths, want)
	}

	// 删键要能看出来：Before 有值、After 缺失。
	back := Diff(after, before)
	for _, c := range back {
		if c.After != nil {
			t.Errorf("删除项不该有 after: %+v", c)
		}
	}
	if len(back) != 2 {
		t.Errorf("删除差异 = %+v", back)
	}
}

// 两段式读取的契约：列表只给参数名，详情才给约束。
//
// 这条一旦被「优化」成列表直出完整画像，上下文成本会静默翻十倍，
// 而调用方不会收到任何错误 —— 它只会变慢、变贵、开始漏字段。必须钉住。
func TestModelsListIsProjectionWithParamNames(t *testing.T) {
	h := newTestHandler(t)
	upstreamStub(h, catalog.Model{
		"id": "m1", "kind": "image",
		"parameters": map[string]any{
			"size": map[string]any{"enum": []any{"1K", "2K"}, "default": "1K"},
			"n":    map[string]any{"min": 1, "max": 4},
		},
	})
	mux := testMux(h)

	var brief struct {
		Projection bool             `json:"projection"`
		Detail     string           `json:"detail"`
		Models     []map[string]any `json:"models"`
	}
	w := do(mux, http.MethodGet, "/admin/models", "")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &brief); err != nil {
		t.Fatal(err)
	}
	if !brief.Projection {
		t.Error("列表必须声明 projection=true，否则调用方会以为它是全量")
	}
	if brief.Detail != "/admin/models/{id}" {
		t.Errorf("详情指路 = %q", brief.Detail)
	}
	if len(brief.Models) != 1 {
		t.Fatalf("模型数 = %d", len(brief.Models))
	}
	m := brief.Models[0]
	if _, ok := m["parameters"]; ok {
		t.Error("列表不该返回完整参数约束")
	}
	names, _ := m["params"].([]any)
	if !slices.Equal(names, []any{"n", "size"}) {
		t.Errorf("params = %v，期望参数名排序后的 [n size]", m["params"])
	}

	// 详情：完整约束 + 三份视图。
	var detail struct {
		Merged  map[string]any `json:"merged"`
		Overlay map[string]any `json:"overlay"`
	}
	w = do(mux, http.MethodGet, "/admin/models/m1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("详情期望 200，实际 %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if _, ok := detail.Merged["parameters"]; !ok {
		t.Error("详情必须给完整参数约束")
	}

	// full=1 是逃生舱：一次拿全，供人导出/调试。
	var full struct {
		Models []map[string]any `json:"models"`
	}
	w = do(mux, http.MethodGet, "/admin/models?full=1", "")
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	if _, ok := full.Models[0]["parameters"]; !ok {
		t.Error("full=1 应返回完整画像")
	}
}

// 两段式的规则必须出现在自描述里，而不是只写在 README 里等人转述。
func TestManifestDeclaresTwoStepRead(t *testing.T) {
	h := newTestHandler(t)
	w := do(testMux(h), http.MethodGet, "/admin/manifest", "")
	var m struct {
		Conventions map[string]any `json:"conventions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m.Conventions["list_vs_detail"].(string)
	if !ok || !strings.Contains(v, "/admin/models/{id}") {
		t.Errorf("conventions.list_vs_detail = %v", m.Conventions["list_vs_detail"])
	}
}

// 读路径缓存：同一份上游列表在 TTL 内只拉一次，且如实报告年龄。
//
// 这条是为了让「两次读取只花一次上游往返」成立；没有它，两段式读取的成本
// 会翻倍，而漂移这类自查还会把上游打得更频繁。
func TestModelsCachedWithinTTL(t *testing.T) {
	h := newTestHandler(t)
	calls := 0
	h.Models = func(context.Context, string) ([]catalog.Model, error) {
		calls++
		return []catalog.Model{{"id": "m1", "kind": "image"}}, nil
	}
	mux := testMux(h)

	for i := 0; i < 3; i++ {
		if w := do(mux, http.MethodGet, "/admin/models", ""); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败: %d", i+1, w.Code)
		}
	}
	if calls != 1 {
		t.Errorf("TTL 内应只拉一次上游，实际 %d 次", calls)
	}

	var body struct {
		UpstreamAgeMS *int64 `json:"upstream_age_ms"`
	}
	w := do(mux, http.MethodGet, "/admin/models", "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.UpstreamAgeMS == nil {
		t.Fatal("响应缺少 upstream_age_ms：调用方无法判断这份列表有多旧")
	}
	if *body.UpstreamAgeMS < 0 {
		t.Errorf("upstream_age_ms = %d", *body.UpstreamAgeMS)
	}
}

// 过期后必须重新拉取；TTL=0 表示不缓存。
func TestModelsCacheExpiry(t *testing.T) {
	h := newTestHandler(t)
	h.SetModelCacheTTL(10 * time.Millisecond)
	calls := 0
	h.Models = func(context.Context, string) ([]catalog.Model, error) {
		calls++
		return []catalog.Model{{"id": "m1", "kind": "image"}}, nil
	}
	mux := testMux(h)

	do(mux, http.MethodGet, "/admin/models", "")
	time.Sleep(20 * time.Millisecond)
	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 2 {
		t.Errorf("过期后应重拉，实际 %d 次", calls)
	}

	h.SetModelCacheTTL(0) // 关闭
	do(mux, http.MethodGet, "/admin/models", "")
	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 4 {
		t.Errorf("关闭缓存后每次都该拉，实际 %d 次", calls)
	}
}

// 缓存键包含 base_url：换了上游就不能复用旧列表。
func TestModelsCacheKeyedByBaseURL(t *testing.T) {
	h, path := newFileHandler(t, "")
	calls := 0
	h.Models = func(context.Context, string) ([]catalog.Model, error) {
		calls++
		return []catalog.Model{{"id": "m1", "kind": "image"}}, nil
	}
	mux := testMux(h)

	do(mux, http.MethodGet, "/admin/models", "")
	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 1 {
		t.Fatalf("前置：期望 1 次，实际 %d", calls)
	}

	seed := "base_url: http://127.0.0.1:3998/v1\napi_key: " + testServiceKey + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	h.store.Reload()
	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 2 {
		t.Errorf("base_url 变了却复用了旧缓存（%d 次）", calls)
	}
}

// ?refresh=1 绕过缓存：只刷新这一次读，且年龄归零。
func TestRefreshBypassesCache(t *testing.T) {
	h := newTestHandler(t)
	calls := 0
	h.Models = func(context.Context, string) ([]catalog.Model, error) {
		calls++
		return []catalog.Model{{"id": "m1", "kind": "image"}}, nil
	}
	mux := testMux(h)

	do(mux, http.MethodGet, "/admin/models", "")
	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 1 {
		t.Fatalf("前置：期望命中缓存 1 次，实际 %d", calls)
	}

	var body struct {
		UpstreamAgeMS int64 `json:"upstream_age_ms"`
	}
	w := do(mux, http.MethodGet, "/admin/models?refresh=1", "")
	if calls != 2 {
		t.Errorf("refresh=1 应强制拉取，实际 %d 次", calls)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.UpstreamAgeMS != 0 {
		t.Errorf("刷新后 age_ms 应为 0，实际 %d", body.UpstreamAgeMS)
	}
}

// /admin/state 与 /admin/models 必须共用同一份上游视图。
//
// 分开两套缓存的话，「面板 59 个、agent 列表 58 个」会变成常态，
// 而两边都是「真实数据」—— 这种不一致最难解释。
func TestStateSharesUpstreamCache(t *testing.T) {
	h := newTestHandler(t)
	calls := 0
	h.Models = func(context.Context, string) ([]catalog.Model, error) {
		calls++
		return []catalog.Model{{"id": "m1", "kind": "image"}}, nil
	}
	mux := testMux(h)

	w := do(mux, http.MethodGet, "/admin/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("state 期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var st stateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Upstream.OK || st.Upstream.Count != 1 {
		t.Fatalf("state.upstream = %+v", st.Upstream)
	}

	do(mux, http.MethodGet, "/admin/models", "")
	if calls != 1 {
		t.Errorf("state 与 models 应共用缓存，实际拉了 %d 次上游", calls)
	}

	// state 也要能被强制刷新。
	do(mux, http.MethodGet, "/admin/state?refresh=1", "")
	if calls != 2 {
		t.Errorf("state?refresh=1 应强制拉取，实际 %d 次", calls)
	}
}
