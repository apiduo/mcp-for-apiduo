#!/usr/bin/env python3
"""极简 mock 上游：只实现 MCP 服务需要的三个端点。"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = {
    "data": [
        {
            "id": "doubao-seedream-mock",
            "object": "model",
            "created": 1762000000,
            "owned_by": "bytedance",
            "kind": "image",
            "execution": {"task": "sync", "transport": "generations"},
            "input": {"max_reference_images": 2},
            "parameters": {
                "size": {"enum": ["1K", "2K"], "default": "2K"},
                "n": {"min": 1, "max": 4, "default": 1},
            },
        },
        {
            "id": "doubao-seedance-mock",
            "object": "model",
            "created": 1745000000,
            "owned_by": "bytedance",
            "kind": "video",
            "execution": {"task": "async", "transport": "video_generations"},
            "input": {"max_reference_images": 1},
            "parameters": {"duration": {"enum": [5, 10], "default": 5}},
        },
        {
            "id": "gpt-image-edits-mock",
            "object": "model",
            "created": 1762000000,
            "owned_by": "openai",
            "kind": "image",
            "execution": {"task": "sync", "transport": "edits_multipart"},
            "input": {"max_reference_images": 4},
            "parameters": {"size": {"enum": ["1024x1024"], "default": "1024x1024"}},
        },
        {
            "id": "doubao-seedance-ark-mock",
            "object": "model",
            "created": 1745000000,
            "owned_by": "bytedance",
            "kind": "video",
            "execution": {"task": "async", "transport": "contents_tasks"},
            "input": {},
            "parameters": {"duration": {"enum": [5], "default": 5}},
        },
        {
            "id": "volcengine-search-mock",
            "object": "model",
            "created": 1762000000,
            "owned_by": "volcengine",
            "kind": "search",
            "execution": {"task": "sync", "transport": "search_query"},
            "input": {},
            "parameters": {
                "count": {"min": 1, "max": 50, "default": 10},
                "include_content": {"enum": [True, False], "default": False},
            },
        },
    ]
}


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("[mock] " + (fmt % args) + "\n")

    def _json(self, code, obj):
        b = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/v1/models":
            self._json(200, MODELS)
        elif self.path.startswith("/v1/contents/generations/tasks/"):
            # 火山的任务查询。产物藏在 content 对象里，不是数组 —— 这正是
            # parseOutputs 最容易漏的形状。
            key = self.path.rsplit("/", 1)[-1]
            if key == "ctask_777":
                self._json(200, {
                    "id": "ctask_777",
                    "model": "doubao-seedance-ark-mock",
                    "status": "succeeded",
                    "content": {"video_url": "https://mock.local/ark-v.mp4"},
                    "usage": {"completion_tokens": 108900},
                })
            else:
                self._json(404, {"error": {"code": "ResourceNotFound", "message": "resource not found"}})
        elif self.path.startswith("/v1/tasks/"):
            key = self.path.rsplit("/", 1)[-1]
            if key == "vtask_123":
                self._json(200, {
                    "status": "completed",
                    "videos": [{"url": "https://mock.local/v.mp4"}],
                })
            else:
                self._json(404, {"error": {"message": "task not found"}})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}

        if not (self.headers.get("Authorization") or "").startswith("Bearer "):
            self._json(401, {"error": {"message": "missing api key", "code": "invalid_api_key"}})
            return

        if self.path == "/v1/images/generations":
            if body.get("response_format") != "url":
                self._json(400, {"error": {"message": "response_format must be url"}})
                return
            self._json(200, {
                "created": 1762000000,
                "data": [{"url": "https://mock.local/img-1.png", "revised_prompt": body.get("prompt")}],
                "usage": {"images": 1},
            })
        elif self.path == "/v1/videos/generations":
            self._json(200, {"task_id": "vtask_123", "status": "running"})
        elif self.path == "/v1/images/edits":
            # mock 不做完整 multipart 解析，只验证「参考图确实作为文件 part 上传」
            ctype = self.headers.get("Content-Type", "")
            if not ctype.startswith("multipart/form-data"):
                self._json(400, {"error": {"message": "edits 需要 multipart/form-data，收到 " + ctype}})
                return
            n_img = raw.count(b'name="image')
            if n_img == 0:
                self._json(400, {"error": {"message": "缺少参考图文件"}})
                return
            self._json(200, {
                "created": 1762000000,
                "data": [{"url": "https://mock.local/edited-%d.png" % n_img}],
                "usage": {"images": 1, "input_images": n_img},
            })
        elif self.path == "/v1/contents/generations/tasks":
            # 火山的形状：model + content 数组，没有 prompt / image 字段。
            # mock 严格按这个校验，才能证明协议族的映射真的发生了。
            if not body.get("model"):
                self._json(400, {"error": {"code": "MissingParameter", "message": "missing `model` parameter"}})
                return
            content = body.get("content")
            if not isinstance(content, list) or not content:
                self._json(400, {"error": {"code": "MissingParameter", "message": "missing `content` parameter"}})
                return
            text = next((c.get("text") for c in content if c.get("type") == "text"), None)
            if not text:
                self._json(400, {"error": {"code": "MissingParameter", "message": "content 缺少 type=text 项"}})
                return
            imgs = [c for c in content if c.get("type") == "image_url"]
            for c in imgs:
                if not (c.get("image_url") or {}).get("url"):
                    self._json(400, {"error": {"code": "InvalidParameter", "message": "image_url.url 为空"}})
                    return
            if body.get("prompt") is not None:
                self._json(400, {"error": {"code": "InvalidParameter", "message": "该端点不接受 prompt 字段"}})
                return
            self._json(200, {
                "id": "ctask_777",
                "status": "queued",
                "_echo": {"text": text, "images": len(imgs), "duration": body.get("duration")},
            })
        elif self.path == "/v1/tools/search":
            self._json(200, {"results": [
                {"title": "示例结果", "url": "https://mock.local/r1", "summary": "摘要", "authority": 3},
            ]})
        else:
            self._json(404, {"error": {"message": "not found: " + self.path}})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 3999
    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
