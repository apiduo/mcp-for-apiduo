package config

import "testing"

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		// 带版本前缀：原样保留
		{"https://api.example.com/v1", "https://api.example.com/v1", false},
		{"https://api.example.com/v1/", "https://api.example.com/v1", false},
		{"https://ark.cn-beijing.volces.com/api/v3", "https://ark.cn-beijing.volces.com/api/v3", false},
		// 只有域名：补 /v1
		{"https://api.example.com", "https://api.example.com/v1", false},
		{"https://api.example.com/", "https://api.example.com/v1", false},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000/v1", false},
		// 非版本路径：不猜，原样相信
		{"https://api.example.com/openai", "https://api.example.com/openai", false},
		// 非法
		{"ftp://api.example.com", "", true},
		{"api.example.com", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		cfg := Config{BaseURL: c.in}
		err := cfg.Validate()
		if c.bad {
			if err == nil {
				t.Errorf("%q 应该报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不该报错: %v", c.in, err)
			continue
		}
		if cfg.BaseURL != c.want {
			t.Errorf("%q → %q，期望 %q", c.in, cfg.BaseURL, c.want)
		}
	}
}
