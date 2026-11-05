import http from "node:http";

/**
 * 一个只实现 CLI 需要的 MCP 子集的 mock：
 * initialize / tools/list / tools/call。
 * 每个请求都记下来，测试可以断言「CLI 到底发了什么」。
 */
export async function startMock(opts = {}) {
  const key = opts.key ?? "test-key";
  const calls = [];
  let taskPolls = 0;

  const models = opts.models ?? [{
    id: "mock-image",
    kind: "image",
    owned_by: "mock",
    execution: { task: "sync", transport: "generations" },
    input: { max_reference_images: 2 },
    parameters: { size: { enum: ["1024x1024", "1024x1536"], default: "1024x1024" } }
  }, {
    id: "mock-video",
    kind: "video",
    owned_by: "mock",
    execution: { task: "async", transport: "video_generations" },
    input: {},
    parameters: { duration: { enum: [5, 10], default: 5 } }
  }, {
    id: "mock-search",
    kind: "search",
    owned_by: "mock",
    execution: { task: "sync", transport: "search_query" },
    input: {},
    parameters: { count: { min: 1, max: 10, default: 3 } }
  }];

  const handlers = {
    "models.list": () => ({ models, count: models.length }),
    "image.generate": (args) => ({
      status: "completed",
      model: args.model || "mock-image",
      outputs: [{ url: "https://mock.example/1.png", mime_type: "image/png", width: 1024, height: 1024 }],
      usage: { images: 1 }
    }),
    "video.generate": () => ({
      status: "working",
      model: "mock-video",
      task_id: "task_1",
      poll_after_ms: 1200
    }),
    "task.get": () => {
      taskPolls += 1;
      if (taskPolls < 2) {
        return { status: "working", task_id: "task_1", model: "mock-video", poll_after_ms: 1200 };
      }
      return {
        status: "completed",
        task_id: "task_1",
        model: "mock-video",
        outputs: [{ url: "https://mock.example/v.mp4", mime_type: "video/mp4" }]
      };
    },
    "search.query": (args) => ({
      model: args.model || "mock-search",
      results: [{ title: "Mock result", url: "https://mock.example/a", snippet: "hello" }],
      count: 1
    }),
    ...(opts.handlers || {})
  };

  const server = http.createServer(async (req, res) => {
    let raw = "";
    for await (const chunk of req) raw += chunk;
    let body;
    try { body = JSON.parse(raw); } catch { body = null; }
    calls.push({ headers: req.headers, body });

    const send = (code, obj) => {
      const text = JSON.stringify(obj);
      res.writeHead(code, { "content-type": "application/json", "content-length": Buffer.byteLength(text) });
      res.end(text);
    };

    if (!body) return send(400, { error: { message: "bad json" } });
    if (req.headers.authorization !== `Bearer ${key}`) {
      return send(401, { error: { message: "unauthorized" } });
    }

    const id = body.id;
    if (body.method === "initialize") {
      return send(200, {
        jsonrpc: "2.0", id,
        result: {
          protocolVersion: "2025-06-18",
          capabilities: { tools: { listChanged: false } },
          serverInfo: { name: "mock-mcp", version: "9.9.9" }
        }
      });
    }
    if (body.method === "tools/list") {
      return send(200, {
        jsonrpc: "2.0", id,
        result: { tools: Object.keys(handlers).map((name) => ({ name })) }
      });
    }
    if (body.method === "tools/call") {
      const name = body.params && body.params.name;
      const args = (body.params && body.params.arguments) || {};
      if (opts.failTool === name) {
        return send(200, {
          jsonrpc: "2.0", id,
          result: { content: [{ type: "text", text: opts.failMessage || "tool failed" }], isError: true }
        });
      }
      const fn = handlers[name];
      if (!fn) return send(200, { jsonrpc: "2.0", id, error: { code: -32601, message: `unknown tool ${name}` } });
      return send(200, {
        jsonrpc: "2.0", id,
        result: {
          content: [{ type: "text", text: "ok" }],
          structuredContent: await fn(args)
        }
      });
    }
    return send(200, { jsonrpc: "2.0", id, error: { code: -32601, message: `unknown method ${body.method}` } });
  });

  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}/mcp`,
    calls,
    close: () => new Promise((resolve) => server.close(resolve))
  };
}
