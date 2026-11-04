package admin

import "net/http"

// handleManifest 是 agent 的入口：一次拿到「有哪些操作、怎么用、会怎么错」。
//
// 多数管理 API 只暴露端点，剩下的靠人读文档转述给模型。这里的取舍不同：
// 端点表从路由表生成（不会漂移），再补上操作语义、错误码与常用流程。
// 目标是让一个没有上下文的 agent 只读这一个端点就能正确操作配置。
func (h *Handler) handleManifest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("ETag", quoteETag(h.Revision()))
	writeJSON(w, http.StatusOK, h.Manifest())
}

// Manifest 返回自描述清单。
func (h *Handler) Manifest() map[string]any {
	cfg := h.store.Get()
	endpoints := make([]map[string]any, 0, 8)
	for _, rt := range h.routes() {
		e := map[string]any{
			"method":     rt.Method,
			"path":       rt.Path,
			"summary":    rt.Summary,
			"idempotent": rt.Idempotent,
			"auth":       "service_bearer",
		}
		if rt.Public {
			e["auth"] = "none"
		}
		if rt.Body != nil {
			e["example_body"] = rt.Body
		}
		endpoints = append(endpoints, e)
	}

	return map[string]any{
		"service": map[string]any{
			"name":     "mcp-for-apiduo admin",
			"version":  h.Version,
			"base_url": cfg.BaseURL,
			"revision": h.Revision(),
			"mcp":      "/admin/mcp",
		},
		"auth": map[string]any{
			"type":                "bearer",
			"header":              "Authorization: Bearer <api_key>",
			"note":                "这是部署者的服务身份 key，不是用户令牌。用户令牌进不了管理端。",
			"status_when_missing": 503,
			"status_when_wrong":   401,
		},
		"conventions": map[string]any{
			"content_type": "application/json",
			"revision": "每次响应带 revision；写请求可选带 If-Match: <revision>，不符返回 409 且不落盘。" +
				"并发场景（面板 + agent 同时改）必须带。",
			"dry_run":       "POST /admin/ops 传 dry_run=true，或任意写端点加 ?dry_run=true：只校验、返回 changes，不落盘。",
			"patch":         "PATCH 的 body 是 RFC 7386 合并补丁：对象递归合并，null 删除键，数组整体替换。",
			"partial_write": "一组 ops 要么全部生效，要么一条都不生效；被拒绝时磁盘与内存都保持原状。",
			"idempotency":   "所有写操作都是幂等的：同一组 ops 重发得到同样的状态与同样的 revision。不需要 Idempotency-Key。",
			"empty_means":   "覆盖层只存差异。没有任何变化的写入不会改动 revision（no_change=true）。",
			"expensive":     "写端点在校验时必然拉一次上游（校验对象是合并结果，不走缓存）。",
			"cache": "读端点把上游模型列表缓存 " + h.modelCache.ModelCacheTTL().String() +
				"（-models-cache-ttl 可调，0 关闭）；响应里的 upstream_age_ms 就是这份列表的年龄，" +
				"0 表示本次刚拉。缓存的是上游事实、不是合并结果 —— 覆盖层改动立刻可见；" +
				"过期后拉取失败会如实报错，绝不用旧值兜底。" +
				"要立刻拿最新（例如刚在上游加了渠道），GET 加 ?refresh=1、MCP 工具传 refresh=true：" +
				"只刷新这一次读，不会清空缓存让并发请求一起 miss。",
			"list_vs_detail": "两段式读取：列表（GET /admin/models）是投影 —— 回答「有哪些、什么类型、支持哪些参数名」，不含参数约束；" +
				"要改字段就先 GET /admin/models/{id} 看三份视图（上游/覆盖层/合并），确认这个值是上游给的还是配置改的。" +
				"第一次请求足够支撑「要不要改它」，第二次只在你真的要动它时才发；已知 id 时直接调详情，列表只用来筛候选。" +
				"?full=1 能一次拿全，但那是给人导出和调试用的逃生舱。",
		},
		"endpoints":   endpoints,
		"ops":         opCatalog(),
		"error_codes": errorCatalog(),
		"workflows":   workflows(),
		"rules":       rules(),
	}
}

// opCatalog 描述每个操作的字段与语义。
func opCatalog() []map[string]any {
	return []map[string]any{
		{
			"op": "model.patch", "target": "models.<id>",
			"fields":  map[string]any{"model": "模型 ID（必填）", "patch": "合并补丁（必填，非空对象）"},
			"effect":  "把 patch 合并进该模型的覆盖层；null 删键；合并后为空则整条删除",
			"example": map[string]any{"op": "model.patch", "model": "m1", "patch": map[string]any{"kind": "image", "input": map[string]any{"max_reference_images": 3}}},
		},
		{
			"op": "model.delete", "target": "models.<id>",
			"fields":  map[string]any{"model": "模型 ID（必填）"},
			"effect":  "删除该模型的整条覆盖层，回到继承协议模板 / 上游",
			"example": map[string]any{"op": "model.delete", "model": "m1"},
		},
		{
			"op": "default.set", "target": "settings.defaults.<kind>",
			"fields":  map[string]any{"kind": "image / video / search", "model": "模型 ID"},
			"effect":  "设置该 kind 的默认模型。默认模型必须能在上游找到且类型相符，否则 422",
			"example": map[string]any{"op": "default.set", "kind": "image", "model": "m1"},
		},
		{
			"op": "default.delete", "target": "settings.defaults.<kind>",
			"fields":  map[string]any{"kind": "image / video / search"},
			"effect":  "取消默认模型，运行时回落到该 kind 的第一个（不推荐长期依赖）",
			"example": map[string]any{"op": "default.delete", "kind": "video"},
		},
		{
			"op": "disabled.add", "target": "disabled[]",
			"fields":  map[string]any{"model": "模型 ID"},
			"effect":  "下架：上游仍在列，但对客户端不可见。重复调用无副作用",
			"example": map[string]any{"op": "disabled.add", "model": "m1"},
		},
		{
			"op": "disabled.remove", "target": "disabled[]",
			"fields":  map[string]any{"model": "模型 ID"},
			"effect":  "恢复下架。上游本来就没了的模型不会因此复活",
			"example": map[string]any{"op": "disabled.remove", "model": "m1"},
		},
		{
			"op": "protocol.patch", "target": "protocols.<name>",
			"fields": map[string]any{"name": "协议族名（内置 image / video，或自定义名）", "template": "部分能力画像"},
			"effect": "合并进模板；模板只给缺失字段兜底，不覆盖上游已声明的值",
			"example": map[string]any{"op": "protocol.patch", "name": "video",
				"template": map[string]any{"execution": map[string]any{"task": "async", "transport": "video_generations"}}},
		},
		{
			"op": "protocol.delete", "target": "protocols.<name>",
			"fields":  map[string]any{"name": "协议族名"},
			"effect":  "删除模板覆盖，回到内置模板（image / video 有内置，其余名删了就没了）",
			"example": map[string]any{"op": "protocol.delete", "name": "ark-video"},
		},
		{
			"op": "overlays.replace", "target": "整份覆盖层",
			"fields":  map[string]any{"overlays": "完整的 Overlays 对象"},
			"effect":  "全量替换。只在导入 / 迁移时用；日常改动请用上面八个定点操作",
			"example": map[string]any{"op": "overlays.replace", "overlays": map[string]any{"models": map[string]any{}, "settings": map[string]any{"defaults": map[string]any{}}}},
		},
	}
}

func errorCatalog() []map[string]any {
	return []map[string]any{
		{"code": "invalid_request", "status": 400, "meaning": "请求体不是合法 JSON，或缺少必填字段",
			"recovery": "按 error.message 修正请求形状；形状见 endpoints[].example_body"},
		{"code": "invalid_op", "status": 400, "meaning": "ops 里有不认识的操作或缺少字段",
			"recovery": "看 error.allowed 与 error.op_index，改用 ops 清单里的操作"},
		{"code": "revision_mismatch", "status": 409, "meaning": "If-Match 与当前版本不符：别人先改了",
			"recovery": "GET /admin/config 拿新 revision，基于新状态重新计算这次改动再提交；不要原样重发"},
		{"code": "validation_failed", "status": 422, "meaning": "操作本身合法，但合并结果会让运行时做错事",
			"recovery": "按 issues[].path 改对应字段；issues[].hint 通常直接给出可选值"},
		{"code": "persist_failed", "status": 500, "meaning": "写配置文件失败，内存仍是旧值",
			"recovery": "配置目录不可写或磁盘满；修好后重试同一组 ops"},
		{"code": "upstream_unavailable", "status": 502, "meaning": "拉不到上游 /v1/models，无法给出事实",
			"recovery": "上游恢复后重试；管理端不会拿缓存冒充事实"},
		{"code": "upstream_not_wired", "status": 503, "meaning": "该端点需要上游查询能力，但进程没注入",
			"recovery": "检查服务启动参数与配置"},
		{"code": "missing_service_key", "status": 503, "meaning": "配置里没有 api_key，管理端整体不可用",
			"recovery": "设置 api_key（部署者凭据），重启进程"},
		{"code": "method_not_allowed", "status": 405, "meaning": "方法不对",
			"recovery": "看 Allow 头或 endpoints 清单"},
	}
}

// workflows 是「目标 → 具体请求」的配方。
//
// 只列端点等于给了零件不给图纸；agent 最容易错的不是字段名，
// 而是顺序（先读版本还是先写、要不要先看漂移）和「什么时候该停」。
func workflows() []map[string]any {
	return []map[string]any{
		{
			"goal": "上游新增了一个模型，但没声明产物类型，客户端看不到它",
			"steps": []any{
				map[string]any{"do": "找到它", "method": "GET", "path": "/admin/models?include_unknown=1"},
				map[string]any{"do": "先空跑，确认只改这一处", "method": "POST", "path": "/admin/ops",
					"body": map[string]any{"dry_run": true, "ops": []any{
						map[string]any{"op": "model.patch", "model": "<id>", "patch": map[string]any{"kind": "video"}},
					}}},
				map[string]any{"do": "去掉 dry_run 提交（带 If-Match）", "method": "POST", "path": "/admin/ops",
					"headers": map[string]any{"If-Match": "<上一步返回的 revision_before>"},
					"body": map[string]any{"ops": []any{
						map[string]any{"op": "model.patch", "model": "<id>", "patch": map[string]any{"kind": "video"}},
					}}},
				map[string]any{"do": "确认它出现在客户端列表里", "method": "GET", "path": "/admin/drift"},
			},
			"stop_if": "validation_failed 里出现「上游不存在」——说明 ID 写错了，不要靠试错猜 ID",
		},
		{
			"goal": "调整某个模型的能力上限（垫图张数、duration 范围等）—— 两段式：先列表筛出候选，再按 id 看详情",
			"steps": []any{
				map[string]any{"do": "看三份视图，确认哪个值是上游给的", "method": "GET", "path": "/admin/models/<id>"},
				map[string]any{"do": "只发改动的那几个叶子", "method": "PATCH", "path": "/admin/models/<id>?dry_run=true",
					"body": map[string]any{"input": map[string]any{"max_reference_images": 3}}},
				map[string]any{"do": "确认后用同样的 body 正式提交", "method": "PATCH", "path": "/admin/models/<id>"},
			},
			"stop_if": "不确定某参数是否被上游支持：宁可不写。键缺失表示「未知、不拦截」，写 0 表示「明确不支持」",
		},
		{
			"goal": "临时下架一个模型（上游仍在列，但不想让客户端看到）",
			"steps": []any{
				map[string]any{"do": "下架", "method": "POST", "path": "/admin/disabled/<id>"},
				map[string]any{"do": "恢复", "method": "DELETE", "path": "/admin/disabled/<id>"},
			},
			"stop_if": "上游已经不再列出它：那不需要下架，它会自动消失；别用 disabled 当换版补偿",
		},
		{
			"goal": "一批模型都缺 kind（例如 53 个聚合网关模型）",
			"steps": []any{
				map[string]any{"do": "一次提交全部改动（原子）", "method": "POST", "path": "/admin/ops",
					"body": map[string]any{"dry_run": true, "ops": []any{
						map[string]any{"op": "model.patch", "model": "<id1>", "patch": map[string]any{"kind": "image"}},
						map[string]any{"op": "model.patch", "model": "<id2>", "patch": map[string]any{"kind": "video"}},
					}},
					"note": "任意一条不合格则整批不落盘；这就是为什么批量要用一个请求而不是循环单发"},
			},
			"stop_if": "issues 里出现大量「上游不存在」——先确认当前连的是哪个 base_url，别在错误的部署上批量改",
		},
		{
			"goal": "确认「我刚改的」是否真的生效",
			"steps": []any{
				map[string]any{"do": "看版本与差异", "method": "GET", "path": "/admin/config"},
				map[string]any{"do": "看谁改的、什么时候改的", "method": "GET", "path": "/admin/history?limit=20"},
				map[string]any{"do": "看与上游的差异分类", "method": "GET", "path": "/admin/drift"},
			},
		},
	}
}

// rules 是操作配置时必须遵守的硬约束。这些不是建议，违反会导致静默错误。
func rules() []string {
	return []string{
		"覆盖层只能描述上游存在的模型。配了 /v1/models 里没有的 ID 不会让它变得可调用：保存时是 warning，运行时静默忽略。",
		"可见性是两层 AND：上游列出 ∧ 未被 disabled。配置中心改不了「上游有没有」这个事实。",
		"覆盖层只存差异。不要抄一整份模型对象进来，否则上游新增的尺寸/参数永远传不到客户端。",
		"键缺失 ≠ 0。max_reference_images 缺失表示「未知、不拦截」，0 表示「明确不支持垫图」。用 0 表达「还没填」会锁死图生图。",
		"disabled 的唯一用途是「上游仍在列，本地主动停用」。上游下架的模型会自动消失，不需要也不会被它救回来。",
		"协议模板只补缺失键，不覆盖上游已声明的字段；模型级覆盖优先级最高。",
		"覆盖层全局只有一份。并发写入必须带 If-Match；不带就是 last-writer-wins，可能抹掉别人的改动。",
		"管理端点不会触发生成调用，不产生费用。要真跑生成用 POST /admin/debug/mcp，且必须带 X-Debug-Allow-Cost: yes。",
		"目录里 base_url 决定带着服务 key 往哪发请求。改它等于改信任边界，只应在部署层由人操作。",
	}
}
