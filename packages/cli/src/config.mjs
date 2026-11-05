import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { configError, usageError } from "./errors.mjs";

export const DEFAULT_ENDPOINT = "https://mcp.apiduo.cn/mcp";

/** 配置目录：APIDUO_CONFIG_DIR > XDG_CONFIG_HOME > ~/.config。测试用第一个覆盖。 */
export function configDir() {
  if (process.env.APIDUO_CONFIG_DIR) return process.env.APIDUO_CONFIG_DIR;
  const base = process.env.XDG_CONFIG_HOME || path.join(os.homedir(), ".config");
  return path.join(base, "apiduo");
}

export function configPath() {
  return path.join(configDir(), "config.json");
}

export function loadConfig() {
  const file = configPath();
  let raw;
  try {
    raw = fs.readFileSync(file, "utf8");
  } catch (err) {
    if (err.code === "ENOENT") return {};
    throw configError(`读不了配置文件 ${file}`, err.message);
  }
  try {
    const obj = JSON.parse(raw);
    return obj && typeof obj === "object" ? obj : {};
  } catch {
    throw configError(`配置文件不是合法 JSON: ${file}`, "修好它，或直接删除后重新 apiduo login");
  }
}

/** 写入 0600：这里存的是能花钱的 Key，不能跟着 dotfiles 一起被读走。 */
export function saveConfig(next) {
  const dir = configDir();
  const file = configPath();
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  const body = JSON.stringify({ ...loadConfig(), ...next, updated_at: new Date().toISOString() }, null, 2) + "\n";
  fs.writeFileSync(file, body, { mode: 0o600 });
  try {
    fs.chmodSync(file, 0o600);
    fs.chmodSync(dir, 0o700);
  } catch {
    // Windows 上 chmod 基本是空操作，忽略
  }
  return file;
}

/** 裸域名补 /mcp；已经有路径的不动，避免把 /api/mcp 改成 /api/mcp/mcp。 */
export function normalizeEndpoint(raw) {
  const s = String(raw || "").trim();
  if (!/^https?:\/\//i.test(s)) {
    throw configError(`端点必须以 http:// 或 https:// 开头，收到 ${JSON.stringify(s)}`);
  }
  let u;
  try {
    u = new URL(s);
  } catch {
    throw configError(`端点不是合法 URL: ${JSON.stringify(s)}`);
  }
  if (!u.pathname || u.pathname === "/") u.pathname = "/mcp";
  u.pathname = u.pathname.replace(/\/+$/, "");
  return u.toString();
}

/**
 * 优先级：命令行 > 环境变量 > 配置文件。
 * 返回 sources 是为了 `apiduo config` 能说清楚「这个值是从哪来的」。
 */
export function resolveSettings(flags = {}) {
  const file = loadConfig();
  const endpointRaw = flags.endpoint || process.env.APIDUO_ENDPOINT || file.endpoint || DEFAULT_ENDPOINT;
  const key = flags.key || process.env.APIDUO_API_KEY || file.apiKey || "";
  return {
    endpoint: normalizeEndpoint(endpointRaw),
    key: String(key || ""),
    file,
    path: configPath(),
    sources: {
      endpoint: flags.endpoint ? "flag" : process.env.APIDUO_ENDPOINT ? "env" : file.endpoint ? "file" : "default",
      key: flags.key ? "flag" : process.env.APIDUO_API_KEY ? "env" : file.apiKey ? "file" : "none"
    }
  };
}

export function requireKey(settings) {
  if (!settings.key) {
    throw usageError("还没有 API Key", "先跑 apiduo login，或设置 APIDUO_API_KEY");
  }
  return settings.key;
}

export function maskKey(key) {
  if (!key) return "—";
  // 掩码的用途是「认出是哪把」，不是「露出大部分」。11 字符的 key 按
  // 「前 5 后 4」会露出 9 个字符 —— 那已经是明文了。短 key 一律全掩。
  if (key.length < 16) return "••••••";
  return key.slice(0, 4) + "••••••" + key.slice(-4);
}
