// Package config 是配置文件唯一的读者与写者。
//
// 设计约束（见 README §3）：
//   - 部署配置只有两项：base_url、api_key；同一份文件里还有管理端覆盖层。
//   - 两者必须由同一个 Store 持有：早先 config 读部署项、admin 自己存一份覆盖层快照，
//     手改文件后覆盖层不生效（README §11「已知问题」），根因是同一份文件有两个读者。
//   - 优先级：配置文件 > 环境变量；配置文件里的空值不覆盖已有值。
//   - 热更新：按 mtime 轮询，原子切换，失败保留旧值。
//   - 每次变更都校验 base_url —— 它决定带着用户的 key 往哪发请求。
//   - 写回不重排、不删注释：这份文件是人读的，也是 git diff 的对象。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// Config 是运行时部署配置。两项，别加第三项。
type Config struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// Redacted 返回可安全写日志的副本。
func (c Config) Redacted() Config {
	out := c
	if out.APIKey != "" {
		out.APIKey = mask(out.APIKey)
	}
	return out
}

func mask(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-2:]
}

// Validate 校验 base_url 并规范化（去尾部斜杠）。
//
// 这是 SSRF 的第一道闸：base_url 合法，请求才会带上 key 发出去。
func (c *Config) Validate() error {
	raw := strings.TrimSpace(c.BaseURL)
	if raw == "" {
		return errors.New("base_url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url 协议必须是 http 或 https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("base_url 缺少主机名")
	}

	// 两种写法都接受：
	//   https://xxx.com/v1        → 原样使用（路径已含版本段）
	//   https://xxx.com           → 补 /v1（OpenAI 兼容的默认约定）
	//   https://ark.../api/v3     → 原样使用（非 /v1 前缀必须显式写出）
	//
	// 判据只看「路径是否为空」：有路径就相信调用方，不猜也不改写 ——
	// 猜错前缀会让请求静默打到错误端点，比报错难查得多。
	base := strings.TrimRight(raw, "/")
	if u.Path == "" || u.Path == "/" {
		base += "/v1"
	}
	c.BaseURL = base
	return nil
}

// FromEnv 读取环境变量。缺省值按 README：base_url 默认指向本机 new-api。
func FromEnv() Config {
	c := Config{
		BaseURL: os.Getenv("BASE_URL"),
		APIKey:  os.Getenv("API_KEY"),
	}
	if c.BaseURL == "" {
		// 必须含版本前缀：new-api 是 /v1，火山 Ark 是 /api/v3。
		c.BaseURL = "http://127.0.0.1:3000/v1"
	}
	return c
}

// Doc 是一次发布出去的全部事实：部署配置 + 管理端覆盖层 + 版本号。
//
// 三者同时切换（atomic.Pointer 指向不可变快照），所以运行中不会出现
// 「新 base_url 配旧覆盖层」这种半更新状态。
type Doc struct {
	Config   Config
	Overlays catalog.Overlays
	Revision string // 覆盖层内容哈希，对外作为 ETag
}

// fileDoc 是配置文件的结构：
//
//	base_url: https://api.example.com/v1   # 或只写 https://api.example.com（自动补 /v1）
//	api_key: sk-xxx
//	models: {...}
//	settings: {defaults: {...}}
//	disabled: [...]
//	protocols: {...}
type fileDoc struct {
	BaseURL   string                    `yaml:"base_url"`
	APIKey    string                    `yaml:"api_key"`
	Models    map[string]map[string]any `yaml:"models"`
	Settings  catalog.Settings          `yaml:"settings"`
	Disabled  []string                  `yaml:"disabled"`
	Protocols map[string]map[string]any `yaml:"protocols"`
}

// MergeFile 把文件内容叠加到 base 上：非空值覆盖，空值保持。
//
// 反过来（文件里的空值覆盖 env）会造成最难查的故障：漏写一个字段，
// 把本来有效的地址清成空串。
func MergeFile(base Config, doc fileDoc) Config {
	out := base
	if v := strings.TrimSpace(doc.BaseURL); v != "" {
		out.BaseURL = v
	}
	if v := strings.TrimSpace(doc.APIKey); v != "" {
		out.APIKey = v
	}
	return out
}

// Overlays 从文件内容里取覆盖层。缺失的段按空处理，不报错。
func (d fileDoc) Overlays() catalog.Overlays {
	return catalog.Overlays{
		Models:    d.Models,
		Settings:  d.Settings,
		Disabled:  d.Disabled,
		Protocols: d.Protocols,
	}.Canonical()
}

// Revision 计算覆盖层的内容版本。
//
// 用内容哈希而不是自增计数：多副本挂同一份文件时，各副本独立算出同一个版本号，
// 不需要额外的协调；重启也不会让版本号跳跃。输入必须是规范形式，否则
// 同一个集合换个顺序就会得到不同的版本，客户端会以为配置被改过。
func Revision(ov catalog.Overlays) string {
	b, err := json.Marshal(ov.Canonical())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Store 持有当前文档，支持原子读取与热替换。
type Store struct {
	cur atomic.Pointer[Doc]

	path     string        // 配置文件路径，空表示纯内存（不落盘、不轮询）
	interval time.Duration // 轮询间隔

	// mu 串行化「轮询重载」与「管理端写入」。
	// 两者都读写 lastMod/lastSize 与磁盘，不串行化就会出现
	// 「刚写完的文件被另一个 goroutine 当成外部变更再解析一遍」。
	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64

	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once

	// Err 记录最近一次重载失败的原因，成功时清空。供 /healthz 暴露。
	err atomic.Pointer[string]

	// listeners 在每次文档切换后调用（热更新与管理端保存都算）。
	// 上层用它失效与配置相关的缓存：base_url 变了，缓存的模型列表就属于旧上游。
	listeners []func()
}

// OnReload 注册重载回调。必须在 Start 之前调用。
func (s *Store) OnReload(fn func()) {
	if fn != nil {
		s.listeners = append(s.listeners, fn)
	}
}

// NewStore 加载初始文档。path 为空时只读环境变量，不启用热更新。
func NewStore(path string, interval time.Duration) (*Store, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	s := &Store{path: path, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}

	cfg := FromEnv()
	ov := catalog.Overlays{}.Canonical()
	if path != "" {
		doc, mod, size, err := loadFile(path)
		if err != nil {
			return nil, err
		}
		cfg = MergeFile(cfg, doc)
		ov = doc.Overlays()
		s.lastMod, s.lastSize = mod, size
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s.publish(cfg, ov)
	return s, nil
}

func (s *Store) publish(cfg Config, ov catalog.Overlays) {
	ov = ov.Canonical()
	s.cur.Store(&Doc{Config: cfg, Overlays: ov, Revision: Revision(ov)})
}

// Get 返回当前部署配置。读路径无锁。
func (s *Store) Get() Config {
	return s.cur.Load().Config
}

// Overlays 返回当前覆盖层快照。
//
// 返回值与内部快照共享 map，调用方只读。需要改就 Clone 之后再改 ——
// 就地改会绕过校验与落盘，直接改到运行时。
func (s *Store) Overlays() catalog.Overlays {
	return s.cur.Load().Overlays
}

// Revision 返回当前覆盖层版本（内容哈希）。
func (s *Store) Revision() string {
	return s.cur.Load().Revision
}

// Path 返回配置文件路径，空表示纯内存模式。
func (s *Store) Path() string { return s.path }

// LastError 返回最近一次重载的错误（可能为 nil）。
func (s *Store) LastError() string {
	if p := s.err.Load(); p != nil {
		return *p
	}
	return ""
}

// Start 启动 mtime 轮询。未设置配置文件时立即返回，不产生 goroutine。
func (s *Store) Start() {
	s.startOnce.Do(func() {
		if s.path == "" {
			close(s.done)
			return
		}
		go s.loop()
	})
}

// Close 停止轮询。
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *Store) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Reload()
		}
	}
}

// Reload 立即检查一次文件是否变化，变了就原子切换。
//
// 轮询每 interval 调一次；测试与运维排障可以直接调，不必等下一个 tick。
func (s *Store) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reloadLocked() {
		s.notify()
	}
}

// reloadLocked 只在 mtime/size 变化时真正解析，避免每秒读盘解析。
// 返回是否发布了新文档；不触发回调（由调用方决定何时通知）。
func (s *Store) reloadLocked() bool {
	if s.path == "" {
		return false
	}
	st, err := os.Stat(s.path)
	if err != nil {
		s.setErr(fmt.Sprintf("stat 配置失败: %v", err))
		return false
	}
	if st.ModTime().Equal(s.lastMod) && st.Size() == s.lastSize {
		return false
	}

	doc, _, _, err := loadFile(s.path)
	if err != nil {
		// 失败保留旧值：运维存了个半截文件，不该让整个服务不可用。
		s.setErr(err.Error())
		return false
	}
	cfg := MergeFile(FromEnv(), doc)
	if err := cfg.Validate(); err != nil {
		s.setErr(err.Error())
		return false
	}

	s.publish(cfg, doc.Overlays())
	s.lastMod, s.lastSize = st.ModTime(), st.Size()
	s.err.Store(nil)
	return true
}

// ErrRevisionChanged 表示从读到写之间，覆盖层被其他进程改过了。
var ErrRevisionChanged = errors.New("覆盖层已被其他进程修改")

// ReplaceOverlays 以 next 替换覆盖层：落盘、发布、通知。
func (s *Store) ReplaceOverlays(next catalog.Overlays) (string, error) {
	return s.ReplaceOverlaysIf(next, "")
}

// ReplaceOverlaysIf 是带比较的写入：expect 非空且与当前版本不符时拒绝写。
//
// 这是真正的 compare-and-swap，而不是「先读后写」：调用方算新状态用的是一个版本，
// 写盘前再确认那个版本还在。少了这一步，两个副本并发写就是后写的静默覆盖先写的
// —— 而覆盖的是别人的配置，且没有任何痕迹。
//
// 顺序是先落盘再发布：写失败时内存保持旧值，不会出现「面板显示已保存、
// 重启后丢失」这种两处不一致。
//
// 写之前先吸收磁盘上的外部改动（例如运维刚手改了 base_url），
// 避免把别人的部署配置覆盖回去。
func (s *Store) ReplaceOverlaysIf(next catalog.Overlays, expect string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next = next.Canonical()
	s.reloadLocked()
	if expect != "" && s.Revision() != expect {
		return s.Revision(), ErrRevisionChanged
	}
	cfg := s.cur.Load().Config

	if s.path != "" {
		if err := writeOverlays(s.path, next); err != nil {
			s.setErr(err.Error())
			return s.Revision(), err
		}
		if st, err := os.Stat(s.path); err == nil {
			s.lastMod, s.lastSize = st.ModTime(), st.Size()
		}
	}

	s.publish(cfg, next)
	s.err.Store(nil)
	s.notify()
	return s.Revision(), nil
}

func (s *Store) notify() {
	for _, fn := range s.listeners {
		fn()
	}
}

func (s *Store) setErr(msg string) {
	m := msg
	s.err.Store(&m)
}

func loadFile(path string) (fileDoc, time.Time, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileDoc{}, time.Time{}, 0, fmt.Errorf("读配置失败: %w", err)
	}
	var doc fileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fileDoc{}, time.Time{}, 0, fmt.Errorf("解析配置失败: %w", err)
	}
	st, _ := os.Stat(path)
	var mod time.Time
	var size int64
	if st != nil {
		mod, size = st.ModTime(), st.Size()
	}
	return doc, mod, size, nil
}

// sectionNames 是覆盖层在文件里的四个顶层键，写回时按这个顺序补新键。
var sectionNames = []string{"models", "settings", "disabled", "protocols"}

// writeOverlays 把覆盖层写回配置文件，保留其余键、键序与注释。
//
// 为什么不用「读成 map 再整体 Marshal」：那会把 base_url 下面的注释、
// 键的排列顺序全部抹掉。这份文件是人维护的、要进 git diff 的，
// 一次面板保存就把它洗成机器格式，是不可接受的副作用。
//
// 做法是直接编辑 YAML 语法树：只替换这四个键的值节点，其余节点原样保留。
func writeOverlays(path string, ov catalog.Overlays) error {
	var root yaml.Node

	data, err := os.ReadFile(path)
	switch {
	case err == nil && len(strings.TrimSpace(string(data))) > 0:
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("解析现有配置失败（拒绝覆盖）: %w", err)
		}
	case err == nil:
		// 空文件：从空映射开始。
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	case os.IsNotExist(err):
		// 启动时文件存在、写入前不见了。拒绝凭空重建：新文件里不会有
		// base_url / api_key，服务重启后会悄悄回落到默认上游 —— 这比报错危险得多。
		return errors.New("配置文件已不存在，拒绝用覆盖层重建（重建会丢失 base_url / api_key）")
	default:
		return fmt.Errorf("读配置失败: %w", err)
	}

	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return errors.New("配置文件顶层必须是键值映射，拒绝改写")
	}
	mapping := root.Content[0]

	sections := map[string]any{
		"models":    ov.Models,
		"settings":  ov.Settings,
		"disabled":  ov.Disabled,
		"protocols": ov.Protocols,
	}
	for _, name := range sectionNames {
		val := sections[name]
		empty := isEmptySection(name, ov)
		idx := mappingIndex(mapping, name)
		if empty {
			// 空段直接删键：留下 `disabled: []` 只是噪音，语义与缺失相同。
			if idx >= 0 {
				mapping.Content = append(mapping.Content[:idx-1], mapping.Content[idx+1:]...)
			}
			continue
		}
		node, err := valueNode(val)
		if err != nil {
			return fmt.Errorf("序列化 %s 失败: %w", name, err)
		}
		if idx >= 0 {
			// 保留原值节点上的注释（形如 `models:  # 覆盖层`），只换内容。
			old := mapping.Content[idx]
			node.HeadComment, node.LineComment, node.FootComment = old.HeadComment, old.LineComment, old.FootComment
			mapping.Content[idx] = node
			continue
		}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, node)
	}

	return encodeAtomic(path, &root)
}

func isEmptySection(name string, ov catalog.Overlays) bool {
	switch name {
	case "models":
		return len(ov.Models) == 0
	case "settings":
		return len(ov.Settings.Defaults) == 0
	case "disabled":
		return len(ov.Disabled) == 0
	case "protocols":
		return len(ov.Protocols) == 0
	}
	return true
}

// mappingIndex 返回键对应「值节点」的下标；mapping 是 key/value 交替排列的。
func mappingIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

func valueNode(v any) (*yaml.Node, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("空文档")
	}
	return doc.Content[0], nil
}

// encodeAtomic 先写临时文件再 rename，避免读到半截内容。
func encodeAtomic(path string, doc *yaml.Node) error {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	tmp := path + ".tmp." + fmt.Sprint(time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
