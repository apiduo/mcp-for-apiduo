import readline from "node:readline/promises";
import { usageError } from "./errors.mjs";

const color = (code) => (s) => (process.stderr.isTTY && !process.env.NO_COLOR ? `\x1b[${code}m${s}\x1b[0m` : String(s));
export const bold = color("1");
export const dim = color("2");
export const red = color("31");
export const green = color("32");
export const yellow = color("33");
export const cyan = color("36");

export function printJSON(value) {
  process.stdout.write(JSON.stringify(value, null, 2) + "\n");
}

/**
 * 进度、状态、提示一律走 stderr。
 * 这样 `apiduo image ... > out.json` 时 stdout 始终只有结果，agent 不需要去剥日志。
 */
export function info(message) {
  process.stderr.write(message + "\n");
}

export function warn(message) {
  process.stderr.write(yellow("! ") + message + "\n");
}

export function failText(err) {
  process.stderr.write(red("error: ") + (err.message || String(err)) + "\n");
  if (err.hint) process.stderr.write(dim("  " + err.hint) + "\n");
}

export function formatMs(ms) {
  if (ms == null) return "—";
  return ms < 1000 ? `${Math.round(ms)}ms` : `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)}s`;
}

export async function confirm(question) {
  if (!process.stdin.isTTY) {
    throw usageError("这里是交互确认，但 stdin 不是终端", "脚本里请显式加 --yes（提交会立即计费）");
  }
  const rl = readline.createInterface({ input: process.stdin, output: process.stderr });
  try {
    const answer = await rl.question(question + " [y/N] ");
    return /^y(es)?$/i.test(answer.trim());
  } finally {
    rl.close();
  }
}

/** 读 Key 时不回显；回退到 readline 会把整把 Key 留在终端历史里。 */
export async function promptHidden(question) {
  if (!process.stdin.isTTY) {
    throw usageError("没有可用的 API Key，且 stdin 不是终端", "用 apiduo login --key ... 或设置 APIDUO_API_KEY");
  }
  return new Promise((resolve) => {
    const stdin = process.stdin;
    process.stderr.write(question);
    stdin.setRawMode(true);
    stdin.resume();
    stdin.setEncoding("utf8");
    let buf = "";

    const cleanup = () => {
      try { stdin.setRawMode(false); } catch {}
      stdin.pause();
      stdin.off("data", onData);
    };
    const onData = (chunk) => {
      for (const ch of chunk) {
        if (ch === "\r" || ch === "\n") {
          cleanup();
          process.stderr.write("\n");
          return resolve(buf);
        }
        if (ch === "\u0003") { // Ctrl-C
          cleanup();
          process.stderr.write("\n");
          process.exit(130);
        }
        if (ch === "\u007f" || ch === "\b") {
          if (buf) { buf = buf.slice(0, -1); process.stderr.write("\b \b"); }
          continue;
        }
        buf += ch;
        process.stderr.write("*");
      }
    };
    stdin.on("data", onData);
  });
}
