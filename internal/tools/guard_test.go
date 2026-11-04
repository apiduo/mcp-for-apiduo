package tools

import (
	"strings"
	"testing"
)

func intPtr(n int) *int { return &n }

// 没有画像的模型：上游给了 n 但不给 max。
// 早先这种模型接受任意 n，一次调用就能把额度打光。
const bareImageModel = `{
  "id": "bare",
  "kind": "image",
  "execution": {"task": "sync", "transport": "generations"}
}`

func TestBuildPayloadCapsUnknownN(t *testing.T) {
	m := model(t, bareImageModel)

	if _, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(maxBatchN)}, nil); err != nil {
		t.Fatalf("n=%d 应放行，得到 %v", maxBatchN, err)
	}
	_, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(maxBatchN + 1)}, nil)
	if err == nil {
		t.Fatalf("n=%d 应被拦下：模型没声明上限不等于没有上限", maxBatchN+1)
	}
	if !strings.Contains(err.Error(), "未声明 n 的上限") {
		t.Fatalf("错误信息应说明原因，得到 %v", err)
	}

	// n < 1 是明显的错误请求，不该原样转给上游。
	if _, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(0)}, nil); err == nil {
		t.Fatal("n=0 应被拦下")
	}
	if _, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(-3)}, nil); err == nil {
		t.Fatal("n=-3 应被拦下")
	}
}

// 画像声明了 max 的模型按声明走，不被兜底常量覆盖。
func TestBuildPayloadHonoursDeclaredN(t *testing.T) {
	m := model(t, imgModel) // parameters.n.max = 4
	if _, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(4)}, nil); err != nil {
		t.Fatalf("n=4（等于声明的 max）应放行: %v", err)
	}
	if _, err := buildPayload(m, GenerateInput{Prompt: "x", N: intPtr(5)}, nil); err == nil {
		t.Fatal("n=5 超过声明的 max=4，应被拦下")
	}
}

const searchModel = `{
  "id": "web-search",
  "kind": "search",
  "execution": {"task": "sync", "transport": "generations"},
  "parameters": {
    "count": {"min": 1, "max": 10, "default": 5},
    "include_content": {"type": "boolean"}
  }
}`

// search.query 的 Extra 曾经只检查「键存不存在」，配置里写的 min/max 等于注释。
func TestBuildSearchPayloadValidatesExtraConstraints(t *testing.T) {
	m := model(t, searchModel)

	if _, err := buildSearchPayload(m, SearchInput{Extra: map[string]any{"count": float64(10)}}, "q"); err != nil {
		t.Fatalf("count=10（等于 max）应放行: %v", err)
	}
	if _, err := buildSearchPayload(m, SearchInput{Extra: map[string]any{"count": float64(99)}}, "q"); err == nil {
		t.Fatal("count=99 超过声明的 max=10，应被拦下")
	} else if !strings.Contains(err.Error(), "count") {
		t.Fatalf("错误信息应点名参数，得到 %v", err)
	}
	if _, err := buildSearchPayload(m, SearchInput{Extra: map[string]any{"unknown_param": 1}}, "q"); err == nil {
		t.Fatal("未声明的参数应被白名单拦下")
	}
}

// 结构参数走标准白名单，模型没画像也能透传；但它们同样要过声明的约束。
func TestBuildSearchPayloadStandardParams(t *testing.T) {
	m := model(t, searchModel)
	p, err := buildSearchPayload(m, SearchInput{Count: intPtr(3), Extra: map[string]any{"duration": 5}}, "q")
	if err != nil {
		t.Fatalf("标准参数应放行: %v", err)
	}
	if p["count"] != 3 || p["duration"] != 5 {
		t.Fatalf("载荷不对: %#v", p)
	}
	if _, err := buildSearchPayload(m, SearchInput{Count: intPtr(11)}, "q"); err == nil {
		t.Fatal("count=11 超过 max=10，应被拦下")
	}
}
