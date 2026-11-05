import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { usageError } from "./errors.mjs";

const MIME = {
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".webp": "image/webp",
  ".gif": "image/gif"
};

/**
 * 参考图入口：
 *   - http/https 原样给网关（由它去取）；
 *   - data: 原样透传（调用方自己内联的）；
 *   - 本地路径 → data URI，服务端只解码、不联网。
 *
 * 总量必须卡住：MCP 请求体经过 nginx 时上限是 8MB，base64 还会膨胀 1/3。
 * 单张 4MB、总量 5MB 是留了 JSON 余量的保守值。
 */
export function prepareRefs(values = [], opts = {}) {
  const maxFileBytes = opts.maxFileBytes ?? 4 * 1024 * 1024;
  const maxTotalBytes = opts.maxTotalBytes ?? 5 * 1024 * 1024;
  const out = [];
  const inlined = [];
  let totalBytes = 0;

  for (const raw of values) {
    const v = String(raw || "").trim();
    if (!v) continue;

    if (/^https?:\/\//i.test(v) || /^data:/i.test(v)) {
      out.push(v);
      continue;
    }

    const abs = path.resolve(v);
    let st;
    try {
      st = fs.statSync(abs);
    } catch {
      throw usageError(`参考图不存在: ${v}`, "路径是相对当前目录的；也可以直接给 http(s) URL");
    }
    if (!st.isFile()) throw usageError(`参考图不是文件: ${v}`);

    const ext = path.extname(abs).toLowerCase();
    const mime = MIME[ext];
    if (!mime) {
      throw usageError(`不支持的图片格式: ${ext || "(无扩展名)"}`, "支持 png / jpg / jpeg / webp / gif");
    }
    if (st.size > maxFileBytes) {
      throw usageError(
        `参考图过大: ${v}（${(st.size / 1024 / 1024).toFixed(2)} MB）`,
        `单张上限 ${(maxFileBytes / 1024 / 1024).toFixed(0)} MB；先压缩，或改用公网 URL`
      );
    }
    totalBytes += st.size;
    if (totalBytes > maxTotalBytes) {
      throw usageError(
        `参考图总量过大（${(totalBytes / 1024 / 1024).toFixed(2)} MB）`,
        `总量上限 ${(maxTotalBytes / 1024 / 1024).toFixed(0)} MB（base64 会膨胀 1/3，网关还有 8MB 请求体限制）`
      );
    }

    const data = fs.readFileSync(abs);
    out.push(`data:${mime};base64,${data.toString("base64")}`);
    inlined.push({ path: abs, bytes: st.size });
  }

  return { values: out, inlined, totalBytes };
}

export function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

/* ── 产物落盘 ────────────────────────────────────────────────────────────
   这是 CLI 相对 MCP 唯一「多做」的一类事：服务端只能把 URL 交出来，
   把字节放到用户磁盘上只有本地进程能做。 */

const MIME_EXT = {
  "image/png": ".png",
  "image/jpeg": ".jpg",
  "image/webp": ".webp",
  "image/gif": ".gif",
  "image/avif": ".avif",
  "video/mp4": ".mp4",
  "video/webm": ".webm",
  "video/quicktime": ".mov"
};

const MAX_SAVE_BYTES = 512 * 1024 * 1024;

function expandHome(p) {
  const v = String(p || "");
  if (v === "~") return os.homedir();
  if (v.startsWith("~/")) return path.join(os.homedir(), v.slice(2));
  return v;
}

function slug(s) {
  return String(s || "output")
    .replace(/[^\w.-]+/g, "_")
    .replace(/^[._]+|[._]+$/g, "")
    .slice(0, 40) || "output";
}

function extFor(url, mime) {
  const key = String(mime || "").split(";")[0].trim().toLowerCase();
  if (MIME_EXT[key]) return MIME_EXT[key];
  if (!/^data:/i.test(url)) {
    try {
      const ext = path.extname(new URL(url).pathname).toLowerCase();
      if (/^\.[a-z0-9]{2,5}$/.test(ext)) return ext;
    } catch {
      /* URL 解析不了就走默认 */
    }
  }
  const guess = String(mime || "").toLowerCase();
  if (guess.startsWith("video/")) return ".mp4";
  if (guess.startsWith("image/")) return ".png";
  return ".bin";
}

function decodeDataURI(raw) {
  const comma = raw.indexOf(",");
  if (comma < 0) throw new Error("data URI 缺少逗号");
  const meta = raw.slice(5, comma);
  const payload = raw.slice(comma + 1);
  const mime = meta.split(";")[0].trim();
  if (/;base64$/i.test(meta)) {
    return { data: Buffer.from(payload, "base64"), mime };
  }
  return { data: Buffer.from(decodeURIComponent(payload), "utf8"), mime };
}

async function download(url, timeoutMs) {
  const res = await fetch(url, {
    redirect: "follow",
    signal: AbortSignal.timeout(timeoutMs)
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const declared = Number(res.headers.get("content-length") || 0);
  if (declared > MAX_SAVE_BYTES) throw new Error(`产物过大（${formatBytes(declared)}）`);
  const data = Buffer.from(await res.arrayBuffer());
  if (data.length > MAX_SAVE_BYTES) throw new Error(`产物过大（${formatBytes(data.length)}）`);
  return { data, mime: res.headers.get("content-type") || "" };
}

function localStamp(d = new Date()) {
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`;
}

function uniquePath(file) {
  if (!fs.existsSync(file)) return file;
  const dir = path.dirname(file);
  const ext = path.extname(file);
  const stem = path.basename(file, ext);
  for (let i = 2; i < 1000; i++) {
    const next = path.join(dir, `${stem}-${i}${ext}`);
    if (!fs.existsSync(next)) return next;
  }
  throw new Error("同名文件过多，换一个 --out 目录");
}

/**
 * 把产物写到本地。
 *
 *   --out ./dir/        目录：自动命名，不覆盖已有文件（同 task 重跑会得到 -2）
 *   --out ./a.png       文件：仅在只有一个产物时允许，此时**覆盖**（用户点了名）
 *
 * 单个产物失败不抛错：URL 已经拿到了，落盘是增值动作，不该让整次调用变成失败。
 */
export async function saveOutputs(outputs, target, opts = {}) {
  const list = (outputs || []).filter((o) => o && o.url);
  if (!list.length) throw usageError("没有可落盘的产物");
  if (!target) throw usageError("缺少 --out 目标路径");

  const timeoutMs = opts.timeoutMs ?? 120000;
  // 文件名用本地时间：toISOString() 是 UTC，和用户看到的钟差 8 小时，对不上账
  const stamp = String(opts.stamp || "").slice(0, 12) || localStamp();
  const base = slug(opts.model);

  const expanded = expandHome(target);
  const looksDir = /[\\/]$/.test(String(target)) ||
    (fs.existsSync(expanded) && fs.statSync(expanded).isDirectory());

  let asFile = null;
  if (!looksDir) {
    if (list.length > 1) {
      throw usageError(
        `--out 指向的是文件，但有 ${list.length} 个产物`,
        "改成目录（如 --out ./out/），或只用 --n 1"
      );
    }
    asFile = expanded;
  }

  const saved = [];
  const failed = [];
  for (let i = 0; i < list.length; i++) {
    const url = String(list[i].url);
    try {
      let data;
      let mime = list[i].mime_type || "";
      if (/^data:/i.test(url)) {
        const d = decodeDataURI(url);
        data = d.data;
        mime = mime || d.mime;
      } else if (/^https?:\/\//i.test(url)) {
        const d = await download(url, timeoutMs);
        data = d.data;
        mime = mime || d.mime;
      } else {
        throw new Error("不支持的产物地址（只认 http(s) 与 data:）");
      }

      const file = asFile || path.join(
        expanded,
        `${base}-${stamp}${list.length > 1 ? `-${i + 1}` : ""}${extFor(url, mime)}`
      );
      fs.mkdirSync(path.dirname(file), { recursive: true });
      const final = asFile ? file : uniquePath(file);
      fs.writeFileSync(final, data);
      saved.push({ url, path: final, bytes: data.length, mime_type: mime || null });
    } catch (e) {
      failed.push({ url, reason: e.message });
    }
  }
  return { saved, failed };
}

/** 产物太多时用 task_id 前几位做标记，重跑不会撞在一起。 */
export function stampOf(out) {
  const id = out && (out.task_id || (out.upstream && out.upstream.id));
  return id ? String(id).slice(0, 12) : "";
}
