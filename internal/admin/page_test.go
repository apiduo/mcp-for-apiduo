package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ServePage 是两个公开页面的唯一出口：管理页禁止嵌入，调试台允许 https 宿主。
// 这几个断言盯的是「不能把两个页面的边界写反」。
func TestServePage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "page.html")
	if err := os.WriteFile(path, []byte("<!doctype html><title>page</title>"), 0o644); err != nil {
		t.Fatal(err)
	}

	serve := func(t *testing.T, method, p, frame string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/playground", nil)
		ServePage(w, r, p, frame)
		return w
	}

	t.Run("调试台允许 https 宿主嵌入", func(t *testing.T) {
		w := serve(t, http.MethodGet, path, "'self' https:")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Fatalf("content-type = %q", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("cache-control = %q，页面每次读盘，不能被缓存", got)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'self' https:") {
			t.Fatalf("csp 缺少嵌入白名单: %q", csp)
		}
		// 视频产物要能播
		if !strings.Contains(csp, "media-src") {
			t.Fatalf("csp 缺少 media-src，视频结果会被拦: %q", csp)
		}
		// X-Frame-Options 不支持白名单，发了会把 frame-ancestors 一起废掉
		if got := w.Header().Get("X-Frame-Options"); got != "" {
			t.Fatalf("不应下发 X-Frame-Options，got %q", got)
		}
		if w.Body.String() != "<!doctype html><title>page</title>" {
			t.Fatalf("body = %q", w.Body.String())
		}
	})

	t.Run("空 frameAncestors 回落到禁止嵌入", func(t *testing.T) {
		w := serve(t, http.MethodGet, path, "")
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("默认必须是 'none': %q", csp)
		}
	})

	t.Run("只接受 GET", func(t *testing.T) {
		w := serve(t, http.MethodPost, path, "'none'")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("code = %d", w.Code)
		}
		if got := w.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("allow = %q", got)
		}
	})

	t.Run("未配置路径", func(t *testing.T) {
		w := serve(t, http.MethodGet, "", "'none'")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d", w.Code)
		}
	})

	t.Run("文件不存在", func(t *testing.T) {
		w := serve(t, http.MethodGet, filepath.Join(dir, "nope.html"), "'none'")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d", w.Code)
		}
	})
}
