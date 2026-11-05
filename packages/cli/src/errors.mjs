/**
 * 预期内的失败：有 kind / hint / exitCode，CLI 只在最外层统一格式化。
 *
 * 退出码约定（agent 和脚本靠它判断，不靠 stderr 文本）：
 *   0 成功 · 2 用法 · 3 配置/鉴权 · 4 工具或上游拒绝 · 5 网络/超时 · 1 程序缺陷
 */
export class CliError extends Error {
  constructor(kind, message, hint = "", exitCode = 4) {
    super(message);
    this.name = "CliError";
    this.kind = kind;
    this.hint = hint;
    this.exitCode = exitCode;
  }
}

export const usageError = (message, hint = "") => new CliError("usage", message, hint, 2);
export const configError = (message, hint = "") => new CliError("config", message, hint, 3);
export const authError = (message, hint = "") => new CliError("auth", message, hint, 3);
export const toolError = (message, hint = "") => new CliError("tool", message, hint, 4);
export const networkError = (message, hint = "") => new CliError("network", message, hint, 5);
export const timeoutError = (message, hint = "") => new CliError("timeout", message, hint, 5);

/**
 * 上游 401/403 会被包进 JSON-RPC 的 isError 返回（HTTP 仍是 200），
 * 所以「是不是鉴权失败」只能从文本判。判错的代价是脚本层面的：
 * 把「key 不对」当成「参数不对」去重试，永远不会成功。
 *
 * 只认明确的措辞，避免把「未声明参数 key」这类文案误判成鉴权问题。
 */
const AUTH_RE = /\b40[13]\b|unauthori[sz]ed|forbidden|invalid[ _-]?(token|api[ _-]?key)|incorrect api key|invalid token|token\s*已?(无效|过期|失效)|鉴权失败|未授权/i;

export function looksLikeAuth(message) {
  return AUTH_RE.test(String(message || ""));
}
