package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// 管理端也提供 MCP 入口：codex / workbuddy 这类客户端只要能连 MCP，
// 就不必为「改配置」写一套 HTTP 胶水代码，直接挂上这个地址即可。
//
// 与 REST 的关系是「同一份操作，两种传输」：工具内部还是走 commit，
// 校验、版本、审计一条都不会少。不存在「MCP 有后门」这种可能。
//
// 写工具默认 dry_run=true：这是刻意的摩擦。agent 拿到一个新工具时最容易
// 直接开写，默认空跑能保证第一次调用一定安全；要提交必须显式传 dry_run=false。

// serveAdminMCP 是 /admin/mcp 的入口：首次请求时构建，之后复用同一个 server。
func (h *Handler) serveAdminMCP(w http.ResponseWriter, r *http.Request) {
	h.mcpOnce.Do(func() { h.mcpHTTP = h.buildAdminMCP() })
	h.mcpHTTP.ServeHTTP(w, r)
}

// buildAdminMCP 返回管理端 MCP 的 HTTP handler（Streamable HTTP，无状态）。
func (h *Handler) buildAdminMCP() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-for-apiduo-admin", Version: h.Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "config.get",
		Description: "读取当前配置覆盖层与版本号（revision）。不拉上游，很便宜。" +
			"任何写操作之前先调它拿 revision，写的时候带上 if_match。",
		InputSchema: emptySchema(),
	}, h.mcpGet)

	mcp.AddTool(s, &mcp.Tool{
		Name: "config.models",
		Description: "列表：投影视图，回答「有哪些、什么类型、支持哪些参数名（params）」，不含参数约束。" +
			"要改某个字段先调 config.model 看详情（上游/覆盖层/合并三份视图），别凭列表猜约束。" +
			"要连未声明产物类型的模型一起看传 include_unknown=true；full=true 只用于导出，别默认用。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":            map[string]any{"type": "string", "description": "只列某一类：image / video / search；留空表示全部"},
				"include_unknown": map[string]any{"type": "boolean", "description": "是否包含上游没声明产物类型的模型（默认 false）"},
				"full":            map[string]any{"type": "boolean", "description": "返回完整合并画像而不是投影（默认 false）"},
				"refresh":         map[string]any{"type": "boolean", "description": "绕过读缓存，强制向上游拉一次（默认 false，缓存在 30 秒内有效）"},
			},
			"additionalProperties": false,
		},
	}, h.mcpModels)

	mcp.AddTool(s, &mcp.Tool{
		Name: "config.model",
		Description: "详情：单个模型的三份视图（上游原样 / 覆盖层 / 合并结果）+ 漂移分类，含完整参数约束。" +
			"这是改字段之前的必经一步：确认「这个值是上游给的还是配置改的」，以及 enum/min/max 的当前值。",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"id": map[string]any{"type": "string", "description": "模型 ID"}},
			"required":             []string{"id"},
			"additionalProperties": false,
		},
	}, h.mcpModel)

	mcp.AddTool(s, &mcp.Tool{
		Name: "config.drift",
		Description: "配置漂移自查：configured / inherited / unconfigured / orphaned / disabled。" +
			"改完配置用它确认模型是否真的进入了客户端可见列表。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"refresh": map[string]any{"type": "boolean", "description": "绕过读缓存，强制向上游拉一次（默认 false）"},
			},
			"additionalProperties": false,
		},
	}, h.mcpDrift)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "config.history",
		Description: "最近的配置变更记录：谁（actor）、什么时候、做了哪些操作、版本从哪到哪。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "返回条数，默认 20，最多 200"},
			},
			"additionalProperties": false,
		},
	}, h.mcpHistory)

	mcp.AddTool(s, &mcp.Tool{
		Name: "config.apply",
		Description: "批量原子操作配置。九个操作：model.patch / model.delete / default.set / default.delete / " +
			"disabled.add / disabled.remove / protocol.patch / protocol.delete / overlays.replace。" +
			"patch 是 RFC 7386 合并补丁（null 删键）。全部成功才落盘，任意一条不合格整批不生效。" +
			"默认 dry_run=true，只校验并返回 changes；确认无误后传 dry_run=false 提交。" +
			"注意：覆盖层只能描述上游存在的模型；max_reference_images 缺失表示未知、0 表示明确不支持。",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ops": map[string]any{
					"type":        "array",
					"description": "操作数组，按顺序应用。每项必须有 op 字段，其余字段随 op 而定",
					"minItems":    1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"op":       map[string]any{"type": "string", "enum": OpNames},
							"model":    map[string]any{"type": "string", "description": "模型 ID（model.* / disabled.* 用）"},
							"kind":     map[string]any{"type": "string", "description": "产物类型（default.* 用）"},
							"name":     map[string]any{"type": "string", "description": "协议族名（protocol.* 用）"},
							"patch":    map[string]any{"type": "object", "description": "合并补丁（model.patch 用）", "additionalProperties": true},
							"template": map[string]any{"type": "object", "description": "模板补丁（protocol.patch 用）", "additionalProperties": true},
							"overlays": map[string]any{"type": "object", "description": "完整覆盖层（overlays.replace 用）", "additionalProperties": true},
						},
						"required":             []string{"op"},
						"additionalProperties": false,
					},
				},
				"dry_run":  map[string]any{"type": "boolean", "default": true, "description": "默认 true 只校验；要落盘必须显式传 false"},
				"actor":    map[string]any{"type": "string", "description": "记录到变更日志里的操作者，建议填客户端名"},
				"if_match": map[string]any{"type": "string", "description": "上一步 config.get 拿到的 revision；不符则整批不执行（409）"},
			},
			"required":             []string{"ops"},
			"additionalProperties": false,
		},
	}, h.mcpApply)

	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

// ── 工具实现 ──────────────────────────────────────────────────────────────

type getOut struct{}

func (h *Handler) mcpGet(_ context.Context, _ *mcp.CallToolRequest, _ getOut) (*mcp.CallToolResult, any, error) {
	ov := h.store.Overlays()
	return nil, map[string]any{
		"ok":           true,
		"revision":     h.Revision(),
		"base_url":     h.store.Get().BaseURL,
		"overlays":     ov,
		"counts":       countsOf(ov),
		"config_error": h.store.LastError(),
	}, nil
}

type modelsIn struct {
	Kind           string `json:"kind"`
	IncludeUnknown bool   `json:"include_unknown"`
	Full           bool   `json:"full"`
	Refresh        bool   `json:"refresh"`
}

type modelIn struct {
	ID      string `json:"id"`
	Refresh bool   `json:"refresh"`
}

type driftIn struct {
	Refresh bool `json:"refresh"`
}

func (h *Handler) mcpModels(ctx context.Context, _ *mcp.CallToolRequest, in modelsIn) (*mcp.CallToolResult, any, error) {
	snap, apiErr := h.loadModels(ctx, in.Refresh)
	if apiErr != nil {
		return nil, mcpError(apiErr), nil
	}
	ov := h.store.Overlays()
	disabled := ov.DisabledSet()
	kind := catalog.NormalizeKind(in.Kind)
	out := make([]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		merged := ov.ApplyOne(m)
		if kind != "" && merged.Kind() != kind {
			continue
		}
		if !in.IncludeUnknown && merged.Kind() == catalog.KindUnknown {
			continue
		}
		if in.Full {
			out = append(out, merged)
			continue
		}
		out = append(out, modelBrief(merged, disabled[merged.ID()]))
	}
	return nil, map[string]any{
		"ok":              true,
		"revision":        h.Revision(),
		"upstream_count":  len(snap.Models),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"count":           len(out),
		"models":          out,
	}, nil
}

func (h *Handler) mcpModel(ctx context.Context, _ *mcp.CallToolRequest, in modelIn) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, mcpError(&apiError{Status: 400, Code: "invalid_request", Message: "缺少 id"}), nil
	}
	snap, apiErr := h.loadModels(ctx, in.Refresh)
	if apiErr != nil {
		return nil, mcpError(apiErr), nil
	}
	ov := h.store.Overlays()
	up, found := catalog.Find(snap.Models, id)
	overlay, hasOverlay := ov.Models[id]
	merged := catalog.Model{}
	if found {
		merged = ov.ApplyOne(up)
	}
	return nil, map[string]any{
		"ok":              true,
		"revision":        h.Revision(),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"id":              id,
		"upstream":        map[string]any{"found": found, "model": up},
		"overlay":         map[string]any{"present": hasOverlay, "patch": overlay},
		"merged":          merged,
		"editable":        found,
	}, nil
}

func (h *Handler) mcpDrift(ctx context.Context, _ *mcp.CallToolRequest, in driftIn) (*mcp.CallToolResult, any, error) {
	snap, apiErr := h.loadModels(ctx, in.Refresh)
	if apiErr != nil {
		return nil, mcpError(apiErr), nil
	}
	d := h.store.Overlays().Drift(snap.Models)
	return nil, map[string]any{
		"ok":              true,
		"revision":        h.Revision(),
		"upstream_count":  len(snap.Models),
		"upstream_age_ms": snap.Age.Milliseconds(),
		"configured":      d.Configured,
		"inherited":       d.Inherited,
		"unconfigured":    d.Unconfigured,
		"orphaned":        d.Orphaned,
		"disabled":        d.Disabled,
		"counts": map[string]int{
			"configured": len(d.Configured), "inherited": len(d.Inherited),
			"unconfigured": len(d.Unconfigured), "orphaned": len(d.Orphaned), "disabled": len(d.Disabled),
		},
	}, nil
}

type historyIn struct {
	Limit int `json:"limit"`
}

func (h *Handler) mcpHistory(_ context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	return nil, map[string]any{"ok": true, "revision": h.Revision(), "entries": h.audit.History(limit)}, nil
}

type applyIn struct {
	Ops     []map[string]any `json:"ops"`
	DryRun  *bool            `json:"dry_run"`
	Actor   string           `json:"actor"`
	IfMatch string           `json:"if_match"`
}

func (h *Handler) mcpApply(_ context.Context, _ *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, any, error) {
	raw, err := json.Marshal(in.Ops)
	if err != nil {
		return nil, mcpError(badRequest("ops 无法序列化: " + err.Error())), nil
	}
	var ops []Op
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil, mcpError(badRequest("ops 解析失败: " + err.Error())), nil
	}

	// 默认空跑：MCP 写工具第一次被调用时最危险，必须先看到差异。
	dryRun := true
	if in.DryRun != nil {
		dryRun = *in.DryRun
	}
	res, apiErr := h.commit(commitRequest{
		Ops:     ops,
		Actor:   orDefault(in.Actor, "mcp"),
		Source:  "mcp",
		DryRun:  dryRun,
		IfMatch: in.IfMatch,
	})
	if apiErr != nil {
		return nil, mcpError(apiErr), nil
	}
	return nil, map[string]any{
		"ok":              true,
		"saved":           res.Saved,
		"dry_run":         res.DryRun,
		"no_change":       res.NoChange,
		"revision":        res.Revision,
		"revision_before": res.RevisionBefore,
		"changes":         res.Changes,
		"warnings":        res.Warnings,
		"note":            res.Note,
	}, nil
}

// mcpError 把 API 错误转成工具结果里的结构化对象（而不是协议层错误）。
//
// 协议层错误会让不少客户端直接中断这一轮对话；配置校验失败是
// 「可纠正的失败」，应该把 path / hint 原样交回给模型让它自己改。
func mcpError(e *apiError) map[string]any {
	out := map[string]any{
		"ok":     false,
		"status": e.Status,
		"error":  map[string]any{"code": e.Code, "message": e.Message},
	}
	obj := out["error"].(map[string]any)
	if e.Hint != "" {
		obj["hint"] = e.Hint
	}
	if len(e.Allowed) > 0 {
		obj["allowed"] = e.Allowed
	}
	if len(e.Issues) > 0 {
		out["issues"] = e.Issues
	}
	if len(e.Warnings) > 0 {
		out["warnings"] = e.Warnings
	}
	if e.Revision != "" {
		out["revision"] = e.Revision
	}
	if e.YourRevision != "" {
		out["your_revision"] = e.YourRevision
	}
	return out
}
