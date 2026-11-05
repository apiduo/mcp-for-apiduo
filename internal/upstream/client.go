// Package upstream 是 new-api 的 HTTP 客户端。
//
// 约束（README §6）：
//   - 只调公开 HTTP 接口，不改 new-api 代码、不读它的库。
//   - 不落用户的 key：日志脱敏，不写磁盘，不放进错误信息。
//   - 401/403/429 原样交给调用方判断，不在本地重试。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
	"github.com/apiduo/mcp-for-apiduo/internal/config"
	"github.com/apiduo/mcp-for-apiduo/internal/protocol"
)

// Client 是上游客户端。配置随 config.Store 热更新，每个请求读取当前值。
type Client struct {
	store *config.Store
	hc    *http.Client
	// fetch 下载参考图，只被 multipart 协议族使用（见 fetch.go 的 SSRF 边界）。
	fetch protocol.Fetcher
}

// New 构造客户端。
//
// 超时取 5 分钟：图片/视频生成是慢操作，但也不能无限等。
// 需要更长的场景走异步任务，不要调大这个值。
func New(store *config.Store) *Client {
	return &Client{
		store: store,
		fetch: newFetcher().Fetch,
		hc: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Error 是上游返回的错误，保留状态码、业务码与实际请求位置。
type Error struct {
	Status int    // HTTP 状态码
	Code   string // 上游业务错误码（若有）
	Msg    string // 已脱敏的消息
	Method string // 实际发出请求的方法
	Path   string // 实际发出请求的路径（相对 base_url，不含 key）

	// 记录实际路径不是为了好看：不同协议族的端点不同，
	// 拿错端点时上游常返回**空 body 的 404**，没有任何线索。
	// 带上路径才能一眼看出「请求打错了地方」还是「参数不对」。
}

func (e *Error) Error() string {
	where := ""
	if e.Method != "" || e.Path != "" {
		where = " (" + strings.TrimSpace(e.Method+" "+e.Path) + ")"
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream %d %s%s: %s", e.Status, e.Code, where, e.Msg)
	}
	return fmt.Sprintf("upstream %d%s: %s", e.Status, where, e.Msg)
}

// IsAuth 判断是否鉴权失败（key 无效或过期）。
func (e *Error) IsAuth() bool { return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden }

// IsRateLimited 判断是否被限流。调用方应原样把 429 交还客户端，不自行排队。
func (e *Error) IsRateLimited() bool { return e.Status == http.StatusTooManyRequests }

// doRaw 发起请求。apiKey 为空时拒绝发请求 —— 没有凭证就不该出网。
//
// body 已是编码后的字节：不同的协议族有不同编码（JSON / multipart），
// 编码工作交给 protocol 包，这一层只负责传输、鉴权与错误映射。
func (c *Client) doRaw(ctx context.Context, apiKey, method, path string, body []byte, contentType string) ([]byte, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, &Error{Status: http.StatusUnauthorized, Msg: "缺少 api_key：请在请求头 Authorization 或配置里提供"}
	}
	cfg := c.store.Get()
	target := cfg.BaseURL + path

	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if len(body) > 0 && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", redactErr(err, apiKey))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}
	upErr := parseError(resp.StatusCode, data)
	// 错误体是外部输入：4xx 常把请求头里的 key 原样回显
	// （"Incorrect API key provided: sk-xxx"）。它要进日志、进 MCP 响应、进终端，
	// 所以必须和网络错误一样过一遍脱敏 —— 只覆盖一半等于没覆盖。
	upErr.Msg = redact(upErr.Msg, apiKey)
	upErr.Code = redact(upErr.Code, apiKey)
	upErr.Method, upErr.Path = method, path
	return nil, upErr
}

// doJSON 是 doRaw 的 JSON 便捷封装。
func (c *Client) doJSON(ctx context.Context, apiKey, method, path string, body any) ([]byte, error) {
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码请求失败: %w", err)
		}
		raw = b
	}
	return c.doRaw(ctx, apiKey, method, path, raw, "application/json")
}

// parseError 把上游错误体转成 Error。兼容 OpenAI 风格 {error:{message,code}} 与扁平结构。
func parseError(status int, data []byte) *Error {
	e := &Error{Status: status}
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Code    any    `json:"code"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(data, &probe); err == nil {
		switch {
		case probe.Error != nil && probe.Error.Message != "":
			e.Msg = probe.Error.Message
			e.Code = fmt.Sprint(probe.Error.Code)
		case probe.Message != "":
			e.Msg = probe.Message
			e.Code = fmt.Sprint(probe.Code)
		case probe.Msg != "":
			e.Msg = probe.Msg
		}
	}
	if e.Msg == "" {
		e.Msg = strings.TrimSpace(string(truncate(data, 300)))
	}
	if e.Code == "<nil>" {
		e.Code = ""
	}
	return e
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}

// redact 抹掉文本里出现的 key。空 key 原样返回 —— 没有可比对的秘密时，
// 把 "***" 塞进去只会让错误信息更难读。
func redact(s, apiKey string) string {
	if apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, apiKey, "***")
}

// redactErr 从网络错误里抹掉可能出现的 key（URL/Header 回显）。
func redactErr(err error, apiKey string) error {
	if err == nil || apiKey == "" {
		return err
	}
	return fmt.Errorf("%s", redact(err.Error(), apiKey))
}

// ListModels 拉取全部模型。返回解析后的对象与上游原始字节（便于排障）。
func (c *Client) ListModels(ctx context.Context, apiKey string) ([]catalog.Model, error) {
	data, err := c.doJSON(ctx, apiKey, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []catalog.Model `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // 保留整数精度，created 之类不能被转成 float
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析 /v1/models 失败: %w", err)
	}
	return doc.Data, nil
}

// Generate 按模型声明的协议族发起一次生成。
//
// 端点、编码方式都来自协议族表（见 protocol 包），这里不写任何 if transport == ...
// —— 一旦把形状判断写进调用链，每加一种上游形状就要改一次这里的代码。
func (c *Client) Generate(ctx context.Context, apiKey string, m catalog.Model, payload map[string]any) (map[string]any, error) {
	spec, err := protocol.Get(m.Transport())
	if err != nil {
		return nil, fmt.Errorf("模型 %s: %w", m.ID(), err)
	}
	if spec.Shape != nil {
		payload, err = spec.Shape(payload)
		if err != nil {
			return nil, fmt.Errorf("模型 %s: %w", m.ID(), err)
		}
	}
	body, ct, err := spec.Encode(ctx, payload, c.fetch)
	if err != nil {
		return nil, err
	}
	data, err := c.doRaw(ctx, apiKey, http.MethodPost, spec.Generate, body, ct)
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

// Search 调用搜索工具端点。
func (c *Client) Search(ctx context.Context, apiKey string, payload map[string]any) (map[string]any, error) {
	data, err := c.doJSON(ctx, apiKey, http.MethodPost, "/tools/search", payload)
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

// GetTask 按协议族查询异步任务。
//
// 查询端点必须与生成端点同源：火山的任务键只认 /contents/generations/tasks/{id}，
// new-api 的只认 /tasks/{id}。拿错端点得到的是**空 body 的 404**，
// 既没有错误码也没有消息，是最难排查的一类失败。
func (c *Client) GetTask(ctx context.Context, apiKey, protocolName, taskKey string) (map[string]any, error) {
	spec, err := protocol.Get(protocolName)
	if err != nil {
		return nil, err
	}
	if !spec.SupportsQuery() {
		return nil, fmt.Errorf("协议 %s 不支持任务查询", spec.Name)
	}
	if strings.TrimSpace(taskKey) == "" {
		return nil, errors.New("缺少上游任务键")
	}
	data, err := c.doRaw(ctx, apiKey, http.MethodGet, spec.QueryPath(taskKey), nil, "")
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

func decodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("解析上游响应失败: %w", err)
	}
	return out, nil
}
