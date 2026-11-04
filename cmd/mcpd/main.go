// Command mcpd 是 MCP 前置层服务。
//
// 一个进程两个端口：
//   - MCP 端口（默认 :8080）：/mcp 走 Streamable HTTP，无状态，可多副本。
//   - 管理端口（默认 :8081）：/admin/*，只在内网暴露，用服务身份 api_key 校验。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/apiduo/mcp-for-apiduo/internal/admin"
	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/config"
	"github.com/apiduo/mcp-for-apiduo/internal/tools"
	"github.com/apiduo/mcp-for-apiduo/internal/upstream"
)

var version = "0.1.0-dev"

func main() {
	var (
		cfgPath    = flag.String("config", os.Getenv("MCP_CONFIG_FILE"), "配置文件路径；设置后启用热更新")
		addr       = flag.String("addr", envOr("MCP_HTTP_ADDR", ":8080"), "MCP 监听地址")
		adminAddr  = flag.String("admin-addr", envOr("MCP_ADMIN_ADDR", ":8081"), "管理端监听地址")
		uiPath     = flag.String("ui", "prototype/admin.html", "管理端页面文件路径；为空则不托管")
		schemaPath = flag.String("schema", "packages/model-capabilities.schema.json", "模型能力契约文件路径；为空则不提供 /admin/schema")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, err := config.NewStore(*cfgPath, 2*time.Second)
	if err != nil {
		log.Error("配置加载失败", "err", err)
		os.Exit(1)
	}
	store.Start()
	defer store.Close()

	cfg := store.Get().Redacted()
	log.Info("启动", "version", version, "base_url", cfg.BaseURL, "api_key", cfg.APIKey, "config", *cfgPath)

	up := upstream.New(store)

	adm, err := admin.New(*cfgPath, store)
	if err != nil {
		log.Error("管理端初始化失败", "err", err)
		os.Exit(1)
	}
	adm.Version = version
	adm.UIPath = *uiPath
	adm.SchemaPath = *schemaPath

	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-for-apiduo", Version: version}, nil)
	deps := &tools.Deps{Up: up, Overlays: adm.Overlays, CacheTTL: 60 * time.Second}
	tools.Register(server, deps)

	// 管理端保存后：失效模型缓存，并校验「合并结果」而不是覆盖层本身。
	adm.OnChange = deps.InvalidateCache
	// 配置重载（base_url 变了）同样要失效：缓存里是旧上游的模型列表。
	store.OnReload(deps.InvalidateCache)
	adm.Validate = func(ov catalog.Overlays) admin.Validation {
		return validateOverlays(context.Background(), up, store, ov)
	}
	// 漂移自查与管理端判定必须用同一份归一化结果，否则「上游有但没配」会对不上。
	adm.Models = func(ctx context.Context, key string) ([]catalog.Model, error) {
		models, err := up.ListModels(ctx, key)
		if err != nil {
			return nil, err
		}
		for i := range models {
			models[i] = catalog.Normalize(models[i])
		}
		return models, nil
	}

	httpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		// Stateless：不读不写 Mcp-Session-Id，任何副本都能处理任何请求。
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", injectKey(httpHandler, store))

	// 调试端点走与真实客户端完全相同的 handler；injectKey 保证用服务身份。
	adm.MCP = injectKey(httpHandler, store)
	mcpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		c := store.Get().Redacted()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"version":      version,
			"base_url":     c.BaseURL,
			"config_error": store.LastError(),
		})
	})

	adminMux := http.NewServeMux()
	adm.Routes(adminMux)

	mcpSrv := &http.Server{Addr: *addr, Handler: mcpMux, ReadHeaderTimeout: 10 * time.Second}
	adminSrv := &http.Server{Addr: *adminAddr, Handler: admin.CORS(adminMux), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() {
		log.Info("MCP 端口监听", "addr", *addr, "path", "/mcp")
		if err := mcpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		log.Info("管理端口监听", "addr", *adminAddr, "ui", "http://127.0.0.1"+*adminAddr+"/admin/ui")
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Error("服务异常退出", "err", err)
	case sig := <-stop:
		log.Info("收到信号，开始关闭", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = mcpSrv.Shutdown(ctx)
	_ = adminSrv.Shutdown(ctx)
	log.Info("已退出")
}

// injectKey 把客户端令牌放进 context。
//
// 客户端带了就用客户端的（多用户），没带才回落到配置里的服务身份（单租户）。
func injectKey(next http.Handler, store *config.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		if key == "" {
			key = store.Get().APIKey
		}
		next.ServeHTTP(w, r.WithContext(tools.WithAPIKey(r.Context(), key)))
	})
}

// validateOverlays 校验「覆盖层合并后的结果」，并区分阻断项与漂移告警。
//
// 覆盖层本身不是合法模型对象（缺 id/object），拿完整 schema 校验它必然报错；
// 所以要对着上游模型逐条合并后再校验。
func validateOverlays(ctx context.Context, up *upstream.Client, store *config.Store, ov catalog.Overlays) admin.Validation {
	var out admin.Validation

	// 结构层面先看默认值是否合理（不依赖上游）。
	for kind, id := range ov.Settings.Defaults {
		if id == "" {
			continue
		}
		switch kind {
		case "image", "video", "search":
		default:
			out.Issues = append(out.Issues, "settings.defaults 含未知类型: "+kind)
		}
	}
	// 协议模板自身先做结构校验（不依赖上游）：enum 为空、min>max、垫图负数等
	// 要在这里拦住，否则会一次性污染所有继承它的模型。
	for name, tpl := range ov.Protocols {
		probe := catalog.Model{"id": "__protocol__" + name}
		for k, v := range tpl {
			probe[k] = v
		}
		for _, e := range catalog.Validate(probe) {
			out.Issues = append(out.Issues, "protocols."+name+": "+e)
		}
	}
	// 只有「什么都没配」时才可跳过上游校验。早先只看 len(ov.Models)，
	// 导致「只改默认模型」「只改下架列表」「只改协议模板」时校验被静默跳过。
	hasDefault := false
	for _, id := range ov.Settings.Defaults {
		if id != "" {
			hasDefault = true
			break
		}
	}
	if len(ov.Models) == 0 && len(ov.Disabled) == 0 && len(ov.Protocols) == 0 && !hasDefault && len(out.Issues) == 0 {
		return out
	}

	key := store.Get().APIKey
	if key == "" {
		// 没有服务身份就拉不到上游模型：只做结构校验，不阻断保存。
		return out
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	raw, err := up.ListModels(ctx, key)
	if err != nil {
		// 事实不可得时保守阻断：连合并结果都验不了，不该落盘。
		out.Issues = append(out.Issues, "拉取上游模型失败，无法校验："+err.Error())
		return out
	}
	// 必须先归一化，否则与管理端的判定不一致：
	// 真实上游（火山 Ark）不在 /models 里给 kind，归一化前它是空字符串，
	// 于是「默认 video 模型」会被判成类型不符而拒绝保存，而运行时却是正常的。
	for i, m := range raw {
		raw[i] = catalog.Normalize(m)
	}
	disabled := ov.DisabledSet()

	for id, patch := range ov.Models {
		if name, _ := patch["_protocol"].(string); strings.TrimSpace(name) != "" {
			if !ov.HasProtocol(strings.TrimSpace(name)) {
				out.Warnings = append(out.Warnings, "模型 "+id+" 引用的协议族 "+name+" 不存在（运行时退化为无模板）")
			}
		}
		m, ok := catalog.Find(raw, id)
		if !ok {
			// 运行时 Apply 会静默忽略它，不会造成错误调用；只提示，不阻断保存。
			out.Warnings = append(out.Warnings, "覆盖层包含上游不存在的模型（已忽略）: "+id)
			continue
		}
		// 与运行时共用同一条合并链：模板补缺 → 模型级覆盖，避免两处判定不一致。
		merged := ov.ApplyOne(m)
		for _, e := range catalog.Validate(merged) {
			out.Issues = append(out.Issues, id+": "+e)
		}
	}
	for id := range disabled {
		if _, ok := catalog.Find(raw, id); !ok {
			out.Warnings = append(out.Warnings, "下架列表含上游不存在的模型（已忽略）: "+id)
		}
	}

	for kind, id := range ov.Settings.Defaults {
		if id == "" {
			continue
		}
		m, ok := catalog.Find(raw, id)
		if !ok {
			// PickDefault 会安全回落到第一个，不阻断。
			out.Warnings = append(out.Warnings, "默认 "+kind+" 模型当前不在上游: "+id+"（运行时回落）")
			continue
		}
		if disabled[id] {
			out.Issues = append(out.Issues, "默认 "+kind+" 模型 "+id+" 已被下架，请改默认值或恢复该模型")
			continue
		}
		if k := m.Kind(); k != kind {
			if k == catalog.KindUnknown {
				k = "未知（上游未声明能力字段）"
			}
			out.Issues = append(out.Issues, "默认 "+kind+" 模型 "+id+" 的实际类型是 "+k)
		}
	}
	return out
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
