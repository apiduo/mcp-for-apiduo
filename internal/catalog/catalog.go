// Package catalog 管理模型画像：拉取、与覆盖层合并、校验。
//
// 设计要点（README §10）：
//   - 模型对象用 map 保存，不映射成自定义 struct —— 上游加字段必须自动可见。
//   - 合并顺序：上游 → 覆盖层逐字段替换 → 调用方过滤。MCP 覆盖优先。
//   - 覆盖层只存差异（overlay），不存快照。
//   - 校验对象是「合并结果」，不是覆盖层。
package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Model 是 /v1/models 里的单个模型对象，原样保留所有字段。
type Model map[string]any

// ID 返回模型标识。
func (m Model) ID() string {
	s, _ := m["id"].(string)
	return s
}

// KindUnknown 表示无法确定产物类型。
//
// 不能默认成 image —— 真实上游（如火山 Ark）的 /models 返回 55 个模型，
// 绝大多数是 LLM / VLM / Embedding，默认成 image 会让它们全部混进图片工具，
// 默认模型更可能选中一个语言模型。
const KindUnknown = ""

// Kind 返回产物类型。缺失返回 KindUnknown，由 Normalize 从上游字段推导。
func (m Model) Kind() string {
	s, _ := m["kind"].(string)
	return s
}

// Normalize 把不同上游的模型对象归一化到本契约。
//
// 只做「结构化字段 → kind」的映射，不做任何基于模型名的猜测：
//   - domain=ImageGeneration / output_modalities 含 image → image
//   - domain=VideoGeneration / 含 video → video
//   - 其余（LLM / VLM / Embedding）→ 保持 unknown，不参与生成类工具
//
// 推断依据必须是上游自己声明的字段，这跟「看到名字里带 4k 就猜尺寸」是两回事。
func Normalize(m Model) Model {
	if m.Kind() != KindUnknown {
		return m
	}
	k := inferKind(m)
	if k == KindUnknown {
		return m
	}
	out := m.Clone()
	out["kind"] = k
	return out
}

// endpointTypeKind 把上游的「端点类型」白名单映射到产物类型。
//
// supported_endpoint_types 这个字段（new-api 系列会返回）里混装了两类信息：
//   - 产物类型：image-generation / video-generation / web-search
//   - 协议类型：openai / gemini / anthropic
//
// 只能白名单识别前一类，协议类型不能推导出任何产物类型。
var endpointTypeKind = map[string]string{
	"image-generation": "image",
	"video-generation": "video",
	"web-search":       "search",
	"search":           "search",
}

// inferKind 按「证据强弱」依次尝试。所有依据都必须是上游自己声明的结构化字段，
// 绝不从模型名推断 —— 名字里带 image 不等于它是图片模型。
func inferKind(m Model) string {
	// 1) supported_endpoint_types（new-api 扩展，最明确）
	for _, t := range anySlice(m["supported_endpoint_types"]) {
		if k, ok := endpointTypeKind[t]; ok {
			return k
		}
	}
	// 2) output_modalities：产物是什么就是什么
	if mod, ok := m["modalities"].(map[string]any); ok {
		for _, v := range anySlice(mod["output_modalities"]) {
			switch v {
			case "image":
				return "image"
			case "video":
				return "video"
			}
		}
	}
	// 3) domain：火山 Ark 的分类字段
	switch s, _ := m["domain"].(string); s {
	case "ImageGeneration":
		return "image"
	case "VideoGeneration":
		return "video"
	}
	return KindUnknown
}

func anySlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) sub(key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// Task 返回 sync/async，缺省 sync。
func (m Model) Task() string {
	if s, ok := m.sub("execution")["task"].(string); ok && s != "" {
		return s
	}
	return "sync"
}

// Transport 返回请求形状，缺省 generations。
func (m Model) Transport() string {
	if s, ok := m.sub("execution")["transport"].(string); ok && s != "" {
		return s
	}
	return "generations"
}

// MaxReferenceImages 返回参考图上限，以及这个上限是否「已知」。
//
// 三态，必须区分开（真实上游逼出来的）：
//   - 键缺失   → known=false，上限未知。不拦截，交给上游判定。
//   - 值为 0   → known=true，明确不支持垫图，本地拒绝。
//   - 值为 N   → known=true，上限 N，超出本地拒绝。
//
// 早先把「缺失」当 0 处理，结果火山 Ark 的 seedream 明明声明了
// task_type=[text_to_image, image_to_image]，却被判成不支持垫图 —— 图生图直接不可用。
func (m Model) MaxReferenceImages() (int, bool) {
	v, ok := m.sub("input")["max_reference_images"]
	if !ok || v == nil {
		return 0, false
	}
	return intOf(v), true
}

// Parameters 返回参数约束表。
func (m Model) Parameters() map[string]any {
	return m.sub("parameters")
}

// ParamDefault 取参数默认值，缺失返回 nil。
func (m Model) ParamDefault(name string) any {
	if p, ok := m.Parameters()[name].(map[string]any); ok {
		return p["default"]
	}
	return nil
}

// ParamNames 返回参数名（排序后），用于日志与错误信息。
func (m Model) ParamNames() []string {
	ps := m.Parameters()
	out := make([]string, 0, len(ps))
	for k := range ps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HasParam 判断画像是否声明了该参数。未声明的参数一律本地拒绝，不透传上游。
func (m Model) HasParam(name string) bool {
	_, ok := m.Parameters()[name]
	return ok
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// Clone 深拷贝，避免调用方改动污染缓存。
func (m Model) Clone() Model {
	b, err := json.Marshal(m)
	if err != nil {
		return Model{}
	}
	var out Model
	if err := json.Unmarshal(b, &out); err != nil {
		return Model{}
	}
	return out
}

// BuiltinProtocols 是图片 / 视频两类模型的内置基础模板。
//
// 只放确定无害的字段：
//   - 不给 input.max_reference_images：上游不声明 ≠ 不支持垫图，写 0 会锁死图生图（§14 教训 3）
//   - 不给 parameters：尺寸 / 时长枚举各上游不同，猜错会把合法调用误杀
//
// video 用 video_generations 是因为主线是经 new-api 网关（§14 教训 5）；
// 直连火山 Ark 时用 protocols.video 覆盖成 contents_tasks，或 _protocol 指向自定义模板。
func BuiltinProtocols() map[string]map[string]any {
	return map[string]map[string]any{
		"image": {"execution": map[string]any{"task": "sync", "transport": "generations"}},
		"video": {"execution": map[string]any{"task": "async", "transport": "video_generations"}},
	}
}

// effectiveProtocols = 内置基础模板 ∪ 管理员模板。管理员逐字段覆盖内置，
// 只写 execution.transport 不会把内置里同级的 execution.task 冲掉。
func (o Overlays) effectiveProtocols() map[string]map[string]any {
	out := BuiltinProtocols()
	for name, tpl := range o.Protocols {
		base, ok := out[name]
		if !ok {
			out[name] = cloneValue(tpl).(map[string]any)
			continue
		}
		merged := cloneValue(base).(map[string]any)
		deepMerge(merged, cloneValue(tpl).(map[string]any))
		out[name] = merged
	}
	return out
}

// HasProtocol 判断协议族名是否存在（含内置），供保存前校验使用。
func (o Overlays) HasProtocol(name string) bool {
	if _, ok := BuiltinProtocols()[name]; ok {
		return true
	}
	_, ok := o.Protocols[name]
	return ok
}

// cloneValue 深拷贝模板里的复合值，避免多个模型共享同一份 enum 切片。
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = cloneValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = cloneValue(vv)
		}
		return out
	default:
		return v
	}
}

// fillMissing 递归补齐 dst 里缺失的键，绝不覆盖已有值。
//
// 模板是「兜底」而不是「覆盖」：上游真声明了能力字段就以事实为准，
// 只有上游没给的才由协议补上。两边都是 map 时继续往下逐键补，
// 所以模板只写 execution.transport 不会把上游已有的 execution.task 冲掉。
func fillMissing(dst, src map[string]any) {
	for k, v := range src {
		dv, exists := dst[k]
		if exists {
			sv, sMap := v.(map[string]any)
			dMap, dOK := dv.(map[string]any)
			if sMap && dOK {
				fillMissing(dMap, sv)
			}
			continue
		}
		dst[k] = cloneValue(v)
	}
}

// Merge 把覆盖层叠加到上游模型上：MCP 覆盖优先。
//
// 只做键级合并，不做结构转换 —— 上游独有的字段原样保留，
// 覆盖层没提到的字段继续跟随上游。
func Merge(up Model, overlay map[string]any) Model {
	out := up.Clone()
	deepMerge(out, overlay)
	return out
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		sv, isMap := v.(map[string]any)
		dv, dstIsMap := dst[k].(map[string]any)
		if isMap && dstIsMap {
			deepMerge(dv, sv)
			continue
		}
		if isMap {
			cp := map[string]any{}
			deepMerge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// Problem 是一条校验结果，Path 是可寻址的字段位置（JSON Pointer，相对模型对象）。
//
// 为什么要 Path 而不是只有一句话：这份结果要给人看，也要给 agent 用。
// agent 拿到「min 不能大于 max」还得自己猜是哪个参数、哪个模型；
// 拿到 /models/x/parameters/n 才能改对地方。同一份数据，两种消费者。
//
// Message 保持自解释（含字段名），所以只读日志的人不需要拼 Path。
type Problem struct {
	Path    string `json:"path,omitempty" yaml:"path,omitempty"`
	Message string `json:"message" yaml:"message"`
	Hint    string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

// String 拼回人类可读的一行，供日志与旧调用方使用。
func (p Problem) String() string {
	if p.Path == "" {
		return p.Message
	}
	return strings.TrimPrefix(p.Path, "/") + "：" + p.Message
}

// Validate 校验「合并结果」，返回纯文本。保留这个签名是为了不把日志与
// 既有调用方绑在结构上；需要定位字段的调用方用 ValidateProblems。
func Validate(m Model) []string {
	ps := ValidateProblems(m)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Message)
	}
	return out
}

// ValidateProblems 校验「合并结果」。能力层面的矛盾已由结构消灭（没有 modes），
// 这里只覆盖输入与参数层面。
//
// 校验对象必须是合并结果，不是覆盖层：覆盖层缺 id/object，单独校验必然报错。
func ValidateProblems(m Model) []Problem {
	var ps []Problem
	add := func(path, msg, hint string) {
		ps = append(ps, Problem{Path: path, Message: msg, Hint: hint})
	}

	if m.ID() == "" {
		add("", "id 不能为空", "覆盖层的键必须是上游 /v1/models 里的模型 ID")
	}
	switch m.Kind() {
	case KindUnknown, "image", "video", "search":
	default:
		add("/kind", fmt.Sprintf("kind 取值非法: %q", m.Kind()),
			"只能是 image / video / search；不确定就先看上游是否声明了 supported_endpoint_types 或 domains")
	}
	switch m.Task() {
	case "sync", "async":
	default:
		add("/execution/task", fmt.Sprintf("execution.task 取值非法: %q", m.Task()),
			"只能是 sync 或 async")
	}

	inp := m.sub("input")
	for _, k := range []string{"max_reference_images", "max_reference_videos", "max_prompt_chars"} {
		if v, ok := inp[k]; ok {
			n := intOf(v)
			if float64(n) != toF(v) || n < 0 {
				add("/input/"+k, fmt.Sprintf("input.%s 必须是非负整数", k),
					"0 表示明确不支持；键缺失表示「未知、不拦截」；不要用 0 表达「还没填」")
			}
		}
	}

	for _, name := range m.ParamNames() {
		spec, _ := m.Parameters()[name].(map[string]any)
		enum, hasEnum := spec["enum"].([]any)
		if hasEnum {
			if len(enum) == 0 {
				add("/parameters/"+name, fmt.Sprintf("parameters.%s：enum 不能为空", name),
					"要么给出至少一个可选值，要么删掉 enum 表示任意值")
			} else if def, ok := spec["default"]; ok && !contains(enum, def) {
				add("/parameters/"+name, fmt.Sprintf("parameters.%s：默认值 %v 不在可选值里", name, def),
					fmt.Sprintf("把 default 改成 enum 里的一个值：%v", enum))
			}
		}
		mn, hasMin := spec["min"]
		mx, hasMax := spec["max"]
		if hasMin && hasMax && toF(mn) > toF(mx) {
			add("/parameters/"+name, fmt.Sprintf("parameters.%s：min 不能大于 max", name),
				"min/max 只要求出现一边；两边都写时必须 min ≤ max")
		}
	}
	return ps
}

func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func contains(list []any, v any) bool {
	for _, item := range list {
		if fmt.Sprint(item) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

// Overlays 是管理端数据：覆盖层 + 全局设置。
//
// 两者放不同层，因为 default_model 是全局设置，不是某个模型的属性。
type Overlays struct {
	Models   map[string]map[string]any `yaml:"models" json:"models"`
	Settings Settings                  `yaml:"settings" json:"settings"`
	// Disabled 是「上游仍在列、本地主动停用」的模型 ID 列表。放顶层而不是模型对象里：
	// 下架是管控动作，不是模型的一种能力，混进 merged JSON 会污染画像。
	//
	// 它不负责处理上游下架：Apply 只遍历上游返回的列表，上游不再列出的模型本来就
	// 自动消失，不需要任何配置，也不会因为这里留着条目而复活。别为了「跟上上游换版」
	// 去做 ID 前缀匹配 —— 那会把刚上线的新版本一起误杀。
	Disabled []string `yaml:"disabled" json:"disabled,omitempty"`

	// Protocols 是「协议族模板」：键默认按 kind 自动匹配（image / video / search），
	// 也允许自定义名字，由模型级覆盖里的 _protocol 显式引用。
	// 值是一份部分能力画像（execution / input / parameters / kind），只用于补缺。
	Protocols map[string]map[string]any `yaml:"protocols" json:"protocols,omitempty"`
}

// Settings 是全局设置。
type Settings struct {
	Defaults map[string]string `yaml:"defaults" json:"defaults"` // kind → model id
}

// DisabledSet 返回下架集合，忽略空串与首尾空白。
func (o Overlays) DisabledSet() map[string]bool {
	out := make(map[string]bool, len(o.Disabled))
	for _, id := range o.Disabled {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// Clone 深拷贝覆盖层，用于「已发布快照」与「工作副本」之间隔离。
//
// 没有它，管理端拿到的 Overlays() 和 store 内部持有的是同一批 map：
// 任何一次「就地改一个小字段」都会绕过校验和落盘，直接改到运行时。
func (o Overlays) Clone() Overlays {
	out := Overlays{
		Models:    make(map[string]map[string]any, len(o.Models)),
		Settings:  Settings{Defaults: make(map[string]string, len(o.Settings.Defaults))},
		Disabled:  append([]string(nil), o.Disabled...),
		Protocols: make(map[string]map[string]any, len(o.Protocols)),
	}
	for id, patch := range o.Models {
		if patch == nil {
			patch = map[string]any{}
		}
		out.Models[id] = cloneValue(patch).(map[string]any)
	}
	for k, v := range o.Settings.Defaults {
		out.Settings.Defaults[k] = v
	}
	for name, tpl := range o.Protocols {
		if tpl == nil {
			tpl = map[string]any{}
		}
		out.Protocols[name] = cloneValue(tpl).(map[string]any)
	}
	return out
}

// Canonical 返回规范形式：集合非 nil、空条目剔除、disabled 去重并排序。
//
// 规范形式同时是「落盘内容」和「版本号输入」。不排序的话，同一份下架列表
// 换个顺序就会得到不同的 ETag，客户端会误以为配置被别人改了。
func (o Overlays) Canonical() Overlays {
	out := Overlays{
		Models:    make(map[string]map[string]any, len(o.Models)),
		Settings:  Settings{Defaults: make(map[string]string, len(o.Settings.Defaults))},
		Disabled:  []string{},
		Protocols: make(map[string]map[string]any, len(o.Protocols)),
	}
	for id, patch := range o.Models {
		id = strings.TrimSpace(id)
		if id == "" || len(patch) == 0 {
			continue // 空补丁没有任何语义，留着只会让版本号抖动
		}
		out.Models[id] = cloneValue(patch).(map[string]any)
	}
	for kind, id := range o.Settings.Defaults {
		kind = strings.TrimSpace(kind)
		if kind == "" || strings.TrimSpace(id) == "" {
			continue
		}
		out.Settings.Defaults[kind] = strings.TrimSpace(id)
	}
	seen := map[string]bool{}
	for _, id := range o.Disabled {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out.Disabled = append(out.Disabled, id)
	}
	sort.Strings(out.Disabled)
	for name, tpl := range o.Protocols {
		name = strings.TrimSpace(name)
		if name == "" || len(tpl) == 0 {
			continue
		}
		out.Protocols[name] = cloneValue(tpl).(map[string]any)
	}
	return out
}

// PickDefault 返回某 kind 的默认模型：显式设置优先，否则取该 kind 的第一个。
func (o Overlays) PickDefault(kind string, models []Model) string {
	if id := o.Settings.Defaults[kind]; id != "" {
		for _, m := range models {
			if m.ID() == id {
				return id
			}
		}
		// 设置的模型已下架：回落到第一个，但调用方应把这种漂移报出来。
	}
	for _, m := range models {
		if m.Kind() == kind {
			return m.ID()
		}
	}
	return ""
}

// Apply 把覆盖层应用到模型列表上，保持上游顺序。
//
// 可见性是两层 AND：
//
//	① 上游 /v1/models 列出 —— 由入参 models 决定。未列出的一定不可见，
//	   覆盖层里的孤儿条目也不能让它复活，Disabled 更不参与这一层。
//	② 上游列出后，MCP 的 Disabled 没有「对下游禁用」该模型。
//
// 之后的 models.list / pickModel / PickDefault 都只用这里输出的同一份列表，没有旁路，
// 所以别在下游再写一遍可见性判断。
func (o Overlays) Apply(models []Model) []Model {
	disabled := o.DisabledSet()
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if disabled[m.ID()] {
			continue
		}
		out = append(out, o.ApplyOne(m))
	}
	return out
}

// protocolFor 决定该模型继承哪份协议族：显式 _protocol 优先，否则按「合并后的产物类型」匹配。
//
// 必须先看覆盖层里的 kind。真实聚合网关（如 newapi-bff）的 /models 里有一大批模型
// 不声明产物类型，只能由配置中心补；若这里仍读上游 kind（空），这些模型永远继承不到
// 协议模板，Transport() 会退回默认的 generations —— 视频模型会被打到图片端点上。
func (o Overlays) protocolFor(m Model, ov map[string]any) string {
	if name, _ := ov["_protocol"].(string); strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	if k, _ := ov["kind"].(string); strings.TrimSpace(k) != "" {
		return strings.TrimSpace(k)
	}
	return m.Kind()
}

// ApplyOne 对单个模型走完整合并链，是运行时 / 校验 / 状态端点的唯一真相源：
//
//	上游原始（已 Normalize）→ 内置/管理员协议模板「只补缺失键」→ 模型级覆盖「逐字段替换」
//
// 顺序不能反：模型级覆盖是人工修正，必须能盖掉自动继承来的值。
// 只有模型级覆盖或协议继承发生时才写 source，方便前端标「继承 / 已覆盖」。
func (o Overlays) ApplyOne(m Model) Model {
	ov, hasOv := o.Models[m.ID()]
	name := o.protocolFor(m, ov)
	tpl := o.effectiveProtocols()[name]
	_, isAdminTemplate := o.Protocols[name]

	work := m.Clone()
	if len(tpl) > 0 {
		fillMissing(work, tpl)
	}
	if hasOv {
		patch := make(map[string]any, len(ov))
		for k, v := range ov {
			if k == "_protocol" { // 管理字段，不进对外画像
				continue
			}
			patch[k] = cloneValue(v)
		}
		deepMerge(work, patch)
	}
	switch {
	case hasOv:
		work["source"] = "mcp"
	case len(tpl) > 0 && isAdminTemplate:
		work["source"] = "protocol"
	case len(tpl) > 0:
		work["source"] = "builtin"
	}
	return work
}

// Drift 是「配置中心 vs 上游」的差异报告。
//
// 前期把模型能力全部交给 MCP 配置中心时，配置与上游列表必须能自查：
// 上游模型 ID 常带日期版本（如 ...-260915），换版 / 下架会让覆盖层变成孤儿条目，
// 而 Apply 对孤儿条目是静默忽略的 —— 没有这份报告，故障只能等用户报错才发现。
type Drift struct {
	Orphaned     []string `json:"orphaned"`     // 配了，上游没有（含失效的下架项）
	Unconfigured []string `json:"unconfigured"` // 上游有且能识别类型，但没配
	Configured   []string `json:"configured"`   // 上游有且已配
	Disabled     []string `json:"disabled"`     // 上游有，已被 MCP 整条下架
	Inherited    []string `json:"inherited"`    // 上游有，靠协议模板补齐（未逐条配置）
}

// Drift 对照上游模型列表计算差异。models 应已完成 Normalize，与运行时判定一致。
//
// 用 ApplyOne 的结果分类，保证与管理端显示、运行时画像完全一致；
// 只统计「能识别产物类型」的模型：LLM / Embedding 本来就不属于这里的业务。
// 观测不等于过滤：unconfigured 里的模型当前仍会被 KnownOnly 放行、出现在 models.list，
// 并可能被 PickDefault 选为默认模型。这是前期路线的已知缺口（README §10）。
func (o Overlays) Drift(models []Model) Drift {
	out := Drift{
		Orphaned: []string{}, Unconfigured: []string{}, Configured: []string{},
		Disabled: []string{}, Inherited: []string{},
	}
	seen := map[string]bool{}
	for _, m := range models {
		id := m.ID()
		if id == "" {
			continue
		}
		seen[id] = true
		eff := o.ApplyOne(m)
		if eff.Kind() == KindUnknown {
			continue
		}
		switch eff["source"] {
		case "mcp":
			out.Configured = append(out.Configured, id)
		case "protocol":
			out.Inherited = append(out.Inherited, id)
		default:
			out.Unconfigured = append(out.Unconfigured, id)
		}
	}
	orphans := map[string]bool{}
	for id := range o.Models {
		if !seen[id] {
			orphans[id] = true
		}
	}
	for id := range o.DisabledSet() {
		if seen[id] {
			out.Disabled = append(out.Disabled, id)
		} else {
			orphans[id] = true
		}
	}
	for id := range orphans {
		out.Orphaned = append(out.Orphaned, id)
	}
	sort.Strings(out.Orphaned)
	sort.Strings(out.Unconfigured)
	sort.Strings(out.Configured)
	sort.Strings(out.Disabled)
	sort.Strings(out.Inherited)
	return out
}

// KnownOnly 只保留能识别产物类型的模型。
//
// 真实上游的 /models 里混着大量 LLM / VLM / Embedding，它们不属于这个 MCP 的业务范围。
// 全量返回只会让客户端在一堆不可用的条目里挑。
func KnownOnly(models []Model) []Model {
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if m.Kind() != KindUnknown {
			out = append(out, m)
		}
	}
	return out
}

// Filter 按产物类型过滤，空字符串表示不过滤。
func Filter(models []Model, kind string) []Model {
	if kind == "" {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if m.Kind() == kind {
			out = append(out, m)
		}
	}
	return out
}

// Find 按 id 查找。
func Find(models []Model, id string) (Model, bool) {
	for _, m := range models {
		if m.ID() == id {
			return m, true
		}
	}
	return nil, false
}

// EscapePointer 按 RFC 6901 转义 JSON Pointer 的一段。
//
// 管理端与校验共用它：模型 ID 里可能出现 ~ 或 /（如 openai/gpt-4），
// 路径要能被客户端原样拼回去。
func EscapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// Summary 生成人类可读的一行摘要，供日志与调试输出。
func (m Model) Summary() string {
	if m.Kind() == "search" {
		return fmt.Sprintf("%s (search, %s)", m.ID(), m.Task())
	}
	ref, known := m.MaxReferenceImages()
	var mode string
	switch {
	case !known:
		mode = "ref=?"
	case ref > 0:
		mode = fmt.Sprintf("ref<=%d", ref)
	default:
		mode = "no-ref"
	}
	return fmt.Sprintf("%s (%s, %s, %s)", m.ID(), m.Kind(), mode, m.Task())
}

// NormalizeKind 归一化外部传入的 kind 过滤值。
func NormalizeKind(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
