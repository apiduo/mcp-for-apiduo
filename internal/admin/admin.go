// Package admin 是控制面板的后端：模型覆盖层与全局设置的读写。
//
// 约束（README §11）：
//   - 独立端口、只在内网暴露，用服务身份的 api_key 校验，不建第二套账号。
//   - 校验对象是「合并结果」：读覆盖层 → 与上游合并 → 校验 → 通过才落盘。
//   - 写操作先校验后落盘，失败保留旧值。
package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/config"
)

// Validation 把「会让运行时做错事」和「配置与事实脱节」分开。
//
// Issues 必须拦：kind 非法、参数约束矛盾、默认值类型不符。
// Warnings 不拦：孤儿覆盖层条目、默认模型当前不在上游 —— 运行时对这两者都有安全回落
// （Apply 静默忽略 / PickDefault 回落第一个），硬拦等于拿瞬时上游快照否决持久配置意图。
type Validation struct {
	Issues   []string `json:"issues,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Handler 提供覆盖层读写。
type Handler struct {
	path  string // 配置文件路径；为空时覆盖层只存在内存里
	store *config.Store

	mu  sync.Mutex
	cur atomic.Pointer[catalog.Overlays]

	// Validate 由上层注入：给定覆盖层，校验每个合并结果。
	// Issues 阻断落盘，Warnings 随 200 返回给管理端。
	Validate func(catalog.Overlays) Validation

	// OnChange 在保存成功后调用（失效模型缓存等）。
	OnChange func()

	// Models 由上层注入：拉取上游模型列表，供配置漂移自查。
	// 为 nil 时 /admin/drift 返回 503，不猜测上游。
	Models func(context.Context, string) ([]catalog.Model, error)

	// MCP 是同一进程内的 MCP Streamable HTTP handler，供 /admin/debug/mcp 透传。
	// 必须由上层注入；为 nil 时调试端点返回 503，不做任何「模拟实现」。
	MCP http.Handler

	// UIPath / SchemaPath 是静态资源路径；为空时对应端点返回 503。
	UIPath     string
	SchemaPath string
	// Version 随 /admin/state 暴露，便于确认页面连的是哪个进程。
	Version string
}

// New 从配置文件加载初始覆盖层。
func New(path string, store *config.Store) (*Handler, error) {
	h := &Handler{path: path, store: store}
	ov := catalog.Overlays{Models: map[string]map[string]any{}, Settings: catalog.Settings{Defaults: map[string]string{}}}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if len(data) > 0 {
			var doc struct {
				Models    map[string]map[string]any `yaml:"models"`
				Settings  catalog.Settings          `yaml:"settings"`
				Disabled  []string                  `yaml:"disabled"`
				Protocols map[string]map[string]any `yaml:"protocols"`
			}
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return nil, err
			}
			if doc.Models != nil {
				ov.Models = doc.Models
			}
			if doc.Settings.Defaults != nil {
				ov.Settings = doc.Settings
			}
			if doc.Disabled != nil {
				ov.Disabled = doc.Disabled
			}
			if doc.Protocols != nil {
				ov.Protocols = doc.Protocols
			}
		}
	}
	h.cur.Store(&ov)
	return h, nil
}

// Overlays 返回当前覆盖层（只读快照）。
func (h *Handler) Overlays() catalog.Overlays {
	return *h.cur.Load()
}

// Routes 挂载管理端路由。
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/overlays", h.auth(h.handleOverlays))
	mux.HandleFunc("/admin/drift", h.auth(h.handleDrift))
	mux.HandleFunc("/admin/healthz", h.handleHealth)
	mux.HandleFunc("/admin/state", h.auth(h.handleState))
	mux.HandleFunc("/admin/schema", h.auth(h.handleSchema))
	mux.HandleFunc("/admin/debug/mcp", h.auth(h.handleDebugMCP))
	mux.HandleFunc("/admin/ui", h.handleUI)
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

func (h *Handler) handleOverlays(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.Overlays())
	case http.MethodPut:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "读取请求体失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		var next catalog.Overlays
		if err := json.Unmarshal(body, &next); err != nil {
			http.Error(w, "解析覆盖层失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		// 兼容不认识 disabled / protocols 的旧管理端：请求体里完全没有这些键时
		// 保留现值，否则按 PUT 全量替换语义，面板每保存一次就会把它们清空。
		// 想清空必须显式传 [] / {}（null 也算显式清空）。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err == nil {
			if _, present := probe["disabled"]; !present {
				next.Disabled = h.Overlays().Disabled
			}
			if _, present := probe["protocols"]; !present {
				next.Protocols = h.Overlays().Protocols
			}
		}
		if next.Models == nil {
			next.Models = map[string]map[string]any{}
		}
		if next.Settings.Defaults == nil {
			next.Settings.Defaults = map[string]string{}
		}
		if next.Disabled == nil {
			next.Disabled = []string{}
		}
		if next.Protocols == nil {
			next.Protocols = map[string]map[string]any{}
		}

		warnings := []string{}
		if h.Validate != nil {
			v := h.Validate(next)
			if len(v.Issues) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"error":  "校验未通过，未保存",
					"issues": v.Issues,
				})
				return
			}
			warnings = append(warnings, v.Warnings...)
		}

		if err := h.save(next); err != nil {
			http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		h.cur.Store(&next)
		if h.OnChange != nil {
			h.OnChange()
		}
		writeJSON(w, http.StatusOK, map[string]any{"saved": true, "warnings": warnings})
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// save 先写临时文件再 rename，避免读到半截内容。
//
// 只更新 models / settings / disabled / protocols 四段，保留文件里其他内容（base_url / api_key 等）。
func (h *Handler) save(next catalog.Overlays) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.path == "" {
		return nil // 纯内存模式
	}

	doc := map[string]any{}
	if data, err := os.ReadFile(h.path); err == nil {
		_ = yaml.Unmarshal(data, &doc)
	}
	doc["models"] = next.Models
	doc["settings"] = next.Settings
	doc["disabled"] = next.Disabled
	doc["protocols"] = next.Protocols

	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

// handleDrift 返回配置中心与上游的差异，是「前期全靠人工配置」这条路线的最低运维闭环。
//
// 只做只读查询，不改任何状态；上游拉不动时如实报错，不拿缓存冒充事实。
func (h *Handler) handleDrift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Models == nil {
		http.Error(w, "未启用上游模型查询", http.StatusServiceUnavailable)
		return
	}
	key := h.store.Get().APIKey
	if key == "" {
		http.Error(w, "缺少服务身份 api_key，无法查询上游", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := h.Models(ctx, key)
	if err != nil {
		// 上游错误体可能回显请求头，落盘前抹掉服务 key。
		msg := strings.ReplaceAll(err.Error(), key, "***")
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "拉取上游模型失败: " + msg})
		return
	}
	d := h.Overlays().Drift(models)
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream_count": len(models),
		"configured":     d.Configured,
		"unconfigured":   d.Unconfigured,
		"orphaned":       d.Orphaned,
		"disabled":       d.Disabled,
		"inherited":      d.Inherited,
		"counts": map[string]int{
			"configured":   len(d.Configured),
			"unconfigured": len(d.Unconfigured),
			"orphaned":     len(d.Orphaned),
			"disabled":     len(d.Disabled),
			"inherited":    len(d.Inherited),
		},
	})
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := h.store.Get().Redacted()
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":     cfg.BaseURL,
		"api_key":      cfg.APIKey,
		"config_error": h.store.LastError(),
		"models":       len(h.Overlays().Models),
		"disabled":     len(h.Overlays().Disabled),
		"protocols":    len(h.Overlays().Protocols),
		"defaults":     h.Overlays().Settings.Defaults,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}
