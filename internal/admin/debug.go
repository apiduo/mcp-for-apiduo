// Package admin 的真实调试能力：状态聚合、Schema 下发、MCP 报文透传、页面托管。
//
// 设计边界：
//   - 调试端点全部走服务身份鉴权，不下放给用户令牌；管理端口只在内网暴露。
//   - 上游拉不动时如实标注 ok=false，不拿缓存或种子数据冒充事实（README §14 教训）。
//   - MCP 调用在同一进程内走与真实客户端完全相同的 handler，不是另写一份「模拟实现」。
//   - 生成类工具可能计费：前端勾选确认 + 后端强制 X-Debug-Allow-Cost 双闸，缺一不可。
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// stateResponse 是一次 GET /admin/state 的全部事实。
//
// 分开 ok / error 而不是让整个请求失败：上游挂了也要能看到覆盖层与服务端配置，
// 但绝不能把「上次拉到的模型」当成这次的事实返回。
type stateResponse struct {
	Server   stateServer      `json:"server"`
	Upstream stateUpstream    `json:"upstream"`
	Overlays catalog.Overlays `json:"overlays"`
	Merged   []catalog.Model  `json:"merged"`
	Drift    stateDrift       `json:"drift"`
}

type stateServer struct {
	Version      string            `json:"version"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"` // 脱敏
	ConfigError  string            `json:"config_error"`
	Revision     string            `json:"revision"` // 覆盖层版本，写请求用它做 If-Match
	OverlayCount int               `json:"overlay_count"`
	Defaults     map[string]string `json:"defaults"`
	Time         string            `json:"time"`
}

type stateUpstream struct {
	OK     bool            `json:"ok"`
	Count  int             `json:"count"`
	AgeMS  int64           `json:"age_ms"` // 这份列表的年龄：0 = 本次刚拉
	Models []catalog.Model `json:"models"`
	Error  string          `json:"error,omitempty"`
}

type stateDrift struct {
	OK           bool     `json:"ok"`
	Configured   []string `json:"configured"`
	Inherited    []string `json:"inherited"`
	Unconfigured []string `json:"unconfigured"`
	Orphaned     []string `json:"orphaned"`
	Disabled     []string `json:"disabled"`
	Error        string   `json:"error,omitempty"`
}

// handleState 一次返回「服务端配置 + 上游模型 + 覆盖层 + 后端合并结果 + 漂移」。
//
// 同时返回后端合并结果，是为了让页面能断言前端合并规则与 Go 侧 catalog.Merge 一致 ——
// 配置页最危险的状态是「页面显示一套、客户端拿到另一套」。
func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := h.store.Get().Redacted()
	ov := h.Overlays()
	out := stateResponse{
		Server: stateServer{
			Version:      h.Version,
			BaseURL:      cfg.BaseURL,
			APIKey:       cfg.APIKey,
			ConfigError:  h.store.LastError(),
			Revision:     h.Revision(),
			OverlayCount: len(ov.Models),
			Defaults:     ov.Settings.Defaults,
			Time:         time.Now().Format(time.RFC3339),
		},
		Upstream: stateUpstream{Models: []catalog.Model{}},
		Overlays: ov,
		Merged:   []catalog.Model{},
		Drift: stateDrift{
			Configured: []string{}, Inherited: []string{},
			Unconfigured: []string{}, Orphaned: []string{}, Disabled: []string{},
		},
	}
	if out.Server.Defaults == nil {
		out.Server.Defaults = map[string]string{}
	}

	// 与 /admin/models 共用同一份上游视图（同一个缓存）。
	// 分成两套会让「面板显示 59 个、agent 列表显示 58 个」变成常态 ——
	// 同一进程里不该有两个真相。要最新就传 ?refresh=1。
	snap, apiErr := h.loadModels(r.Context(), parseBool(r.URL.Query().Get("refresh")))
	if apiErr != nil {
		out.Upstream.Error = apiErr.Message
		out.Drift.Error = "上游不可用，差异视图不可用"
		writeJSON(w, http.StatusOK, out)
		return
	}
	models := snap.Models

	out.Upstream.OK = true
	out.Upstream.Count = len(models)
	out.Upstream.AgeMS = snap.Age.Milliseconds()
	out.Upstream.Models = models
	out.Merged = ov.Apply(models)
	d := ov.Drift(models)
	out.Drift = stateDrift{
		OK:           true,
		Configured:   d.Configured,
		Unconfigured: d.Unconfigured,
		Orphaned:     d.Orphaned,
		Disabled:     d.Disabled,
		Inherited:    d.Inherited,
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSchema 下发仓库里那份唯一的机器可读契约。
//
// 前端不内嵌 schema 副本：三处共用一份定义才是 README §11 的前提。
func (h *Handler) handleSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.SchemaPath == "" {
		http.Error(w, "未配置 schema 文件路径（启动参数 -schema）", http.StatusServiceUnavailable)
		return
	}
	data, err := os.ReadFile(h.SchemaPath)
	if err != nil {
		http.Error(w, "读取 schema 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !json.Valid(data) {
		http.Error(w, "schema 文件不是合法 JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/schema+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Schema-Path", h.SchemaPath)
	_, _ = w.Write(data)
}

// costTools 是会真实产生上游生成/检索调用的工具。
//
// 后端这道闸独立于前端勾选：脚本、误触、旧页面都拦得住。
// models.list / tools/list / task.get 是只读查询，不在此列。
var costTools = map[string]bool{
	"image.generate": true,
	"video.generate": true,
	"search.query":   true,
}

// handleDebugMCP 把一条 JSON-RPC 原样送进同一进程内的 MCP handler。
//
// 返回体里同时带原始请求与原始响应（含 HTTP 状态、耗时），页面拿到的东西
// 与真实客户端一致；不在这里重新实现一遍工具逻辑。
func (h *Handler) handleDebugMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.MCP == nil {
		http.Error(w, "未注入 MCP handler，调试端点不可用", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "读取请求体失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(w, "请求体必须是 JSON-RPC JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if probe.Method == "" {
		http.Error(w, "缺少 method", http.StatusBadRequest)
		return
	}
	if costTools[probe.Params.Name] && r.Header.Get("X-Debug-Allow-Cost") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error": "工具 " + probe.Params.Name + " 会真实调用上游（可能产生费用）；请先在页面勾选确认，并带上 X-Debug-Allow-Cost: yes",
			"tool":  probe.Params.Name,
		})
		return
	}

	// 与上游客户端一致的 5 分钟：图片/视频生成是慢操作，
	// 早先这里是 2 分钟，视频任务在调试台里必然超时，看起来像「上游挂了」。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	inner := r.Clone(ctx)
	inner.Body = io.NopCloser(bytes.NewReader(body))
	inner.ContentLength = int64(len(body))
	inner.Header = r.Header.Clone()
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Set("Accept", "application/json, text/event-stream")
	if inner.Header.Get("MCP-Protocol-Version") == "" {
		inner.Header.Set("MCP-Protocol-Version", "2025-06-18")
	}

	rec := newCaptureWriter()
	start := time.Now()
	h.MCP.ServeHTTP(rec, inner)
	elapsed := time.Since(start)

	status := rec.code
	if status == 0 {
		status = http.StatusOK
	}
	respBody := rec.body.Bytes()
	if key := h.store.Get().APIKey; key != "" {
		respBody = bytes.ReplaceAll(respBody, []byte(key), []byte("***"))
	}

	var reqJSON, respJSON any
	_ = json.Unmarshal(body, &reqJSON)
	if len(bytes.TrimSpace(respBody)) > 0 {
		if json.Valid(respBody) {
			_ = json.Unmarshal(respBody, &respJSON)
		} else {
			respJSON = string(respBody)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          status >= 200 && status < 300,
		"status":      status,
		"duration_ms": elapsed.Milliseconds(),
		"transport":   "in-process",
		"identity":    "service",
		"request":     reqJSON,
		"response":    respJSON,
		"error":       debugError(probe.Method, probe.Params.Name, respJSON),
	})
}

// debugError 从 MCP 错误体里抽一句人话，页面不必解析 JSON-RPC 的嵌套结构。
func debugError(method, name string, resp any) string {
	m, ok := resp.(map[string]any)
	if !ok {
		return ""
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		return ""
	}
	msg, _ := e["message"].(string)
	if name != "" {
		return fmt.Sprintf("%s: %s", name, msg)
	}
	return fmt.Sprintf("%s: %s", method, msg)
}

// handleUI 托管单文件管理页面。每次请求读盘，改页面无需重启进程。
func (h *Handler) handleUI(w http.ResponseWriter, r *http.Request) {
	if h.UIPath == "" {
		http.Error(w, "未配置页面路径（启动参数 -ui）", http.StatusServiceUnavailable)
		return
	}
	ServePage(w, r, h.UIPath, "'none'")
}

// ServePage 托管单文件页面：每次读盘、禁缓存、统一安全头。
//
// 两个页面的差别只有 frame-ancestors：
//   - 管理面板存着服务身份 key，禁止任何页面嵌入（'none'）；
//   - 客户调试台未来要嵌进 new-api，允许 https 宿主与 localhost（本地联调）。
//
// 允许嵌入的页面不能下发 X-Frame-Options（它不支持白名单，DENY 会把
// frame-ancestors 一起废掉）；禁止嵌入的页面仍然补一条，给不看 CSP 的老客户端。
func ServePage(w http.ResponseWriter, r *http.Request, path, frameAncestors string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if path == "" {
		http.Error(w, "未配置页面路径（启动参数 -ui / -playground）", http.StatusServiceUnavailable)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "读取页面失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if frameAncestors == "" {
		frameAncestors = "'none'"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 页面是公开的，而管理页把服务身份 key 存在 sessionStorage 里 ——
	// 一旦有脚本注入，等同于交出凭证。CSP 不防「页面自己写的内联脚本」
	// （单文件页面必须允许 inline），但能挡住外链脚本、eval、以及把数据发去第三方。
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"img-src 'self' data: blob: https: http://localhost:* http://127.0.0.1:*; "+
			"media-src 'self' data: blob: https: http://localhost:* http://127.0.0.1:*; "+
			"font-src 'self' data:; "+
			"connect-src 'self' https: http://localhost:* http://127.0.0.1:*; "+
			"base-uri 'none'; form-action 'none'; frame-ancestors "+frameAncestors)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if frameAncestors == "'none'" {
		w.Header().Set("X-Frame-Options", "DENY")
	}
	_, _ = w.Write(data)
}

// captureWriter 是进程内 HTTP 调用的响应收集器。
// 实现 Flush 以兼容检查 http.Flusher 的流式 handler。
type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newCaptureWriter() *captureWriter {
	return &captureWriter{header: make(http.Header)}
}
func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(b)
}
func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}
func (c *captureWriter) Flush() {}

// CORS 只决定「浏览器能不能把响应交给脚本」，不承担鉴权。
//
// 这里踩过一个真实的坑：把 Origin 当成鉴权闸门，非本机来源一律 403。
// 结果是线上面板自己也不能保存 —— 它跑在 https://mcp.apiduo.cn，
// 浏览器对**同源**的 POST/PUT 同样会带 Origin（GET 不带），于是每一个写请求
// 都被自己挡掉，表现成「读得到、写不了」，而日志里只有一行 origin not allowed。
//
// 第一性原理：凭证是 Bearer，存在 sessionStorage 里。跨站页面读不到它，
// 也过不了预检；所以非白名单来源根本不需要 403 —— 它没有凭证，只会拿到 401。
// 这里只做两件事：同源直接放行；白名单来源补 CORS 头让脚本能读响应。
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r) // 同源请求或非浏览器客户端
			return
		}
		// 本机调试来源（含 file:// 的 null）才补 CORS 头。
		// 其余来源不补，也不需要拒绝：没有 Bearer 它只能拿到 401。
		if localOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, If-Match, X-Admin-Actor, X-Agent-Name, X-Debug-Allow-Cost, MCP-Protocol-Version, Mcp-Session-Id")
			// 浏览器里读 ETag 需要显式放行，否则脚本拿不到版本号。
			w.Header().Set("Access-Control-Expose-Headers", "ETag")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		// 预检统一在这里收口：白名单来源已带 ACAO，其他来源没有，浏览器自己会拦。
		// 交给 mux 只会得到一个 405，反而像是服务坏了。
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localOrigin(raw string) bool {
	if raw == "null" { // file:// 打开的本地页面
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
