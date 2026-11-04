package catalog

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) Model {
	t.Helper()
	var m Model
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return m
}

const upstreamJSON = `{
  "id": "doubao-seedream",
  "object": "model",
  "created": 1762000000,
  "owned_by": "bytedance",
  "kind": "image",
  "execution": {"task": "async", "transport": "generations"},
  "input": {"max_reference_images": 3},
  "parameters": {
    "size": {"enum": ["1K","2K"], "default": "2K"},
    "n": {"min": 1, "max": 4, "default": 1}
  },
  "future_field": {"kept": true}
}`

func TestAccessors(t *testing.T) {
	m := decode(t, upstreamJSON)
	if m.ID() != "doubao-seedream" {
		t.Errorf("ID = %q", m.ID())
	}
	if m.Kind() != "image" {
		t.Errorf("Kind = %q", m.Kind())
	}
	if m.Task() != "async" {
		t.Errorf("Task = %q", m.Task())
	}
	if m.Transport() != "generations" {
		t.Errorf("Transport = %q", m.Transport())
	}
	if v, known := m.MaxReferenceImages(); !known || v != 3 {
		t.Errorf("MaxReferenceImages = %d (known=%v)", v, known)
	}
	if !m.HasParam("size") || m.HasParam("style") {
		t.Error("HasParam 判定错误")
	}
}

// 缺省值必须稳定。kind 的缺省必须是「未知」而不是 image —— 真实上游里
// 大部分模型是 LLM/VLM，默认成 image 会让它们混进图片工具。
func TestDefaults(t *testing.T) {
	m := Model{"id": "x"}
	if m.Kind() != KindUnknown {
		t.Errorf("缺省 kind 应为未知，得到 %q", m.Kind())
	}
	if m.Task() != "sync" || m.Transport() != "generations" {
		t.Errorf("缺省执行属性不对: task=%s transport=%s", m.Task(), m.Transport())
	}
	if _, known := m.MaxReferenceImages(); known {
		t.Error("缺省应为「上限未知」，而不是被当成 0")
	}
}

// 归一化只认上游声明的结构化字段，不靠模型名猜。
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   Model
		want string
	}{
		{"已有 kind 不动", Model{"id": "a", "kind": "video"}, "video"},
		{"domain=ImageGeneration", Model{"id": "a", "domain": "ImageGeneration"}, "image"},
		{"domain=VideoGeneration", Model{"id": "a", "domain": "VideoGeneration"}, "video"},
		{"output_modalities 含 image", Model{"id": "a", "modalities": map[string]any{
			"output_modalities": []any{"image"},
		}}, "image"},
		{"output_modalities 含 video 优先于 domain", Model{
			"id": "a", "domain": "VideoGeneration",
			"modalities": map[string]any{"output_modalities": []any{"video"}},
		}, "video"},
		{"supported_endpoint_types=image-generation", Model{
			"id": "gpt-image-2.5", "supported_endpoint_types": []any{"image-generation", "openai"},
		}, "image"},
		{"只有协议类型推不出产物", Model{
			"id": "gpt-4o", "supported_endpoint_types": []any{"openai"},
		}, KindUnknown},
		{"LLM 保持未知", Model{"id": "kimi-k3", "domain": "LLM"}, KindUnknown},
		{"纯文本输出保持未知", Model{"id": "x", "modalities": map[string]any{
			"output_modalities": []any{"text"},
		}}, KindUnknown},
		{"什么都没给保持未知", Model{"id": "x"}, KindUnknown},
		// 关键反例：名字里带 image 也不能据此判成图片模型
		{"名字带 image 但不声明不能猜", Model{"id": "some-image-model"}, KindUnknown},
	}
	for _, c := range cases {
		if got := Normalize(c.in).Kind(); got != c.want {
			t.Errorf("%s: 得到 %q，期望 %q", c.name, got, c.want)
		}
	}
}

// MCP 覆盖优先：覆盖层改到什么就是什么。
func TestMergeOverrideWins(t *testing.T) {
	up := decode(t, upstreamJSON)
	merged := Merge(up, map[string]any{
		"input": map[string]any{"max_reference_images": 1},
	})
	if v, known := merged.MaxReferenceImages(); !known || v != 1 {
		t.Errorf("覆盖未生效: %d (known=%v)", v, known)
	}
	// 未覆盖的字段必须保留
	if merged.Task() != "async" {
		t.Errorf("未覆盖的 execution.task 丢了: %q", merged.Task())
	}
	if _, ok := merged["future_field"]; !ok {
		t.Error("上游未知字段被吃掉了——这会让上游加字段时必须改代码")
	}
}

// 合并不能改动上游对象本身。
func TestMergeDoesNotMutateUpstream(t *testing.T) {
	up := decode(t, upstreamJSON)
	_ = Merge(up, map[string]any{"input": map[string]any{"max_reference_images": 9}})
	if v, _ := up.MaxReferenceImages(); v != 3 {
		t.Errorf("上游对象被污染: %d", v)
	}
}

// 只做键级合并，插入新参数时不能破坏已有的同名字段。
func TestMergeAddsNewParam(t *testing.T) {
	up := decode(t, upstreamJSON)
	merged := Merge(up, map[string]any{
		"parameters": map[string]any{
			"style": map[string]any{"enum": []any{"photo"}, "default": "photo"},
		},
	})
	if !merged.HasParam("style") {
		t.Fatal("新增参数丢失")
	}
	if !merged.HasParam("size") {
		t.Error("合并覆盖了 parameters 整个对象，导致 size 丢失")
	}
}

func TestValidate(t *testing.T) {
	base := func() Model { return decode(t, upstreamJSON) }

	cases := []struct {
		name    string
		mutate  func(Model)
		wantErr bool
	}{
		{"原样合法", func(Model) {}, false},
		{"kind 非法", func(m Model) { m["kind"] = "audio" }, true},
		{"task 非法", func(m Model) { m["execution"].(map[string]any)["task"] = "later" }, true},
		{"垫图负数", func(m Model) { m["input"].(map[string]any)["max_reference_images"] = -1 }, true},
		{"垫图非整数", func(m Model) { m["input"].(map[string]any)["max_reference_images"] = 1.5 }, true},
		{"enum 为空", func(m Model) {
			m["parameters"].(map[string]any)["size"] = map[string]any{"enum": []any{}}
		}, true},
		{"默认值不在 enum", func(m Model) {
			m["parameters"].(map[string]any)["size"] = map[string]any{
				"enum": []any{"1K"}, "default": "9K",
			}
		}, true},
		{"min 大于 max", func(m Model) {
			m["parameters"].(map[string]any)["n"] = map[string]any{"min": 5, "max": 2}
		}, true},
	}
	for _, c := range cases {
		m := base()
		c.mutate(m)
		errs := Validate(m)
		if c.wantErr != (len(errs) > 0) {
			t.Errorf("%s: 期望报错=%v，实际 errs=%v", c.name, c.wantErr, errs)
		}
	}
}

func TestPickDefault(t *testing.T) {
	models := []Model{
		{"id": "a", "kind": "image"},
		{"id": "b", "kind": "video"},
		{"id": "c", "kind": "image"},
	}

	// 未设置时取该 kind 的第一个
	ov := Overlays{Settings: Settings{Defaults: map[string]string{}}}
	if got := ov.PickDefault("image", models); got != "a" {
		t.Errorf("未设置应取第一个，得到 %q", got)
	}

	// 显式设置优先
	ov.Settings.Defaults["image"] = "c"
	if got := ov.PickDefault("image", models); got != "c" {
		t.Errorf("显式设置应生效，得到 %q", got)
	}

	// 设置的模型已下架：回落到第一个，不能返回一个不存在的 id
	ov.Settings.Defaults["image"] = "gone"
	if got := ov.PickDefault("image", models); got != "a" {
		t.Errorf("失效设置应回落，得到 %q", got)
	}

	// 没有该 kind 的模型
	if got := ov.PickDefault("search", models); got != "" {
		t.Errorf("无匹配应返回空，得到 %q", got)
	}
}

// 前期能力全靠配置中心时，漂移报告是唯一的自查手段：
// 孤儿条目必须可见，LLM 这类非业务模型不能混进「未配置」噪音。
func TestDrift(t *testing.T) {
	models := []Model{
		{"id": "img-a", "kind": "image"},
		{"id": "vid-b", "kind": "video"},
		{"id": "llm-c", "domain": "LLM"},
		{"id": ""},
	}
	ov := Overlays{Models: map[string]map[string]any{
		"img-a":           {"kind": "image"},
		"gone-old-260915": {"kind": "image"},
	}}
	d := ov.Drift(models)
	if !slices.Equal(d.Configured, []string{"img-a"}) {
		t.Errorf("configured = %v", d.Configured)
	}
	if !slices.Equal(d.Unconfigured, []string{"vid-b"}) {
		t.Errorf("unconfigured = %v", d.Unconfigured)
	}
	if !slices.Equal(d.Orphaned, []string{"gone-old-260915"}) {
		t.Errorf("orphaned = %v", d.Orphaned)
	}

	// 下架项：上游有的进 Disabled，上游没有的算孤儿。
	withDisabled := Overlays{
		Models:   map[string]map[string]any{"img-a": {"kind": "image"}},
		Disabled: []string{"vid-b", " gone-disabled ", ""},
	}.Drift(models)
	if !slices.Equal(withDisabled.Disabled, []string{"vid-b"}) {
		t.Errorf("disabled = %v", withDisabled.Disabled)
	}
	if !slices.Equal(withDisabled.Orphaned, []string{"gone-disabled"}) {
		t.Errorf("失效下架项应算孤儿，得到 %v", withDisabled.Orphaned)
	}

	// 空配置时所有可识别模型都应列为「未配置」，且没有孤儿。
	empty := Overlays{Models: map[string]map[string]any{}}.Drift(models)
	if len(empty.Configured) != 0 || len(empty.Orphaned) != 0 {
		t.Errorf("空配置不该有 configured/orphaned: %+v", empty)
	}
	if !slices.Equal(empty.Unconfigured, []string{"img-a", "vid-b"}) {
		t.Errorf("空配置 unconfigured = %v", empty.Unconfigured)
	}
}

// 整条下架在合并之前生效，且不能污染上游对象。
func TestApplyDisabled(t *testing.T) {
	up := []Model{
		{"id": "img-a", "kind": "image", "input": map[string]any{"max_reference_images": 3}},
		{"id": "vid-b", "kind": "video"},
		{"id": "img-c", "kind": "image"},
	}
	ov := Overlays{
		Models:   map[string]map[string]any{"img-a": {"input": map[string]any{"max_reference_images": 1}}},
		Disabled: []string{"vid-b", "  "},
	}
	out := ov.Apply(up)
	if len(out) != 2 {
		t.Fatalf("应剩 2 条，得到 %d: %v", len(out), out)
	}
	if out[0].ID() != "img-a" || out[1].ID() != "img-c" {
		t.Errorf("顺序或被下架项残留: %v", out)
	}
	if v, known := out[0].MaxReferenceImages(); !known || v != 1 {
		t.Errorf("下架不该影响覆盖层合并: %d (known=%v)", v, known)
	}
	if out[0]["source"] != "mcp" {
		t.Error("被覆盖的条目缺 source 标记")
	}
	if v, _ := up[0].MaxReferenceImages(); v != 3 {
		t.Error("上游对象被污染")
	}
	if _, ok := Find(out, "vid-b"); ok {
		t.Error("下架模型仍可被 pickModel 找到")
	}
}

// 上游不再列出的模型自动消失，不需要任何下架配置。
// 这条把「配置中心不是模型目录」钉成可执行契约：Apply 只遍历上游列表，
// 覆盖层里的孤儿条目不会让模型复活，disabled 也不是用来补偿上游下架的。
func TestUpstreamRemovalIsAutomatic(t *testing.T) {
	ov := Overlays{
		Models: map[string]map[string]any{
			"img-alive": {"kind": "image"},
			"img-gone":  {"kind": "image"}, // 上游已移除，配置还留着
		},
		Settings: Settings{Defaults: map[string]string{"image": "img-gone"}},
	}
	up := []Model{{"id": "img-alive", "kind": "image"}}
	out := ov.Apply(up)
	if len(out) != 1 || out[0].ID() != "img-alive" {
		t.Fatalf("上游已移除的模型不该复活: %v", out)
	}
	if _, ok := Find(out, "img-gone"); ok {
		t.Error("上游已移除的模型仍可被 pickModel 找到")
	}
	// 默认值指向已消失的模型时安全回落，不会返回一个不存在的 ID。
	if got := ov.PickDefault("image", out); got != "img-alive" {
		t.Errorf("PickDefault = %q", got)
	}
	// 配置残留仍然可自查，但只是报告，不影响可见性。
	d := ov.Drift(up)
	if !slices.Equal(d.Orphaned, []string{"img-gone"}) {
		t.Errorf("orphaned = %v", d.Orphaned)
	}
	if len(d.Disabled) != 0 {
		t.Errorf("上游下架不需要 disabled 参与，得到 %v", d.Disabled)
	}
}

// 对称的一半：上游新上、能识别类型但没配画像的模型，当前仍会自动可见，
// 甚至可能被 PickDefault 选为默认模型。这是前期路线的已知缺口（README §10），
// 本测试只钉住现状；将来若收紧为「具备完整画像才可见」，它会失败并提醒同步文档。
func TestUpstreamAdditionIsVisibleUntilConfigured(t *testing.T) {
	ov := Overlays{Models: map[string]map[string]any{}}
	up := []Model{{"id": "img-new-261104", "kind": "image"}}
	out := KnownOnly(ov.Apply(up))
	if len(out) != 1 {
		t.Fatalf("现状：未配置的新模型应仍可见，得到 %v", out)
	}
	if got := ov.PickDefault("image", out); got != "img-new-261104" {
		t.Errorf("现状：未配置的新模型可能被选为默认，得到 %q", got)
	}
	if d := ov.Drift(up); !slices.Equal(d.Unconfigured, []string{"img-new-261104"}) {
		t.Errorf("unconfigured 应能观测到它，得到 %v", d.Unconfigured)
	}
}

// 可见性 = 上游列出 AND 未被 MCP 对下游禁用。把四种组合钉成表，
// 防止以后有人在 Filter / pickModel 里另写一遍判断、绕过 Apply。
func TestVisibilityIsUpstreamAndNotDisabled(t *testing.T) {
	const target = "img-x"
	cases := []struct {
		name     string
		up       []Model
		disabled []string
		want     bool
	}{
		{"上游未列 / MCP 未禁用", []Model{}, nil, false},
		{"上游未列 / MCP 已禁用", []Model{}, []string{target}, false},
		{"上游已列 / MCP 未禁用", []Model{{"id": target, "kind": "image"}}, nil, true},
		{"上游已列 / MCP 已禁用", []Model{{"id": target, "kind": "image"}}, []string{target}, false},
	}
	for _, c := range cases {
		ov := Overlays{
			// 故意留着 target 的覆盖层条目：上游未列时它不该让模型复活。
			Models:   map[string]map[string]any{target: {"kind": "image"}},
			Disabled: c.disabled,
		}
		out := KnownOnly(ov.Apply(c.up))
		_, visible := Find(out, target)
		if visible != c.want {
			t.Errorf("%s: visible=%v, want %v", c.name, visible, c.want)
		}
	}
}

// 未配置的新模型不会从「默认模型」或同系列模板继承任何能力。
// 能力只来自：上游原始字段 + 代码硬编码缺省。settings.defaults 只用于选模，
// 从不参与 Merge —— 这是「配置中心是唯一能力来源」的实际含义。
func TestUnconfiguredModelDoesNotInherit(t *testing.T) {
	configured := Model{
		"id":         "img-old-260915",
		"kind":       "image",
		"execution":  map[string]any{"task": "async", "transport": "edits_multipart"},
		"input":      map[string]any{"max_reference_images": 3},
		"parameters": map[string]any{"size": map[string]any{"enum": []any{"1K", "2K"}, "default": "2K"}},
	}
	fresh := Model{"id": "img-new-261104", "domain": "ImageGeneration"} // 上游新加，没配画像
	ov := Overlays{
		Models:   map[string]map[string]any{"img-old-260915": {"kind": "image"}},
		Settings: Settings{Defaults: map[string]string{"image": "img-old-260915"}},
	}
	out := ov.Apply([]Model{Normalize(configured), Normalize(fresh)})
	got := out[1]

	if got.Kind() != "image" {
		t.Errorf("kind 应由上游 domain 推导，得到 %q", got.Kind())
	}
	if got.Task() != "sync" || got.Transport() != "generations" {
		t.Errorf("继承了已配模型的 execution: task=%s transport=%s", got.Task(), got.Transport())
	}
	if v, known := got.MaxReferenceImages(); known || v != 0 {
		t.Errorf("垫图上限应为「未知」而非继承 3，得到 %d (known=%v)", v, known)
	}
	if got.HasParam("size") || got.ParamDefault("size") != nil {
		t.Error("继承了已配模型的 parameters 表")
	}
	if out[0].Task() != "async" || out[0].Transport() != "edits_multipart" {
		t.Error("已配模型自身的能力被改动了")
	}
	if id := ov.PickDefault("image", out); id != "img-old-260915" {
		t.Errorf("defaults 应只决定选模，得到 %q", id)
	}

	// 内置基础模板兜底：视频新模型不再落到 generations（图片端点）。
	vid := ov.ApplyOne(Normalize(Model{"id": "vid-new", "domain": "VideoGeneration"}))
	if vid.Kind() != "video" || vid.Transport() != "video_generations" {
		t.Errorf("内置 video 模板未生效: kind=%s transport=%s", vid.Kind(), vid.Transport())
	}
}

func videoProtocol() map[string]any {
	return map[string]any{
		"execution": map[string]any{"task": "async", "transport": "video_generations"},
		"input":     map[string]any{"max_reference_images": 1},
		"parameters": map[string]any{
			"duration": map[string]any{"enum": []any{5, 10}, "default": 5},
		},
	}
}

// 协议模板给裸奔模型补齐能力，且不共享可变状态。
func TestProtocolInheritFillsMissing(t *testing.T) {
	ov := Overlays{Protocols: map[string]map[string]any{"video": videoProtocol()}}
	up := []Model{
		Normalize(Model{"id": "vid-new", "domain": "VideoGeneration"}),
		Normalize(Model{"id": "vid-new2", "domain": "VideoGeneration"}),
	}
	out := ov.Apply(up)
	for _, m := range out {
		if m.Kind() != "video" {
			t.Errorf("kind = %q", m.Kind())
		}
		// 关键：不再落到 generations（图片端点）
		if m.Task() != "async" || m.Transport() != "video_generations" {
			t.Errorf("%s 未继承协议: task=%s transport=%s", m.ID(), m.Task(), m.Transport())
		}
		if v, known := m.MaxReferenceImages(); !known || v != 1 {
			t.Errorf("%s 垫图上限 = %d (known=%v)", m.ID(), v, known)
		}
		if !m.HasParam("duration") || m["source"] != "protocol" {
			t.Errorf("%s 参数表或来源标记不对: %v", m.ID(), m)
		}
	}
	// 改一个模型不能污染模板或另一个模型（enum/default 必须是独立拷贝）
	out[0]["parameters"].(map[string]any)["duration"].(map[string]any)["default"] = 99
	if ov.Protocols["video"]["parameters"].(map[string]any)["duration"].(map[string]any)["default"] != 5 {
		t.Error("模板被合并结果改脏")
	}
	if out[1].ParamDefault("duration") != 5 {
		t.Error("另一个模型共享了同一份参数表")
	}
}

// 模板只补缺：上游真声明过的字段以事实为准。
func TestProtocolDoesNotOverrideUpstream(t *testing.T) {
	ov := Overlays{Protocols: map[string]map[string]any{
		"image": {"execution": map[string]any{"task": "sync", "transport": "generations"}},
	}}
	m := Model{
		"id":        "img-up",
		"kind":      "image",
		"execution": map[string]any{"task": "async"},
	}
	got := ov.ApplyOne(m)
	if got.Task() != "async" {
		t.Errorf("上游的 task 被模板盖掉: %q", got.Task())
	}
	if got.Transport() != "generations" {
		t.Errorf("缺失的 transport 没补上: %q", got.Transport())
	}
	if m.Task() != "async" || m.Transport() != "generations" {
		t.Error("上游对象被污染")
	}
}

// 模型级覆盖是人工修正，必须能盖掉自动继承来的值，且保留模板里没被覆盖的部分。
func TestModelOverlayWinsOverProtocol(t *testing.T) {
	ov := Overlays{
		Protocols: map[string]map[string]any{"video": videoProtocol()},
		Models: map[string]map[string]any{
			"vid-x": {"execution": map[string]any{"transport": "contents_tasks"}},
		},
	}
	got := ov.ApplyOne(Normalize(Model{"id": "vid-x", "domain": "VideoGeneration"}))
	if got.Transport() != "contents_tasks" {
		t.Errorf("覆盖未生效: %q", got.Transport())
	}
	if got.Task() != "async" {
		t.Errorf("覆盖不该吃掉模板里未冲突的 task: %q", got.Task())
	}
	if got["source"] != "mcp" {
		t.Errorf("source = %v", got["source"])
	}
}

// 同一 kind 需要不同协议族时（直连 Ark vs 经网关），用 _protocol 显式指定；
// 该管理字段不能泄漏进对外画像。
func TestExplicitProtocolName(t *testing.T) {
	ov := Overlays{
		Protocols: map[string]map[string]any{
			"video":     {"execution": map[string]any{"transport": "video_generations"}},
			"ark-video": {"execution": map[string]any{"task": "async", "transport": "contents_tasks"}},
		},
		Models: map[string]map[string]any{"vid-x": {"_protocol": "ark-video"}},
	}
	got := ov.ApplyOne(Normalize(Model{"id": "vid-x", "domain": "VideoGeneration"}))
	if got.Transport() != "contents_tasks" || got.Task() != "async" {
		t.Errorf("显式协议族未生效: %+v", got["execution"])
	}
	if _, leaked := got["_protocol"]; leaked {
		t.Error("_protocol 泄漏进对外画像")
	}
}

// new-api 那类只有裸字段、推不出 kind 的模型，可由协议模板补出 kind 并变得可用。
func TestProtocolCanSupplyKind(t *testing.T) {
	ov := Overlays{
		Protocols: map[string]map[string]any{
			"generic-image": {
				"kind":      "image",
				"execution": map[string]any{"task": "sync", "transport": "generations"},
			},
		},
		Models: map[string]map[string]any{"brand-new": {"_protocol": "generic-image"}},
	}
	raw := Normalize(Model{"id": "brand-new"})
	if raw.Kind() != KindUnknown {
		t.Fatal("前提：上游裸字段推不出 kind")
	}
	got := ov.ApplyOne(raw)
	if got.Kind() != "image" || len(KnownOnly([]Model{got})) != 1 {
		t.Errorf("模板补出 kind 后应可见: %+v", got)
	}
}

// 漂移报告要区分「靠模板补齐」和「仍然裸奔」。
func TestDriftInherited(t *testing.T) {
	ov := Overlays{Protocols: map[string]map[string]any{
		"image": {"execution": map[string]any{"task": "sync"}},
	}}
	up := []Model{
		Normalize(Model{"id": "a", "domain": "ImageGeneration"}),
		Normalize(Model{"id": "b", "domain": "VideoGeneration"}),
		Normalize(Model{"id": "c", "domain": "LLM"}),
	}
	d := ov.Drift(up)
	if !slices.Equal(d.Inherited, []string{"a"}) {
		t.Errorf("inherited = %v", d.Inherited)
	}
	if !slices.Equal(d.Unconfigured, []string{"b"}) {
		t.Errorf("unconfigured = %v", d.Unconfigured)
	}
}

// 覆盖层补的 kind 必须参与协议族选择。
//
// 真实聚合网关（newapi-bff）的 /models 不给 kind，只能由配置中心补；
// 若选择模板时仍读上游 kind，模型会拿到默认 transport（generations），
// 视频请求会被打到 /images/generations 上。
func TestOverlayKindSelectsProtocolTemplate(t *testing.T) {
	ov := Overlays{
		Protocols: map[string]map[string]any{
			"video": {"execution": map[string]any{"task": "async", "transport": "video_generations"}},
		},
		Models: map[string]map[string]any{"seedance": {"kind": "video"}},
	}
	raw := Normalize(Model{"id": "seedance"}) // 上游未声明 kind
	if raw.Kind() != KindUnknown {
		t.Fatal("前提：上游裸字段推不出 kind")
	}
	got := ov.ApplyOne(raw)
	if got.Kind() != "video" {
		t.Fatalf("kind = %q", got.Kind())
	}
	if got.Transport() != "video_generations" {
		t.Errorf("transport = %q，覆盖层声明的 kind 没有参与模板选择", got.Transport())
	}
	if got["source"] != "mcp" {
		t.Errorf("source = %v", got["source"])
	}
}

// 内置基础模板：管理员完全不配 protocols，图片/视频新模型也拿到正确协议族。
func TestBuiltinProtocolsApplied(t *testing.T) {
	ov := Overlays{}
	cases := []struct {
		id, domain, task, transport string
	}{
		{"img-new", "ImageGeneration", "sync", "generations"},
		{"vid-new", "VideoGeneration", "async", "video_generations"},
	}
	for _, c := range cases {
		got := ov.ApplyOne(Normalize(Model{"id": c.id, "domain": c.domain}))
		if got.Task() != c.task || got.Transport() != c.transport {
			t.Errorf("%s: task=%s transport=%s，期望 %s/%s",
				c.id, got.Task(), got.Transport(), c.task, c.transport)
		}
		if got["source"] != "builtin" {
			t.Errorf("%s: source = %v", c.id, got["source"])
		}
	}
	if _, known := ov.ApplyOne(Normalize(Model{"id": "img-new", "domain": "ImageGeneration"})).MaxReferenceImages(); known {
		t.Error("内置模板声明了垫图上限，会锁死上游已支持的图生图")
	}
	if p := ov.ApplyOne(Normalize(Model{"id": "img-new", "domain": "ImageGeneration"})).Parameters(); len(p) != 0 {
		t.Errorf("内置模板不该猜参数枚举: %v", p)
	}
}

// 管理员模板逐字段盖内置，没写的字段仍继承内置。
func TestBuiltinProtocolCanBeOverridden(t *testing.T) {
	ov := Overlays{Protocols: map[string]map[string]any{
		"video": {"execution": map[string]any{"transport": "contents_tasks"}},
	}}
	got := ov.ApplyOne(Normalize(Model{"id": "vid-x", "domain": "VideoGeneration"}))
	if got.Transport() != "contents_tasks" {
		t.Errorf("覆盖未生效: %q", got.Transport())
	}
	if got.Task() != "async" {
		t.Errorf("只改 transport 不该冲掉内置 task: %q", got.Task())
	}
	if got["source"] != "protocol" {
		t.Errorf("source = %v", got["source"])
	}
	if !ov.HasProtocol("image") || !ov.HasProtocol("video") {
		t.Error("HasProtocol 应认内置名")
	}
	ov.Protocols["ark-video"] = map[string]any{"execution": map[string]any{"transport": "contents_tasks"}}
	if !ov.HasProtocol("ark-video") {
		t.Error("HasProtocol 应认管理员自定义名")
	}
	if ov.HasProtocol("no-such-protocol") {
		t.Error("HasProtocol 误报不存在的协议族")
	}
}

// 内置模板只覆盖 image/video；推不出 kind 又没 _protocol 的模型仍无模板可用。
func TestBuiltinDoesNotCoverUnknownKind(t *testing.T) {
	ov := Overlays{}
	got := ov.ApplyOne(Normalize(Model{"id": "bare-new"}))
	if got.Kind() != KindUnknown || got["source"] != nil {
		t.Errorf("未知类型不该套内置模板: %+v", got)
	}
	if ov.HasProtocol("search") {
		t.Error("内置模板当前只含 image/video")
	}
}
