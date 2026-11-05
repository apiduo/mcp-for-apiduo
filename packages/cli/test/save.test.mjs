import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { saveOutputs, stampOf } from "../src/files.mjs";
import { CliError } from "../src/errors.mjs";

// 1×1 PNG：只看落盘行为，不关心像素
const PNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII=";
const MP4 = "data:video/mp4;base64,aGVsbG8=";

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), "apiduo-save-"));

test("--out 目录：多产物自动命名并带序号", async () => {
  const dir = tmp();
  const r = await saveOutputs(
    [{ url: PNG, mime_type: "image/png" }, { url: PNG, mime_type: "image/png" }],
    dir,
    { model: "gpt-image-1", stamp: "t1" }
  );
  assert.equal(r.saved.length, 2);
  assert.equal(r.failed.length, 0);
  assert.equal(path.basename(r.saved[0].path), "gpt-image-1-t1-1.png");
  assert.equal(path.basename(r.saved[1].path), "gpt-image-1-t1-2.png");
  for (const f of r.saved) {
    assert.ok(fs.statSync(f.path).size > 0);
    assert.equal(f.mime_type, "image/png");
  }
});

test("--out 目录：末尾斜杠即使目录不存在也按目录处理", async () => {
  const base = tmp();
  const target = path.join(base, "deep", "out") + path.sep;
  const r = await saveOutputs([{ url: MP4, mime_type: "video/mp4" }], target, { model: "seedance", stamp: "t2" });
  assert.equal(r.saved.length, 1);
  assert.ok(r.saved[0].path.startsWith(path.join(base, "deep", "out")));
  assert.ok(r.saved[0].path.endsWith(".mp4"));
});

test("--out 文件：单产物用它命名，且覆盖同名（用户点了名）", async () => {
  const dir = tmp();
  const target = path.join(dir, "cover.png");
  fs.writeFileSync(target, "old");

  const r = await saveOutputs([{ url: PNG, mime_type: "image/png" }], target, { stamp: "t3" });
  assert.equal(r.saved.length, 1);
  assert.equal(r.saved[0].path, target);
  assert.ok(fs.statSync(target).size > 3);
});

test("--out 文件：多个产物必须报用法错误，而不是悄悄只存一个", async () => {
  const dir = tmp();
  await assert.rejects(
    saveOutputs([{ url: PNG }, { url: PNG }], path.join(dir, "a.png"), {}),
    (err) => err instanceof CliError && err.kind === "usage" && err.exitCode === 2
  );
});

test("--out 目录：重跑不覆盖，自动加序号", async () => {
  const dir = tmp();
  const first = await saveOutputs([{ url: PNG, mime_type: "image/png" }], dir, { model: "m", stamp: "t4" });
  const second = await saveOutputs([{ url: PNG, mime_type: "image/png" }], dir, { model: "m", stamp: "t4" });

  assert.notEqual(first.saved[0].path, second.saved[0].path);
  assert.equal(path.basename(second.saved[0].path), "m-t4-2.png");
  assert.equal(fs.readdirSync(dir).length, 2);
});

test("不支持的产物地址进 failed，不影响其它产物", async () => {
  const dir = tmp();
  const r = await saveOutputs(
    [{ url: "file:///etc/passwd" }, { url: PNG, mime_type: "image/png" }],
    dir,
    { model: "m", stamp: "t5" }
  );
  assert.equal(r.saved.length, 1);
  assert.equal(r.failed.length, 1);
  assert.match(r.failed[0].reason, /不支持的产物地址/);
  assert.ok(!fs.existsSync("/etc/passwd.copy"));
});

test("没有产物 / 没有 --out 都报用法错误", async () => {
  const dir = tmp();
  for (const args of [[[], dir], [[{ url: PNG }], ""]]) {
    await assert.rejects(
      saveOutputs(args[0], args[1], {}),
      (err) => err instanceof CliError && err.kind === "usage"
    );
  }
});

test("stampOf 优先 task_id，退化到上游 id，再退化到空", () => {
  assert.equal(stampOf({ task_id: "task_abcdef123456" }), "task_abcdef1");
  assert.equal(stampOf({ upstream: { id: "1234567890abcdef" } }), "1234567890ab");
  assert.equal(stampOf({}), "");
});

test("http 产物：按 content-type 定扩展名并在失败时给出原因", async () => {
  const http = await import("node:http");
  const server = http.createServer((req, res) => {
    if (req.url === "/ok") {
      res.writeHead(200, { "content-type": "image/webp" });
      res.end(Buffer.from("RIFFxxxxWEBP"));
      return;
    }
    res.writeHead(404).end("nope");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;
  try {
    const dir = tmp();
    const r = await saveOutputs(
      [{ url: `${base}/ok` }, { url: `${base}/missing` }],
      dir,
      { model: "m", stamp: "t6" }
    );
    assert.equal(r.saved.length, 1);
    assert.ok(r.saved[0].path.endsWith(".webp"), r.saved[0].path);
    assert.equal(r.saved[0].mime_type, "image/webp");
    assert.equal(r.failed.length, 1);
    assert.match(r.failed[0].reason, /HTTP 404/);
  } finally {
    await new Promise((r) => server.close(r));
  }
});

test("data URI 的 ;base64 标记必须收尾，不能被别处的 base64 字样骗到", async () => {
  const dir = tmp();
  // 参数里出现 base64，但内容其实是百分号转义的明文
  const r = await saveOutputs([{ url: "data:image/png;name=base64x,hello" }], dir, { model: "m", stamp: "t7" });
  assert.equal(r.saved.length, 1);
  assert.equal(fs.readFileSync(r.saved[0].path, "utf8"), "hello");
});

test("自动命名的时间戳是本地时间，不是 UTC", async () => {
  const dir = tmp();
  const before = Date.now();
  // 不传 stamp，走 localStamp
  const r = await saveOutputs([{ url: PNG, mime_type: "image/png" }], dir, { model: "m" });

  const base = path.basename(r.saved[0].path);
  const m = base.match(/^m-(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})\.png$/);
  assert.ok(m, `文件名要带 YYYYMMDD-HHMMSS 时间戳: ${base}`);

  const t = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6]).getTime();
  // 用 UTC 的话会差 8 小时，这条断言就是那次修复的护栏
  assert.ok(Math.abs(t - before) < 120_000, `时间戳与本地钟差 ${Math.round((t - before) / 1000)} 秒`);
});

test("产物下载超时进 failed，不会拖住整条命令", async () => {
  const http = await import("node:http");
  const server = http.createServer(() => {}); // 永不响应
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${server.address().port}/slow`;
  try {
    const t0 = Date.now();
    const r = await saveOutputs([{ url }], tmp(), { model: "m", stamp: "t8", timeoutMs: 200 });
    assert.equal(r.saved.length, 0);
    assert.equal(r.failed.length, 1);
    assert.ok(Date.now() - t0 < 5000, "应在超时后立刻返回");
  } finally {
    server.closeAllConnections?.();
    await new Promise((r) => server.close(r));
  }
});
