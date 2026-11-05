// Package tools 注册 MCP 工具。
//
// 工具分两类（README §8）：
//   - 业务工具：image.generate / video.generate / search.query —— 按产物类型分
//   - 通用工具：models.list / task.get —— 跨模态共用
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/taskid"
	"github.com/apiduo/mcp-for-apiduo/internal/upstream"
)

// Deps 是工具依赖。Overlays 用函数注入，便于管理端热更新。
type Deps struct {
	Up       *upstream.Client
	Overlays func() catalog.Overlays
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	models  []catalog.Model
	expires time.Time
}

// Register 把所有工具挂到 server 上。
func Register(s *mcp.Server, d *Deps) {
	if d.CacheTTL <= 0 {
		d.CacheTTL = 60 * time.Second
	}
	d.cache = map[string]*cacheEntry{}

	mcp.AddTool(s, &mcp.Tool{
		Name: "models.list",
		Description: "列出当前 key 可用的模型及其能力（kind / execution / input / parameters）。" +
			"上游 /v1/models 与 MCP 覆盖层合并后返回，覆盖优先。调用生成类工具前若不确定参数，先查这里。",
	}, d.listModels)

	mcp.AddTool(s, &mcp.Tool{
		Name: "image.generate",
		Description: "生成图片。不传 reference_images 即文生图，传了即图生图——模式由垫图推导，无需单独声明。" +
			"若模型是异步的，立即返回 task_id，用 task.get 查询结果。" +
			"size/n/quality 留空即交给上游默认；若从提示词推断，只能取 models.list 里声明的 enum 值，不要臆造。",
	}, d.generateImage)

	mcp.AddTool(s, &mcp.Tool{
		Name: "video.generate",
		Description: "生成视频。语义与 image.generate 相同，产物是视频；" +
			"传 reference_images 可做图生视频。多数视频模型是异步的，请用 task.get 轮询。" +
			"duration/size 留空即交给上游默认；从提示词推断时只能取画像声明的 enum 值。",
	}, d.generateVideo)

	mcp.AddTool(s, &mcp.Tool{
		Name: "search.query",
		Description: "联网检索。query 支持 site: / -site: / \"精确短语\" / lang: / after: / before: 等算符，" +
			"原样透传给上游搜索引擎。返回结构化结果数组，不是单个资产。",
	}, d.search)

	mcp.AddTool(s, &mcp.Tool{
		Name: "task.get",
		Description: "查询异步任务状态。task_id 由 image.generate / video.generate 返回，" +
			"自包含签名，任何副本都能处理。",
	}, d.getTask)
}

// ── 公共：取 key、取模型 ──────────────────────────────────────────────────

type keyCtxKey struct{}

// WithAPIKey 把客户端令牌放进 context。由 HTTP 层注入。
func WithAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyCtxKey{}, key)
}

func apiKeyFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyCtxKey{}).(string)
	return s
}

// APIKey 从 context 取本次请求的令牌。
//
// 导出是为了让 HTTP 层能断言「这个请求最终用的是谁的身份」——
// 鉴权绕过正是发生在这个取值上，测试必须能看见它。
func APIKey(ctx context.Context) string { return apiKeyFrom(ctx) }

// models 拉取并合并模型列表。按 key 分桶缓存，因为不同 key 可见的模型可能不同。
func (d *Deps) models(ctx context.Context, apiKey string) ([]catalog.Model, error) {
	bucket := hashKey(apiKey)

	d.mu.Lock()
	if e, ok := d.cache[bucket]; ok && time.Now().Before(e.expires) {
		out := e.models
		d.mu.Unlock()
		return out, nil
	}
	d.mu.Unlock()

	raw, err := d.Up.ListModels(ctx, apiKey)
	if err != nil {
		return nil, err
	}
	// 先按上游声明的结构化字段归一化 kind，再叠加 MCP 覆盖层。
	// 顺序不能反：覆盖层是人工修正，应该能盖掉推导结果。
	normalized := make([]catalog.Model, 0, len(raw))
	for _, m := range raw {
		normalized = append(normalized, catalog.Normalize(m))
	}
	merged := d.Overlays().Apply(normalized)

	d.mu.Lock()
	d.cache[bucket] = &cacheEntry{models: merged, expires: time.Now().Add(d.CacheTTL)}
	d.mu.Unlock()
	return merged, nil
}

// InvalidateCache 在管理端保存覆盖层后调用，让新配置立刻生效。
func (d *Deps) InvalidateCache() {
	d.mu.Lock()
	d.cache = map[string]*cacheEntry{}
	d.mu.Unlock()
}

func hashKey(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// pickModel 选模型：显式指定优先，否则取该 kind 的默认（管理端设置 → 第一个）。
func (d *Deps) pickModel(models []catalog.Model, kind, want string) (catalog.Model, error) {
	if want != "" {
		m, ok := catalog.Find(models, want)
		if !ok {
			return nil, fmt.Errorf("模型 %q 不在可用列表里，先用 models.list 查看", want)
		}
		if m.Kind() != kind {
			return nil, fmt.Errorf("模型 %q 的产物类型是 %s，不能用于 %s", want, m.Kind(), kind)
		}
		return m, nil
	}
	id := d.Overlays().PickDefault(kind, models)
	if id == "" {
		return nil, fmt.Errorf("没有可用的 %s 模型，检查上游渠道或管理端配置", kind)
	}
	m, ok := catalog.Find(models, id)
	if !ok {
		return nil, fmt.Errorf("默认 %s 模型 %q 已不可用", kind, id)
	}
	return m, nil
}

// ── models.list ─────────────────────────────────────────────────────────

// ListInput 是 models.list 的入参。
type ListInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"按产物类型过滤：image / video / search。留空返回全部"`
}

// ListOutput 是 models.list 的出参。
type ListOutput struct {
	Models []catalog.Model `json:"models"`
	Count  int             `json:"count"`
}

func (d *Deps) listModels(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListOutput, error) {
	key := apiKeyFrom(ctx)
	models, err := d.models(ctx, key)
	if err != nil {
		return nil, ListOutput{}, err
	}
	kind := catalog.NormalizeKind(in.Kind)
	filtered := catalog.Filter(models, kind)
	if kind == "" {
		// 不指定类型时也不返回无法识别的模型：它们是 LLM/Embedding，不属于这里的业务。
		filtered = catalog.KnownOnly(models)
	}
	return text(fmt.Sprintf("共 %d 个模型", len(filtered))), ListOutput{Models: filtered, Count: len(filtered)}, nil
}

// ── 生成：image / video ─────────────────────────────────────────────────

// GenerateInput 是生成类工具的入参。
//
// 只放通用参数；模型特有参数走 Extra，且必须被画像声明过。
type GenerateInput struct {
	Prompt          string          `json:"prompt" jsonschema:"提示词，描述想要的内容"`
	Model           string          `json:"model,omitempty" jsonschema:"模型 id。留空用该类型的默认模型"`
	ReferenceImages any             `json:"reference_images,omitempty" jsonschema:"参考图 URL，单个字符串或字符串数组。传了就由图生图/图生视频"`
	Size            string          `json:"size,omitempty" jsonschema:"尺寸，取值见 models.list 里该模型的 parameters.size.enum，如 2K 或 1024x1024"`
	N               *int            `json:"n,omitempty" jsonschema:"生成数量，上限由模型画像约束"`
	Quality         string          `json:"quality,omitempty" jsonschema:"质量档位，取值见该模型的 parameters.quality.enum"`
	Extra           map[string]any  `json:"extra,omitempty" jsonschema:"该模型声明的其他参数，键必须在画像里存在"`
}

// Output 是单个产物。
type Output struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// GenerateOutput 是生成类工具的出参。
type GenerateOutput struct {
	Status   string         `json:"status"` // completed | working
	Model    string         `json:"model"`
	TaskID   string         `json:"task_id,omitempty"`
	Outputs  []Output       `json:"outputs,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
	PollMS   int            `json:"poll_after_ms,omitempty"`
	Upstream map[string]any `json:"upstream,omitempty"` // 原始响应，便于排障
}

func (d *Deps) generateImage(ctx context.Context, req *mcp.CallToolRequest, in GenerateInput) (*mcp.CallToolResult, GenerateOutput, error) {
	return d.generate(ctx, "image", in)
}

func (d *Deps) generateVideo(ctx context.Context, req *mcp.CallToolRequest, in GenerateInput) (*mcp.CallToolResult, GenerateOutput, error) {
	return d.generate(ctx, "video", in)
}

func (d *Deps) generate(ctx context.Context, kind string, in GenerateInput) (*mcp.CallToolResult, GenerateOutput, error) {
	key := apiKeyFrom(ctx)
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, GenerateOutput{}, fmt.Errorf("prompt 不能为空")
	}

	models, err := d.models(ctx, key)
	if err != nil {
		return nil, GenerateOutput{}, err
	}
	m, err := d.pickModel(models, kind, in.Model)
	if err != nil {
		return nil, GenerateOutput{}, err
	}

	refs, err := parseRefs(in.ReferenceImages)
	if err != nil {
		return nil, GenerateOutput{}, err
	}

	payload, err := buildPayload(m, in, refs)
	if err != nil {
		return nil, GenerateOutput{}, err
	}

	resp, err := d.Up.Generate(ctx, key, m, payload)
	if err != nil {
		return nil, GenerateOutput{}, err
	}

	// 声明决定预期，响应决定事实：即便是 sync 模型，上游给了任务 ID 也照实返回任务。
	if tid, ok := firstString(resp, "task_id", "id"); ok && m.Task() == "async" {
		signed := taskid.Sign(key, taskid.Payload{
			Model:    m.ID(),
			Upstream: tid,
			Kind:     m.Kind(),
			Protocol: m.Transport(), // 查询端点必须与生成端点同源
			IssuedAt: time.Now().Unix(),
		})
		return text("已提交，任务 " + signed + " 进行中"), GenerateOutput{
			Status: "working", Model: m.ID(), TaskID: signed, PollMS: 2000, Upstream: resp,
		}, nil
	}

	out := parseOutputs(resp)
	if len(out) == 0 {
		// 上游没给可用产物：如实说明，不要伪造成功。
		return nil, GenerateOutput{}, fmt.Errorf("上游未返回图片/视频地址：%s", brief(resp))
	}
	return text(fmt.Sprintf("生成完成，共 %d 个产物", len(out))), GenerateOutput{
		Status: "completed", Model: m.ID(), Outputs: out, Usage: usageOf(resp), Upstream: resp,
	}, nil
}

// buildPayload 把 MCP 入参翻译成上游请求体。
//
// 三条规则（README §8）：
//   - reference_images → image，统一发数组
//   - response_format 固定 url，不暴露给调用方
//   - 只有画像声明过的参数才接受，其余本地拒绝
func buildPayload(m catalog.Model, in GenerateInput, refs []string) (map[string]any, error) {
	p := map[string]any{
		"model":           m.ID(),
		"prompt":          in.Prompt,
		"response_format": "url",
	}
	if len(refs) > 0 {
		// 只有「已知上限」才拦截。未知就交给上游判定 ——
		// 大多数上游（如火山 Ark）根本不声明这个字段，按 0 处理会误杀图生图能力。
		if maxRef, known := m.MaxReferenceImages(); known {
			if maxRef <= 0 {
				return nil, fmt.Errorf("模型 %s 不支持参考图（垫图上限 0）", m.ID())
			}
			if len(refs) > maxRef {
				return nil, fmt.Errorf("模型 %s 最多接受 %d 张参考图，收到 %d 张", m.ID(), maxRef, len(refs))
			}
		}
		p["image"] = refs
	}

	if in.Size != "" {
		if err := checkEnum(m, "size", in.Size); err != nil {
			return nil, err
		}
		p["size"] = in.Size
	}
	if in.Quality != "" {
		if err := checkEnum(m, "quality", in.Quality); err != nil {
			return nil, err
		}
		p["quality"] = in.Quality
	}
	if in.N != nil {
		if *in.N < 1 {
			return nil, fmt.Errorf("n 必须 ≥ 1，收到 %d", *in.N)
		}
		if err := checkRange(m, "n", float64(*in.N)); err != nil {
			return nil, err
		}
		if _, declared := m.Parameters()["n"].(map[string]any); !declared && *in.N > maxBatchN {
			// 画像没声明上限时的兜底：上游常常只认 n、不给 max，
			// 于是一次 n=1000 就能把额度打光。这不是替上游做主，
			// 是不让「没配置」等价于「没有上限」。
			return nil, fmt.Errorf("模型 %s 未声明 n 的上限，单次最多 %d；要更多请分多次调用", m.ID(), maxBatchN)
		}
		p["n"] = *in.N
	}
	for k, v := range in.Extra {
		if !m.HasParam(k) && !standardParams[k] {
			return nil, fmt.Errorf("模型 %s 未声明参数 %q，可用参数：%s", m.ID(), k, strings.Join(m.ParamNames(), ", "))
		}
		// 声明了约束就要真的用上：否则配置中心里填的 enum / min / max 只是文档，
		// 越界值照样打到上游再吃 400（甚至按非法参数计费）。
		if err := checkExtra(m, k, v); err != nil {
			return nil, err
		}
		p[k] = v
	}
	return p, nil
}

// standardParams 是契约里定义的通用参数。
//
// 模型画像缺失时（真实上游常常不给），这几个参数仍然允许透传 ——
// 否则 size/n 这类标准参数会被白名单误杀，模型直接不可用。
// 非标准参数仍必须由画像显式声明。
// maxBatchN 是「模型画像没声明 n 的上限」时的兜底数量。
// 声明了 max 的模型按声明走 —— 有约束的地方不该被一个常量覆盖。
const maxBatchN = 8

var standardParams = map[string]bool{
	"size": true, "quality": true, "n": true, "format": true,
	"duration": true, "resolution": true, "aspect_ratio": true,
	"count": true, "include_content": true,
}

func checkEnum(m catalog.Model, name, val string) error {
	spec, ok := m.Parameters()[name].(map[string]any)
	if !ok {
		// 没画像就无从校验，交给上游判定 —— 但不代表可以随便传，调用方只能传标准参数。
		return nil
	}
	enum, ok := spec["enum"].([]any)
	if !ok {
		return nil
	}
	for _, v := range enum {
		if fmt.Sprint(v) == val {
			return nil
		}
	}
	opts := make([]string, 0, len(enum))
	for _, v := range enum {
		opts = append(opts, fmt.Sprint(v))
	}
	return fmt.Errorf("%s 取值 %q 不被模型 %s 接受，可选：%s", name, val, m.ID(), strings.Join(opts, ", "))
}

func checkRange(m catalog.Model, name string, val float64) error {
	spec, _ := m.Parameters()[name].(map[string]any)
	if spec == nil {
		return nil // 无画像，交给上游
	}
	if mn, ok := toFloat(spec["min"]); ok && val < mn {
		return fmt.Errorf("%s 不能小于 %v", name, mn)
	}
	if mx, ok := toFloat(spec["max"]); ok && val > mx {
		return fmt.Errorf("%s 不能大于 %v", name, mx)
	}
	return nil
}

// checkExtra 校验 Extra 里的模型特有参数。enum 优先（和 catalog.Validate 同序），
// 没有 enum 才看 min / max；两样都没声明就放行 —— 声明了参数名但没给约束是允许的，
// 那种情况等于「任意值」。值类型对不上时只跳过数值比较，不做类型强制。
func checkExtra(m catalog.Model, name string, val any) error {
	spec, ok := m.Parameters()[name].(map[string]any)
	if !ok {
		return nil
	}
	if enum, ok := spec["enum"].([]any); ok && len(enum) > 0 {
		s := fmt.Sprint(val)
		for _, v := range enum {
			if fmt.Sprint(v) == s {
				return nil
			}
		}
		opts := make([]string, 0, len(enum))
		for _, v := range enum {
			opts = append(opts, fmt.Sprint(v))
		}
		return fmt.Errorf("%s 取值 %q 不被模型 %s 接受，可选：%s", name, s, m.ID(), strings.Join(opts, ", "))
	}
	f, numeric := toFloat(val)
	if !numeric {
		return nil
	}
	if mn, ok := toFloat(spec["min"]); ok && f < mn {
		return fmt.Errorf("%s 不能小于 %v", name, mn)
	}
	if mx, ok := toFloat(spec["max"]); ok && f > mx {
		return fmt.Errorf("%s 不能大于 %v", name, mx)
	}
	return nil
}

// ── search.query ────────────────────────────────────────────────────────

// SearchInput 是 search.query 的入参。
type SearchInput struct {
	Query          string         `json:"query" jsonschema:"检索词，1-128 字符，支持 site: / lang: / after: 等算符"`
	Model          string         `json:"model,omitempty" jsonschema:"搜索模型 id。留空用默认搜索模型"`
	Count          *int           `json:"count,omitempty" jsonschema:"返回条数，上限由模型画像约束"`
	IncludeContent *bool          `json:"include_content,omitempty" jsonschema:"是否返回网页正文，部分供应商支持"`
	Extra          map[string]any `json:"extra,omitempty" jsonschema:"该模型声明的其他参数"`
}

// SearchOutput 是 search.query 的出参。
type SearchOutput struct {
	Model   string           `json:"model"`
	Results []map[string]any `json:"results"`
	Count   int              `json:"count"`
}

func (d *Deps) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	key := apiKeyFrom(ctx)
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return nil, SearchOutput{}, fmt.Errorf("query 不能为空")
	}
	if len([]rune(q)) > 128 {
		return nil, SearchOutput{}, fmt.Errorf("query 不能超过 128 个字符")
	}

	models, err := d.models(ctx, key)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	m, err := d.pickModel(models, "search", in.Model)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	p, err := buildSearchPayload(m, in, q)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	resp, err := d.Up.Search(ctx, key, p)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	results := toSlice(resp["results"])
	return text(fmt.Sprintf("检索到 %d 条结果", len(results))), SearchOutput{
		Model: m.ID(), Results: results, Count: len(results),
	}, nil
}

// buildSearchPayload 与 buildPayload 同构：先白名单，再校验约束。
// 拆出来是为了能被单测直接盯住 —— 「声明的约束必须拦得住调用」这条规则
// 在两个入口上必须一模一样，靠肉眼比对迟早会漏。
func buildSearchPayload(m catalog.Model, in SearchInput, q string) (map[string]any, error) {
	p := map[string]any{"model": m.ID(), "query": q}
	if in.Count != nil {
		if err := checkRange(m, "count", float64(*in.Count)); err != nil {
			return nil, err
		}
		p["count"] = *in.Count
	}
	if in.IncludeContent != nil {
		if !m.HasParam("include_content") {
			return nil, fmt.Errorf("模型 %s 不支持 include_content", m.ID())
		}
		p["need_content"] = *in.IncludeContent
	}
	for k, v := range in.Extra {
		if !m.HasParam(k) && !standardParams[k] {
			return nil, fmt.Errorf("模型 %s 未声明参数 %q", m.ID(), k)
		}
		// 早先这里只检查「键存不存在」，于是「配置中心写了 count ≤ 10」
		// 在 search.query 上等于一句注释，越界值照样打到上游。
		if err := checkExtra(m, k, v); err != nil {
			return nil, err
		}
		p[k] = v
	}
	return p, nil
}

// ── task.get ────────────────────────────────────────────────────────────

// TaskInput 是 task.get 的入参。
type TaskInput struct {
	TaskID string `json:"task_id" jsonschema:"image.generate 或 video.generate 返回的 task_id"`
}

// TaskOutput 是 task.get 的出参。
type TaskOutput struct {
	Status   string         `json:"status"` // working | completed | failed
	TaskID   string         `json:"task_id"`
	Model    string         `json:"model"`
	Outputs  []Output       `json:"outputs,omitempty"`
	Error    string         `json:"error,omitempty"`
	PollMS   int            `json:"poll_after_ms,omitempty"`
	Upstream map[string]any `json:"upstream,omitempty"`
}

func (d *Deps) getTask(ctx context.Context, _ *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, TaskOutput, error) {
	key := apiKeyFrom(ctx)
	p, err := taskid.Parse(key, in.TaskID)
	if err != nil {
		return nil, TaskOutput{}, fmt.Errorf("task_id 无效或已过期（换过 api_key？）")
	}

	resp, err := d.Up.GetTask(ctx, key, p.Protocol, p.Upstream)
	if err != nil {
		return nil, TaskOutput{}, err
	}

	status := normalizeStatus(str(resp, "status"))
	out := TaskOutput{Status: status, TaskID: in.TaskID, Model: p.Model, Upstream: resp}
	switch status {
	case "completed":
		out.Outputs = parseOutputs(resp)
	case "working":
		out.PollMS = 2000
	case "failed":
		out.Error = firstNonEmpty(str(resp, "error"), str(resp, "message"), "上游任务失败")
	}
	return text("任务状态：" + status), out, nil
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// parseRefs 兼容 string 与 string[]：入口宽松，内部统一成数组。
//
// 入参声明为 any 而不是 json.RawMessage：后者是 []byte，
// SDK 推断 schema 时会当成字节数组，客户端传字符串直接被拦在协议层。
func parseRefs(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			sv, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("reference_images 数组元素必须是字符串")
			}
			if v := strings.TrimSpace(sv); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	case []string:
		out := make([]string, 0, len(t))
		for _, sv := range t {
			if v := strings.TrimSpace(sv); v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("reference_images 需为字符串或字符串数组")
}

// parseOutputs 从上游响应里提取产物地址，兼容几种常见形状。
func parseOutputs(resp map[string]any) []Output {
	var out []Output
	// OpenAI 风格：data: [{url}]
	for _, item := range toSlice(resp["data"]) {
		if u := str(item, "url"); u != "" {
			out = append(out, Output{URL: u, MimeType: str(item, "mime_type")})
		}
	}
	// new-api 任务风格：result / outputs
	for _, key := range []string{"result", "outputs", "videos", "images"} {
		for _, item := range toSlice(resp[key]) {
			if u := str(item, "url"); u != "" {
				out = append(out, Output{URL: u, MimeType: str(item, "mime_type")})
			}
		}
	}
	// 火山 Ark 视频：产物在嵌套对象里，content: {video_url}
	// 这是最容易漏的一种形状：字段没错，只是多了一层，结果被当成「上游没返回产物」。
	for _, holder := range []string{"content", "output", "result"} {
		obj, ok := resp[holder].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"video_url", "image_url", "url"} {
			if u := str(obj, key); u != "" {
				out = append(out, Output{URL: u, MimeType: str(obj, "mime_type")})
			}
		}
	}

	// 单值兜底
	if len(out) == 0 {
		for _, key := range []string{"url", "video_url", "image_url"} {
			if u := str(resp, key); u != "" {
				out = append(out, Output{URL: u})
			}
		}
	}
	return out
}

func usageOf(resp map[string]any) map[string]any {
	if u, ok := resp["usage"].(map[string]any); ok {
		return u
	}
	return nil
}

func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "succeeded", "success", "done":
		return "completed"
	case "failed", "error", "cancelled", "canceled":
		return "failed"
	default:
		return "working"
	}
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s := str(m, k); s != "" {
			return s, true
		}
	}
	return "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func toSlice(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return t
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func brief(resp map[string]any) string {
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, _ := json.Marshal(resp)
	s := string(b)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return fmt.Sprintf("字段 [%s] %s", strings.Join(keys, ","), s)
}
