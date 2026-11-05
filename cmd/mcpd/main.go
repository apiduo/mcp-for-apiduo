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
		playground = flag.String("playground", "prototype/playground.html", "客户调试台页面文件路径；为空则不托管")
		schemaPath = flag.String("schema", "packages/model-capabilities.schema.json", "模型能力契约文件路径；为空则不提供 /admin/schema")
		cacheTTL   = flag.Duration("models-cache-ttl", 30*time.Second, "管理端读路径缓存上游模型列表的时长；0 表示关闭")
		trustProxy = flag.String("trust-proxy", "", "在反向代理后面时设为 x-real-ip，审计日志才会记真实来源（默认关闭）")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store, err := config.NewStore(*cfgPath, 2*time.Second)
	if err != nil {
		log.Error("配置加载失败", "err", err)
		os.Exit(1)
	}
	// 注意：Start 放到所有回调注册之后。轮询一旦跑起来就会触发回调，
	// 那时 listeners 还在被追加，既会漏发也可能产生数据竞争。
	defer store.Close()

	cfg := store.Get().Redacted()
	log.Info("启动", "version", version, "base_url", cfg.BaseURL, "api_key", cfg.APIKey, "config", *cfgPath)

	up := upstream.New(store)

	adm := admin.New(store)
	adm.Version = version
	adm.UIPath = *uiPath
	adm.SchemaPath = *schemaPath
	adm.SetModelCacheTTL(*cacheTTL)
	adm.TrustProxy = strings.EqualFold(strings.TrimSpace(*trustProxy), "x-real-ip")

	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-for-apiduo", Version: version}, nil)
	deps := &tools.Deps{Up: up, Overlays: adm.Overlays, CacheTTL: 60 * time.Second}
	tools.Register(server, deps)

	// 热更新与管理端保存走同一条通知：缓存里是旧上游的模型列表时，
	// base_url 与覆盖层任一变化都必须失效它。两者共用一个 Store 之后，
	// 这里只剩一个注册点 —— 不会再出现「面板保存了但缓存没失效」。
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

	store.Start()

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", injectKey(httpHandler, store))

	// 调试端点走与真实客户端完全相同的 handler；injectKey 保证用服务身份。
	adm.MCP = injectKey(httpHandler, store)
	mcpMux.HandleFunc("/healthz", publicHealth(store, version))

	// 客户调试台与 /mcp 同源部署：页面拿客户自己的 Key 直接打 /mcp，
	// 因此不需要给公网 API 开跨域口子（CORS 一开就是所有站点）。
	// frame-ancestors 放行 https 宿主，为将来嵌进 new-api 留位置；
	// 管理页仍然禁止嵌入（它存着服务身份）。
	mcpMux.HandleFunc("/playground", func(w http.ResponseWriter, r *http.Request) {
		admin.ServePage(w, r, *playground, "'self' https: http://localhost:* http://127.0.0.1:*")
	})

	adminMux := http.NewServeMux()
	adm.Routes(adminMux)

	// 超时按「请求侧封闭、响应侧不封」配：
	//   ReadHeaderTimeout / ReadTimeout 挡住 slowloris（发完头再按字节拖 body）；
	//   IdleTimeout 回收空转连接；
	//   **不设 WriteTimeout** —— 同步生成最长 5 分钟，写超时会把正常的长任务掐断。
	//   MaxHeaderBytes 从默认 1MB 收到 64KB：调试报文都在 body 里，头不需要那么大。
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
	}
	mcpSrv := newServer(*addr, mcpMux)
	adminSrv := newServer(*adminAddr, admin.CORS(adminMux))

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

// publicHealth 是公网探活。它只回答「活着吗、哪个版本」。
//
// 早先这里返回 base_url 与 config_error：前者暴露内网服务名（http://apiduo:3000/v1），
// 后者会把配置路径、YAML 解析行号一并吐到公网。要排障走 /admin/healthz（需服务身份）。
func publicHealth(store *config.Store, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"version":   version,
			"config_ok": store.LastError() == "",
		})
	}
}

// injectKey 把客户端令牌放进 context。
//
// 三条规则，缺一不可：
//   - 没有 Authorization 头：回落到配置里的服务身份（单租户内网的常态）。
//   - `Bearer <token>`：用客户端的（多用户）。
//   - 头在、但语法不对：**401，绝不回落**。
//
// 第三条是安全边界，不是洁癖。公网入口的 nginx 只判断「这个头非空」，
// 所以 `Authorization: x` 能一路穿到这里；此时若回落到服务身份，
// 任何人发一个乱写的头就等于拿到了部署者的凭证 —— 匿名即可用服务额度生成。
// 「解析失败」与「没带」是两件事，混在一起就变成了鉴权绕过。
func injectKey(next http.Handler, store *config.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, present := bearer(r)
		if !present {
			key = store.Get().APIKey
		} else if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "Authorization 头不是 `Bearer <token>`：不要发送空令牌或裸 scheme",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(tools.WithAPIKey(r.Context(), key)))
	})
}

// validateOverlays 校验「覆盖层合并后的结果」，并区分阻断项与漂移告警。
//
// 覆盖层本身不是合法模型对象（缺 id/object），拿完整 schema 校验它必然报错；
// 所以要对着上游模型逐条合并后再校验。
//
// 每条问题都带 Path：管理端把它直接回给调用方（包括 agent），
// 「哪一条配置错了」必须是机器可寻址的，否则只能靠人猜。
func validateOverlays(ctx context.Context, up *upstream.Client, store *config.Store, ov catalog.Overlays) admin.Validation {
	var out admin.Validation
	issue := func(path, msg, hint string) {
		out.Issues = append(out.Issues, catalog.Problem{Path: path, Message: msg, Hint: hint})
	}
	warn := func(path, msg, hint string) {
		out.Warnings = append(out.Warnings, catalog.Problem{Path: path, Message: msg, Hint: hint})
	}

	// 结构层面先看默认值是否合理（不依赖上游）。
	for kind := range ov.Settings.Defaults {
		switch kind {
		case "image", "video", "search":
		default:
			issue("/settings/defaults/"+catalog.EscapePointer(kind),
				"settings.defaults 含未知类型: "+kind,
				"默认模型只能按 image / video / search 设置")
		}
	}

	// 协议模板自身先做结构校验（不依赖上游）：enum 为空、min>max、垫图负数等
	// 要在这里拦住，否则会一次性污染所有继承它的模型。
	for name, tpl := range ov.Protocols {
		probe := catalog.Model{"id": "__protocol__" + name}
		for k, v := range tpl {
			probe[k] = v
		}
		for _, p := range catalog.ValidateProblems(probe) {
			p.Path = "/protocols/" + catalog.EscapePointer(name) + p.Path
			out.Issues = append(out.Issues, p)
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
		issue("", "拉取上游模型失败，无法校验："+err.Error(),
			"管理端不拿缓存冒充事实；等上游可用后重试")
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
		base := "/models/" + catalog.EscapePointer(id)
		if name, _ := patch["_protocol"].(string); strings.TrimSpace(name) != "" {
			if !ov.HasProtocol(strings.TrimSpace(name)) {
				warn(base+"/_protocol", "模型 "+id+" 引用的协议族 "+name+" 不存在（运行时退化为无模板）",
					"改成已存在的协议族名，或先用 protocol.patch 建一份模板")
			}
		}
		m, ok := catalog.Find(raw, id)
		if !ok {
			// 运行时 Apply 会静默忽略它，不会造成错误调用；只提示，不阻断保存。
			warn(base, "覆盖层包含上游不存在的模型（已忽略）: "+id,
				"上游列表才是事实来源；确认 ID 拼写，或等上游上线后再配")
			continue
		}
		// 与运行时共用同一条合并链：模板补缺 → 模型级覆盖，避免两处判定不一致。
		merged := ov.ApplyOne(m)
		for _, p := range catalog.ValidateProblems(merged) {
			p.Path = base + p.Path
			out.Issues = append(out.Issues, p)
		}
	}
	for id := range disabled {
		if _, ok := catalog.Find(raw, id); !ok {
			warn("/disabled/"+catalog.EscapePointer(id), "下架列表含上游不存在的模型（已忽略）: "+id,
				"上游下架的模型会自动消失，不需要下架；从列表里删掉即可")
		}
	}

	for kind, id := range ov.Settings.Defaults {
		if id == "" {
			continue
		}
		path := "/settings/defaults/" + catalog.EscapePointer(kind)
		m, ok := catalog.Find(raw, id)
		if !ok {
			// PickDefault 会安全回落到第一个，不阻断。
			warn(path, "默认 "+kind+" 模型当前不在上游: "+id+"（运行时回落）",
				"换一个上游存在的同类型模型，避免默认值随上游列表顺序漂移")
			continue
		}
		if disabled[id] {
			issue(path, "默认 "+kind+" 模型 "+id+" 已被下架，请改默认值或恢复该模型",
				"先用 disabled.remove 恢复它，或改设另一个默认模型")
			continue
		}
		if k := m.Kind(); k != kind {
			if k == catalog.KindUnknown {
				k = "未知（上游未声明能力字段）"
			}
			issue(path, "默认 "+kind+" 模型 "+id+" 的实际类型是 "+k,
				"默认模型必须与 kind 相符；不确定就先 GET /admin/models?include_unknown=1")
		}
	}
	return out
}

// bearer 解析 Authorization，第二个返回值区分「头不存在」与「头存在但不可用」。
//
// 只返回字符串无法表达这个区别，调用方就必然把两者当成同一件事 ——
// 这正是上一个版本的漏洞所在。
func bearer(r *http.Request) (token string, present bool) {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return "", false
	}
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		if tok := strings.TrimSpace(h[len(p):]); tok != "" {
			return tok, true
		}
	}
	return "", true
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
