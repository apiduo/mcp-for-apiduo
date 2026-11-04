package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const seedDoc = `# 部署配置：这一段由运维维护，注释不要删
base_url: http://127.0.0.1:3000
api_key: sk-service-key-1234

# 模型覆盖层：只写差异
models:   # 行内说明
  keep-me:
    kind: image

disabled:
  - gone-260915
`

// 手改配置文件必须立即生效。
//
// 这是 README §11 末尾那条「已知问题」的回归测试：早先覆盖层在 admin.New 时
// 读一次快照，之后怎么改文件都不生效，只能重启。
func TestReloadPicksUpOverlayEdits(t *testing.T) {
	path := writeTemp(t, seedDoc)
	store, err := NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Overlays().Models) != 1 {
		t.Fatalf("初始覆盖层 = %v", store.Overlays().Models)
	}
	revBefore := store.Revision()

	// 模拟运维直接编辑文件。
	edited := strings.Replace(seedDoc, "  keep-me:\n    kind: image\n",
		"  keep-me:\n    kind: image\n  hand-added:\n    kind: video\n", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Reload()

	if _, ok := store.Overlays().Models["hand-added"]; !ok {
		t.Fatalf("手改的模型没有生效: %v", store.Overlays().Models)
	}
	if store.Revision() == revBefore {
		t.Error("覆盖层变了，版本号却没变")
	}
}

// 文件写坏时保留上一份可用配置，而不是把服务带进空配置。
func TestReloadKeepsOldValueOnCorruptFile(t *testing.T) {
	path := writeTemp(t, seedDoc)
	store, err := NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("models: [\n  broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Reload()
	if store.LastError() == "" {
		t.Error("解析失败应记录错误")
	}
	if _, ok := store.Overlays().Models["keep-me"]; !ok {
		t.Error("解析失败却丢掉了旧配置")
	}
}

// 写回必须保留顶层注释、键序与部署配置，只替换覆盖层那四段。
func TestReplaceOverlaysPreservesHumanFile(t *testing.T) {
	path := writeTemp(t, seedDoc)
	store, err := NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReplaceOverlays(catalog.Overlays{
		Models: map[string]map[string]any{
			"keep-me":   {"kind": "image", "input": map[string]any{"max_reference_images": 2}},
			"brand-new": {"kind": "video"},
		},
		Settings: catalog.Settings{Defaults: map[string]string{"image": "keep-me"}},
		Disabled: []string{"gone-260915"},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"# 部署配置：这一段由运维维护，注释不要删",
		"# 模型覆盖层：只写差异",
		"行内说明",
		// base_url 原样保留（/v1 是内存里补的，不回写文件）
		"base_url: http://127.0.0.1:3000",
		"api_key: sk-service-key-1234",
		"brand-new",
		"max_reference_images: 2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("写回后丢了 %q:\n%s", want, text)
		}
	}
	// 键序：base_url 仍在最前，不是被 map 重排到末尾。
	if strings.Index(text, "base_url") > strings.Index(text, "models") {
		t.Errorf("写回把键序打乱了:\n%s", text)
	}
	// 空段删键，不留 `disabled: []` 这种噪音之外的其它空段。
	if strings.Contains(text, "protocols:") {
		t.Errorf("空段仍被写进文件:\n%s", text)
	}
}

// 规范形式让版本号只跟内容有关：同一集合换顺序不得产生新版本。
func TestRevisionStableAcrossOrdering(t *testing.T) {
	a := catalog.Overlays{Disabled: []string{"b", "a"}, Models: map[string]map[string]any{}}
	b := catalog.Overlays{Disabled: []string{"a", "b"}, Models: map[string]map[string]any{}}
	if Revision(a) != Revision(b) {
		t.Errorf("顺序不同导致版本号不同: %s vs %s", Revision(a), Revision(b))
	}
	if Revision(a) == Revision(catalog.Overlays{}) {
		t.Error("内容不同却得到同一个版本号")
	}
}

// 纯内存模式也要能发布覆盖层（不落盘、不报错）。
func TestReplaceOverlaysInMemory(t *testing.T) {
	t.Setenv("API_KEY", "sk-test")
	store, err := NewStore("", 0)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := store.ReplaceOverlays(catalog.Overlays{Disabled: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if rev != store.Revision() {
		t.Errorf("返回的版本 %s 与当前 %s 不一致", rev, store.Revision())
	}
	if !store.Overlays().DisabledSet()["x"] {
		t.Error("内存模式没有发布")
	}
}

// 带比较的写入：版本不符必须拒绝，且不动磁盘。
//
// 这是多副本并发写的安全网 —— 少了它，后写的会静默覆盖先写的。
func TestReplaceOverlaysIfRejectsStaleRevision(t *testing.T) {
	path := writeTemp(t, seedDoc)
	store, err := NewStore(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ReplaceOverlaysIf(catalog.Overlays{Disabled: []string{"x"}}, "deadbeefdeadbeef")
	if !errors.Is(err, ErrRevisionChanged) {
		t.Fatalf("期望 ErrRevisionChanged，实际 %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("版本冲突却写了盘")
	}

	if _, err := store.ReplaceOverlaysIf(catalog.Overlays{Disabled: []string{"x"}}, store.Revision()); err != nil {
		t.Fatalf("版本正确却失败: %v", err)
	}
	if !store.Overlays().DisabledSet()["x"] {
		t.Error("写入未生效")
	}
}
