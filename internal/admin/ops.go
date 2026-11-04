package admin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/apiduo/mcp-for-apiduo/internal/catalog"
)

// Op 是一次配置变更的最小单位。
//
// 为什么不直接 PUT 整份覆盖层：agent（以及脚本）没有长期记忆，只能
// 「读全量 → 改一处 → 写全量」，中间任何一次他人修改都会被它顺手抹掉。
// 拆成定址的小操作之后，每个操作自己就是完整的意图，重试也只影响自己那一处。
//
// 九个操作覆盖全部状态，因为它们正好作用在四个集合上：
//
//	models（按 id 的补丁）  settings.defaults（kind → id）  disabled（集合）  protocols（按名的模板）
//
// 全部幂等：同样的 ops 重放一次，结果不变、版本号不变。
type Op struct {
	Op       string            `json:"op"`
	Model    string            `json:"model,omitempty"`
	Kind     string            `json:"kind,omitempty"`
	Name     string            `json:"name,omitempty"`
	Patch    map[string]any    `json:"patch,omitempty"`
	Template map[string]any    `json:"template,omitempty"`
	Overlays *catalog.Overlays `json:"overlays,omitempty"`
}

// OpNames 是操作名清单，供 manifest 与错误信息使用。
var OpNames = []string{
	"model.patch", "model.delete",
	"default.set", "default.delete",
	"disabled.add", "disabled.remove",
	"protocol.patch", "protocol.delete",
	"overlays.replace",
}

// Change 是一条可核对的差异。Path 是 JSON Pointer，指到最末一级叶子。
type Change struct {
	Path   string `json:"path"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// opError 是操作本身写错了（400），与「校验不通过」（422）区分开：
// 前者是调用方拼错了请求，后者是配置内容会让运行时做错事。
type opError struct {
	Index   int
	Op      string
	Message string
	Hint    string
}

func (e *opError) Error() string { return e.Message }

// Apply 在 base 的副本上顺序执行 ops，返回新覆盖层与差异。
//
// 全部成功才返回结果：中途任何一个 op 出错，调用方拿不到半成品。
func Apply(base catalog.Overlays, ops []Op) (catalog.Overlays, []Change, error) {
	if len(ops) == 0 {
		return base.Clone(), nil, &opError{Message: "ops 不能为空", Hint: "至少给一个操作，或改用 GET 读取当前配置"}
	}
	next := base.Clone()
	for i, op := range ops {
		if err := applyOne(&next, op); err != nil {
			if oe, ok := err.(*opError); ok {
				oe.Index = i
				oe.Op = op.Op
			}
			return catalog.Overlays{}, nil, err
		}
	}
	next = next.Canonical()
	return next, Diff(base, next), nil
}

func applyOne(ov *catalog.Overlays, op Op) error {
	name := strings.TrimSpace(op.Op)
	switch name {
	case "model.patch":
		if err := needModel(op); err != nil {
			return err
		}
		if len(op.Patch) == 0 {
			return &opError{Message: "model.patch 缺少 patch", Hint: "patch 是 RFC 7386 合并补丁，例如 {\"kind\":\"video\"}"}
		}
		if ov.Models == nil {
			ov.Models = map[string]map[string]any{}
		}
		dst := ov.Models[op.Model]
		if dst == nil {
			dst = map[string]any{}
		}
		mergePatch(dst, op.Patch)
		prune(dst)
		if len(dst) == 0 {
			delete(ov.Models, op.Model) // 删成空补丁等于没有覆盖
		} else {
			ov.Models[op.Model] = dst
		}
		return nil

	case "model.delete":
		if err := needModel(op); err != nil {
			return err
		}
		delete(ov.Models, op.Model)
		return nil

	case "default.set":
		kind := catalog.NormalizeKind(op.Kind)
		if kind == "" {
			return &opError{Message: "default.set 缺少 kind", Hint: "kind 取 image / video / search"}
		}
		if strings.TrimSpace(op.Model) == "" {
			return &opError{Message: "default.set 缺少 model", Hint: "要清空默认模型用 default.delete"}
		}
		if ov.Settings.Defaults == nil {
			ov.Settings.Defaults = map[string]string{}
		}
		ov.Settings.Defaults[kind] = strings.TrimSpace(op.Model)
		return nil

	case "default.delete":
		kind := catalog.NormalizeKind(op.Kind)
		if kind == "" {
			return &opError{Message: "default.delete 缺少 kind"}
		}
		delete(ov.Settings.Defaults, kind)
		return nil

	case "disabled.add", "disabled.remove":
		if err := needModel(op); err != nil {
			return err
		}
		set := ov.DisabledSet()
		if name == "disabled.add" {
			set[op.Model] = true
		} else {
			delete(set, op.Model)
		}
		ov.Disabled = ov.Disabled[:0]
		for id := range set {
			ov.Disabled = append(ov.Disabled, id)
		}
		return nil

	case "protocol.patch":
		pname := strings.TrimSpace(op.Name)
		if pname == "" {
			return &opError{Message: "protocol.patch 缺少 name", Hint: "name 是协议族名，内置 image / video，或自定义名由 _protocol 引用"}
		}
		if len(op.Template) == 0 {
			return &opError{Message: "protocol.patch 缺少 template"}
		}
		if ov.Protocols == nil {
			ov.Protocols = map[string]map[string]any{}
		}
		dst := ov.Protocols[pname]
		if dst == nil {
			dst = map[string]any{}
		}
		mergePatch(dst, op.Template)
		prune(dst)
		if len(dst) == 0 {
			delete(ov.Protocols, pname)
		} else {
			ov.Protocols[pname] = dst
		}
		return nil

	case "protocol.delete":
		pname := strings.TrimSpace(op.Name)
		if pname == "" {
			return &opError{Message: "protocol.delete 缺少 name"}
		}
		delete(ov.Protocols, pname)
		return nil

	case "overlays.replace":
		if op.Overlays == nil {
			return &opError{Message: "overlays.replace 缺少 overlays", Hint: "空配置请传 {\"overlays\":{}}"}
		}
		*ov = op.Overlays.Clone()
		return nil

	case "":
		return &opError{Message: "缺少 op 字段", Hint: "可用操作：" + strings.Join(OpNames, ", ")}

	default:
		return &opError{
			Message: fmt.Sprintf("未知操作 %q", name),
			Hint:    "可用操作：" + strings.Join(OpNames, ", "),
		}
	}
}

func needModel(op Op) error {
	if strings.TrimSpace(op.Model) == "" {
		return &opError{Message: op.Op + " 缺少 model", Hint: "model 必须是上游 /v1/models 里的模型 ID"}
	}
	return nil
}

// mergePatch 实现 RFC 7386：对象递归合并，null 删除键，其余（含数组）整体替换。
//
// 用标准语义而不是自创规则，是因为 agent 对 merge patch 有稳定预期；
// 自创规则会让它每次都要回来查文档。
func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			dm, ok := dst[k].(map[string]any)
			if !ok {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergePatch(dm, pm)
			continue
		}
		dst[k] = v
	}
}

// prune 递归删掉空对象。合并产生的 {"input":{}} 没有任何语义，
// 留着只会让配置文件越长越乱、让版本号无谓抖动。
func prune(m map[string]any) {
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			prune(sub)
			if len(sub) == 0 {
				delete(m, k)
			}
		}
	}
}

// Diff 逐叶子比较两份覆盖层，供调用方核对「这次到底改了什么」。
//
// 返回的是差异而不是快照：agent 用它确认操作生效，人用它写变更记录。
func Diff(before, after catalog.Overlays) []Change {
	var out []Change
	out = append(out, diffSection("/models", flatten(plainMap(before.Models)), flatten(plainMap(after.Models)))...)
	out = append(out, diffSection("/settings/defaults", flatDefaults(before), flatDefaults(after))...)
	out = append(out, diffSection("/disabled", flatDisabled(before), flatDisabled(after))...)
	out = append(out, diffSection("/protocols", flatten(plainMap(before.Protocols)), flatten(plainMap(after.Protocols)))...)

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// plainMap 把具名 map 类型转成 map[string]any。
//
// 不做这层转换，flatten 里的类型断言会失败（map[string]map[string]any
// 不是 map[string]any），整棵子树被当成一个叶子，差异退化成「整段被替换」。
func plainMap(m map[string]map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// flatten 把嵌套 map 摊平成 path → 叶子值，便于逐叶子比较。
//
// 根为空映射时返回空集而不是「一个空叶子」：否则「从 {} 长出一个模型」
// 会被算成整棵 /models 被替换，差异粒度退化成整段配置。
func flatten(v any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, cur any)
	walk = func(prefix string, cur any) {
		m, ok := cur.(map[string]any)
		if !ok {
			out[prefix] = cur
			return
		}
		if len(m) == 0 {
			if prefix != "" {
				out[prefix] = map[string]any{}
			}
			return
		}
		for k, val := range m {
			walk(prefix+"/"+escapePointer(k), val)
		}
	}
	walk("", v)
	return out
}

func escapePointer(s string) string { return catalog.EscapePointer(s) }

func flatDefaults(ov catalog.Overlays) map[string]any {
	out := map[string]any{}
	for k, v := range ov.Settings.Defaults {
		out["/"+escapePointer(k)] = v
	}
	return out
}

func flatDisabled(ov catalog.Overlays) map[string]any {
	out := map[string]any{}
	for _, id := range ov.Disabled {
		out["/"+escapePointer(id)] = true
	}
	return out
}

// diffSection 比较两棵「path → 叶子」树。
func diffSection(prefix string, before, after map[string]any) []Change {
	var out []Change
	for path, bv := range before {
		av, ok := after[path]
		if !ok {
			out = append(out, Change{Path: prefix + path, Before: bv})
			continue
		}
		if !sameJSON(bv, av) {
			out = append(out, Change{Path: prefix + path, Before: bv, After: av})
		}
	}
	for path, av := range after {
		if _, ok := before[path]; !ok {
			out = append(out, Change{Path: prefix + path, After: av})
		}
	}
	return out
}

func sameJSON(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}
