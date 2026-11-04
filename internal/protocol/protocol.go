// Package protocol 把「请求形状」显式定义为协议族（protocol family）。
//
// 为什么需要这一层：transport 不是一个 URL，而是**一整套端点**。
// 真实上游的差异不在「生成打到哪」，而在「生成 + 查询两处路径的配对关系」：
//
//	火山 Ark  ：POST /contents/generations/tasks      GET /contents/generations/tasks/{id}
//	new-api   ：POST /videos/generations              GET /tasks/{id}
//
// 只记生成端点的话，task.get 会拿着火山的任务键去打 /tasks/{id}，
// 结果是空 body 的 404 —— 报错信息里什么都看不出来，只能靠猜。
//
// 所以协议族是「生成端点 + 查询端点 + 请求编码」三件套的绑定，
// 三者必须同源，不能分开配置。
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Default 是缺省协议族，对应最常见的 OpenAI 兼容形状。
const Default = "generations"

// Fetcher 下载外部资源。只被 multipart 编码器使用。
//
// 拆成函数类型而不是直接依赖 http.Client：protocol 包不该知道网络细节，
// 而「怎么安全地下载第三方 URL」属于 upstream 的职责（见 upstream/fetch.go）。
type Fetcher func(ctx context.Context, rawURL string) (data []byte, contentType string, err error)

// Encoder 把通用 payload 编码成请求体。
type Encoder func(ctx context.Context, payload map[string]any, fetch Fetcher) (body []byte, contentType string, err error)

// Shape 把通用 payload 映射到某个协议族的上游字段。
//
// nil 表示字段名通用，原样发送。非 nil 用于「结构不同」的上游 ——
// 换字段名能靠配置解决，结构不同不能：火山视频没有 prompt 字段，
// 提示词与素材都在 content 数组里按 type 区分。
type Shape func(payload map[string]any) (map[string]any, error)

// Spec 是一个协议族。
type Spec struct {
	Name string
	// Generate 是生成端点，相对 base_url。
	Generate string
	// Query 是任务查询端点模板，{id} 为上游任务键占位。空表示该协议不支持查询。
	Query string
	// Encode 生成请求体。
	Encode Encoder
	// Shape 映射请求字段，nil 表示原样发送。
	Shape Shape
}

// SupportsQuery 报告该协议能否查询任务。
func (s Spec) SupportsQuery() bool { return s.Query != "" }

// QueryPath 把上游任务键填进查询端点模板。
//
// taskKey 必须转义：它来自 task_id 的签名内容，虽然经过 HMAC 校验，
// 但仍然不该让一个含 "/" 或 "?" 的键改变请求路径。
func (s Spec) QueryPath(taskKey string) string {
	return strings.ReplaceAll(s.Query, "{id}", url.PathEscape(taskKey))
}

var specs = map[string]Spec{
	// OpenAI 兼容图片生成：image 字段统一发数组
	"generations": {
		Name:     "generations",
		Generate: "/images/generations",
		Query:    "/tasks/{id}",
		Encode:   EncodeJSON,
	},
	// OpenAI 原厂编辑端点：multipart 表单，参考图必须是文件本体
	"edits_multipart": {
		Name:     "edits_multipart",
		Generate: "/images/edits",
		Query:    "/tasks/{id}",
		Encode:   EncodeMultipart,
	},
	// new-api 视频生成
	"video_generations": {
		Name:     "video_generations",
		Generate: "/videos/generations",
		Query:    "/tasks/{id}",
		Encode:   EncodeJSON,
	},
	// 火山 Ark 内容生成：生成与查询同在 /contents/generations/tasks 之下，
	// 且请求体是 content 数组而非 prompt/image 字段。
	"contents_tasks": {
		Name:     "contents_tasks",
		Generate: "/contents/generations/tasks",
		Query:    "/contents/generations/tasks/{id}",
		Encode:   EncodeJSON,
		Shape:    shapeArkContents,
	},
	// 搜索工具端点：同步返回结果数组，没有任务概念
	"search_query": {
		Name:     "search_query",
		Generate: "/tools/search",
		Query:    "",
		Encode:   EncodeJSON,
	},
}

// ErrUnknown 表示模型画像里写了不认识的 transport。
var ErrUnknown = errors.New("未知的 transport")

// Get 按名称取协议族。空名按默认处理。
//
// 不认识的名称必须报错而不是回落到默认：那意味着画像与代码版本不匹配，
// 静默回落会让请求打到错误端点，而错误只在运行时才显形。
func Get(name string) (Spec, error) {
	if name == "" {
		name = Default
	}
	s, ok := specs[name]
	if !ok {
		return Spec{}, fmt.Errorf("%w: %q（可用：%s）", ErrUnknown, name, strings.Join(Names(), ", "))
	}
	return s, nil
}

// Names 返回全部协议族名，用于错误提示与文档一致性检查。
func Names() []string {
	out := make([]string, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	// 固定顺序，避免错误消息随 map 迭代变化
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// EncodeJSON 是默认编码器：payload 直接序列化。
func EncodeJSON(_ context.Context, payload map[string]any, _ Fetcher) ([]byte, string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("编码请求失败: %w", err)
	}
	return b, "application/json", nil
}

// EncodeMultipart 把 payload 编码成 multipart/form-data。
//
// 与 JSON 的两处关键差异：
//   - image 不能是 URL，必须是文件本体 ⇒ 逐个下载后作为 part 上传
//   - 数组与对象无法用表单表达 ⇒ 只发标量字段，其余跳过（不能发字符串 "[object]"）
//
// 单图用 field 名 image，多图用 image[]：OpenAI 原厂按后者收多张参考图。
func EncodeMultipart(ctx context.Context, payload map[string]any, fetch Fetcher) ([]byte, string, error) {
	if fetch == nil {
		return nil, "", errors.New("multipart 编码需要资源下载器")
	}
	refs := refList(payload["image"])
	if len(refs) == 0 {
		return nil, "", errors.New("该协议要求上传参考图文件，但没有可用的 image URL")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	for k, v := range payload {
		if k == "image" {
			continue // 单独作为文件 part 处理
		}
		if s, ok := scalarString(v); ok {
			if err := w.WriteField(k, s); err != nil {
				return nil, "", fmt.Errorf("写表单字段 %s 失败: %w", k, err)
			}
		}
	}

	field := "image"
	if len(refs) > 1 {
		field = "image[]"
	}
	for i, ref := range refs {
		data, mime, err := fetch(ctx, ref)
		if err != nil {
			return nil, "", fmt.Errorf("下载第 %d 张参考图失败: %w", i+1, err)
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`,
			field, filenameFor(ref, mime, i)))
		if mime == "" {
			mime = "application/octet-stream"
		}
		h.Set("Content-Type", mime)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", fmt.Errorf("创建 multipart part 失败: %w", err)
		}
		if _, err := part.Write(data); err != nil {
			return nil, "", fmt.Errorf("写入参考图失败: %w", err)
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("结束 multipart 失败: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// shapeArkContents 把通用 payload 映射成火山 Ark 内容生成的请求体。
//
// 依据方舟「创建视频生成任务」文档（POST /contents/generations/tasks）：
//
//	{"model": "...", "content": [
//	    {"type": "text", "text": "..."},
//	    {"type": "image_url", "image_url": {"url": "..."}, "role": "reference_image"}
//	], "ratio": "16:9", "resolution": "720p", "duration": 5}
//
// 三个必须在这里消化、不能推给调用方的差异：
//   - 没有 prompt 字段，提示词是 content 里的一项
//   - 没有 image 字段，素材是 content 里的若干项
//   - 没有 size 字段，尺寸拆成 ratio（宽高比）+ resolution（分辨率）
//
// ⚠️ 当前账号未开通火山的视频模型（ModelNotOpen），该映射经文档与 mock 验证，
// 尚未通过真实调用确认。开通后应补一次最小规格的真实请求。
func shapeArkContents(p map[string]any) (map[string]any, error) {
	prompt, _ := p["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt 不能为空")
	}

	content := []any{map[string]any{"type": "text", "text": prompt}}
	for _, u := range refList(p["image"]) {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": u},
			"role":      "reference_image",
		})
	}

	out := map[string]any{"model": p["model"], "content": content}
	for k, v := range p {
		switch k {
		case "model", "prompt", "image", "response_format":
			continue // 已处理，或对端不存在该概念
		case "size":
			// 静默丢弃会让调用方以为尺寸生效了，而实际用的是上游默认值。
			return nil, errors.New("该协议不接受 size，请改用 aspect_ratio 与 resolution")
		case "aspect_ratio":
			// 契约里的宽高比，火山叫 ratio。
			// 调用方只按契约的名字传，翻译由协议族负责 —— 否则每接一个上游，
			// 客户端就要多记一套参数名。
			out["ratio"] = v
			continue
		}
		out[k] = v
	}
	return out, nil
}

// refList 把 image 字段归一化成 URL 列表，兼容 string 与 string[]。
func refList(v any) []string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
	case []string:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// scalarString 把标量转成表单值。数组/对象返回 false（无法用表单表达）。
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// filenameFor 给上传的文件起名。OpenAI 不看名字，但缺扩展名时某些实现会误判类型。
func filenameFor(rawURL, mime string, idx int) string {
	name := "image"
	if u, err := url.Parse(rawURL); err == nil {
		if b := path.Base(u.Path); b != "" && b != "." && b != "/" {
			name = b
		}
	}
	if path.Ext(name) == "" {
		switch mime {
		case "image/jpeg":
			name += ".jpg"
		case "image/webp":
			name += ".webp"
		case "image/gif":
			name += ".gif"
		default:
			name += ".png"
		}
	}
	if idx > 0 {
		return fmt.Sprintf("%d-%s", idx+1, name)
	}
	return name
}
