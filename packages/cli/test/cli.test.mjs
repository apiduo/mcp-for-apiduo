import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { startMock } from "./mock.mjs";

const exec = promisify(execFile);
const BIN = fileURLToPath(new URL("../bin/apiduo.mjs", import.meta.url));

function workdir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "apiduo-cli-"));
}

async function runCli(args, { url, key = "test-key", dir = workdir() } = {}) {
  const env = {
    ...process.env,
    NO_COLOR: "1",
    APIDUO_ENDPOINT: url,
    APIDUO_API_KEY: key,
    APIDUO_CONFIG_DIR: dir
  };
  try {
    const { stdout, stderr } = await exec(process.execPath, [BIN, ...args], {
      env, timeout: 20_000, maxBuffer: 4 * 1024 * 1024
    });
    return { code: 0, stdout, stderr };
  } catch (err) {
    return { code: err.code ?? 1, stdout: err.stdout || "", stderr: err.stderr || "" };
  }
}

test("models --json：带鉴权、输出结构化模型", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["models", "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.ok, true);
  assert.equal(out.command, "models");
  assert.equal(out.models.length, 3);
  assert.equal(out.models.find((m) => m.kind === "video").id, "mock-video");

  const call = mock.calls.find((c) => c.body && c.body.method === "tools/call");
  assert.equal(call.body.params.name, "models.list");
  assert.equal(call.headers.authorization, "Bearer test-key");
});

test("image：位置参数、--n、本地参考图内联", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());
  const dir = workdir();
  const png = path.join(dir, "a.png");
  fs.writeFileSync(png, Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII=",
    "base64"
  ));

  const r = await runCli(["image", "把背景换成海边", "--ref", png, "--n", "2", "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.result.outputs[0].url, "https://mock.example/1.png");
  assert.equal(out.meta.model, "mock-image");
  assert.match(r.stderr, /已内联 1 张本地图/);

  const call = mock.calls.find((c) => c.body?.params?.name === "image.generate");
  assert.equal(call.body.params.arguments.prompt, "把背景换成海边");
  assert.equal(call.body.params.arguments.n, 2);
  assert.match(call.body.params.arguments.reference_images[0], /^data:image\/png;base64,/);
});

test("video --wait：提交后轮询到 completed", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["video", "海浪慢镜头", "--yes", "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.result.status, "completed");
  assert.equal(out.result.outputs[0].url, "https://mock.example/v.mp4");
  assert.ok(out.meta.polls >= 2, "至少轮询两次");

  const call = mock.calls.find((c) => c.body?.params?.name === "video.generate");
  assert.equal(call.body.params.arguments.prompt, "海浪慢镜头");
});

test("video --no-wait：只返回 task_id，不轮询", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["video", "海浪慢镜头", "--yes", "--no-wait", "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.result.status, "working");
  assert.equal(out.meta.task_id, "task_1");
  assert.equal(mock.calls.filter((c) => c.body?.params?.name === "task.get").length, 0);
});

test("video 非交互又没 --yes：拒绝提交，退出码 2", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["video", "海浪慢镜头", "--json"], { url: mock.url });
  assert.equal(r.code, 2);
  const out = JSON.parse(r.stdout);
  assert.equal(out.ok, false);
  assert.equal(out.error.kind, "usage");
  assert.match(out.error.hint, /--yes/);
  assert.equal(mock.calls.filter((c) => c.body?.params?.name === "video.generate").length, 0);
});

test("没有 Key：配置错误，退出码 3", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["models", "--json"], { url: mock.url, key: "" });
  assert.equal(r.code, 3);
  const out = JSON.parse(r.stdout);
  assert.equal(out.error.kind, "config");
});

test("工具拒绝（isError）：退出码 4，hint 对应错误", async (t) => {
  const mock = await startMock({
    failTool: "image.generate",
    failMessage: "模型 mock-image 未声明 n 的上限，单次最多 8"
  });
  t.after(() => mock.close());

  const r = await runCli(["image", "hi", "--json"], { url: mock.url });
  assert.equal(r.code, 4);
  const out = JSON.parse(r.stdout);
  assert.equal(out.error.kind, "tool");
  assert.match(out.error.message, /单次最多 8/);
  assert.match(out.error.hint, /8/);
});

test("task --wait：直接查询并轮询", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  const r = await runCli(["task", "task_1", "--wait", "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  const out = JSON.parse(r.stdout);
  assert.equal(out.result.status, "completed");
  assert.ok(out.meta.polls >= 2);
});

const PNG_DATA = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII=";

test("image --out：产物落盘，--json 里给出本地路径", async (t) => {
  const mock = await startMock({
    handlers: {
      "image.generate": (args) => ({
        status: "completed",
        model: args.model || "mock-image",
        outputs: [
          { url: PNG_DATA, mime_type: "image/png" },
          { url: PNG_DATA, mime_type: "image/png" }
        ],
        usage: { images: 2 }
      })
    }
  });
  t.after(() => mock.close());

  const outDir = path.join(workdir(), "out") + path.sep;
  const r = await runCli(["image", "两张", "--n", "2", "--out", outDir, "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);

  const j = JSON.parse(r.stdout);
  assert.equal(j.meta.saved.length, 2);
  for (const f of j.meta.saved) {
    assert.ok(fs.existsSync(f.path), `应存在 ${f.path}`);
    assert.ok(f.bytes > 0);
  }
  assert.equal(fs.readdirSync(outDir).length, 2);
});

test("image --out：人类可读输出只打本地路径，不再铺长 URL", async (t) => {
  const mock = await startMock({
    handlers: {
      "image.generate": () => ({
        status: "completed",
        model: "mock-image",
        outputs: [{ url: PNG_DATA, mime_type: "image/png" }]
      })
    }
  });
  t.after(() => mock.close());

  const target = path.join(workdir(), "cover.png");
  const r = await runCli(["image", "一张", "--out", target], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);
  assert.ok(r.stdout.includes(target), r.stdout);
  assert.ok(!r.stdout.includes("base64"), "终端里不该出现内联数据");
});

test("image --out：多个产物只能给目录，给文件路径退出码 2", async (t) => {
  const mock = await startMock({
    handlers: {
      "image.generate": () => ({
        status: "completed",
        model: "mock-image",
        outputs: [{ url: PNG_DATA }, { url: PNG_DATA }]
      })
    }
  });
  t.after(() => mock.close());

  const target = path.join(workdir(), "a.png");
  const r = await runCli(["image", "两张", "--out", target, "--json"], { url: mock.url });
  assert.equal(r.code, 2);
  assert.equal(JSON.parse(r.stdout).error.kind, "usage");
});

test("image --out：落盘失败进 save_failed，退出码仍是 0", async (t) => {
  const mock = await startMock({
    handlers: {
      // 127.0.0.1:1 立刻 ECONNREFUSED，不会拖时间
      "image.generate": () => ({
        status: "completed",
        model: "mock-image",
        outputs: [{ url: "http://127.0.0.1:1/nope.png", mime_type: "image/png" }]
      })
    }
  });
  t.after(() => mock.close());

  const outDir = path.join(workdir(), "out") + path.sep;
  const r = await runCli(["image", "x", "--out", outDir, "--json"], { url: mock.url });
  assert.equal(r.code, 0, r.stderr);

  const j = JSON.parse(r.stdout);
  assert.equal(j.meta.saved, undefined, "没成功落盘就不该出现 saved");
  assert.equal(j.meta.save_failed.length, 1);
  assert.ok(j.meta.save_failed[0].reason, "失败要给出原因，否则脚本没法重试");
  assert.equal(fs.existsSync(outDir), false, "失败时不该留下空目录");
});

test("video --wait 超时：退出码 5，且能看出是等超时而不是上游失败", async (t) => {
  const mock = await startMock();
  t.after(() => mock.close());

  // mock 的 task.get 要 1.2s 才转 completed，这里只等 1 秒
  const r = await runCli(["video", "x", "--yes", "--wait", "--wait-timeout", "1", "--json"], { url: mock.url });
  assert.equal(r.code, 5, r.stdout + r.stderr);
  const j = JSON.parse(r.stdout);
  assert.equal(j.ok, false);
  assert.equal(j.error.kind, "timeout");
});

test("image --ref 本地图：老网关的报错要翻成升级提示，没内联时不该乱加", async (t) => {
  const mock = await startMock({
    failTool: "image.generate",
    failMessage: "参考图必须是 http/https URL"
  });
  t.after(() => mock.close());

  const png = path.join(workdir(), "a.png");
  fs.writeFileSync(png, Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z8WQAAAAASUVORK5CYII=",
    "base64"
  ));

  const withRef = await runCli(["image", "x", "--ref", png, "--json"], { url: mock.url });
  assert.equal(withRef.code, 4);
  assert.match(JSON.parse(withRef.stdout).error.hint || "", /data:image|升级/);

  // 没传参考图时，同一条上游报错不该被讲成「版本问题」
  const withoutRef = await runCli(["image", "x", "--json"], { url: mock.url });
  assert.doesNotMatch(JSON.parse(withoutRef.stdout).error.hint || "", /data:image/);
});
