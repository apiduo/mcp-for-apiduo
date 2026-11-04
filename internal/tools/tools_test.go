package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

func model(t *testing.T, s string) catalog.Model {
	t.Helper()
	var m catalog.Model
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return m
}

const imgModel = `{
  "id": "seedream",
  "kind": "image",
  "execution": {"task": "sync", "transport": "generations"},
  "input": {"max_reference_images": 2},
  "parameters": {
    "size": {"enum": ["1K","2K"], "default": "2K"},
    "n": {"min": 1, "max": 4, "default": 1}
  }
}`

// reference_images 入口宽松：单值与数组都要收，内部统一成数组。
func TestParseRefs(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`"https://a/1.png"`, 1},
		{`["https://a/1.png"]`, 1},
		{`["https://a/1.png","https://a/2.png"]`, 2},
		{`[]`, 0},
		{`null`, 0},
		{``, 0},
		{`""`, 0},
	}
	for _, c := range cases {
		var v any
		if c.in != "" {
			if err := json.Unmarshal([]byte(c.in), &v); err != nil {
				t.Fatalf("%s 不是合法 JSON: %v", c.in, err)
			}
		}
		got, err := parseRefs(v)
		if err != nil {
			t.Errorf("%s 解析失败: %v", c.in, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s 得到 %d 张，期望 %d 张", c.in, len(got), c.want)
		}
	}

	var num any = float64(123)
	if _, err := parseRefs(num); err == nil {
		t.Error("非字符串输入应报错")
	}
	if _, err := parseRefs([]any{float64(1)}); err == nil {
		t.Error("数组里混入非字符串应报错")
	}
}

// 三项硬约束：response_format 固定、参考图超限拒绝、未声明参数拒绝。
func TestBuildPayload(t *testing.T) {
	m := model(t, imgModel)

	t.Run("文生图不带 image", func(t *testing.T) {
		p, err := buildPayload(m, GenerateInput{Prompt: "猫"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p["response_format"] != "url" {
			t.Errorf("response_format 必须固定为 url，得到 %v", p["response_format"])
		}
		if _, ok := p["image"]; ok {
			t.Error("没有参考图却带了 image 字段")
		}
	})

	t.Run("图生图带数组", func(t *testing.T) {
		p, err := buildPayload(m, GenerateInput{Prompt: "换衣"}, []string{"a", "b"})
		if err != nil {
			t.Fatal(err)
		}
		arr, ok := p["image"].([]string)
		if !ok || len(arr) != 2 {
			t.Errorf("image 应为 2 元素数组，得到 %v", p["image"])
		}
	})

	t.Run("超过垫图上限拒绝", func(t *testing.T) {
		_, err := buildPayload(m, GenerateInput{Prompt: "x"}, []string{"a", "b", "c"})
		if err == nil || !strings.Contains(err.Error(), "最多接受 2 张") {
			t.Errorf("应该因超限报错，得到 %v", err)
		}
	})

	t.Run("enum 越界拒绝", func(t *testing.T) {
		_, err := buildPayload(m, GenerateInput{Prompt: "x", Size: "9K"}, nil)
		if err == nil || !strings.Contains(err.Error(), "可选") {
			t.Errorf("应该拒绝非法 size，得到 %v", err)
		}
	})

	t.Run("enum 命中放行", func(t *testing.T) {
		p, err := buildPayload(m, GenerateInput{Prompt: "x", Size: "1K"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p["size"] != "1K" {
			t.Errorf("size 未透传: %v", p["size"])
		}
	})

	t.Run("n 超范围拒绝", func(t *testing.T) {
		n := 5
		_, err := buildPayload(m, GenerateInput{Prompt: "x", N: &n}, nil)
		if err == nil || !strings.Contains(err.Error(), "不能大于 4") {
			t.Errorf("应该拒绝 n=5，得到 %v", err)
		}
	})

	t.Run("未声明参数拒绝", func(t *testing.T) {
		_, err := buildPayload(m, GenerateInput{
			Prompt: "x",
			Extra:  map[string]any{"unknown_knob": 1},
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "未声明参数") {
			t.Errorf("应该拒绝未声明参数，得到 %v", err)
		}
	})

	t.Run("已声明参数透传", func(t *testing.T) {
		p, err := buildPayload(m, GenerateInput{
			Prompt: "x",
			Extra:  map[string]any{"size": "2K"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p["size"] != "2K" {
			t.Errorf("extra 未透传: %v", p["size"])
		}
	})
}

// 画像里写了 min/max/enum，Extra 传越界值就必须当场拦住。
// 早先只查「参数名是否声明过」，值直接透传 —— 配置中心填的约束形同文档。
func TestBuildPayloadValidatesExtraConstraints(t *testing.T) {
	m := model(t, `{
	  "id": "seedance",
	  "kind": "video",
	  "execution": {"task": "async", "transport": "video_generations"},
	  "parameters": {
	    "duration":   {"min": 3, "max": 10, "default": 5},
	    "resolution": {"enum": ["480p", "720p"]}
	  }
	}`)

	t.Run("范围越界拒绝", func(t *testing.T) {
		_, err := buildPayload(m, GenerateInput{
			Prompt: "x",
			Extra:  map[string]any{"duration": float64(99)},
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "不能大于 10") {
			t.Errorf("应该拒绝 duration=99，得到 %v", err)
		}
	})

	t.Run("枚举越界拒绝", func(t *testing.T) {
		_, err := buildPayload(m, GenerateInput{
			Prompt: "x",
			Extra:  map[string]any{"resolution": "4K"},
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "可选") {
			t.Errorf("应该拒绝 resolution=4K，得到 %v", err)
		}
	})

	t.Run("范围内放行并透传", func(t *testing.T) {
		p, err := buildPayload(m, GenerateInput{
			Prompt: "x",
			Extra:  map[string]any{"duration": float64(5), "resolution": "720p"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p["duration"] != float64(5) || p["resolution"] != "720p" {
			t.Errorf("合法值未透传: %v", p)
		}
	})

	t.Run("未给约束的参数放行", func(t *testing.T) {
		m2 := model(t, `{"id":"x","kind":"image","parameters":{"seed":{}}}`)
		if _, err := buildPayload(m2, GenerateInput{Prompt: "x", Extra: map[string]any{"seed": float64(42)}}, nil); err != nil {
			t.Errorf("空约束应放行，得到 %v", err)
		}
	})
}

// 不支持垫图的模型传了参考图要当场拒绝，不能透传给上游再吃 400。
func TestBuildPayloadRejectsRefsWhenUnsupported(t *testing.T) {
	m := model(t, `{"id":"dalle","kind":"image","input":{"max_reference_images":0},"parameters":{}}`)
	if _, err := buildPayload(m, GenerateInput{Prompt: "x"}, []string{"a"}); err == nil {
		t.Error("不支持垫图的模型应拒绝参考图")
	}
}

// 上游响应解析要能吃下几种常见形状。
func TestParseOutputs(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want int
	}{
		{"openai data", map[string]any{"data": []any{map[string]any{"url": "u1"}}}, 1},
		{"newapi result", map[string]any{"result": []any{map[string]any{"url": "u1"}, map[string]any{"url": "u2"}}}, 2},
		{"单值 url", map[string]any{"url": "u1"}, 1},
		{"空", map[string]any{}, 0},
	}
	for _, c := range cases {
		if got := parseOutputs(c.in); len(got) != c.want {
			t.Errorf("%s: 得到 %d 个，期望 %d 个", c.name, len(got), c.want)
		}
	}
}

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"completed": "completed", "succeeded": "completed", "done": "completed",
		"failed": "failed", "cancelled": "failed", "error": "failed",
		"running": "working", "": "working", "whatever": "working",
	}
	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}
