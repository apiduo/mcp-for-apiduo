# apiduo CLI

apiduo MCP 的命令行客户端：模型发现、图片 / 视频生成、检索、异步任务轮询。

零依赖，Node 18+。它**只连 `/mcp`**，不绕过网关 —— 参数校验、覆盖层、计费边界
与 playground、Claude、Cursor 完全同源。

## 安装

```bash
# 发布后
npm i -g @apiduo/cli

# 仓库内直接用
node packages/cli/bin/apiduo.mjs --help

# 或者软链到全局
npm link ./packages/cli
```

## 登录

```bash
apiduo login --endpoint https://mcp.apiduo.cn/mcp
# API Key 在终端里隐藏输入，三步握手全部通过才写入配置
```

配置写在 `~/.config/apiduo/config.json`（目录 0700、文件 0600），
可以用 `APIDUO_CONFIG_DIR` 改位置。优先级：

```
--endpoint / --key  >  APIDUO_ENDPOINT / APIDUO_API_KEY  >  配置文件  >  默认端点
```

```bash
apiduo config          # 看路径、端点来源、Key 掩码
apiduo config path     # 只打印路径，给脚本用
apiduo whoami          # 服务名/版本、工具、模型计数，不写配置
```

## 命令

```bash
apiduo models [--kind image|video|search]
apiduo image  "一只戴宇航头盔的柴犬" --size 1024x1536 --n 2
apiduo image  "把背景换成海边" --ref ./photo.png --model gpt-image-1
apiduo video  "海浪拍打礁石，慢镜头" --duration 5 --wait
apiduo video  "..." --no-wait           # 只拿 task_id
apiduo search "MCP 协议 2026 有什么更新" --count 5
apiduo task   <task_id> --wait
apiduo image  "竖版海报，两张" --n 2 --out ./out/     # 产物直接落到本地
```

`models --json` 是查参数的唯一入口：返回每个模型的 `execution`、`input`
和完整的 `parameters`（enum / min / max / default）。CLI 不在本地发明默认值，
模型没声明 `size` 就允许留空，交给上游。

生成类命令的公共 flags：

| flag | 说明 |
|---|---|
| `--prompt <文本>` | 也可用位置参数；`-` 从 stdin 读 |
| `--model <id>` | 留空用该类型的默认模型 |
| `--size` / `--n` / `--quality` | 取值以模型画像为准 |
| `--ref <URL\|路径>` | 可重复；本地图会内联成 data URI |
| `--extra '<JSON>'` | 模型声明的其它参数，例如 `{"aspect_ratio":"16:9"}` |
| `--wait` / `--no-wait` | 异步模型默认等待；`--no-wait` 只返回 task_id |
| `--yes` | 视频提交的计费确认；非交互环境必须显式加 |
| `--out <目录\|文件>` | 把产物下载到本地；目录以 `/` 结尾或已存在时按目录处理 |

## JSON 输出与退出码

`--json` 时 **stdout 只有一个 JSON**，进度、警告、错误提示全部走 stderr：

```json
{
  "ok": true,
  "command": "image",
  "result": { "status": "completed", "outputs": [{ "url": "https://…" }] },
  "meta": { "model": "gpt-image-2", "elapsed_ms": 1234 }
}
```

失败时也是 JSON，只是 `ok:false`：

```json
{ "ok": false, "command": "image", "error": { "kind": "tool", "message": "…", "hint": "…" } }
```

| 退出码 | 含义 |
|---|---|
| 0 | 成功 |
| 2 | 用法错误（参数、缺提示词、非交互没 `--yes`） |
| 3 | 配置 / 鉴权（没登录、Key 无效） |
| 4 | 工具或上游拒绝（模型不支持、参数越界、额度不足） |
| 5 | 网络 / 超时 |
| 1 | 程序缺陷 |

> 上游的 401/403 会被网关包进 JSON-RPC 的 `isError` 返回（HTTP 仍是 200），
> 所以「鉴权失败」是从错误文本判的（`401` / `403` / `unauthorized` / `invalid token` / `鉴权失败`…）。
> 极端情况下，一条措辞特殊的参数错误可能被归到 3；脚本要更精确就解析 `--json` 的 `error.kind`。

## 本地参考图

`--ref ./photo.png` 会被读成 `data:image/png;base64,…` 塞进 `reference_images`。
服务端在 `upstream/fetch.go` 里只解码、不联网，仍然限 `image/*` 与 20 MiB。

客户端自己先卡一道：**单张 ≤ 4 MB、总量 ≤ 5 MB**。原因是 base64 会膨胀 1/3，
公网入口 nginx 的请求体上限是 8 MB。更大的图请先压缩，或改用公网 URL。

服务端需要是包含 `data:image` 支持的版本；老版本会回
「参考图必须是 http/https」，CLI 会把这条错误翻成升级提示。

> `edits_multipart` 模型（如 gpt-image 系列垫图）最稳；`generations` 或视频
> 走 data URI 时，取决于上游是否接受，CLI 不做额外保证。

## 产物落盘

网关只把产物 URL 交出来（图片是签名链接，视频是 mp4）。把它们变成磁盘上的文件，
是 CLI 相对 `/mcp` 唯一「多做」的一类事 —— 服务端写不到用户的磁盘。

```bash
apiduo image "两张海报" --n 2 --out ./out/        # → out/gpt-image-2-20261105-030530-1.png
apiduo image "一张" --out ./cover.png             # 单产物：用你给的文件名（会覆盖）
apiduo task <task_id> --wait --out ./out/         # 从已有任务补取产物
```

规则只有三条：

- **目录不覆盖**。自动命名带时间戳，重跑同一任务会得到 `-2`，不会悄悄盖掉上一张。
- **文件覆盖**。显式给了文件名（且只有一个产物），说明你知道自己在指什么。
- **落盘失败不改退出码**。产物 URL 已经拿到了，写不进去是本地目录的问题；
  `--json` 里会多一个 `save_failed`，脚本可以据此重试。

```json
{ "ok": true, "command": "image",
  "meta": { "model": "gpt-image-2", "elapsed_ms": 1234,
            "saved": [{ "path": "/home/me/out/gpt-image-2-20261105-030530-1.png",
                        "bytes": 812345, "mime_type": "image/png" }] } }
```

`--no-wait` 时不落盘（任务还没完成，落不了）；`search` 没有 `--out`（结果是文本）。

## 计费

- 提交视频任务会立即计费，所以 `video` 是唯一会先确认再提交的命令；
  非交互环境必须加 `--yes`，否则直接拒绝（退出码 2）。
- 轮询 `task.get` 不计费，`--wait` 可以放心等。
- `--json` 会把上游的 `usage` 原样带回来。

## 开发

```bash
cd packages/cli
npm test          # node --test，起一个本地 mock MCP，不碰真实上游
```

目录：

```
bin/apiduo.mjs    入口
src/args.mjs      参数解析（--flag value / --flag=value / --no-flag / --）
src/config.mjs    ~/.config/apiduo/config.json（0600） + 环境变量优先级
src/rpc.mjs       JSON-RPC over Streamable HTTP，JSON/SSE 都能解
src/files.mjs     本地参考图 → data URI；产物下载与落盘（命名、覆盖、上限）
src/ui.mjs        输出分流（结果走 stdout，日志走 stderr）、隐藏输入
src/cli.mjs       命令与轮询
test/             单元 + 端到端（mock MCP）
```

发布前确认 npm scope：`@apiduo/cli` 需要 apiduo 组织拥有该 scope；
没有的话改成可用的名字，`bin` 仍然是 `apiduo`。
