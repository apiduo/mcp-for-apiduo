import { VERSION } from "./version.mjs";
import {
  authError,
  configError,
  looksLikeAuth,
  networkError,
  timeoutError,
  toolError
} from "./errors.mjs";

/** Streamable HTTP 可能回 JSON，也可能回 SSE；两种都要能解。 */
export function parseBody(text, contentType = "") {
  if (!text) return null;
  const ct = String(contentType).toLowerCase();
  if (ct.includes("text/event-stream") || /^\s*(?:event|data):/m.test(text)) {
    let last = null;
    for (const line of text.split(/\r?\n/)) {
      if (!line.startsWith("data:")) continue;
      const chunk = line.slice(5).trim();
      if (!chunk || chunk === "[DONE]") continue;
      try { last = JSON.parse(chunk); } catch { /* 忽略坏行，最后一条有效的算数 */ }
    }
    return last;
  }
  try { return JSON.parse(text); }
  catch { return { __raw: text }; }
}

function contentText(result) {
  if (!result || !Array.isArray(result.content)) return "";
  return result.content.filter((c) => c && c.type === "text").map((c) => c.text).join("\n");
}

function hintForTool(message) {
  const m = String(message || "");
  if (/未声明\s*\w*\s*的上限|单次最多/.test(m)) {
    return "模型没声明批量上限，网关按 8 兜底：把 --n 调小，或分多次调用。";
  }
  if (/未声明参数/.test(m)) {
    return "这个参数不在模型画像里；先 apiduo models --json 看模型声明了哪些参数。";
  }
  if (/不支持/.test(m)) {
    return "模型声明了不支持这个能力（例如参考图上限为 0）。";
  }
  if (/参考图必须是|data URI/.test(m)) {
    return "服务端还不支持 data: 参考图：升级到包含该改动的版本，或先把图传到公网 URL。";
  }
  if (/不在可用列表|产物类型/.test(m)) {
    return "模型不可用或类型不匹配：先 apiduo models 看当前 Key 能用的 id。";
  }
  if (/task_id/.test(m)) {
    return "task_id 无效或已过期（换过 Key？），重新提交任务。";
  }
  if (/key|unauthor|401/i.test(m)) {
    return "Key 被上游拒绝，检查渠道与额度。";
  }
  return "";
}

function httpHint(status) {
  if (status === 401 || status === 403) return "Key 无效或没有权限：重新 apiduo login。";
  if (status === 404) return "端点路径不对：确认是 …/mcp，而不是站点根路径。";
  if (status === 413) return "请求体过大：本地参考图会内联成 base64，单张建议 < 4MB、总量 < 5MB。";
  if (status === 429) return "被限流了，等一会儿再试。";
  return "";
}

export class McpClient {
  constructor({ endpoint, key, timeoutMs = 120_000 }) {
    this.endpoint = endpoint;
    this.key = key;
    this.timeoutMs = timeoutMs;
    this.seq = 0;
  }

  async request(method, params, opts = {}) {
    const id = ++this.seq;
    const body = { jsonrpc: "2.0", id, method };
    if (params !== undefined) body.params = params;

    const timeoutMs = opts.timeoutMs || this.timeoutMs;
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), timeoutMs);

    const started = Date.now();
    let res;
    let text = "";
    try {
      res = await fetch(this.endpoint, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Accept": "application/json, text/event-stream",
          "Authorization": `Bearer ${this.key}`,
          "User-Agent": `apiduo-cli/${VERSION} (node/${process.versions.node})`
        },
        body: JSON.stringify(body),
        signal: ac.signal
      });
      text = await res.text();
    } catch (err) {
      if (err && err.name === "AbortError") {
        throw timeoutError(
          `请求超时（${Math.round(timeoutMs / 1000)}s）`,
          "同步生成可能超过 2 分钟：用 --timeout 调大；视频请用 --no-wait 拿 task_id 再 task --wait"
        );
      }
      throw networkError(
        `请求没有发出去：${err && err.message ? err.message : err}`,
        "确认端点可访问、证书有效；跨域不是 CLI 的问题，但代理可能会吞掉带 Authorization 的请求"
      );
    } finally {
      clearTimeout(timer);
    }

    const elapsed = Date.now() - started;
    const ct = res.headers.get("content-type") || "";
    const data = parseBody(text, ct);

    if (!res.ok) {
      const msg = data && data.error && (data.error.message || data.error);
      const detail = typeof msg === "string" && msg ? msg : (data && data.__raw ? String(data.__raw).slice(0, 200) : `HTTP ${res.status}`);
      if (res.status === 401 || res.status === 403) {
        throw authError(`HTTP ${res.status} · ${detail}`, httpHint(res.status));
      }
      if (res.status === 404) {
        throw configError(`HTTP 404 · ${detail}`, httpHint(404));
      }
      throw toolError(`HTTP ${res.status} · ${detail}`, httpHint(res.status));
    }

    if (!data) throw toolError("服务返回了空响应", "确认端点真的是 MCP 服务。");
    if (data.error) {
      throw toolError(
        data.error.message || "JSON-RPC 错误",
        data.error.code === -32601 ? "服务没有这个方法：端点可能不是 MCP。" : ""
      );
    }

    if (opts.withMeta) {
      return { result: data.result, elapsed, status: res.status, contentType: ct };
    }
    return data.result;
  }

  /** tools/call 的薄封装：isError 也当失败，因为 CLI 的退出码只有一层。 */
  async tool(name, args) {
    const result = await this.request("tools/call", { name, arguments: args || {} });
    if (!result) throw toolError(`${name} 没有返回结果`);
    if (result.isError) {
      const msg = contentText(result) || `${name} 执行失败`;
      // 鉴权失败要单独成类（退出码 3）：调用方据此决定「换 key」还是「改参数」，
      // 混进 tool（4）里就只剩一句无差别的「失败了」。
      if (looksLikeAuth(msg)) {
        throw authError(msg, "Key 无效或过期：检查 APIDUO_API_KEY，或重新 apiduo login。");
      }
      throw toolError(msg, hintForTool(msg));
    }
    if (result.structuredContent && typeof result.structuredContent === "object") {
      return result.structuredContent;
    }
    // 不是本服务的形状时也不要假装成功：把文本带回去，至少能读。
    return { text: contentText(result) };
  }
}
