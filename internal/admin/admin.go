// Package admin 是控制面板的后端：模型覆盖层与全局设置的读写。
//
// 约束（README §11）：
//   - 独立端口、只在内网暴露，用服务身份的 api_key 校验，不建第二套账号。
//   - 校验对象是「合并结果」：读覆盖层 → 与上游合并 → 校验 → 通过才落盘。
//   - 写操作先校验后落盘，失败保留旧值。
//
// 对 agent 友好不是「多几个接口」，而是四条性质：
//   - 可发现：GET /admin/manifest 用机器可读的形式说明所有操作、错误码与常用流程。
//   - 细粒度且幂等：POST /admin/ops 是定址的小操作，重试不会抹掉别人的字段。
//   - 可并发：每个写请求都能带 If-Match，版本不符返回 409 而不是静默覆盖。
//   - 可空跑：dry_run 只校验、返回差异，不落盘。
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/config"
)

// Validation 把「会让运行时做错事」和「配置与事实脱节」分开。
//
// Issues 必须拦：kind 非法、参数约束矛盾、默认值类型不符。
// Warnings 不拦：孤儿覆盖层条目、默认模型当前不在上游 —— 运行时对这两者都有安全回落
// （Apply 静默忽略 / PickDefault 回落第一个），硬拦等于拿瞬时上游快照否决持久配置意图。
type Validation struct {
	Issues   []catalog.Problem `json:"issues,omitempty"`
	Warnings []catalog.Problem `json:"warnings,omitempty"`
}

// Handler 提供覆盖层读写。
//
// 它不持有配置：文件只有 config.Store 一个读者。早先这里存了一份覆盖层快照，
// 手改文件不会生效，页面上写着「已保存」而运行时用的是旧值。
type Handler struct {
	store *config.Store

	// Validate 由上层注入：给定覆盖层，校验每个合并结果。
	// Issues 阻断落盘，Warnings 随 200 返回给管理端。
	Validate func(catalog.Overlays) Validation

	// Models 由上层注入：拉取上游模型列表，供漂移自查与模型视图。
	// 为 nil 时相关端点返回 503，不猜测上游。
	Models func(context.Context, string) ([]catalog.Model, error)

	// MCP 是同一进程内的业务 MCP handler，供 /admin/debug/mcp 透传。
	// 必须由上层注入；为 nil 时调试端点返回 503，不做任何「模拟实现」。
	MCP http.Handler

	// UIPath / SchemaPath 是静态资源路径；为空时对应端点返回 503。
	UIPath     string
	SchemaPath string
	// Version 随 /admin/state 暴露，便于确认页面连的是哪个进程。
	Version string

	audit *auditLog

	// modelCache 缓存上游模型列表，只服务读路径（见 cache.go）。
	// 写校验不走它：那必须基于当下事实。
	modelCache *modelCache

	// mu 串行化「读当前 → 校验 → 落盘」这一整段。
	// 只锁写路径：读路径无锁，走的是 Store 的原子快照。
	mu sync.Mutex

	// 管理端 MCP handler 惰性构建：Version 是构造后注入的，
	// 提前构建会把空版本号写进 tools/list 的 serverInfo。
	mcpOnce sync.Once
	mcpHTTP http.Handler
}

// New 构造管理端。
//
// 不再接收配置文件路径：覆盖层从 Store 读，写也写回 Store。
// 同一份文件只能有一个读者，这是「手改配置立即生效」的前提。
func New(store *config.Store) *Handler {
	return &Handler{
		store:      store,
		audit:      newAuditLog(store.Path()),
		modelCache: newModelCache(30 * time.Second),
	}
}

// SetModelCacheTTL 调整读路径缓存有效期。必须在开始服务之前调用。
//
// 缓存的是上游模型列表（≠ 合并结果），所以有效期只影响「看到的上游有多新」，
// 不影响覆盖层是否立即生效。默认 30 秒，够覆盖 agent 一次会话里的连续读取；
// 传 0 关闭缓存（每次读都直连上游）。
func (h *Handler) SetModelCacheTTL(ttl time.Duration) {
	h.modelCache = newModelCache(ttl)
}

// Overlays 返回当前覆盖层（只读快照）。
func (h *Handler) Overlays() catalog.Overlays { return h.store.Overlays() }

// Revision 返回当前覆盖层版本。
func (h *Handler) Revision() string { return h.store.Revision() }

// ── 路由表 ────────────────────────────────────────────────────────────────

// route 是一行路由定义。表本身就是 manifest 的数据源，
// 所以「文档里写了但没实现」「实现了但没文档」这两种漂移在结构上不可能发生。
type route struct {
	Method     string
	Path       string
	Summary    string
	Idempotent bool
	Public     bool // 不走服务身份鉴权
	Body       any  // 示例请求体，进 manifest
	Handler    http.HandlerFunc
}

func (h *Handler) routes() []route {
	return []route{
		{Method: "GET", Path: "/admin/manifest", Idempotent: true,
			Summary: "自描述清单：端点、操作、错误码、推荐流程。agent 的第一站，不拉上游，很便宜",
			Handler: h.handleManifest},

		{Method: "GET", Path: "/admin/config", Idempotent: true,
			Summary: "读取覆盖层 + 版本号（不拉上游）。拿到 revision 后才能安全地写",
			Handler: h.handleConfig},

		{Method: "GET", Path: "/admin/overlays", Idempotent: true,
			Summary: "只读覆盖层原文（兼容旧面板；新代码用 /admin/config）",
			Handler: h.handleOverlays},
		{Method: "PUT", Path: "/admin/overlays", Idempotent: true,
			Summary: "全量替换覆盖层（兼容旧面板）。agent 不要用：全量写会抹掉并发修改，请用 /admin/ops",
			Body: map[string]any{
				"models":    map[string]any{"doubao-seedream-5-0-flash-260915": map[string]any{"kind": "image"}},
				"settings":  map[string]any{"defaults": map[string]any{"image": "doubao-seedream-5-0-flash-260915"}},
				"disabled":  []string{},
				"protocols": map[string]any{},
			},
			Handler: h.handleOverlays},

		{Method: "POST", Path: "/admin/ops", Idempotent: true,
			Summary: "批量原子操作。全部成功才落盘；dry_run=true 只校验并返回差异。这是 agent 的主要写入口",
			Body: map[string]any{
				"dry_run": true,
				"actor":   "codex",
				"ops": []any{
					map[string]any{"op": "model.patch", "model": "doubao-seedream-5-0-flash-260915",
						"patch": map[string]any{"kind": "image", "input": map[string]any{"max_reference_images": 3}}},
					map[string]any{"op": "default.set", "kind": "image", "model": "doubao-seedream-5-0-flash-260915"},
				},
			},
			Handler: h.handleOps},

		{Method: "GET", Path: "/admin/models", Idempotent: true,
			Summary: "列表（投影，不含参数约束）：回答「有哪些、什么类型、支持哪些参数名」。?kind= 过滤，?include_unknown=1 含未声明类型，?refresh=1 绕过 30 秒缓存；?full=1 是导出/调试的逃生舱，agent 默认不要用",
			Handler: h.handleModels},
		{Method: "GET", Path: "/admin/models/{id...}", Idempotent: true,
			Summary: "详情（完整约束）：上游原样 / 覆盖层 / 合并结果三份视图 + 漂移分类。改任何字段之前必须看这里，确认这个值是上游给的还是配置改的；?refresh=1 绕过缓存",
			Handler: h.handleModelDetail},
		{Method: "PATCH", Path: "/admin/models/{id...}", Idempotent: true,
			Summary: "对单个模型做 RFC 7386 合并补丁（null 删键）。body 就是补丁本身，?dry_run=true 只校验",
			Body:    map[string]any{"kind": "video", "parameters": map[string]any{"duration": map[string]any{"min": 3, "max": 10}}},
			Handler: h.handlePatchModel},
		{Method: "DELETE", Path: "/admin/models/{id...}", Idempotent: true,
			Summary: "删除单个模型的整条覆盖层（回到继承上游/模板）。这不等于下架，下架用 /admin/disabled",
			Handler: h.handleDeleteModel},

		{Method: "PUT", Path: "/admin/settings/defaults/{kind}", Idempotent: true,
			Summary: "设置某 kind 的默认模型",
			Body:    map[string]any{"model": "doubao-seedream-5-0-flash-260915"},
			Handler: h.handleDefault},
		{Method: "DELETE", Path: "/admin/settings/defaults/{kind}", Idempotent: true,
			Summary: "取消某 kind 的默认模型（运行时回落到第一个）",
			Handler: h.handleDefault},

		{Method: "POST", Path: "/admin/disabled/{id...}", Idempotent: true,
			Summary: "下架某模型：上游仍在列，但对下游不开放。重复调用无副作用",
			Handler: h.handleDisabled},
		{Method: "DELETE", Path: "/admin/disabled/{id...}", Idempotent: true,
			Summary: "恢复某模型（从下架列表移除）",
			Handler: h.handleDisabled},

		{Method: "PATCH", Path: "/admin/protocols/{name...}", Idempotent: true,
			Summary: "对协议族模板做合并补丁（只补缺失键，不覆盖上游已声明的字段）",
			Body:    map[string]any{"execution": map[string]any{"task": "async", "transport": "contents_tasks"}},
			Handler: h.handleProtocol},
		{Method: "DELETE", Path: "/admin/protocols/{name...}", Idempotent: true,
			Summary: "删除协议族模板覆盖（回到内置模板）",
			Handler: h.handleProtocol},

		{Method: "GET", Path: "/admin/history", Idempotent: true,
			Summary: "变更记录（最近 limit 条，默认 50）。回答「谁在什么时候动了配置」",
			Handler: h.handleHistory},

		{Method: "GET", Path: "/admin/state", Idempotent: true,
			Summary: "一次返回服务端配置、上游模型、覆盖层、合并结果、漂移（与 /admin/models 共用同一份上游缓存，upstream.age_ms 标注年龄；?refresh=1 强制刷新）",
			Handler: h.handleState},
		{Method: "GET", Path: "/admin/drift", Idempotent: true,
			Summary: "配置漂移自查：configured / inherited / unconfigured / orphaned / disabled；?refresh=1 绕过缓存",
			Handler: h.handleDrift},
		{Method: "GET", Path: "/admin/schema", Idempotent: true,
			Summary: "模型能力契约原文（JSON Schema draft 2020-12）",
			Handler: h.handleSchema},
		{Method: "POST", Path: "/admin/debug/mcp", Idempotent: false,
			Summary: "把一条 JSON-RPC 透传到业务 MCP handler；计费工具需 X-Debug-Allow-Cost: yes",
			Handler: h.handleDebugMCP},
		{Method: "POST", Path: "/admin/mcp", Idempotent: false,
			Summary: "管理端 MCP（Streamable HTTP，无状态）：codex / workbuddy 直接连这里操作配置",
			Handler: h.serveAdminMCP},

		{Method: "GET", Path: "/admin/healthz", Idempotent: true, Public: true,
			Summary: "健康检查（唯一免鉴权的信息端点）",
			Handler: h.handleHealth},
		{Method: "GET", Path: "/admin/ui", Idempotent: true, Public: true,
			Summary: "托管单文件管理页面（每次读盘）",
			Handler: h.handleUI},
	}
}

// Routes 挂载管理端路由。
func (h *Handler) Routes(mux *http.ServeMux) {
	for _, rt := range h.routes() {
		handler := rt.Handler
		if !rt.Public {
			handler = h.auth(handler)
		}
		mux.Handle(rt.Method+" "+rt.Path, handler)
	}
}

// auth 校验服务身份。这里的 key 是部署者凭据，不是用户令牌。
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := h.store.Get().APIKey
		if want == "" {
			// 没有服务身份时管理端不可用：宁可关掉，也不要无鉴权开放。
			http.Error(w, "管理端未启用：配置里缺少 api_key", http.StatusServiceUnavailable)
			return
		}
		if bearer(r) != want {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ── 写管线 ────────────────────────────────────────────────────────────────

// commitRequest 是一次写入的全部输入。
type commitRequest struct {
	Ops     []Op
	Actor   string
	Source  string // rest / mcp / panel
	Remote  string
	DryRun  bool
	IfMatch string
}

// commitResult 是写入结果。ok=false 时不会以 2xx 返回，只用于日志。
type commitResult struct {
	OK             bool              `json:"ok"`
	Saved          bool              `json:"saved"`
	DryRun         bool              `json:"dry_run"`
	Applied        int               `json:"applied"`
	NoChange       bool              `json:"no_change,omitempty"`
	Revision       string            `json:"revision"`
	RevisionBefore string            `json:"revision_before"`
	Changes        []Change          `json:"changes"`
	Truncated      bool              `json:"changes_truncated,omitempty"`
	Warnings       []catalog.Problem `json:"warnings,omitempty"`
	Note           string            `json:"note,omitempty"`
}

// maxChanges 限制返回的差异条数：一次 replace 53 个模型会产生几百条，
// 全塞进上下文对 agent 没有额外价值，前面的已经足够核对。
const maxChanges = 200

// commit 执行一次写入：对齐磁盘 → 版本校验 → 应用操作 → 校验合并结果 → 落盘 → 审计。
//
// 六步的顺序都是必需的：
//   - 先 Reload：多副本共享同一份文件时，别的副本刚写的内容要先看到。
//   - 再比 If-Match：拿旧基线算新状态，等于把别人刚改的东西抹掉。
//   - 校验放在落盘之前：这就是 §11「先校验后落盘」。
//   - 审计放在落盘之后：只有真正生效的变更才值得记（被拒绝的尝试单独记）。
func (h *Handler) commit(req commitRequest) (*commitResult, *apiError) {
	if len(req.Ops) == 0 {
		return nil, &apiError{
			Status: http.StatusBadRequest, Code: "invalid_request",
			Message: "ops 不能为空", Hint: "至少给一个操作；只读请用 GET /admin/config",
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.store.Reload()

	before := h.store.Overlays()
	current := h.store.Revision()
	if req.IfMatch != "" {
		if got := normalizeETag(req.IfMatch); got != current {
			return nil, &apiError{
				Status: http.StatusConflict, Code: "revision_mismatch",
				Message:      "配置已被其他客户端修改，本次写入未执行",
				Hint:         "重新 GET /admin/config 拿到新 revision，基于新值重算这次改动后重试；不要盲目重发",
				Revision:     current,
				YourRevision: got,
			}
		}
	}

	next, changes, err := Apply(before, req.Ops)
	if err != nil {
		var oe *opError
		if errors.As(err, &oe) {
			return nil, &apiError{
				Status:  http.StatusBadRequest,
				Code:    "invalid_op",
				Message: oe.Message,
				Hint:    oe.Hint,
				Allowed: OpNames,
				OpIndex: &oe.Index,
			}
		}
		return nil, &apiError{Status: http.StatusBadRequest, Code: "invalid_op", Message: err.Error()}
	}

	revAfter := config.Revision(next)
	capped, truncated := capChanges(changes)
	result := &commitResult{
		OK:             true,
		DryRun:         req.DryRun,
		Applied:        len(req.Ops),
		RevisionBefore: current,
		Revision:       current,
		Changes:        capped,
		Truncated:      truncated,
	}

	// 没有任何变化：不写盘、不动版本号，也不触发一次上游校验。
	// 这是幂等重试的常态，不该被当成一次失败。
	if len(changes) == 0 {
		result.NoChange = true
		result.Note = "操作未产生变化，未写盘"
		return result, nil
	}

	if h.Validate != nil {
		v := h.Validate(next)
		if len(v.Issues) > 0 {
			h.recordAudit(req, "rejected", current, revAfter, len(v.Issues), len(v.Warnings))
			return nil, &apiError{
				Status:   http.StatusUnprocessableEntity,
				Code:     "validation_failed",
				Message:  "校验未通过，未保存",
				Hint:     "按 issues[].path 改对应字段后重试；warnings 不会阻断保存",
				Issues:   v.Issues,
				Warnings: v.Warnings,
				Revision: current,
			}
		}
		result.Warnings = v.Warnings
	}

	if req.DryRun {
		result.Revision = revAfter
		h.recordAudit(req, "dry_run", current, revAfter, 0, len(result.Warnings))
		result.Note = "dry_run：未落盘。确认 changes 后去掉 dry_run 重发同一组 ops 即可提交"
		return result, nil
	}

	newRev, err := h.store.ReplaceOverlaysIf(next, current)
	if errors.Is(err, config.ErrRevisionChanged) {
		// 校验期间别人写了一次盘。这不是故障，是并发：让调用方重读再算。
		h.recordAudit(req, "conflict", current, "", 0, 0)
		return nil, &apiError{
			Status: http.StatusConflict, Code: "revision_mismatch",
			Message:      "写入期间配置被其他客户端修改，本次未保存",
			Hint:         "重新 GET /admin/config 拿新 revision，基于新值重算这次改动后重试",
			Revision:     newRev,
			YourRevision: req.IfMatch,
		}
	}
	if err != nil {
		h.recordAudit(req, "failed", current, "", 0, 0)
		return nil, &apiError{
			Status:   http.StatusInternalServerError,
			Code:     "persist_failed",
			Message:  "写入配置文件失败: " + err.Error(),
			Hint:     "配置目录不可写或磁盘满；内存仍保持旧值，可修复后重试",
			Revision: current,
		}
	}
	result.Revision = newRev
	result.Saved = true
	h.recordAudit(req, "saved", current, newRev, 0, len(result.Warnings))
	return result, nil
}

func capChanges(changes []Change) ([]Change, bool) {
	if len(changes) <= maxChanges {
		return changes, false
	}
	return changes[:maxChanges], true
}

func (h *Handler) recordAudit(req commitRequest, outcome, before, after string, issues, warnings int) {
	_ = h.audit.append(auditEntry{
		Actor:          actorOf(req),
		Source:         req.Source,
		Remote:         req.Remote,
		DryRun:         req.DryRun,
		Outcome:        outcome,
		Ops:            summarizeOps(req.Ops),
		RevisionBefore: before,
		RevisionAfter:  after,
		Issues:         issues,
		Warnings:       warnings,
	})
}

func actorOf(req commitRequest) string {
	if strings.TrimSpace(req.Actor) != "" {
		return strings.TrimSpace(req.Actor)
	}
	return "unknown"
}

// ── 兼容旧形态：整份覆盖层的读写 ──────────────────────────────────────────

func (h *Handler) handleOverlays(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("ETag", quoteETag(h.Revision()))
		writeJSON(w, http.StatusOK, h.Overlays())
	case http.MethodPut:
		body, err := readBody(w, r)
		if err != nil {
			writeAPIError(w, badRequest(err.Error()))
			return
		}
		var next catalog.Overlays
		if err := json.Unmarshal(body, &next); err != nil {
			writeAPIError(w, badRequest("解析覆盖层失败: "+err.Error()))
			return
		}
		// 兼容不认识 disabled / protocols 的旧管理端：请求体里完全没有这些键时
		// 保留现值，否则按 PUT 全量替换语义，面板每保存一次就会把它们清空。
		// 想清空必须显式传 [] / {}（null 也算显式清空）。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err == nil {
			cur := h.Overlays()
			if _, present := probe["disabled"]; !present {
				next.Disabled = cur.Disabled
			}
			if _, present := probe["protocols"]; !present {
				next.Protocols = cur.Protocols
			}
		}

		res, apiErr := h.commit(commitRequest{
			Ops:     []Op{{Op: "overlays.replace", Overlays: &next}},
			Actor:   actorFromRequest(r),
			Source:  "rest",
			Remote:  r.RemoteAddr,
			DryRun:  isDryRun(r),
			IfMatch: r.Header.Get("If-Match"),
		})
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		w.Header().Set("ETag", quoteETag(res.Revision))
		writeJSON(w, http.StatusOK, res)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeAPIError(w, &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed",
			Message: "method not allowed"})
	}
}

// ── 错误形态 ──────────────────────────────────────────────────────────────

// apiError 是给调用方看的错误：code 可编程判断，message 给人读，
// hint 说明「下一步怎么做」，path/issues 指出错在哪。
//
// agent 只能靠这些字段自我纠正，所以它们不是装饰：只有一句话的错误
// 会让 agent 陷入「重试同一个错」的循环。
type apiError struct {
	Status       int
	Code         string
	Message      string
	Hint         string
	Issues       []catalog.Problem
	Warnings     []catalog.Problem
	Allowed      []string
	Revision     string
	YourRevision string
	OpIndex      *int
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func badRequest(msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Message: msg}
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	body := map[string]any{
		"error": map[string]any{"code": e.Code, "message": e.Message},
	}
	if e.Hint != "" {
		body["error"].(map[string]any)["hint"] = e.Hint
	}
	if len(e.Allowed) > 0 {
		body["error"].(map[string]any)["allowed"] = e.Allowed
	}
	if e.OpIndex != nil {
		body["error"].(map[string]any)["op_index"] = *e.OpIndex
	}
	// issues / warnings 放顶层：旧面板按 data.issues 读，保持兼容。
	if len(e.Issues) > 0 {
		body["issues"] = e.Issues
	}
	if len(e.Warnings) > 0 {
		body["warnings"] = e.Warnings
	}
	if e.Revision != "" {
		body["revision"] = e.Revision
	}
	if e.YourRevision != "" {
		body["your_revision"] = e.YourRevision
	}
	writeJSON(w, e.Status, body)
}

// ── 小工具 ────────────────────────────────────────────────────────────────

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
}

func isDryRun(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("dry_run")))
	on, _ := strconv.ParseBool(v)
	return on
}

func actorFromRequest(r *http.Request) string {
	for _, k := range []string{"X-Admin-Actor", "X-Agent-Name"} {
		if v := strings.TrimSpace(r.Header.Get(k)); v != "" {
			return v
		}
	}
	if ua := strings.TrimSpace(r.UserAgent()); ua != "" {
		return ua
	}
	return "rest"
}

func normalizeETag(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "W/")
	return strings.Trim(v, `"`)
}

func quoteETag(rev string) string { return `"` + rev + `"` }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
