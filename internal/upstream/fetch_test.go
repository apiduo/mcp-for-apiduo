package upstream

import (
	"context"
	"net"
	"strings"
	"testing"
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
