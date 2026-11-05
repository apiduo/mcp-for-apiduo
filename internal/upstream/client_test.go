package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/config"
)

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc := "base_url: " + baseURL + "\napi_key: sk-service\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(path, 0)
	if err != nil {
		t.Fatalf("构造 store 失败: %v", err)
	}
	return New(store)
}

// 上游把 key 回显在 4xx 的错误体里是常见行为（OpenAI 官方就是
// "Incorrect API key provided: sk-..."）。这条消息会进日志、进 MCP 响应、
// 进终端 —— 和网络错误一样，必须过脱敏。
func TestUpstreamErrorDoesNotLeakKey(t *testing.T) {
	const key = "sk-secret-abcdef123456"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"Incorrect API key provided: %s. Check your key.","code":"invalid_api_key"}}`, key)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.ListModels(context.Background(), key)
	if err == nil {
		t.Fatal("401 应返回错误")
	}

	msg := err.Error()
	if strings.Contains(msg, key) {
		t.Fatalf("错误里泄漏了 key：%s", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Errorf("应留下脱敏痕迹，才能判断上游确实回显了：%s", msg)
	}
	// 脱敏不能把诊断信息一起吃掉
	if !strings.Contains(msg, "invalid_api_key") || !strings.Contains(msg, "/models") {
		t.Errorf("错误码与路径不该被抹掉：%s", msg)
	}
}

// 上游的错误体形状不止一种，认不出来就只能回退到截断原文。
func TestParseErrorShapes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantMsg  string
		wantCode string
	}{
		{"OpenAI 风格", 401, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, "bad key", "invalid_api_key"},
		{"扁平 message", 400, `{"message":"boom","code":"E1"}`, "boom", "E1"},
		{"中文 msg", 400, `{"msg":"参数错误"}`, "参数错误", ""},
		{"非 JSON 回退原文", 502, `upstream is down`, "upstream is down", ""},
		{"code 为 null", 500, `{"error":{"message":"x","code":null}}`, "x", ""},
	}
	for _, c := range cases {
		e := parseError(c.status, []byte(c.body))
		if e.Status != c.status {
			t.Errorf("%s：Status = %d，期望 %d", c.name, e.Status, c.status)
		}
		if e.Msg != c.wantMsg {
			t.Errorf("%s：Msg = %q，期望 %q", c.name, e.Msg, c.wantMsg)
		}
		if e.Code != c.wantCode {
			t.Errorf("%s：Code = %q，期望 %q", c.name, e.Code, c.wantCode)
		}
	}

	// 超大错误体要截断，否则一个 HTML 错误页能灌满日志
	big := parseError(500, []byte(strings.Repeat("x", 5000)))
	if len(big.Msg) > 300 {
		t.Errorf("非 JSON 错误体应截断到 300，实际 %d", len(big.Msg))
	}
}

func TestRedact(t *testing.T) {
	if got := redact("nothing to hide", ""); got != "nothing to hide" {
		t.Errorf("空 key 不该改动文本，得到 %q", got)
	}
	if got := redact("a sk-1 b sk-1", "sk-1"); got != "a *** b ***" {
		t.Errorf("应替换所有出现，得到 %q", got)
	}
	if got := redact("", "sk-1"); got != "" {
		t.Errorf("空文本应返回空，得到 %q", got)
	}
	if redactErr(nil, "sk-1") != nil {
		t.Error("nil error 应原样返回")
	}
}

func TestTruncate(t *testing.T) {
	if got := string(truncate([]byte("abcdef"), 3)); got != "abc" {
		t.Errorf("超长应截断，得到 %q", got)
	}
	if got := string(truncate([]byte("ab"), 3)); got != "ab" {
		t.Errorf("未超长应原样返回，得到 %q", got)
	}
}
