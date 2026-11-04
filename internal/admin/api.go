package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// 本文件是面向 agent 的细粒度管理端点。每个写端点都只做一件事，
// 内部统一转成 Op 走 commit —— 校验、版本、审计只有一条路径。

// handleConfig 读取覆盖层与版本号，不拉上游。
//
// agent 的固定起手式：先读这里拿 revision，再带着 If-Match 写回。
func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ov := h.store.Overlays()
	w.Header().Set("ETag", quoteETag(h.Revision()))
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":     h.Revision(),
		"base_url":     h.store.Get().BaseURL,
		"overlays":     ov,
		"counts":       countsOf(ov),
		"config_error": h.store.LastError(),
	})
}

// handleOps 是 agent 的主要写入口：一组操作要么全部生效，要么一条都不生效。
func (h *Handler) handleOps(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeAPIError(w, badRequest(err.Error()))
		return
	}
	var in struct {
		Ops    []Op   `json:"ops"`
		DryRun bool   `json:"dry_run"`
		Actor  string `json:"actor"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeAPIError(w, badRequest("解析请求失败: "+err.Error()))
		return
	}
	h.respond(w, r, commitRequest{
		Ops:     in.Ops,
		Actor:   orDefault(in.Actor, actorFromRequest(r)),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  in.DryRun || isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	})
}

// handleModels 返回管理端视角的模型列表。
//
// 与管理端不同，这里**包含已下架的模型**并显式标注 disabled：
// 调用方要能看见全量状态才能做「恢复」，客户端列表才需要过滤。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	snap, apiErr := h.loadModels(r.Context(), parseBool(r.URL.Query().Get("refresh")))
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	models := snap.Models
	ov := h.store.Overlays()
	disabled := ov.DisabledSet()
	kind := catalog.NormalizeKind(r.URL.Query().Get("kind"))
	includeUnknown := parseBool(r.URL.Query().Get("include_unknown"))
	full := parseBool(r.URL.Query().Get("full"))

	out := make([]any, 0, len(models))
	for _, m := range models {
		merged := ov.ApplyOne(m)
		if kind != "" && merged.Kind() != kind {
			continue
		}
		if !includeUnknown && merged.Kind() == catalog.KindUnknown {
			continue
		}
		if full {
			out = append(out, merged)
			continue
		}
		out = append(out, modelBrief(merged, disabled[merged.ID()]))
	}
	w.Header().Set("ETag", quoteETag(h.Revision()))
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":        h.Revision(),
		"upstream_count":  len(models),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"count":           len(out),
		// 显式声明「这是投影」，并给出下一步该调哪里 ——
		// 让契约跟着响应走，而不是只写在文档里等人转述。
		"projection": true,
		"detail":     "/admin/models/{id}",
		"models":     out,
	})
}

// modelBrief 是列表视图的投影。
//
// 为什么默认不返回完整画像：上游 59 个模型 × 每个几千字节的 parameters，
// 一次列表就能吃掉 agent 的大半个上下文，而它多数时候只需要 id 和类型。
// 要看细节用 GET /admin/models/{id}。
func modelBrief(m catalog.Model, disabled bool) map[string]any {
	b := map[string]any{
		"id":          m.ID(),
		"kind":        m.Kind(),
		"source":      m["source"],
		"task":        m.Task(),
		"transport":   m.Transport(),
		"disabled":    disabled,
		"has_overlay": m["source"] == "mcp",
	}
	// 三态：null = 未知（不拦截），0 = 明确不支持，N = 上限。
	if n, known := m.MaxReferenceImages(); known {
		b["max_reference_images"] = n
	} else {
		b["max_reference_images"] = nil
	}
	// 只给参数名，不给约束。这是「两次请求」的切分线：
	// 列表回答「有没有这个参数」，详情回答「它能取什么值」。
	// 少这一行，agent 连「支不支持 duration」都得再问一次，两次就变成三次。
	if names := m.ParamNames(); len(names) > 0 {
		b["params"] = names
	}
	return b
}

// handleModelDetail 返回单个模型的三份视图：上游原样 / 覆盖层 / 合并结果。
//
// 三份都给，是因为「字段来源」是配置里最容易搞错的一件事：
// 管理员（或 agent）必须能区分「这个值是我配的」还是「上游给的」。
func (h *Handler) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少模型 ID"})
		return
	}
	snap, apiErr := h.loadModels(r.Context(), parseBool(r.URL.Query().Get("refresh")))
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	ov := h.store.Overlays()
	up, found := catalog.Find(snap.Models, id)
	overlay, hasOverlay := ov.Models[id]
	merged := catalog.Model{}
	if found {
		merged = ov.ApplyOne(up)
	}
	drift := "unconfigured"
	switch {
	case !found && hasOverlay:
		drift = "orphaned"
	case !found:
		drift = "not_upstream"
	case ov.DisabledSet()[id]:
		drift = "disabled"
	case merged["source"] == "mcp":
		drift = "configured"
	case merged["source"] == "protocol":
		drift = "inherited"
	}
	w.Header().Set("ETag", quoteETag(h.Revision()))
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":        h.Revision(),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"id":              id,
		"upstream":        map[string]any{"found": found, "model": up},
		"overlay":         map[string]any{"present": hasOverlay, "patch": overlay},
		"merged":          merged,
		"drift":           drift,
		"editable":        found,
	})
}

func (h *Handler) handlePatchModel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPatch) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少模型 ID"})
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeAPIError(w, badRequest(err.Error()))
		return
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		writeAPIError(w, badRequest("body 必须是 JSON 对象（RFC 7386 合并补丁）: "+err.Error()))
		return
	}
	if len(patch) == 0 {
		writeAPIError(w, &apiError{
			Status: http.StatusBadRequest, Code: "invalid_request",
			Message: "空的补丁没有意义", Hint: `要清空某字段传 {"字段名": null}；要整条删除用 DELETE`,
		})
		return
	}
	h.respond(w, r, commitRequest{
		Ops:     []Op{{Op: "model.patch", Model: id, Patch: patch}},
		Actor:   actorFromRequest(r),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	})
}

func (h *Handler) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少模型 ID"})
		return
	}
	h.respond(w, r, commitRequest{
		Ops:     []Op{{Op: "model.delete", Model: id}},
		Actor:   actorFromRequest(r),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	})
}

func (h *Handler) handleDefault(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.PathValue("kind"))
	if kind == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少 kind"})
		return
	}
	req := commitRequest{
		Actor:   actorFromRequest(r),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	}
	switch r.Method {
	case http.MethodPut:
		body, err := readBody(w, r)
		if err != nil {
			writeAPIError(w, badRequest(err.Error()))
			return
		}
		var in struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeAPIError(w, badRequest(`body 形如 {"model":"模型 ID"}: `+err.Error()))
			return
		}
		req.Ops = []Op{{Op: "default.set", Kind: kind, Model: in.Model}}
	case http.MethodDelete:
		req.Ops = []Op{{Op: "default.delete", Kind: kind}}
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeAPIError(w, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"})
		return
	}
	h.respond(w, r, req)
}

func (h *Handler) handleDisabled(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少模型 ID"})
		return
	}
	op := Op{Op: "disabled.add", Model: id}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		op.Op = "disabled.remove"
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeAPIError(w, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"})
		return
	}
	h.respond(w, r, commitRequest{
		Ops:     []Op{op},
		Actor:   actorFromRequest(r),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	})
}

func (h *Handler) handleProtocol(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeAPIError(w, &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "缺少协议族名"})
		return
	}
	req := commitRequest{
		Actor:   actorFromRequest(r),
		Source:  "rest",
		Remote:  h.remoteOf(r),
		DryRun:  isDryRun(r),
		IfMatch: r.Header.Get("If-Match"),
	}
	switch r.Method {
	case http.MethodPatch:
		body, err := readBody(w, r)
		if err != nil {
			writeAPIError(w, badRequest(err.Error()))
			return
		}
		var tpl map[string]any
		if err := json.Unmarshal(body, &tpl); err != nil {
			writeAPIError(w, badRequest("body 必须是 JSON 对象（协议族模板的合并补丁）: "+err.Error()))
			return
		}
		if len(tpl) == 0 {
			writeAPIError(w, &apiError{
				Status: http.StatusBadRequest, Code: "invalid_request",
				Message: "空模板没有意义", Hint: `要删除协议族覆盖用 DELETE /admin/protocols/{name}`,
			})
			return
		}
		req.Ops = []Op{{Op: "protocol.patch", Name: name, Template: tpl}}
	case http.MethodDelete:
		req.Ops = []Op{{Op: "protocol.delete", Name: name}}
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		writeAPIError(w, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"})
		return
	}
	h.respond(w, r, req)
}

// handleDrift 返回配置中心与上游的差异，是「前期全靠人工配置」这条路线的最低运维闭环。
//
// 只做只读查询，不改任何状态；上游拉不动时如实报错，不拿缓存冒充事实。
func (h *Handler) handleDrift(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	snap, apiErr := h.loadModels(r.Context(), parseBool(r.URL.Query().Get("refresh")))
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	d := h.store.Overlays().Drift(snap.Models)
	w.Header().Set("ETag", quoteETag(h.Revision()))
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":        h.Revision(),
		"upstream_count":  len(snap.Models),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"configured":      d.Configured,
		"unconfigured":    d.Unconfigured,
		"orphaned":        d.Orphaned,
		"disabled":        d.Disabled,
		"inherited":       d.Inherited,
		"counts": map[string]int{
			"configured":   len(d.Configured),
			"unconfigured": len(d.Unconfigured),
			"orphaned":     len(d.Orphaned),
			"disabled":     len(d.Disabled),
			"inherited":    len(d.Inherited),
		},
	})
}

func (h *Handler) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": h.Revision(),
		"entries":  h.audit.History(limit),
	})
}

// respond 是所有写端点的统一出口：成功带 ETag 与差异，失败带结构化错误。
func (h *Handler) respond(w http.ResponseWriter, r *http.Request, req commitRequest) {
	res, apiErr := h.commit(req)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	w.Header().Set("ETag", quoteETag(res.Revision))
	writeJSON(w, http.StatusOK, res)
}

// loadModels 读上游模型列表（带短期缓存），返回快照与年龄。
//
// 缓存命中直接返回；未命中才真正请求上游。缓存键包含 base_url 与 key，
// 所以部署配置热更新后自然 miss，不需要显式失效。
//
// force=true 绕过缓存读并刷新它：给「刚在上游加了渠道，要立刻确认」这种场景用。
// 它只影响这一次读取，不会清空缓存让别人也 miss。
func (h *Handler) loadModels(ctx context.Context, force bool) (modelSnapshot, *apiError) {
	if h.Models == nil {
		return modelSnapshot{}, &apiError{Status: http.StatusServiceUnavailable, Code: "upstream_not_wired",
			Message: "未启用上游模型查询", Hint: "服务启动时未注入模型查询函数"}
	}
	cfg := h.store.Get()
	if cfg.APIKey == "" {
		return modelSnapshot{}, &apiError{Status: http.StatusServiceUnavailable, Code: "missing_service_key",
			Message: "缺少服务身份 api_key，无法查询上游", Hint: "在配置里设置 api_key（部署者凭据，不是用户令牌）"}
	}

	cacheKey := cfg.BaseURL + "\x00" + cfg.APIKey
	if !force {
		if snap, ok := h.modelCache.get(cacheKey); ok {
			return snap, nil
		}
	}

	key := cfg.APIKey
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	models, err := h.Models(ctx, key)
	if err != nil {
		// 过期后拉取失败就报错，绝不用旧值兜底 —— 那才是「缓存冒充事实」。
		msg := strings.ReplaceAll(err.Error(), key, "***")
		return modelSnapshot{}, &apiError{Status: http.StatusBadGateway, Code: "upstream_unavailable",
			Message: "拉取上游模型失败: " + msg,
			Hint:    "上游不可用时管理端不拿缓存冒充事实；修好上游后重试"}
	}
	for i := range models {
		models[i] = catalog.Normalize(models[i])
	}
	h.modelCache.put(cacheKey, models)
	return modelSnapshot{Models: models}, nil
}

func countsOf(ov catalog.Overlays) map[string]int {
	return map[string]int{
		"models":    len(ov.Models),
		"disabled":  len(ov.Disabled),
		"protocols": len(ov.Protocols),
		"defaults":  len(ov.Settings.Defaults),
	}
}

// handleHealth 是**唯一免鉴权**的信息端点，所以它只回答「活着吗」。
//
// 早先这里返回了掩码后的 api_key 与 base_url。掩码不是脱敏：`sk-5***30`
// 已经是服务身份 key 的前 4 位加后 2 位，公网任何人都能拿到；
// base_url 则是内网服务名。一个匿名端点不该有这个信息量。
// 要排障请看 /admin/state（带服务身份），那里有完整信息。
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	ov := h.store.Overlays()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"version":   h.Version,
		"config_ok": h.store.LastError() == "",
		"revision":  h.Revision(),
		"models":    len(ov.Models),
		"disabled":  len(ov.Disabled),
		"protocols": len(ov.Protocols),
	})
}

// ── 小工具 ────────────────────────────────────────────────────────────────

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeAPIError(w, &apiError{
		Status: http.StatusMethodNotAllowed, Code: "method_not_allowed",
		Message: "只接受 " + method, Hint: "见 GET /admin/manifest 的端点清单",
	})
	return false
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
