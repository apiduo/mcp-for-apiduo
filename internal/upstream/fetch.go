package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 下载外部资源的边界。三个都必须有，缺一个就会变成可利用面：
//   - 大小：不做限制等于允许把任意大的文件拉进内存
//   - 时长：不做限制等于允许一个慢响应占住连接直到进程退出
//   - 重定向次数：不做限制等于允许把请求跳到任意深处
const (
	maxFetchBytes = 20 << 20 // 20 MiB
	fetchTimeout  = 30 * time.Second
	maxRedirects  = 3
)

// fetcher 下载参考图。
//
// 这是本服务唯一会主动访问「调用方提供的第三方 URL」的路径，
// 所以它必须当作 SSRF 面来对待：能访问 127.0.0.1 或 10.0.0.0/8，
// 就相当于给调用方一个内网探测与打洞的接口，而日志里只会显示「一次生成」。
//
// 只允许 http/https + 公网地址；另外允许 data:image/* —— 那是 CLI 把本地小图
// 内联进请求用的，不发起任何网络请求，所以不经过拨号层校验，但仍然限 image/* 与 20 MiB。
// 不提供配置开关：这是安全默认值，一旦可关，最省事的部署一定会把它关掉。
// 内网图床走 generations 模式（由上游去取 URL）。
type fetcher struct {
	hc *http.Client
}

func newFetcher() *fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	tr := &http.Transport{
		// 在拨号层校验，而不是只在请求前 Parse 一次。
		// 只在前面校验的话，DNS rebinding 可以让第二次解析指向内网地址 ——
		// 校验的地址和真正连上的地址必须是同一个，所以这里直接按校验过的 IP 拨号。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("解析目标地址失败: %w", err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("解析域名 %s 失败: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("域名 %s 无解析结果", host)
			}
			var lastErr error
			for _, ip := range ips {
				if !IsPublicIP(ip.IP) {
					return nil, fmt.Errorf("拒绝访问非公网地址 %s（解析自 %s）", ip.IP, host)
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}

	return &fetcher{hc: &http.Client{
		Timeout:   fetchTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("重定向超过 %d 次", maxRedirects)
			}
			// 重定向目标同样过 DialContext 的公网校验，这里只需限制次数。
			return nil
		},
	}}
}

// Fetch 下载一个外部资源，返回内容与 Content-Type。
func (f *fetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	raw := strings.TrimSpace(rawURL)
	// CLI 传本地图片时用 data URI 内联：只解码，不联网，不走下面的公网校验。
	if len(raw) >= 5 && strings.EqualFold(raw[:5], "data:") {
		return decodeDataURI(raw[5:], maxFetchBytes)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("参考图地址不合法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("参考图必须是 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", errors.New("参考图地址缺少主机名")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("构造下载请求失败: %w", err)
	}
	req.Header.Set("Accept", "image/*,*/*")

	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("下载参考图失败: 上游返回 %d", resp.StatusCode)
	}
	if resp.ContentLength > maxFetchBytes {
		return nil, "", fmt.Errorf("参考图过大: %d 字节，上限 %d", resp.ContentLength, maxFetchBytes)
	}

	// 多读一个字节用来判断是否超限：Content-Length 可能缺失或撒谎。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取参考图失败: %w", err)
	}
	if len(data) > maxFetchBytes {
		return nil, "", fmt.Errorf("参考图超过上限 %d 字节", maxFetchBytes)
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return data, ct, nil
}

// decodeDataURI 解 data:image/...;base64,... 形态的参考图。
//
// 它不发起网络请求，所以不存在 SSRF 面；但它是「把调用方的字节塞进上游请求」
// 的入口，边界仍然要有：只收 image/*，解码前后都按 max 截断。
func decodeDataURI(body string, max int) ([]byte, string, error) {
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return nil, "", errors.New("data URI 缺少逗号分隔符")
	}
	meta, payload := body[:comma], body[comma+1:]
	base64Encoded := false
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		base64Encoded = true
		meta = meta[:len(meta)-len(";base64")]
	}
	ct := strings.TrimSpace(meta)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	// MIME 类型大小写不敏感，但上游实现常按字符串比较：统一成小写再往下走。
	ct = strings.ToLower(ct)
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("data URI 只支持 image/*，收到 %q", ct)
	}

	var data []byte
	var err error
	if base64Encoded {
		// 先按长度粗算，别为一个超大字符串先分配解码缓冲。
		if len(payload) > (max/3+1)*4 {
			return nil, "", fmt.Errorf("参考图超过上限 %d 字节", max)
		}
		data, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(payload)
		}
		if err != nil {
			return nil, "", fmt.Errorf("data URI base64 解码失败: %w", err)
		}
	} else {
		s, decErr := url.PathUnescape(payload)
		if decErr != nil {
			return nil, "", fmt.Errorf("data URI 转义解码失败: %w", decErr)
		}
		data = []byte(s)
	}
	if len(data) > max {
		return nil, "", fmt.Errorf("参考图超过上限 %d 字节", max)
	}
	if len(data) == 0 {
		return nil, "", errors.New("data URI 解出来是空内容")
	}
	return data, ct, nil
}

// IsPublicIP 判断地址是否可以对外访问。
//
// 排除：环回、私网（10/8、172.16/12、192.168/16、fc00::/7）、链路本地（含 169.254 云元数据）、
// 未指定地址、组播，以及 100.64/10（运营商级 NAT，云上常被用作内部网络）、
// 0.0.0.0/8、198.18/15（基准测试段）、240/4（保留）与广播地址。
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4.IsLoopback(), v4.IsPrivate(), v4.IsUnspecified(),
			v4.IsLinkLocalUnicast(), v4.IsLinkLocalMulticast(), v4.IsMulticast():
			return false
		case v4[0] == 0: // 0.0.0.0/8
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 及其他保留块
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
			return false
		case v4[0] >= 240: // 240.0.0.0/4 保留 + 255.255.255.255 广播
			return false
		}
		return true
	}
	// IPv6
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return false
	}
	return true
}
