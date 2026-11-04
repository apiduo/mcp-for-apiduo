package admin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// auditEntry 是一条变更记录。
//
// 记「谁在什么时候用哪些操作把版本从 A 推到 B」，不记补丁内容：
// 变更的内容看 diff / git，日志只回答「是不是有人（或某个 agent）动了配置」。
// 把整份 patch 写进日志等于复制一份配置，还会顺手把密钥类字段带进文件。
type auditEntry struct {
	Time           string      `json:"time"`
	Actor          string      `json:"actor,omitempty"`
	Source         string      `json:"source,omitempty"`
	Remote         string      `json:"remote,omitempty"`
	DryRun         bool        `json:"dry_run"`
	Outcome        string      `json:"outcome"` // saved / dry_run / rejected / failed
	Ops            []opSummary `json:"ops"`
	RevisionBefore string      `json:"revision_before"`
	RevisionAfter  string      `json:"revision_after"`
	Issues         int         `json:"issues,omitempty"`
	Warnings       int         `json:"warnings,omitempty"`
}

type opSummary struct {
	Op     string `json:"op"`
	Target string `json:"target,omitempty"`
}

// auditLog 是追加式变更记录：落盘为 <config>.audit.jsonl，内存里留一份最近记录。
//
// 为什么要有它：管理 API 一旦对 agent 开放，配置就可能在无人值守时被改。
// 没有这条时间线，出了事只能靠猜；有了它，一句 `GET /admin/history` 就能定位。
type auditLog struct {
	mu   sync.Mutex
	path string // 空表示纯内存
	ring []auditEntry
}

const auditRingSize = 200

func newAuditLog(configPath string) *auditLog {
	a := &auditLog{ring: make([]auditEntry, 0, auditRingSize)}
	if configPath != "" {
		a.path = configPath + ".audit.jsonl"
	}
	return a
}

func (a *auditLog) append(e auditEntry) error {
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ring = append(a.ring, e)
	if len(a.ring) > auditRingSize {
		a.ring = a.ring[len(a.ring)-auditRingSize:]
	}
	if a.path == "" {
		return nil
	}

	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// History 返回最近 limit 条记录（时间正序）。
//
// 优先读文件：它跨重启保留，也是多副本共享那份配置时的共同账本。
// 文件不可读时回落到内存环，不因审计文件损坏而让管理端整体不可用。
func (a *auditLog) History(limit int) []auditEntry {
	if limit <= 0 || limit > auditRingSize {
		limit = 50
	}
	if a.path != "" {
		if entries, err := readAuditFile(a.path, limit); err == nil {
			return entries
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	start := 0
	if len(a.ring) > limit {
		start = len(a.ring) - limit
	}
	out := make([]auditEntry, len(a.ring[start:]))
	copy(out, a.ring[start:])
	return out
}

func readAuditFile(path string, limit int) ([]auditEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 配置文件变更频率是「人工改配置」级别，整文件读取足够；
	// 上限保护的是「有人把审计文件当日志写爆」的意外。
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		lines = append(lines, sc.Text())
		if len(lines) > 4*limit {
			lines = lines[len(lines)-limit:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	start := 0
	if len(lines) > limit {
		start = len(lines) - limit
	}
	out := make([]auditEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 半截行或人工编辑过的行，跳过而不是让整个历史不可读
		}
		out = append(out, e)
	}
	return out, nil
}

func summarizeOps(ops []Op) []opSummary {
	out := make([]opSummary, 0, len(ops))
	for _, op := range ops {
		s := opSummary{Op: op.Op}
		switch op.Op {
		case "model.patch", "model.delete", "disabled.add", "disabled.remove":
			s.Target = op.Model
		case "default.set":
			s.Target = op.Kind + " → " + op.Model
		case "default.delete":
			s.Target = op.Kind
		case "protocol.patch", "protocol.delete":
			s.Target = op.Name
		case "overlays.replace":
			if op.Overlays != nil {
				s.Target = fmt.Sprintf("models=%d disabled=%d protocols=%d",
					len(op.Overlays.Models), len(op.Overlays.Disabled), len(op.Overlays.Protocols))
			}
		}
		out = append(out, s)
	}
	return out
}
