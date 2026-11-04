package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/config"
	"github.com/apiduo/mcp-for-apiduo/internal/tools"
)

const serviceKey = "sk-service-identity"

func newTestStore(t *testing.T) *config.Store {
	t.Helper()
	t.Setenv("BASE_URL", "http://127.0.0.1:3000/v1")
	t.Setenv("API_KEY", serviceKey)
	st, err := config.NewStore("", 0)
	if err != nil {
		t.Fatalf("构造 config.Store 失败: %v", err)
	}
	return st
}

// TestInjectKeyIdentity 锁住「谁的身份」这条边界。
//
// 这里曾经有一个鉴权绕过：公网 nginx 只判断 Authorization 非空，
// 而服务端把「解析不出来」当成「没带」，回落到服务身份。
// 于是 `Authorization: x` 就能匿名使用部署者的额度。
func TestInjectKeyIdentity(t *testing.T) {
	store := newTestStore(t)

	cases := []struct {
		name   string
		header string
		set    bool
		want   int
		key    string // 期望 context 里的身份；空表示不该进入下游
	}{
		{name: "没带：回落到服务身份", set: false, want: http.StatusOK, key: serviceKey},
		{name: "正常 Bearer：用客户端的", header: "Bearer sk-user", set: true, want: http.StatusOK, key: "sk-user"},
		{name: "大小写不敏感", header: "bearer sk-user", set: true, want: http.StatusOK, key: "sk-user"},
		{name: "裸 scheme", header: "Bearer", set: true, want: http.StatusUnauthorized},
		{name: "空令牌", header: "Bearer ", set: true, want: http.StatusUnauthorized},
		{name: "只有空格", header: "Bearer    ", set: true, want: http.StatusUnauthorized},
		{name: "别的 scheme", header: "Basic abc", set: true, want: http.StatusUnauthorized},
		{name: "任意字符串", header: "x", set: true, want: http.StatusUnauthorized},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotKey string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotKey = tools.APIKey(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			if c.set {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			injectKey(next, store).ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Fatalf("状态码 %d，期望 %d（body=%s）", rec.Code, c.want, rec.Body.String())
			}
			if gotKey != c.key {
				t.Fatalf("下游身份 %q，期望 %q", gotKey, c.key)
			}
			if c.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 必须带 WWW-Authenticate，否则客户端不知道该用什么凭证")
			}
		})
	}
}

// TestPublicHealthDoesNotLeak 锁住「公开端点不吐内网信息」。
// 具体到线上：base_url 曾是 http://apiduo:3000/v1，config_error 曾是解析细节。
func TestPublicHealthDoesNotLeak(t *testing.T) {
	store := newTestStore(t)
	rec := httptest.NewRecorder()
	publicHealth(store, "v9.9.9")(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	for _, k := range []string{"base_url", "api_key", "config_error"} {
		if _, ok := body[k]; ok {
			t.Errorf("公开探活不应包含 %q：%s", k, rec.Body.String())
		}
	}
	if body["version"] != "v9.9.9" {
		t.Errorf("version 应透传，得到 %v", body["version"])
	}
	if _, ok := body["config_ok"]; !ok {
		t.Error("config_ok 应存在：部署后要靠它判断配置有没有读进来")
	}
}
