// Package taskid 生成与校验自包含的任务 ID。
//
// 为什么自包含：服务无状态，任何副本收到 task.get 都要能处理，
// 不能依赖「创建任务的那个实例」。所以把 model 与上游任务键编进 ID，
// 外面套一层 HMAC 防止伪造。
//
// 密钥由 api_key 派生：换 key 即作废旧任务 ID，这正是期望行为——
// key 换了，旧任务的调用资格本来就该一起失效。
package taskid

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Payload 是任务 ID 的内容。
type Payload struct {
	Model    string `json:"m"` // 模型 id，用于排障与展示
	Upstream string `json:"u"` // 上游任务键
	Kind     string `json:"k"` // image / video
	IssuedAt int64  `json:"t"` // Unix 秒，仅用于排障

	// Protocol 是生成该任务时用的协议族。查询端点必须与生成端点同源，
	// 所以它必须随任务一起传递，而不是查询时再猜或再查一次模型列表：
	//   - 查询时模型可能已下架，任务却还在跑
	//   - 多一次 /models 往返，且那一次也可能失败
	//
	// 省略字段的旧 ID 按 Default 处理（见 Parse）。
	Protocol string `json:"p,omitempty"`
}

// ErrMalformed 表示 ID 结构或签名不合法。
var ErrMalformed = errors.New("task_id 不合法")

func deriveKey(apiKey string) []byte {
	h := sha256.Sum256([]byte("mcp-for-apiduo/task-id/v1\x00" + apiKey))
	return h[:]
}

// Sign 编码并签名。
func Sign(apiKey string, p Payload) string {
	body, _ := json.Marshal(p)
	payload := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, deriveKey(apiKey))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

// Parse 校验签名并解出内容。
func Parse(apiKey, token string) (Payload, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Payload{}, ErrMalformed
	}
	mac := hmac.New(sha256.New, deriveKey(apiKey))
	mac.Write([]byte(parts[0]))
	want := mac.Sum(nil)

	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(got, want) {
		return Payload{}, ErrMalformed
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Payload{}, ErrMalformed
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, ErrMalformed
	}
	if p.Model == "" || p.Upstream == "" {
		return Payload{}, ErrMalformed
	}
	if p.Protocol == "" {
		// 兼容早期没有该字段的 ID：那时只有 generations 一种形状。
		p.Protocol = "generations"
	}
	return p, nil
}
