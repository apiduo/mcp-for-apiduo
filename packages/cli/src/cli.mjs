import fs from "node:fs";
import { VERSION } from "./version.mjs";
import { parseArgs, hasJsonFlag } from "./args.mjs";
import {
  DEFAULT_ENDPOINT,
  normalizeEndpoint,
  resolveSettings,
  saveConfig,
  maskKey
} from "./config.mjs";
import { McpClient } from "./rpc.mjs";
import { prepareRefs, formatBytes, saveOutputs, stampOf } from "./files.mjs";
import * as ui from "./ui.mjs";
import { CliError, usageError, configError, timeoutError, toolError } from "./errors.mjs";

const DEFAULT_PROTOCOL = "2025-06-18";

/* ── 通用小工具 ─────────────────────────────────────────────────────── */

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const secs = (flags, name, fallback) => {
  const v = flags[name];
  const s = Number.isFinite(v) ? v : fallback;
  if (s <= 0) throw usageError(`--${name} 必须大于 0`);
  return Math.round(s * 1000);
};

function readText(flags, pos, flagName, what, cmdName) {
  const fromPos = pos.join(" ").trim();
  const fromFlag = flags[flagName];
  if (fromPos && fromFlag) throw usageError(`${what}给了两次`, `位置参数和 --${flagName} 只能用一个`);
  let text = fromFlag !== undefined ? fromFlag : fromPos;
  if (text === "-") {
    try { text = fs.readFileSync(0, "utf8"); }
    catch { throw usageError(`读不了 stdin`, `${what}用 - 表示从标准输入读，但没有输入`); }
  }
  if (!text || !String(text).trim()) {
    throw usageError(`缺少${what}`, `例如：apiduo ${cmdName} "一句话描述"`);
  }
  return String(text).trim();
}

function parseObject(raw, flag) {
  let v;
  try { v = JSON.parse(raw); }
  catch (e) { throw usageError(`${flag} 不是合法 JSON`, e.message); }
  if (!v || Array.isArray(v) || typeof v !== "object") {
    throw usageError(`${flag} 需要一个 JSON 对象`);
  }
  return v;
}

function needKey(settings) {
  if (!settings.key) {
    throw configError("还没有 API Key", "先跑 apiduo login，或设置 APIDUO_API_KEY");
  }
  return settings.key;
}

function kindCounts(models) {
  const by = { image: 0, video: 0, search: 0 };
  for (const m of models) if (by[m.kind] !== undefined) by[m.kind] += 1;
  return by;
}

function execLabel(m) {
  const task = m && m.execution && m.execution.task;
  if (task === "async") return "异步";
  if (task === "sync") return "同步";
  return "—";
}

const charWidth = (s) => Array.from(String(s)).reduce((n, ch) =>
  n + (/[\u1100-\u115f\u2e80-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe30-\ufe6f\uff00-\uff60\uffe0-\uffe6]/.test(ch) ? 2 : 1), 0);

function pad(s, n) {
  s = String(s == null ? "" : s);
  return s + " ".repeat(Math.max(0, n - charWidth(s)));
}

function clip(s, n) {
  s = String(s == null ? "" : s);
  return charWidth(s) <= n ? s : Array.from(s).slice(0, n - 1).join("") + "…";
}

/* ── 任务轮询 ───────────────────────────────────────────────────────── */

async function waitForTask(client, taskId, flags) {
  const timeoutMs = secs(flags, "wait-timeout", 600);
  const started = Date.now();
  let polls = 0;
  const tty = process.stderr.isTTY;

  for (;;) {
    if (Date.now() - started > timeoutMs) {
      throw timeoutError(
        `任务 ${taskId} 在 ${Math.round(timeoutMs / 1000)}s 内没有完成`,
        `用 apiduo task ${taskId} --wait 继续查；任务在上游仍然有效`
      );
    }
    polls += 1;
    const out = await client.tool("task.get", { task_id: taskId });
    if (out.status === "completed") {
      if (tty) process.stderr.write("\r\x1b[K");
      return { out, polls, elapsedMs: Date.now() - started };
    }
    if (out.status === "failed") {
      if (tty) process.stderr.write("\r\x1b[K");
      throw toolError(out.error || "任务失败", "展开 JSON（--json）可以看到 upstream 原始响应");
    }
    const elapsed = Date.now() - started;
    if (tty) {
      process.stderr.write(`\r轮询 #${polls} · ${ui.formatMs(elapsed)}`);
    } else if (polls === 1 || polls % 5 === 0) {
      ui.info(`轮询 #${polls} · ${ui.formatMs(elapsed)}`);
    }
    const wait = Math.max(1200, Number(out.poll_after_ms) || 2000);
    await sleep(wait);
  }
}

/* ── 输出 ───────────────────────────────────────────────────────────── */

function emit(flags, payload, human) {
  if (flags.json) {
    ui.printJSON(payload);
    return;
  }
  human();
}

function printOutputs(outputs = [], saved = []) {
  // 落了盘就只打路径：URL 还在 --json 里，终端里再铺一遍长链接只是噪音。
  const byUrl = new Map((saved || []).map((f) => [f.url, f]));
  outputs.forEach((o, i) => {
    const local = byUrl.get(o.url);
    const meta = [o.mime_type, o.width && o.height ? `${o.width}×${o.height}` : ""].filter(Boolean).join(" · ");
    const where = local ? `${local.path}${local.bytes ? `  (${formatBytes(local.bytes)})` : ""}` : o.url;
    process.stdout.write(`  ${i + 1}. ${where}${meta && !local ? `  (${meta})` : ""}\n`);
  });
}

function printUsage(u) {
  if (!u || typeof u !== "object") return;
  const parts = Object.entries(u).filter(([, v]) => typeof v === "number" || typeof v === "string").slice(0, 5)
    .map(([k, v]) => `${k}=${v}`);
  if (parts.length) process.stdout.write(`  usage: ${parts.join(" ")}\n`);
}

function emitGen(flags, command, out, elapsedMs, extra = {}) {
  const payload = {
    ok: true,
    command,
    result: out,
    meta: {
      model: out.model || extra.model || null,
      elapsed_ms: elapsedMs,
      ...(out.task_id ? { task_id: out.task_id } : {}),
      ...extra
    }
  };
  emit(flags, payload, () => {
    const model = out.model || extra.model || "(default)";
    if (out.status === "working" && out.task_id) {
      process.stdout.write(`${model} · 已提交 · ${ui.formatMs(elapsedMs)}\n`);
      process.stdout.write(`  task_id: ${out.task_id}\n`);
      process.stdout.write(`  用 apiduo task ${out.task_id} --wait 取结果\n`);
      return;
    }
    const n = Array.isArray(out.outputs) ? out.outputs.length : 0;
    process.stdout.write(`${model} · ${out.status || "completed"} · ${n} 个产物 · ${ui.formatMs(elapsedMs)}\n`);
    printOutputs(out.outputs || [], extra.saved || []);
    printUsage(out.usage);
    if (!n) {
      process.stdout.write("  上游没有返回产物，--json 查看完整响应\n");
    }
  });
}

/* 内联参考图失败时补一句「为什么」。
   老版本网关只回「参考图必须是 http/https」，既不说这是版本差异，也不说该怎么办。 */
function withRefHint(err, args) {
  const inline = (args.reference_images || []).some((v) => String(v).startsWith("data:"));
  if (!inline || !(err instanceof CliError) || err.kind !== "tool") return err;
  if (!/参考图|reference_images|image/i.test(err.message)) return err;
  err.hint = "网关还不支持内联参考图（data:image）：升级到包含该支持的版本，或先用公网 URL。";
  return err;
}

/* --out 的落盘：失败只提示、不改退出码 —— 产物 URL 已经拿到了，
   写不进去是本地目录的问题，不该把一次成功的生成报成失败。 */
async function maybeSave(flags, out, extra = {}) {
  if (!flags.out) return null;
  if (out.status === "working") return null;
  const outputs = out.outputs || [];
  if (!outputs.length) {
    ui.info(ui.dim("没有产物可落盘"));
    return null;
  }
  const r = await saveOutputs(outputs, flags.out, {
    model: out.model || extra.model,
    stamp: stampOf(out),
    timeoutMs: secs(flags, "timeout", 120)
  });
  for (const f of r.saved) ui.info(ui.dim(`已保存 ${f.path}`));
  for (const f of r.failed) {
    ui.info(ui.dim(`保存失败 ${String(f.url).slice(0, 60)}：${f.reason}`));
  }
  return r;
}

/* 只有真发生过才写进 JSON：saved: [] 会让脚本误以为有一步「落盘了但为空」。 */
function saveMeta(r) {
  if (!r) return {};
  return {
    ...(r.saved.length ? { saved: r.saved } : {}),
    ...(r.failed.length ? { save_failed: r.failed } : {})
  };
}

/* ── commands ───────────────────────────────────────────────────────── */

async function cmdLogin({ flags, settings }) {
  const endpoint = normalizeEndpoint(flags.endpoint || settings.endpoint || DEFAULT_ENDPOINT);
  let key = flags.key || process.env.APIDUO_API_KEY || settings.file.apiKey || "";
  if (!key) key = (await ui.promptHidden("API Key: ")).trim();
  if (!key) throw configError("没有 API Key", "用 apiduo login --key ... 或在交互终端里输入");

  const client = new McpClient({ endpoint, key, timeoutMs: secs(flags, "timeout", 30) });
  ui.info(ui.dim(`→ ${endpoint}`));

  const init = await client.request("initialize", {
    protocolVersion: DEFAULT_PROTOCOL,
    capabilities: {},
    clientInfo: { name: "apiduo-cli", version: VERSION }
  });
  const tools = (await client.request("tools/list", {}))?.tools || [];
  const models = (await client.tool("models.list", {}))?.models || [];
  const by = kindCounts(models);
  const path = saveConfig({ endpoint, apiKey: key });

  emit(flags, {
    ok: true,
    command: "login",
    server: init?.serverInfo || null,
    protocol: init?.protocolVersion || null,
    endpoint,
    key: maskKey(key),
    tools: tools.map((t) => t.name),
    models: { count: models.length, ...by },
    config_path: path
  }, () => {
    process.stdout.write(`[ok] 已连接 ${init?.serverInfo?.name || "MCP"} ${init?.serverInfo?.version || ""}\n`);
    process.stdout.write(`     endpoint  ${endpoint}\n`);
    process.stdout.write(`     auth      ${maskKey(key)}\n`);
    process.stdout.write(`     tools     ${tools.length}${tools.length ? " · " + tools.map((t) => t.name).join(", ") : ""}\n`);
    process.stdout.write(`     models    ${models.length}（图片 ${by.image} · 视频 ${by.video} · 检索 ${by.search}）\n`);
    process.stdout.write(`     config    ${path}\n`);
  });
}

async function cmdWhoami({ flags, settings }) {
  needKey(settings);
  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 30) });
  const init = await client.request("initialize", {
    protocolVersion: DEFAULT_PROTOCOL,
    capabilities: {},
    clientInfo: { name: "apiduo-cli", version: VERSION }
  });
  const tools = (await client.request("tools/list", {}))?.tools || [];
  const models = (await client.tool("models.list", {}))?.models || [];
  const by = kindCounts(models);

  emit(flags, {
    ok: true,
    command: "whoami",
    server: init?.serverInfo || null,
    protocol: init?.protocolVersion || null,
    endpoint: settings.endpoint,
    key: maskKey(settings.key),
    tools: tools.map((t) => t.name),
    models: { count: models.length, ...by }
  }, () => {
    process.stdout.write(`server    ${init?.serverInfo?.name || "?"} ${init?.serverInfo?.version || ""}  (protocol ${init?.protocolVersion || "?"})\n`);
    process.stdout.write(`endpoint  ${settings.endpoint}\n`);
    process.stdout.write(`auth      ${maskKey(settings.key)}\n`);
    process.stdout.write(`tools     ${tools.length}${tools.length ? " · " + tools.map((t) => t.name).join(", ") : ""}\n`);
    process.stdout.write(`models    ${models.length}（图片 ${by.image} · 视频 ${by.video} · 检索 ${by.search}）\n`);
  });
}

async function cmdModels({ flags, settings }) {
  needKey(settings);
  const kind = flags.kind ? String(flags.kind) : "";
  if (kind && !["image", "video", "search"].includes(kind)) {
    throw usageError(`--kind 只能是 image / video / search，收到 ${kind}`);
  }
  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 30) });
  const out = await client.tool("models.list", kind ? { kind } : {});
  const models = out.models || [];

  emit(flags, { ok: true, command: "models", count: models.length, models }, () => {
    if (!models.length) {
      process.stdout.write("没有可用模型。检查当前 Key 的渠道与模型权限。\n");
      return;
    }
    const groups = kind
      ? [[kind, models]]
      : ["image", "video", "search"].map((k) => [k, models.filter((m) => m.kind === k)]).filter(([, arr]) => arr.length);
    const names = { image: "图片", video: "视频", search: "检索" };
    for (const [k, arr] of groups) {
      process.stdout.write(`${names[k] || k} · ${arr.length}\n`);
      const w = Math.min(38, Math.max(...arr.map((m) => charWidth(m.id))) + 2);
      for (const m of arr) {
        const owner = clip(m.owned_by || "", 16);
        const transport = (m.execution && m.execution.transport) || "—";
        process.stdout.write(`  ${pad(m.id, w)}${pad(owner, 18)}${pad(execLabel(m), 6)}${transport}\n`);
      }
    }
    process.stdout.write(ui.dim("提示：--json 会带完整 parameters，可用来查 enum / max。\n"));
  });
}

function buildGenArgs(flags, pos, kind) {
  const prompt = readText(flags, pos, "prompt", "提示词", kind);
  const args = { prompt };
  if (flags.model) args.model = String(flags.model);
  if (flags.size) args.size = String(flags.size);
  if (flags.quality) args.quality = String(flags.quality);
  if (flags.n !== undefined) {
    if (!Number.isInteger(flags.n) || flags.n < 1) throw usageError("--n 必须是 ≥ 1 的整数");
    args.n = flags.n;
  }

  const refs = prepareRefs(flags.ref || []);
  if (refs.values.length) args.reference_images = refs.values;
  if (refs.inlined.length) {
    ui.info(ui.dim(`已内联 ${refs.inlined.length} 张本地图（${formatBytes(refs.totalBytes)}）`));
  }

  const extra = {};
  if (flags.duration !== undefined) {
    if (flags.duration <= 0) throw usageError("--duration 必须大于 0");
    extra.duration = flags.duration;
  }
  if (flags.extra) Object.assign(extra, parseObject(flags.extra, "--extra"));
  for (const k of ["prompt", "model", "size", "n", "quality", "reference_images", "extra"]) {
    if (Object.prototype.hasOwnProperty.call(extra, k)) {
      throw usageError(`--extra 里不能再写 ${k}`, "这些字段有专门的参数");
    }
  }
  if (Object.keys(extra).length) args.extra = extra;

  return { args, refs, kind };
}

async function cmdImage({ flags, pos, settings }) {
  needKey(settings);
  const { args } = buildGenArgs(flags, pos, "image");
  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 120) });
  const started = Date.now();
  let out;
  try {
    out = await client.tool("image.generate", args);
  } catch (e) {
    throw withRefHint(e, args);
  }

  if (out.status === "working" && out.task_id) {
    if (flags.wait === false) {
      emitGen(flags, "image", out, Date.now() - started);
      return;
    }
    ui.info(ui.dim(`任务已提交，开始轮询 · ${clip(out.task_id, 26)}`));
    const done = await waitForTask(client, out.task_id, flags);
    out = done.out;
    const saved = await maybeSave(flags, out, { model: args.model });
    emitGen(flags, "image", out, Date.now() - started, {
      polls: done.polls, task_id: out.task_id, ...saveMeta(saved)
    });
    return;
  }
  const saved = await maybeSave(flags, out, { model: args.model });
  emitGen(flags, "image", out, Date.now() - started, saveMeta(saved));
}

async function cmdVideo({ flags, pos, settings }) {
  needKey(settings);
  const { args } = buildGenArgs(flags, pos, "video");
  if (!flags.yes) {
    const desc = [args.model || "默认视频模型", args.extra?.duration ? `${args.extra.duration}s` : ""].filter(Boolean).join(" · ");
    const ok = await ui.confirm(`提交视频任务会立即计费（${desc}），继续？`);
    if (!ok) {
      ui.info("已取消");
      return;
    }
  }

  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 120) });
  const started = Date.now();
  let out;
  try {
    out = await client.tool("video.generate", args);
  } catch (e) {
    throw withRefHint(e, args);
  }

  if (out.status === "working" && out.task_id) {
    if (flags.wait === false) {
      emitGen(flags, "video", out, Date.now() - started);
      return;
    }
    ui.info(ui.dim(`任务已提交，开始轮询 · ${clip(out.task_id, 26)}`));
    const done = await waitForTask(client, out.task_id, flags);
    out = done.out;
    const saved = await maybeSave(flags, out, { model: args.model });
    emitGen(flags, "video", out, Date.now() - started, {
      polls: done.polls, task_id: out.task_id, ...saveMeta(saved)
    });
    return;
  }
  const saved = await maybeSave(flags, out, { model: args.model });
  emitGen(flags, "video", out, Date.now() - started, saveMeta(saved));
}

async function cmdSearch({ flags, pos, settings }) {
  needKey(settings);
  const query = readText(flags, pos, "query", "检索词", "search");
  const args = { query };
  if (flags.model) args.model = String(flags.model);
  if (flags.count !== undefined) {
    if (!Number.isInteger(flags.count) || flags.count < 1) throw usageError("--count 必须是 ≥ 1 的整数");
    args.count = flags.count;
  }
  if (flags.content) args.include_content = true;
  if (flags.extra) args.extra = parseObject(flags.extra, "--extra");

  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 60) });
  const started = Date.now();
  const out = await client.tool("search.query", args);
  const elapsed = Date.now() - started;

  emit(flags, { ok: true, command: "search", result: out, meta: { elapsed_ms: elapsed } }, () => {
    const results = out.results || [];
    process.stdout.write(`${out.model || "(default)"} · ${out.count ?? results.length} 条结果 · ${ui.formatMs(elapsed)}\n`);
    results.forEach((r, i) => {
      const title = r.title || r.name || r.site_name || r.url || `结果 ${i + 1}`;
      process.stdout.write(`${i + 1}. ${title}\n`);
      if (r.url) process.stdout.write(`   ${r.url}\n`);
      const snippet = r.snippet || r.summary || r.description || r.content || r.text || "";
      if (snippet) process.stdout.write(`   ${clip(String(snippet).replace(/\s+/g, " "), 160)}\n`);
    });
    if (!results.length) process.stdout.write("  没有结果。\n");
  });
}

async function cmdTask({ flags, pos, settings }) {
  needKey(settings);
  if (!pos[0]) throw usageError("缺少 task_id", "apiduo task <task_id> [--wait]");
  if (pos.length > 1) throw usageError("task 只接受一个 task_id");
  const taskId = String(pos[0]);
  const client = new McpClient({ endpoint: settings.endpoint, key: settings.key, timeoutMs: secs(flags, "timeout", 30) });

  const started = Date.now();
  let out = await client.tool("task.get", { task_id: taskId });
  let polls = 0;
  if (flags.wait && out.status === "working") {
    ui.info(ui.dim(`任务还在进行，开始轮询 · ${taskId}`));
    const done = await waitForTask(client, taskId, flags);
    out = done.out;
    polls = done.polls + 1; // +1 是进来时那次直接查询

  }
  const elapsed = Date.now() - started;
  const saved = await maybeSave(flags, out);

  emit(flags, {
    ok: true,
    command: "task",
    result: out,
    meta: { elapsed_ms: elapsed, polls, ...saveMeta(saved) }
  }, () => {
    process.stdout.write(`${taskId} · ${out.status || "?"}${out.model ? " · " + out.model : ""} · ${ui.formatMs(elapsed)}\n`);
    if (out.status === "working") {
      process.stdout.write("  还在进行；加 --wait 会一直等到完成。\n");
    }
    printOutputs(out.outputs || [], (saved && saved.saved) || []);
    if (out.error) process.stdout.write(`  error: ${out.error}\n`);
    printUsage(out.usage);
  });
}

async function cmdConfig({ flags, pos, settings }) {
  if (pos[0] === "path") {
    if (flags.json) ui.printJSON({ ok: true, command: "config", path: settings.path });
    else process.stdout.write(settings.path + "\n");
    return;
  }
  if (pos[0]) throw usageError(`未知的 config 子命令 ${pos[0]}`, "apiduo config 或 apiduo config path");

  emit(flags, {
    ok: true,
    command: "config",
    path: settings.path,
    endpoint: settings.endpoint,
    endpoint_source: settings.sources.endpoint,
    key: maskKey(settings.key),
    key_source: settings.sources.key,
    env: {
      APIDUO_ENDPOINT: Boolean(process.env.APIDUO_ENDPOINT),
      APIDUO_API_KEY: Boolean(process.env.APIDUO_API_KEY)
    }
  }, () => {
    process.stdout.write(`config    ${settings.path}\n`);
    process.stdout.write(`endpoint  ${settings.endpoint}  (${settings.sources.endpoint})\n`);
    process.stdout.write(`api key   ${maskKey(settings.key)}  (${settings.sources.key})\n`);
    process.stdout.write(`env       APIDUO_ENDPOINT ${process.env.APIDUO_ENDPOINT ? "已设置" : "未设置"} · APIDUO_API_KEY ${process.env.APIDUO_API_KEY ? "已设置" : "未设置"}\n`);
  });
}

/* ── help ───────────────────────────────────────────────────────────── */

const HELP = `apiduo ${VERSION} — apiduo MCP 命令行

用法
  apiduo <command> [参数] [flags]

命令
  login       连接服务，验证 Key，保存端点与密钥
  whoami      查看服务身份、工具与模型概览
  models      列出当前 Key 可用的模型
  image       生成图片（同步直返，异步自动等待）
  video       生成视频（异步，默认等待结果）
  search      联网检索
  task        查询异步任务
  config      查看配置路径与当前端点

全局 flags
  --endpoint <url>     MCP 端点（默认读配置 / APIDUO_ENDPOINT）
  --key <key>          API Key（默认读配置 / APIDUO_API_KEY）
  --json               只把 JSON 打到 stdout（日志仍走 stderr）
  --timeout <秒>       单次请求超时，默认 120
  --wait-timeout <秒>  异步任务最长等待，默认 600
  -h, --help
  -v, --version

示例
  apiduo login
  apiduo models --kind image
  apiduo image "一只戴宇航头盔的柴犬" --size 1024x1536 --n 2
  apiduo image "把背景换成海边" --ref ./photo.png --model gpt-image-1
  apiduo image "竖版海报，两张" --n 2 --out ./out/
  apiduo video "海浪拍打礁石，慢镜头" --duration 5 --wait --out ./out/
  apiduo search "MCP 协议 2026 有什么更新" --count 5
  apiduo task <task_id> --wait

退出码：0 成功 · 2 用法 · 3 配置/鉴权 · 4 工具或上游拒绝 · 5 网络/超时
`;

const CMD_HELP = {
  login: `用法: apiduo login [--endpoint URL] [--key KEY] [--json]

连接服务，执行 initialize → tools/list → models.list；三步都通过才写入配置。
不传 --key 时在终端里隐藏输入。配置写入 0600。`,
  whoami: `用法: apiduo whoami [--json]

只看身份与能力，不写配置。`,
  models: `用法: apiduo models [--kind image|video|search] [--json]

--json 会带完整的 parameters（enum / min / max / default），是查参数的唯一入口。`,
  image: `用法: apiduo image "<提示词>" [flags]

flags
  --prompt <文本>       也可以用它代替位置参数；- 表示从 stdin 读
  --model <id>          留空用该类型的默认模型
  --size <值>           取值见 apiduo models --json
  --n <数量>            生成数量
  --quality <档位>      质量档位
  --ref <URL|路径>      参考图，可重复；本地图会内联成 data URI（单张 ≤4MB、总量 ≤5MB）
  --extra '<JSON>'      模型声明的其它参数，例如 {"aspect_ratio":"16:9"}
  --wait / --no-wait    异步模型默认等待；--no-wait 只拿 task_id
  --out <目录|文件>     把产物下载到本地；目录以 / 结尾或已存在时按目录处理`,
  video: `用法: apiduo video "<提示词>" [flags]

flags 与 image 相同，另有：
  --duration <秒>       视频时长
  --yes                 跳过计费确认（非交互环境必须显式加）
  --wait / --no-wait    默认等待；--no-wait 只拿 task_id
  --out <目录|文件>     把产物下载到本地

提交视频会立即计费，这是唯一会先确认再提交的命令。`,
  search: `用法: apiduo search "<检索词>" [--model ID] [--count N] [--content] [--json]

--content 让上游额外返回网页正文（仅模型声明支持时可用）。`,
  task: `用法: apiduo task <task_id> [--wait] [--json]

--wait 会按 poll_after_ms 一直轮询到 completed / failed。
--out 把已完成的产物下载到本地；还没完成时不会落盘。`,
  config: `用法: apiduo config [path] [--json]

不带参数显示配置路径、端点来源与 Key 掩码；path 只打印路径。`
};

function printHelp(name) {
  if (name && CMD_HELP[name]) process.stdout.write(CMD_HELP[name] + "\n");
  else process.stdout.write(HELP);
}

/* ── 命令表 ─────────────────────────────────────────────────────────── */

const COMMANDS = {
  login: {
    spec: { flags: {} },
    run: cmdLogin
  },
  whoami: {
    spec: { flags: {} },
    run: cmdWhoami
  },
  models: {
    spec: { flags: { kind: "string" } },
    run: cmdModels
  },
  image: {
    spec: {
      defaults: { wait: true },
      flags: {
        prompt: "string", model: "string", size: "string", n: "number",
        quality: "string", ref: "list", extra: "string", wait: "bool", out: "string"
      }
    },
    run: cmdImage
  },
  video: {
    spec: {
      defaults: { wait: true },
      flags: {
        prompt: "string", model: "string", size: "string", n: "number",
        quality: "string", duration: "number", ref: "list", extra: "string",
        wait: "bool", yes: "bool", out: "string"
      }
    },
    run: cmdVideo
  },
  search: {
    spec: {
      flags: {
        query: "string", model: "string", count: "number",
        content: "bool", extra: "string"
      }
    },
    run: cmdSearch
  },
  task: {
    spec: { defaults: { wait: false }, flags: { wait: "bool", out: "string" } },
    run: cmdTask
  },
  config: {
    spec: { flags: {} },
    run: cmdConfig
  }
};

function handleError(err, json, command) {
  const e = err instanceof CliError
    ? err
    : new CliError("internal", err && err.message ? err.message : String(err), "", 1);
  if (json) {
    ui.printJSON({ ok: false, command, error: { kind: e.kind, message: e.message, hint: e.hint } });
  } else {
    ui.failText(e);
  }
  process.exitCode = e.exitCode;
}

/* ── 入口 ───────────────────────────────────────────────────────────── */

export async function main(argv) {
  const first = argv[0];

  if (!first || first === "-h" || first === "--help" || first === "help") {
    printHelp(argv[1]);
    return;
  }
  if (first === "-v" || first === "--version" || first === "version") {
    process.stdout.write(VERSION + "\n");
    return;
  }
  if (first.startsWith("-")) {
    handleError(usageError(`缺少命令，收到 ${first}`, "apiduo --help"), hasJsonFlag(argv), "");
    return;
  }

  const command = COMMANDS[first];
  if (!command) {
    handleError(usageError(`未知命令 ${first}`, "apiduo --help"), hasJsonFlag(argv), first);
    return;
  }

  let parsed;
  try {
    parsed = parseArgs(argv.slice(1), command.spec);
  } catch (err) {
    handleError(err, hasJsonFlag(argv), first);
    return;
  }
  if (parsed.flags.help) {
    printHelp(first);
    return;
  }

  let settings;
  try {
    settings = resolveSettings(parsed.flags);
  } catch (err) {
    handleError(err, parsed.flags.json, first);
    return;
  }

  try {
    await command.run({
      flags: parsed.flags,
      pos: parsed.pos,
      settings,
      argv: argv.slice(1)
    });
  } catch (err) {
    handleError(err, parsed.flags.json, first);
  }
}
