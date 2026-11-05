import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { parseArgs } from "../src/args.mjs";
import { parseBody } from "../src/rpc.mjs";
import { prepareRefs } from "../src/files.mjs";
import { maskKey, saveConfig } from "../src/config.mjs";
import { looksLikeAuth } from "../src/errors.mjs";
import { CliError } from "../src/errors.mjs";

const spec = {
  defaults: { wait: true },
  flags: { wait: "bool", n: "number", ref: "list", model: "string" }
};

test("parseArgs: 重复 --ref、数字、--no- 与位置参数", () => {
  const { flags, pos } = parseArgs(
    ["一只柴犬", "--no-wait", "--n", "2", "--ref", "a.png", "--ref=b.png", "--model", "m1"],
    spec
  );
  assert.equal(flags.wait, false);
  assert.equal(flags.n, 2);
  assert.deepEqual(flags.ref, ["a.png", "b.png"]);
  assert.equal(flags.model, "m1");
  assert.deepEqual(pos, ["一只柴犬"]);
});

test("parseArgs: 未知参数 / 缺值 / 非数字都报 usage", () => {
  for (const argv of [["--wat"], ["--n"], ["--n", "abc"]]) {
    assert.throws(() => parseArgs(argv, spec), (err) => err instanceof CliError && err.kind === "usage" && err.exitCode === 2);
  }
});

test("parseBody: JSON 与 SSE 都能解", () => {
  assert.deepEqual(parseBody('{"jsonrpc":"2.0","id":1,"result":{"ok":true}}', "application/json").result, { ok: true });
  const sse = 'event: message\ndata: {"jsonrpc":"2.0","id":2,"result":{"ok":1}}\n\n';
  assert.equal(parseBody(sse, "text/event-stream").result.ok, 1);
});

test("prepareRefs: 本地图内联成 data URI，URL 原样透传", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "apiduo-ref-"));
  const png = path.join(dir, "a.png");
  const pngBytes = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII=",
    "base64"
  );
  fs.writeFileSync(png, pngBytes);

  const r = prepareRefs([png, "https://example.com/x.png", "data:image/png;base64,aGk="]);
  assert.equal(r.values.length, 3);
  assert.match(r.values[0], /^data:image\/png;base64,/);
  assert.equal(r.values[1], "https://example.com/x.png");
  assert.equal(r.values[2], "data:image/png;base64,aGk=");
  assert.equal(r.inlined.length, 1);
  assert.equal(r.totalBytes, pngBytes.length);

  assert.throws(() => prepareRefs([path.join(dir, "nope.png")]), /不存在/);
  const txt = path.join(dir, "a.txt");
  fs.writeFileSync(txt, "hi");
  assert.throws(() => prepareRefs([txt]), /不支持的图片格式/);
  assert.throws(() => prepareRefs([png], { maxFileBytes: 1 }), /过大/);
  assert.throws(() => prepareRefs([png, png], { maxTotalBytes: 1 }), /总量过大/);
});

test("prepareRefs: 单张与总量上限都必须生效", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "apiduo-cap-"));

  // 单张 4MB + 1 字节：必须在这里就拒，不能等 nginx 回 413
  const one = path.join(dir, "big.png");
  fs.writeFileSync(one, Buffer.alloc(4 * 1024 * 1024 + 1));
  assert.throws(
    () => prepareRefs([one]),
    (err) => err instanceof CliError && err.kind === "usage" && /单张上限/.test(err.hint || "")
  );

  // 每张都合规、但加起来超 5MB：base64 膨胀后会把 8MB 请求体撑爆
  const half = path.join(dir, "half.png");
  fs.writeFileSync(half, Buffer.alloc(3 * 1024 * 1024));
  assert.throws(
    () => prepareRefs([half, half]),
    (err) => err instanceof CliError && err.kind === "usage" && /总量/.test(err.message)
  );

  // 边界内必须放行：刚好 4MB 不该被误杀
  const edge = path.join(dir, "edge.png");
  fs.writeFileSync(edge, Buffer.alloc(4 * 1024 * 1024));
  const ok = prepareRefs([edge]);
  assert.equal(ok.values.length, 1);
  assert.ok(ok.values[0].startsWith("data:image/png;base64,"));
});

test("maskKey: 短 key 全掩，长 key 才露首尾", () => {
  assert.equal(maskKey(""), "—");
  assert.equal(maskKey(undefined), "—");

  // 11 字符按「前 5 后 4」会露 9 个 —— 等于明文。短 key 一律全掩。
  for (const k of ["short", "1234567890", "12345678901", "123456789012345"]) {
    assert.equal(maskKey(k), "••••••", `${k}（${k.length} 字符）应全掩`);
  }

  const long = "sk-abcdefghijklmnopqrstuvwxyz012345";
  const masked = maskKey(long);
  assert.equal(masked, "sk-a••••••2345");
  assert.ok(!masked.includes("defghij"), "中间段不能出现");
});

test("saveConfig: 文件 0600、目录 0700，内容可读回", (t) => {
  const dir = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "apiduo-cfg-")), "apiduo");
  const prev = process.env.APIDUO_CONFIG_DIR;
  process.env.APIDUO_CONFIG_DIR = dir;
  t.after(() => {
    if (prev === undefined) delete process.env.APIDUO_CONFIG_DIR;
    else process.env.APIDUO_CONFIG_DIR = prev;
  });

  const file = saveConfig({ endpoint: "https://mcp.apiduo.cn/mcp", apiKey: "sk-test-1234567890abcdef" });
  assert.equal(file, path.join(dir, "config.json"));
  assert.equal(fs.statSync(file).mode & 0o777, 0o600, "配置文件必须是 0600");
  assert.equal(fs.statSync(dir).mode & 0o777, 0o700, "目录必须是 0700");

  const back = JSON.parse(fs.readFileSync(file, "utf8"));
  assert.equal(back.apiKey, "sk-test-1234567890abcdef");
  assert.equal(back.endpoint, "https://mcp.apiduo.cn/mcp");
});

test("parseArgs: -- 之后一律当位置参数，不再解析 flag", () => {
  const { flags, pos } = parseArgs(["--n", "2", "--", "--no-wait", "-x"], spec);
  assert.equal(flags.n, 2);
  assert.equal(flags.wait, true, "-- 之后的 --no-wait 不该生效");
  assert.deepEqual(pos, ["--no-wait", "-x"]);
});

test("looksLikeAuth: 认得出鉴权失败，也不会把参数错误误判成鉴权", () => {
  // 上游 401 被包在 JSON-RPC 里返回（HTTP 200），只能从文本判
  for (const m of [
    "upstream 401 (GET /models): Invalid token (request id: 2026...)",
    "403 Forbidden",
    "Incorrect API key provided: sk-***",
    "Unauthorized: invalid api-key",
    "token 已过期",
    "鉴权失败"
  ]) {
    assert.equal(looksLikeAuth(m), true, `应判为鉴权: ${m}`);
  }

  // 这些含 key / 数字，但不是鉴权 —— 误判会让脚本不去改参数而是换 key
  for (const m of [
    "模型 m 未声明参数 key，可用参数：size, n",
    "n 不能大于 8",
    "upstream 429: rate limit exceeded",
    "模型 m 不支持参考图（垫图上限 0）",
    "生成超时，task_id 仍可查询"
  ]) {
    assert.equal(looksLikeAuth(m), false, `不该判为鉴权: ${m}`);
  }
});
