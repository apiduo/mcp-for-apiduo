package admin

import (
	"sync"
	"time"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// 上游模型列表的短期缓存。
//
// 缓存的是什么，决定了它能有多旧：
//   - 缓存的是上游 /v1/models 的原始事实（已归一化），不是合并结果。
//     覆盖层每次现算，所以「刚配完却看不到」这种最烦人的问题不存在。
//   - 上游模型列表的变更频率是「渠道上下线」，小时级；30 秒的旧值不改变任何判断。
//   - 只有读路径走缓存。写路径（validateOverlays）直连上游：校验必须基于当下事实，
//     而写是人工频率，多一次往返无所谓。这是刻意的分层，别把缓存接进校验。
//
// 缓存不做过期上报：调用方拿到 Age 自己判断。有效期内的旧值不是「缓存冒充事实」，
// 它确实是事实，只是 30 秒前的事实 —— 响应里的 upstream_age_ms 把这件事说清楚。

// modelSnapshot 是一次读到的上游视图。
type modelSnapshot struct {
	Models []catalog.Model
	// Age 是距真正向上游拉取的时长。0 表示本次刚拉。
	Age time.Duration
}

type modelCache struct {
	ttl time.Duration

	mu     sync.Mutex
	key    string // base_url + api_key；配置热更新后自然 miss
	models []catalog.Model
	at     time.Time
}

func newModelCache(ttl time.Duration) *modelCache {
	if ttl == 0 {
		ttl = -1 // 负数 = 关闭：任何 age 都大于它，永远 miss
	}
	return &modelCache{ttl: ttl}
}

// get 命中时返回快照副本与年龄。
//
// 返回切片是拷贝，避免调用方替换元素影响缓存；元素（map）按只读约定共享，
// 与 Store.Overlays() 的约定一致。
func (c *modelCache) get(key string) (modelSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.models == nil || c.key != key {
		return modelSnapshot{}, false
	}
	age := time.Since(c.at)
	if c.ttl < 0 || age > c.ttl {
		return modelSnapshot{}, false
	}
	out := make([]catalog.Model, len(c.models))
	copy(out, c.models)
	return modelSnapshot{Models: out, Age: age}, true
}

func (c *modelCache) put(key string, models []catalog.Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]catalog.Model, len(models))
	copy(out, models)
	c.key, c.models, c.at = key, out, time.Now()
}

// invalidate 丢弃缓存。配置变更时不需要调用（key 里含 base_url，变了就 miss），
// 但测试和「强制刷新」场景需要。
func (c *modelCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.models, c.at = "", nil, time.Time{}
}

// ModelCacheTTL 返回当前缓存有效期，供 /admin/manifest 自报。
func (c *modelCache) ModelCacheTTL() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttl
}
