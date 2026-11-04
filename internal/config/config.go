// Package config 负责两项部署配置的加载、校验与热更新。
//
// 设计约束（见 README §3）：
//   - 只有两项：base_url、api_key。
//   - 优先级：配置文件 > 环境变量；配置文件里的空值不覆盖已有值。
//   - 热更新：按 mtime 轮询，原子切换，失败保留旧值。
//   - 每次变更都校验 base_url —— 它决定带着用户的 key 往哪发请求。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是运行时配置。两项，别加第三项。
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

// fileDoc 是配置文件的顶层结构：
//
//	base_url: https://api.example.com/v1   # 或只写 https://api.example.com（自动补 /v1）
//	api_key: sk-xxx
type fileDoc struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	// 允许同一份文件里放管理端数据（models / settings / disabled / protocols），此处不解析，
	// 由 admin/catalog 各自读取，避免配置文件格式分裂成两份。
	Models    map[string]any `yaml:"models"`
	Settings  map[string]any `yaml:"settings"`
	Disabled  []string       `yaml:"disabled"`
	Protocols map[string]any `yaml:"protocols"`
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

// Store 持有当前配置，支持原子读取与热替换。
type Store struct {
	cur atomic.Pointer[Config]

	path     string        // 配置文件路径，空表示不启用热更新
	interval time.Duration // 轮询间隔
	lastMod  time.Time
	lastSize int64

	stop chan struct{}
	done chan struct{}

	// Err 记录最近一次重载失败的原因，成功时清空。供 /healthz 暴露。
	err atomic.Pointer[string]

	// onReload 在每次成功重载后调用。上层用它失效与配置相关的缓存：
	// base_url 变了，缓存的模型列表就属于旧上游，必须丢掉。
	onReload func()
}

// OnReload 注册重载回调。必须在 Start 之前调用。
func (s *Store) OnReload(fn func()) { s.onReload = fn }

// NewStore 加载初始配置。path 为空时只读环境变量，不启用热更新。
func NewStore(path string, interval time.Duration) (*Store, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	s := &Store{path: path, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}

	cfg := FromEnv()
	if path != "" {
		next, mod, size, err := loadFile(path, cfg)
		if err != nil {
			return nil, err
		}
		cfg = next
		s.lastMod, s.lastSize = mod, size
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s.cur.Store(&cfg)
	return s, nil
}

// Get 返回当前配置。读路径无锁。
func (s *Store) Get() Config {
	return *s.cur.Load()
}

// LastError 返回最近一次重载的错误（可能为 nil）。
func (s *Store) LastError() string {
	if p := s.err.Load(); p != nil {
		return *p
	}
	return ""
}

// Start 启动 mtime 轮询。未设置配置文件时立即返回，不产生 goroutine。
func (s *Store) Start() {
	if s.path == "" {
		close(s.done)
		return
	}
	go s.loop()
}

// Close 停止轮询。
func (s *Store) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
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
			s.reload()
		}
	}
}

// reload 只在 mtime/size 变化时真正解析，避免每秒读盘解析。
func (s *Store) reload() {
	st, err := os.Stat(s.path)
	if err != nil {
		s.setErr(fmt.Sprintf("stat 配置失败: %v", err))
		return
	}
	if st.ModTime().Equal(s.lastMod) && st.Size() == s.lastSize {
		return
	}

	next, _, _, err := loadFile(s.path, FromEnv())
	if err != nil {
		// 失败保留旧值：运维存了个半截文件，不该让整个服务不可用。
		s.setErr(err.Error())
		return
	}
	if err := next.Validate(); err != nil {
		s.setErr(err.Error())
		return
	}

	s.cur.Store(&next)
	s.lastMod, s.lastSize = st.ModTime(), st.Size()
	s.err.Store(nil)
	if s.onReload != nil {
		s.onReload()
	}
}

func (s *Store) setErr(msg string) {
	m := msg
	s.err.Store(&m)
}

func loadFile(path string, base Config) (Config, time.Time, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, time.Time{}, 0, fmt.Errorf("读配置失败: %w", err)
	}
	var doc fileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Config{}, time.Time{}, 0, fmt.Errorf("解析配置失败: %w", err)
	}
	st, _ := os.Stat(path)
	var mod time.Time
	var size int64
	if st != nil {
		mod, size = st.ModTime(), st.Size()
	}
	return MergeFile(base, doc), mod, size, nil
}

// LoadFileDoc 读取完整的配置文件（含 models / settings 段），供管理端使用。
func LoadFileDoc(path string) (fileDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileDoc{}, err
	}
	var doc fileDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fileDoc{}, err
	}
	return doc, nil
}

// SaveFileDoc 先写临时文件再 rename，避免读到半截内容。
func SaveFileDoc(path string, doc fileDoc) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
