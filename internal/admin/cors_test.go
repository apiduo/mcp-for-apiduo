package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCORSServer 把完整的路由表挂上 CORS，模拟线上那一层。
func newCORSServer(t *testing.T) (http.Handler, *Handler) {
	t.Helper()
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	return CORS(mux), h
}

// TestCORSAllowsSameOriginWrites 是线上故障的回归测试。
//
// 线上面板跑在 https://mcp.apiduo.cn，浏览器对同源的 POST/PUT 也会带 Origin。
// 旧实现把所有「非本机」来源一律 403，于是面板读得到、写不了。
func TestCORSAllowsSameOriginWrites(t *testing.T) {
	srv, _ := newCORSServer(t)

	cases := []struct {
		name, method, path, body string
		wantNot                  int
		want                     int
	}{
		{name: "同源 GET", method: http.MethodGet, path: "/admin/config", want: http.StatusOK},
		{name: "同源 POST", method: http.MethodPost, path: "/admin/ops", body: `{"ops":[]}`, want: http.StatusBadRequest},
		{name: "同源 PUT", method: http.MethodPut, path: "/admin/overlays", body: `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "https://mcp.apiduo.cn"+c.path, strings.NewReader(c.body))
			req.Host = "mcp.apiduo.cn"
			req.Header.Set("Origin", "https://mcp.apiduo.cn")
			req.Header.Set("Authorization", "Bearer "+testServiceKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "origin") {
				t.Fatalf("同源请求被 CORS 挡下：%d %s", rec.Code, rec.Body.String())
			}
			if c.want != 0 && rec.Code != c.want {
				t.Fatalf("状态码 %d，期望 %d（body=%s）", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestCORSDoesNotGateForeignOrigin 确认「跨站来源不被 403」。
//
// 403 是错的：它既挡不住真正的攻击者（非浏览器客户端根本不发 Origin），
// 又会让任何反向代理/自定义域名部署直接坏掉。鉴权由 Bearer 负责 ——
// 没有令牌的跨站请求应当走到鉴权并拿到 401。
func TestCORSDoesNotGateForeignOrigin(t *testing.T) {
	srv, _ := newCORSServer(t)

	req := httptest.NewRequest(http.MethodPost, "https://mcp.apiduo.cn/admin/ops", strings.NewReader(`{"ops":[]}`))
	req.Host = "mcp.apiduo.cn"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证的跨站请求应 401，得到 %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("非白名单来源不应拿到 CORS 头，得到 %q", got)
	}

	// 有凭证的非浏览器客户端（curl / agent）不受 Origin 影响。
	req2 := httptest.NewRequest(http.MethodGet, "https://mcp.apiduo.cn/admin/config", nil)
	req2.Host = "mcp.apiduo.cn"
	req2.Header.Set("Origin", "https://some-other-deployment.example")
	req2.Header.Set("Authorization", "Bearer "+testServiceKey)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("带凭证的跨站请求应正常处理，得到 %d %s", rec2.Code, rec2.Body.String())
	}
}

// TestCORSStillAllowsLocalPreflight 确认本机调试来源（file:// 与 localhost）不受影响。
func TestCORSStillAllowsLocalPreflight(t *testing.T) {
	srv, _ := newCORSServer(t)

	for _, origin := range []string{"null", "http://localhost:5173", "http://127.0.0.1:39091"} {
		req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:39091/admin/ops", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s 预检状态码 %d", origin, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Fatalf("%s 预检未放行，ACAO=%q", origin, got)
		}
		if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Fatalf("%s 预检未放行 Authorization 头", origin)
		}
	}

	// 非白名单来源的预检：收回 204，但不发 CORS 头，浏览器自己会拦。
	req := httptest.NewRequest(http.MethodOptions, "https://mcp.apiduo.cn/admin/ops", nil)
	req.Host = "mcp.apiduo.cn"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("非白名单来源不该拿到 CORS 头")
	}
}

// TestPublicHealthDoesNotLeakKey 锁住「匿名端点不吐凭证」。
// 旧实现返回掩码后的 key（sk-s***ey）与 base_url，公网人人可读。
func TestPublicHealthDoesNotLeakKey(t *testing.T) {
	srv, _ := newCORSServer(t)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:39091/admin/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-s") || strings.Contains(body, "***") {
		t.Fatalf("公开探活不应出现密钥的任何片段：%s", body)
	}
	if strings.Contains(body, "base_url") {
		t.Fatalf("公开探活不应暴露 base_url：%s", body)
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if doc["ok"] != true {
		t.Fatalf("ok 应为 true：%v", doc)
	}
}

// TestUIShipsSecurityHeaders 页面是公开的，而它把 key 存在 sessionStorage 里。
// 没有 CSP / 禁止 framing 时，一次注入就等于交出凭证。
func TestUIShipsSecurityHeaders(t *testing.T) {
	srv, h := newCORSServer(t)
	h.UIPath = filepath.Join(t.TempDir(), "ui.html")
	if err := os.WriteFile(h.UIPath, []byte("<!doctype html><title>t</title>"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:39091/admin/ui", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s=%q，期望 %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, must := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP 缺少 %q：%s", must, csp)
		}
	}
}
