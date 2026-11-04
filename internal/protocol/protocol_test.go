package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	// 空名按默认处理
	spec, err := Get("")
	if err != nil || spec.Name != Default {
		t.Fatalf("空名应回落到默认: %v %q", err, spec.Name)
	}

	for _, name := range []string{
		"generations", "edits_multipart", "video_generations", "contents_tasks", "search_query",
	} {
		if _, err := Get(name); err != nil {
			t.Errorf("%s 应可取到: %v", name, err)
		}
	}

	// 未知名称必须报错，不能静默回落 —— 否则请求会打到错误端点
	if _, err := Get("grpc"); !errors.Is(err, ErrUnknown) {
		t.Errorf("未知 transport 应返回 ErrUnknown，得到 %v", err)
	}
}

// 查询端点必须与生成端点同源：这是整次重构的核心。
func TestQueryPathMatchesGenerate(t *testing.T) {
	cases := []struct {
		name     string
		generate string
		query    string
	}{
		{"generations", "/images/generations", "/tasks/u1"},
		{"edits_multipart", "/images/edits", "/tasks/u1"},
		{"video_generations", "/videos/generations", "/tasks/u1"},
		{"contents_tasks", "/contents/generations/tasks", "/contents/generations/tasks/u1"},
	}
	for _, c := range cases {
		spec, err := Get(c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if spec.Generate != c.generate {
			t.Errorf("%s 生成端点 = %q，期望 %q", c.name, spec.Generate, c.generate)
		}
		if got := spec.QueryPath("u1"); got != c.query {
			t.Errorf("%s 查询端点 = %q，期望 %q", c.name, got, c.query)
		}
	}
}

// 搜索没有任务概念，必须明确报告不支持，而不是给出一个会 404 的路径。
func TestSearchHasNoQuery(t *testing.T) {
	spec, err := Get("search_query")
	if err != nil {
		t.Fatal(err)
	}
	if spec.SupportsQuery() {
		t.Error("search_query 不该支持任务查询")
	}
}

// 任务键来自签名内容，仍须转义：含 / 或 ? 的键不能改变请求路径。
func TestQueryPathEscapes(t *testing.T) {
	spec, _ := Get("contents_tasks")
	got := spec.QueryPath("a/b?c=d")
	if strings.Contains(got, "a/b") || strings.Contains(got, "?") {
		t.Errorf("任务键未转义: %q", got)
	}
	if !strings.HasPrefix(got, "/contents/generations/tasks/") {
		t.Errorf("路径前缀被破坏: %q", got)
	}
}

func TestEncodeJSON(t *testing.T) {
	b, ct, err := EncodeJSON(context.Background(), map[string]any{"model": "m", "n": 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if string(b) != `{"model":"m","n":2}` {
		t.Errorf("编码结果 = %s", b)
	}
}

// 解析 multipart 请求体，返回字段表与文件 part 列表。
type parsedForm struct {
	fields map[string]string
	files  []struct{ name, filename, ctype string; size int }
}

func parseMultipart(t *testing.T, body []byte, contentType string) parsedForm {
	t.Helper()
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("解析 Content-Type 失败: %v", err)
	}
	if mt != "multipart/form-data" {
		t.Fatalf("Content-Type 不是 multipart: %s", mt)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	out := parsedForm{fields: map[string]string{}}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取 part 失败: %v", err)
		}
		if part.FileName() != "" {
			data, _ := io.ReadAll(part)
			out.files = append(out.files, struct{ name, filename, ctype string; size int }{
				part.FormName(), part.FileName(), part.Header.Get("Content-Type"), len(data),
			})
		} else {
			v, _ := io.ReadAll(part)
			out.fields[part.FormName()] = string(v)
		}
	}
	return out
}

func fakeFetcher(calls *[]string) Fetcher {
	return func(_ context.Context, rawURL string) ([]byte, string, error) {
		*calls = append(*calls, rawURL)
		return []byte("PNGDATA-" + rawURL), "image/png", nil
	}
}

func TestEncodeMultipartSingleImage(t *testing.T) {
	var calls []string
	payload := map[string]any{
		"model": "gpt-image-1", "prompt": "改背景", "response_format": "url",
		"n": 1, "size": "1024x1024",
		"image": []string{"https://cdn.example.com/a.png"},
	}
	body, ct, err := EncodeMultipart(context.Background(), payload, fakeFetcher(&calls))
	if err != nil {
		t.Fatal(err)
	}
	f := parseMultipart(t, body, ct)

	if len(f.files) != 1 {
		t.Fatalf("应有 1 个文件 part，得到 %d", len(f.files))
	}
	// 单图用 image，多图才用 image[]
	if f.files[0].name != "image" {
		t.Errorf("单图字段名 = %q，期望 image", f.files[0].name)
	}
	if f.files[0].ctype != "image/png" {
		t.Errorf("文件 Content-Type = %q", f.files[0].ctype)
	}
	if f.fields["model"] != "gpt-image-1" || f.fields["prompt"] != "改背景" {
		t.Errorf("字段缺失: %v", f.fields)
	}
	// image 不能以字符串形式出现在普通字段里
	if _, ok := f.fields["image"]; ok {
		t.Error("image 不该作为普通字段出现")
	}
	// 数字要转成字符串
	if f.fields["n"] != "1" || f.fields["size"] != "1024x1024" {
		t.Errorf("标量转换不对: n=%q size=%q", f.fields["n"], f.fields["size"])
	}
	if len(calls) != 1 || calls[0] != "https://cdn.example.com/a.png" {
		t.Errorf("下载调用不对: %v", calls)
	}
}

func TestEncodeMultipartMultipleImages(t *testing.T) {
	var calls []string
	payload := map[string]any{
		"model": "m", "prompt": "融合",
		"image": []string{"https://a/1.png", "https://b/2.jpg"},
	}
	body, ct, err := EncodeMultipart(context.Background(), payload, fakeFetcher(&calls))
	if err != nil {
		t.Fatal(err)
	}
	f := parseMultipart(t, body, ct)

	if len(f.files) != 2 {
		t.Fatalf("应有 2 个文件 part，得到 %d", len(f.files))
	}
	for _, file := range f.files {
		if file.name != "image[]" {
			t.Errorf("多图字段名 = %q，期望 image[]", file.name)
		}
	}
	if len(calls) != 2 {
		t.Errorf("应下载 2 次，实际 %d", len(calls))
	}
}

func TestEncodeMultipartRequiresImage(t *testing.T) {
	var calls []string
	if _, _, err := EncodeMultipart(context.Background(), map[string]any{"prompt": "x"}, fakeFetcher(&calls)); err == nil {
		t.Error("没有参考图时应报错")
	}
	if _, _, err := EncodeMultipart(context.Background(), map[string]any{
		"image": []string{"https://a/1.png"},
	}, nil); err == nil {
		t.Error("缺少下载器时应报错")
	}
}

// 下载失败必须指出是第几张，否则多图场景无法定位。
func TestEncodeMultipartFetchError(t *testing.T) {
	fail := func(_ context.Context, rawURL string) ([]byte, string, error) {
		return nil, "", errors.New("拒绝访问非公网地址 127.0.0.1")
	}
	_, _, err := EncodeMultipart(context.Background(), map[string]any{
		"image": []string{"https://a/1.png", "https://a/2.png"},
	}, fail)
	if err == nil {
		t.Fatal("下载失败应报错")
	}
	if !strings.Contains(err.Error(), "第 1 张") {
		t.Errorf("错误应指明第几张: %v", err)
	}
}

// 数组/对象无法用表单表达，必须跳过而不是写成 "[object]" 一类的垃圾。
func TestEncodeMultipartSkipsNonScalar(t *testing.T) {
	var calls []string
	body, ct, err := EncodeMultipart(context.Background(), map[string]any{
		"model": "m",
		"image": []string{"https://a/1.png"},
		"extra": map[string]any{"k": "v"},
		"tags":  []string{"x", "y"},
	}, fakeFetcher(&calls))
	if err != nil {
		t.Fatal(err)
	}
	f := parseMultipart(t, body, ct)
	if _, ok := f.fields["extra"]; ok {
		t.Error("对象不该被写成表单字段")
	}
	if _, ok := f.fields["tags"]; ok {
		t.Error("数组不该被写成表单字段")
	}
}

func TestFilenameFor(t *testing.T) {
	cases := []struct{ url, mime, want string }{
		{"https://a/b/c.png", "image/png", "c.png"},
		{"https://a/b/c", "image/jpeg", "c.jpg"},
		{"https://a/b/c", "image/webp", "c.webp"},
		{"https://a/", "image/png", "image.png"},
	}
	for _, c := range cases {
		if got := filenameFor(c.url, c.mime, 0); got != c.want {
			t.Errorf("filenameFor(%q,%q) = %q，期望 %q", c.url, c.mime, got, c.want)
		}
	}
}

// 火山把提示词与素材都塞进 content 数组，与 OpenAI 的结构完全不同。
// 这一层必须由协议族消化，否则调用方要自己拼数组。
func TestShapeArkContents(t *testing.T) {
	out, err := shapeArkContents(map[string]any{
		"model":  "doubao-seedance",
		"prompt": "海边日落",
		"image":  []string{"https://a/1.png", "https://a/2.png"},
		"duration":     float64(5),
		"aspect_ratio": "16:9", // 契约名；上游叫 ratio
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "doubao-seedance" {
		t.Errorf("model 丢失: %v", out["model"])
	}
	content, ok := out["content"].([]any)
	if !ok {
		t.Fatalf("content 类型不对: %T", out["content"])
	}
	if len(content) != 3 {
		t.Fatalf("content 应有 1 段文本 + 2 张图，实际 %d 项", len(content))
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "海边日落" {
		t.Errorf("首项应为文本: %v", first)
	}
	img := content[1].(map[string]any)
	if img["type"] != "image_url" || img["role"] != "reference_image" {
		t.Errorf("图片项结构不对: %v", img)
	}
	if got := img["image_url"].(map[string]any)["url"]; got != "https://a/1.png" {
		t.Errorf("图片地址不对: %v", got)
	}

	// 通用参数原样透传，宽高比要改名，prompt/image/response_format 不该再出现
	if out["duration"] != float64(5) {
		t.Errorf("duration 未透传: %v", out["duration"])
	}
	if out["ratio"] != "16:9" {
		t.Errorf("aspect_ratio 应改名为 ratio: %v", out["ratio"])
	}
	if _, ok := out["aspect_ratio"]; ok {
		t.Error("aspect_ratio 不该留在请求体里（上游不认这个名字）")
	}
	for _, k := range []string{"prompt", "image", "response_format"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s 不该出现在火山请求体里", k)
		}
	}
}

func TestShapeArkContentsVideoOnly(t *testing.T) {
	out, err := shapeArkContents(map[string]any{"model": "m", "prompt": "猫"})
	if err != nil {
		t.Fatal(err)
	}
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Errorf("文生视频只有 1 项文本，实际 %d", len(content))
	}
}

// size 在这里没有对应字段：静默丢弃会让调用方以为生效了。
func TestShapeArkRejectsSize(t *testing.T) {
	_, err := shapeArkContents(map[string]any{
		"model": "m", "prompt": "x", "size": "1024x1024",
	})
	if err == nil {
		t.Fatal("size 应被拒绝")
	}
	if !strings.Contains(err.Error(), "aspect_ratio") {
		t.Errorf("错误应给出替代方案: %v", err)
	}
}

func TestShapeArkRequiresPrompt(t *testing.T) {
	if _, err := shapeArkContents(map[string]any{"model": "m", "prompt": "   "}); err == nil {
		t.Error("空 prompt 应报错")
	}
}
