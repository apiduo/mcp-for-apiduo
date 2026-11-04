package taskid

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := Payload{
		Model: "seedream", Upstream: "task_abc", Kind: "image",
		Protocol: "contents_tasks", // 查询端点必须与生成端点同源，所以要随任务传递
		IssuedAt: 1762000000,
	}
	tok := Sign("sk-test", p)

	got, err := Parse("sk-test", tok)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != p {
		t.Errorf("往返不一致: %+v != %+v", got, p)
	}
}

// 早期 ID 里没有 protocol 字段（那时只有一种形状），必须回落到默认而不是报错 ——
// 否则升级那一刻，所有正在跑的任务都会查不出来。
func TestLegacyTokenFallsBackToDefault(t *testing.T) {
	tok := Sign("sk-test", Payload{Model: "m", Upstream: "u", Kind: "image"})
	got, err := Parse("sk-test", tok)
	if err != nil {
		t.Fatalf("旧 ID 不该解析失败: %v", err)
	}
	if got.Protocol != "generations" {
		t.Errorf("旧 ID 的 protocol 应回落为 generations，得到 %q", got.Protocol)
	}
}

// 换 key 必须让旧任务 ID 失效 —— key 换了，调用资格本来就该一起失效。
func TestWrongKeyRejected(t *testing.T) {
	tok := Sign("sk-a", Payload{Model: "m", Upstream: "u", Kind: "image"})
	if _, err := Parse("sk-b", tok); err == nil {
		t.Error("换 key 后仍能解析，说明签名没起作用")
	}
}

// 篡改 payload 必须被签名挡住，防止把别人任务的 model 改成自己的。
func TestTamperedPayloadRejected(t *testing.T) {
	tok := Sign("sk-test", Payload{Model: "m1", Upstream: "u1", Kind: "image"})
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		t.Fatalf("格式异常: %q", tok)
	}

	// 换掉 payload，保留原签名
	forged := Sign("sk-other", Payload{Model: "m2", Upstream: "u2", Kind: "image"}) // 用别的 key 签
	forged = strings.Split(forged, ".")[0] + "." + parts[1]
	if _, err := Parse("sk-test", forged); err == nil {
		t.Error("伪造的 payload 通过了校验")
	}
}

func TestMalformed(t *testing.T) {
	bad := []string{"", ".", "abc", "a.b.c", "!!!.???", "onlypayload"}
	for _, s := range bad {
		if _, err := Parse("sk-test", s); err == nil {
			t.Errorf("非法输入 %q 未被拒绝", s)
		}
	}
}

// 空 model/upstream 不能签出可用的 ID。
func TestEmptyFieldsRejected(t *testing.T) {
	tok := Sign("sk-test", Payload{Model: "", Upstream: "u", Kind: "image"})
	if _, err := Parse("sk-test", tok); err == nil {
		t.Error("空 model 的 ID 不应可用")
	}
}
