import { usageError } from "./errors.mjs";

/**
 * 全局 flags 每个命令都认；子命令再追加自己的。
 * 类型只有四种：bool / string / number / list（可重复）。
 */
export const GLOBAL_FLAGS = {
  json: "bool",
  endpoint: "string",
  key: "string",
  timeout: "number",
  "wait-timeout": "number",
  verbose: "bool"
};

/**
 * 解析 `--flag value` / `--flag=value` / `--no-flag` / `--`。
 * 不引第三方库：参数表很小，手写反而能把「未知参数」和「缺值」分开报。
 */
export function parseArgs(argv, spec = {}) {
  const defs = { ...GLOBAL_FLAGS, ...(spec.flags || {}) };
  const flags = { json: false, verbose: false, timeout: 120, "wait-timeout": 600, ...(spec.defaults || {}) };
  const pos = [];

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--") {
      pos.push(...argv.slice(i + 1));
      break;
    }
    if (arg === "-h" || arg === "--help") {
      flags.help = true;
      continue;
    }
    if (arg === "-v" || arg === "--version") {
      flags.version = true;
      continue;
    }
    if (!arg.startsWith("--")) {
      pos.push(arg);
      continue;
    }

    let name;
    let inline;
    const eq = arg.indexOf("=");
    if (eq >= 0) {
      name = arg.slice(2, eq);
      inline = arg.slice(eq + 1);
    } else {
      name = arg.slice(2);
    }

    let negated = false;
    if (name.startsWith("no-")) {
      negated = true;
      name = name.slice(3);
    }

    const type = defs[name];
    if (!type) {
      throw usageError(`未知参数 --${name}`, "用 --help 查看当前命令支持的参数");
    }

    if (type === "bool") {
      flags[name] = inline === undefined ? !negated : inline !== "false";
      continue;
    }
    if (negated) {
      throw usageError(`--${name} 不支持 --no- 形式`);
    }
    if (inline === undefined) {
      const next = argv[i + 1];
      if (next === undefined || next.startsWith("--")) {
        throw usageError(`--${name} 缺少值`);
      }
      inline = next;
      i += 1;
    }

    if (type === "number") {
      const n = Number(inline);
      if (!Number.isFinite(n)) {
        throw usageError(`--${name} 需要数字，收到 ${JSON.stringify(inline)}`);
      }
      flags[name] = n;
    } else if (type === "list") {
      if (!Array.isArray(flags[name])) flags[name] = [];
      flags[name].push(inline);
    } else {
      flags[name] = inline;
    }
  }

  return { flags, pos };
}

export function hasJsonFlag(argv) {
  return argv.includes("--json") || argv.includes("--json=true");
}
