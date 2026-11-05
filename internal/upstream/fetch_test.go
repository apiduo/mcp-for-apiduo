package upstream

import (
	"bytes"
	"context"
	"mime"
	"mime/multipart"
	"net"
	"strings"
	"testing"

	"github.com/apiduo/mcp-for-apiduo/internal/protocol"
)

// SSRF 是本服务唯一主动访问第三方 URL 的路径，边界必须逐个钉住。
func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		// 必须拒绝
		{"127.0.0.1", false, "环回"},
		{"127.1.2.3", false, "环回段"},
		{"10.0.0.1", false, "私网 10/8"},
		{"172.16.5.4", false, "私网 172.16/12"},
		{"192.168.1.1", false, "私网 192.168/16"},
		{"169.254.169.254", false, "云元数据服务"},
		{"100.64.0.1", false, "运营商级 NAT"},
		{"0.0.0.0", false, "未指定"},
		{"198.18.0.1", false, "基准测试段"},
		{"240.0.0.1", false, "保留段"},
		{"255.255.255.255", false, "广播"},
		{"::1", false, "IPv6 环回"},
		{"fc00::1", false, "IPv6 唯一本地"},
		{"fd12:3456::1", false, "IPv6 唯一本地"},
		{"fe80::1", false, "IPv6 链路本地"},
		{"::", false, "IPv6 未指定"},
		{"::ffff:127.0.0.1", false, "IPv4 映射的环回"},
		{"::ffff:10.0.0.1", false, "IPv4 映射的私网"},

		// 必须放行
		{"8.8.8.8", true, "公网 DNS"},
		{"1.1.1.1", true, "公网 DNS"},
		{"93.184.216.34", true, "普通公网"},
		{"2001:4860:4860::8888", true, "公网 IPv6"},
	}

	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("测试用例 IP 非法: %s", c.ip)
		}
		if got := IsPublicIP(ip); got != c.want {
			t.Errorf("IsPublicIP(%s) = %v，期望 %v（%s）", c.ip, got, c.want, c.why)
		}
	}

	if IsPublicIP(nil) {
		t.Error("nil 不该被当作公网")
	}
}

// 非 http/https 必须在发起任何连接之前就被拒绝。
func TestFetchRejectsBadScheme(t *testing.T) {
	f := newFetcher()
	for _, u := range []string{
		"file:///etc/passwd",
		"ftp://example.com/a.png",
		"gopher://example.com/",
		"https://", // 缺主机
	} {
		if _, _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("%q 应被拒绝", u)
		}
	}
}

// 内网地址必须在拨号层被拦住 —— 这是 rebinding 防线，不能只在 URL 层面判。
func TestFetchRejectsPrivateHost(t *testing.T) {
	f := newFetcher()
	_, _, err := f.Fetch(context.Background(), "http://127.0.0.1:9/a.png")
	if err == nil {
		t.Fatal("环回地址应被拒绝")
	}
	if !strings.Contains(err.Error(), "非公网") {
		t.Errorf("错误应说明是非公网地址，实际: %v", err)
	}

	// 直接写私网 IP 也一样
	if _, _, err := f.Fetch(context.Background(), "http://10.0.0.1/a.png"); err == nil {
		t.Error("私网地址应被拒绝")
	}
}

// CLI 会把本地小图内联成 data URI。它不联网，但仍然是「调用方字节进上游」的入口，
// 所以只收 image/*、并且解码前后都要有上限。
func TestDecodeDataURI(t *testing.T) {
	const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII="

	data, ct, err := decodeDataURI("image/png;base64,"+onePixelPNG, 1<<20)
	if err != nil {
		t.Fatalf("合法 data URI 被拒绝: %v", err)
	}
	if ct != "image/png" || len(data) < 8 || data[0] != 0x89 || data[1] != 'P' {
		t.Fatalf("解码结果不对: ct=%q len=%d", ct, len(data))
	}

	// 非 base64 的百分号转义（SVG 常用）
	data, ct, err = decodeDataURI("image/svg+xml,%3Csvg%2F%3E", 1<<20)
	if err != nil || string(data) != "<svg/>" || ct != "image/svg+xml" {
		t.Fatalf("百分号转义解码失败: %v ct=%q data=%q", err, ct, data)
	}

	if _, _, err := decodeDataURI("text/plain;base64,aGk=", 1<<20); err == nil {
		t.Error("非 image/* 必须拒绝")
	}
	if _, _, err := decodeDataURI("image/png;base64,aGVsbG8=", 4); err == nil {
		t.Error("超过上限必须拒绝")
	}
	if _, _, err := decodeDataURI("image/png;base64", 1<<20); err == nil {
		t.Error("缺少逗号必须拒绝")
	}
}

func TestFetchAcceptsDataImage(t *testing.T) {
	f := newFetcher()
	if _, _, err := f.Fetch(context.Background(), "data:text/plain;base64,aGk="); err == nil {
		t.Error("Fetch 不该接受非图片 data URI")
	}
	data, ct, err := f.Fetch(context.Background(), "data:image/png;base64,aGVsbG8=")
	if err != nil || ct != "image/png" || string(data) != "hello" {
		t.Fatalf("Fetch 应接受 data:image: err=%v ct=%q data=%q", err, ct, data)
	}
}

// 接线测试：CLI 把本地图内联成 data URI，走的正是这条链路
// Client.fetch(= newFetcher().Fetch) → protocol.EncodeMultipart → 上游收到的 part。
// 单独测 decodeDataURI 不够 —— 如果 multipart 把整个 URI 当文件名，上游收到的仍是垃圾。
func TestMultipartAcceptsInlinedRefs(t *testing.T) {
	const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII="
	fetch := newFetcher().Fetch

	body, ct, err := protocol.EncodeMultipart(context.Background(), map[string]any{
		"model": "gpt-image-1", "prompt": "把背景换成海边",
		"image": []string{"data:image/png;base64," + onePixelPNG},
	}, fetch)
	if err != nil {
		t.Fatalf("multipart 不接受内联参考图: %v", err)
	}

	mr := multipart.NewReader(bytes.NewReader(body), boundaryOf(t, ct))
	form, err := mr.ReadForm(4 << 20)
	if err != nil {
		t.Fatalf("解析 multipart 失败: %v", err)
	}
	files := form.File["image"]
	if len(files) != 1 {
		t.Fatalf("应有 1 个 image part，得到 %d", len(files))
	}
	f := files[0]
	if got := f.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q，期望沿用 data URI 里的 image/png", got)
	}
	// 文件名不能把整个 data URI 塞进去：上游会当扩展名判断内容。
	if strings.Contains(f.Filename, "base64") || strings.Contains(f.Filename, "data:") || len(f.Filename) > 64 {
		t.Errorf("文件名不该来自 data URI 本体，得到 %q", f.Filename)
	}
	if !strings.HasSuffix(f.Filename, ".png") {
		t.Errorf("文件名应带 .png 扩展名，得到 %q", f.Filename)
	}
	if form.Value["prompt"][0] != "把背景换成海边" {
		t.Errorf("普通字段丢了: %v", form.Value)
	}

	// 多图要走 image[]，且两张内联图都要在
	body, ct, err = protocol.EncodeMultipart(context.Background(), map[string]any{
		"model": "m", "prompt": "融合",
		"image": []string{"data:image/png;base64," + onePixelPNG, "data:image/jpeg;base64,aGVsbG8="},
	}, fetch)
	if err != nil {
		t.Fatalf("多张内联图失败: %v", err)
	}
	form, err = multipart.NewReader(bytes.NewReader(body), boundaryOf(t, ct)).ReadForm(4 << 20)
	if err != nil {
		t.Fatalf("解析多图 multipart 失败: %v", err)
	}
	if len(form.File["image[]"]) != 2 {
		t.Errorf("多图应落在 image[] 字段，得到 %v", keysOf(form.File))
	}

	// 非图片的 data URI 必须在编码阶段就被拒，不能带病发到上游
	if _, _, err := protocol.EncodeMultipart(context.Background(), map[string]any{
		"prompt": "x", "image": []string{"data:text/html;base64,PHNjcmlwdD4="},
	}, fetch); err == nil {
		t.Error("data:text/html 应被拒绝")
	}
}

func boundaryOf(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		t.Fatalf("拿不到 multipart boundary: %q err=%v", contentType, err)
	}
	return params["boundary"]
}

func keysOf(m map[string][]*multipart.FileHeader) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// "image/" 是带斜杠的前缀。放宽成 "image" 会放行 imageevil/* ——
// 那是一个能把任意 Content-Type 塞进上游请求的入口。
func TestDecodeDataURIContentType(t *testing.T) {
	if _, _, err := decodeDataURI("imageevil/png;base64,aGk=", 1<<20); err == nil {
		t.Error("imageevil/png 不是 image/*，应拒绝")
	}

	// MIME 大小写不敏感；归一化成小写，免得上游按字符串比较时判错
	data, ct, err := decodeDataURI("IMAGE/PNG;base64,aGk=", 1<<20)
	if err != nil {
		t.Fatalf("大小写不同的 image/* 应被接受: %v", err)
	}
	if ct != "image/png" {
		t.Errorf("Content-Type 应归一化成小写，得到 %q", ct)
	}
	if string(data) != "hi" {
		t.Errorf("解码结果 = %q", data)
	}

	// 空图没有意义：本地拒，比让上游回「invalid image」清楚
	if _, _, err := decodeDataURI("image/png;base64,", 1<<20); err == nil {
		t.Error("空的 data URI 应拒绝")
	}
}
