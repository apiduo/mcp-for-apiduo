# mcp-for-apiduo

**new-api 的 MCP 前置层**：把 MCP 协议翻译成 new-api 的 OpenAI 兼容调用。

鉴权直接复用 new-api 的 API key，不建第二套身份体系；与 new-api 完全解耦 ——
不改它的代码、不读它的库、不要求它升级。官方 `QuantumNous/new-api` 与
[`apiduo/apiduo`](https://github.com/apiduo/apiduo)（本地 `~/PycharmProjects/AI/apiduo`）同样适用。

## 1. 链路

```
MCP 客户端                    MCP Server（本仓库）              new-api
Authorization: Bearer sk-x ──▶ 无状态协议翻译 ──▶ /v1/images/generations
                                 透传同一把 key        /v1/models
                                                      /v1/chat/completions
                                                        ↓ 身份·额度·计费·渠道
                                                      上游供应商
```

MCP Server 不认识用户，只认识 key —— 这把 key 可能来自请求头，也可能来自配置。key 是谁的、有多少额度、能调哪些模型、
这次调用怎么计费 —— 全部由 new-api 回答。**网关已有的能力，不在网关前面再建一遍。**

## 2. 鉴权：复用，不新增

| 环节 | 做法 |
|---|---|
| key 从哪来 | 请求头有 `Authorization: Bearer sk-...` 就用它；没有则用配置里的 `api_key` |
| MCP Server → new-api | 用上一步选定的那把 key 调用，不换取、不签发、不缓存 |
| 校验时机 | 不在 MCP Server 校验，直接放行，401/403 由 new-api 返回 |

不在入口先验一次 key，是因为那要多一次往返，且两处判定迟早不一致。
new-api 是唯一权威，MCP Server 只负责翻译协议和搬运。

这条也决定了权限边界：MCP Server 无法比 new-api 更细。要限制"某把 key 只能文生图"，
用 new-api 的令牌分组与模型限制实现，不要在 MCP 层另造一套 ACL。

## 3. 配置：两项

| 项 | 默认 | 说明 |
|---|---|---|
| `base_url` | `http://127.0.0.1:3000/v1` | API 根。可写全 `https://x.com/v1`，也可只写 `https://x.com`（自动补 `/v1`） |
| `api_key` | 空 | 上游令牌 `sk-...`，直接用 new-api 的 |

两项都可热更新：改配置文件即生效，不用重启。

```yaml
base_url: https://api.example.com
api_key: sk-xxxx
```

### 谁来用这把 key

默认单租户：服务用配置里的 `api_key` 调上游，客户端不必带 key。

这带来一个必须自己承担的后果 —— **入口没有身份校验**。谁能连上 MCP 端口，谁就能用这把 key 花你的额度。
所以单租户模式假定入口在网络层已经隔离（内网、VPN、IP 白名单至少占一样）。

要支持多用户，让客户端带自己的 key：请求头有 `Authorization: Bearer sk-...` 时用它、不理会配置里的，
没有时才回落到配置值。这样额度与计费天然归属每个真实用户，服务端仍然不存任何身份状态。

### 客户端怎么接

```json
{
  "mcpServers": {
    "apiduo": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer sk-..." }
    }
  }
}
```

`headers` 可省：单租户部署下服务用配置里的 `api_key`，客户端只填 `url` 就能用；
多用户场景才需要各自带上自己的令牌。

### 热更新

一份文件只有一个读者 —— `config.Store`。部署配置与覆盖层在**同一个快照**里切换，
按 `mtime` 轮询，改文件即生效：

1. **原子切换**：整个文档（`base_url` / `api_key` / 覆盖层 / 版本号）用 `atomic.Pointer`
   持有，读路径无锁，在途请求不受影响，也不会看到「新 base_url 配旧覆盖层」。
2. **失败保留旧值**：文件写坏或字段非法时保留上一份可用配置，错误进日志与 `/admin/healthz`。
3. **每次变更都校验**：`base_url` 限 `http`/`https`、非空、可解析、去尾部 `/`。

早先是两个读者：`config.Store` 轮询部署项，`admin.Handler` 在启动时读一次覆盖层快照。
结果手改 `models` 永远不生效，只能重启 —— 不是缓存问题，是两条读路径必然分叉。
现在 Write 也走同一个 Store，所以「面板保存」和「手改文件」是同一条路径，
区别只是谁触发写入。

第 3 条不是洁癖。`base_url` 决定本服务**带着 key** 往哪发请求 —— 能被改成任意地址的话，
它就是一个持有凭证的 SSRF 跳板，而日志里只会留下一行「配置变更」。

优先级：配置文件 > 环境变量，且文件里的空值不覆盖已有值。反过来会出现
「改文件不生效」或「漏写字段把有效地址清成空串」这类最难查的故障。

多副本挂同一份配置文件（ConfigMap / 共享 PVC），热更新天然一致。不要给单个副本挂独立配置。

### 写回不重排、不删注释

管理端保存时不是「读成 map 再整体 Marshal」——那会把 `base_url` 下面的注释、
键的排列顺序全部抹掉。这份文件是人维护的、要进 git diff 的，一次面板保存就把它
洗成机器格式是不可接受的副作用。

做法是直接改 YAML 语法树：只替换 `models` / `settings` / `disabled` / `protocols`
四个键的值节点，其余节点原样保留，键上的注释也跟着保留；空段直接删键，
不留 `disabled: []` 这种噪音。`TestReplaceOverlaysPreservesHumanFile` 守住这条。

### 版本号：内容哈希

覆盖层每次发布都算一个 `revision`（规范化 JSON 的 SHA-256 前 16 位），
出现在响应体、`ETag` 与 `/admin/manifest` 里。

用内容哈希而不是自增计数：多副本挂同一份文件时各副本独立算出同一个版本，
不需要协调；重启也不会让版本号跳跃。规范化（`disabled` 排序去重、空补丁剔除）
是前提 —— 否则同一集合换个顺序就得到新版本，客户端会以为配置被别人改了。

## 4. 为什么不需要扩展控制面

前一版设计（以及 `docs/` 里 V1.0 方案）要求宿主先落地注册表、一次性票据、委托凭证，
再由 Gateway 聚合扩展。那套机制解决的是「宿主进程内的子应用如何安全拿到用户会话」，
前提是**扩展要嵌进宿主 UI**。

本项目的形态是独立 MCP 服务、复用 key、无 UI 登录态，这个前提不存在：

| 那套机制要解决的问题 | 本方案里为什么消失 |
|---|---|
| 浏览器 iframe 怎么拿到用户身份 | 没有 iframe，客户端自带 key |
| 一次性票据，60 秒过期 | 没有票据，key 本身就是凭证 |
| 委托凭证收窄权限、可撤销 | 不换凭证，撤销 key 即撤销访问 |
| 多副本下 jti 必须共享存储 | 无状态，没有 jti |

**代价要认**：MCP Server 做不了「比 new-api 更细」的授权，也拿不到用户的 cookie 会话。
如果将来要做嵌入宿主导航的图片工坊 UI，那是另一个入口，走 new-api 的登录态，与本服务的 key 链路互不干扰。

## 5. 多副本：无状态，天然支持

MCP Server 不保存任何用户状态：不存 key、不存会话、不存额度。所以横向扩容不需要共享 Redis，
也不需要粘滞会话 —— 任何副本收到请求，都能独立完成「翻译 + 透传」。

唯一需要留意的是 **MCP 协议本身的有状态会话**：streamable HTTP 用 `Mcp-Session-Id` 串联一次会话内的多次调用。
两种处理，按场景选：

- **无状态模式**（推荐默认）：每个请求独立，会话 ID 仅作回显，不落服务端内存。
- **有状态模式**：会话存共享存储，或让负载均衡按 `Mcp-Session-Id` 粘滞。

除非工具需要跨调用的上下文（如长任务轮询），一律用无状态模式。

## 6. 与 new-api 的边界

只调公开 HTTP 接口，且只用 OpenAI 兼容的那部分：

| 用途 | 端点 | 备注 |
|---|---|---|
| 图片生成 | `POST /v1/images/generations` | **文生图与图生图共用这一个端点**，靠 `image` 字段带垫图 |
| 图片编辑（兜底） | `POST /v1/images/edits` | 仅当模型画像 `image.transport = edits_multipart` 时使用，由适配层内部转换 |
| 模型发现 | `GET /v1/models` | 代理后与 MCP 配置合并，MCP 覆盖优先，不另建模型表 |
| 文本（可选） | `POST /v1/chat/completions` | 供需要提示词改写的工具使用 |

真实请求是这样，两种模式只差有没有 `image`：

```json
{
  "model": "doubao-seedream-5-0-flash-260915",
  "prompt": "将图1的服装换为图2的服装",
  "image": [
    "https://ark-project.tos-cn-beijing.volces.com/doc_image/seedream4_imagesToimage_1.png",
    "https://ark-project.tos-cn-beijing.volces.com/doc_image/seedream4_imagesToimage_2.png"
  ],
  "response_format": "url",
  "size": "2K"
}
```

删掉 `image` 就是文生图。**没有第二个端点、没有第二种 payload 形状** —— 这也正是模式不需要单独声明的原因。

注意 `size` 是 `"2K"` 而不是 `"1024x1024"`：尺寸的写法每个厂商都不一样（档位 / 像素 / 宽高比），
所以它只能由每个模型的 `parameters.size.enum` 各自描述，不能统一成一种格式。

三条硬约束：

1. **不改 new-api 代码**。上游怎么升都行，`apiduo` 与官方版共用同一份 MCP Server。
2. **不读 new-api 数据库**，只走 HTTP。
3. **不落用户的 key**：日志脱敏，不写磁盘，不放进错误信息。

## 7. 目录规划

```
cmd/mcpd/            进程入口：MCP 端口 + 管理端口（两个监听分开）
internal/
  config/            配置文件唯一的读者与写者：部署配置 + 覆盖层 + 版本号，
                     mtime 热更新（原子切换、失败保留旧值），写回保留注释与键序
  catalog/           模型画像：访问器、kind 归一化、覆盖层合并、校验
  protocol/          协议族：生成/查询端点、编码（JSON/multipart）、字段映射
  upstream/          上游客户端：传输、错误映射、参考图下载（出站防护）
  tools/             5 个 MCP 工具
  taskid/            自包含 task_id 的 HMAC 签名（含协议族）
  admin/             管理端：九个幂等操作（ops）、manifest、审计、管理端 MCP
packages/
  model-capabilities.schema.json   模型能力扩展的机器可读定义（draft 2020-12，版本 4）
prototype/
  admin.html         管理端交互原型（单文件，无外部依赖）
testdata/
  mock_upstream.py   假上游，五种协议族各自的端点都实现了
docs/                设计文档
```

分层的判据只有一条：**会一起变的东西放在一起**。
协议族把「端点 + 编码 + 字段映射」关在 `protocol` 里，所以新增一种上游形状时
不需要动 `tools`、`catalog`、`admin` 任何一行。

## 8. 工具设计

工具分两类，切法不同：

| 类别 | 工具 | 分法 |
|---|---|---|
| 业务工具 | `image.generate` / `video.generate` / `search.query` | **按产物类型分**，一个 kind 一个 |
| 通用工具 | `models.list` / `task.get` | **跨模态共用**，不按 kind 分 |

为什么这样切：产物类型决定参数形状 —— 图片要 `size`，视频要 `duration`，搜索要 `top_k`，
硬合成一个工具只会得到一个什么都收、什么都不校验的接口。而「有哪些模型」和「任务跑到哪了」
与产物无关，按模态拆只会造出 `image.get_task` + `video.get_task` + `search.get_task` 三份一样的代码。

### 少不是目的，「一个概念一个工具」才是

对 MCP 调用方来说，**端点数量根本不可见** —— 它看到的是工具。真正决定简单与否的是三件事：

1. 完成一件事要调几个工具；
2. 第一次调用需不需要先查询；
3. 同一个能力有没有第二种表达方式。

**刻意不做的工具**：

| 想加的 | 不加的理由 |
|---|---|
| `image.text_to_image` / `image.image_to_image` | 同一件事的两个分支，该做参数不做工具 |
| `image.edit` | 图生图就是编辑，多一个名字等于多一种理解方式 |
| `image.get_task` / `video.get_task` | 生命周期与产物无关，合并成 `task.get` |
| `image.list_models` / `video.list_models` | 同上，合并成 `models.list`，用 `kind` 过滤 |
| `image.get_model` | `models.list` 已返回完整能力，单查只是列表的特例 |
| `*.cancel_task` | 需求未明确；加了就得处理竞态与幂等，成本远大于收益 |

### 真正让调用变简单的，是默认值

工具少 ≠ 简单。如果第一次生成必须先 `models.list` 查尺寸再 `image.generate`，那就是两次调用 ——
工具数量的收益被这一次多余往返吃掉了。

所以生成工具在缺 `model` / 参数时必须有可用默认值：

| 参数 | 默认值来源 |
|---|---|
| `model` | ① 该 kind 下管理端设置的默认模型；② 没设置则取 `models.list` 里该 kind 的第一个 |
| `size` / `duration` / 其他 | 该模型 `parameters.*.default` |

**查询只在需要精细控制时才发生。**

⚠️ 「取第一个」是兜底，不该当常态。上游列表顺序一变（新增模型、渠道调整），默认模型就跟着变，
而调用方毫不知情 —— 同一句 prompt 昨天和今天可能落到不同模型上。

所以管理端应当显式设置默认模型，**而且按 kind 分开设**：默认出图模型和默认出视频模型是两件事。

```yaml
settings:
  defaults:
    image: doubao-seedream-5-0-flash-260915
    video: doubao-seedance-1-0-pro
models:
  doubao-seedream-5-0-flash-260915:
    input:
      max_reference_images: 2
```

`/v1/models` 不表达默认模型 —— 它是全局设置，不是某个模型的字段。

### `image.generate` 的入参映射

| MCP 入参 | 上游字段 | 说明 |
|---|---|---|
| `prompt` | `prompt` | 原样透传 |
| `reference_images` | `image` | `string` 或 `string[]`；不传即文生图，传了即图生图 |
| `model` | `model` | 取值来自 `models.list` |
| `size` | `size` | 取值来自模型画像，可能是 `"2K"` 也可能是 `"1024x1024"` |
| `n` / `quality` / `format` … | 同名 | 只有模型画像声明过的参数才接受，其余本地拒绝 |
| —（固定） | `response_format` | MCP 恒为 `"url"`，不暴露给调用方 |

`reference_images` 接受单张字符串或数组两种写法：只有一张图时不必包成 `["url"]`。
服务端统一归一化为数组再发，调用方不需要知道上游吃哪种 —— 入口宽松，内部统一。

`reference_images` → `image` 是唯一一处改名。MCP 侧不能也叫 `image`：返回值里还有生成的图，
两个 `image` 混在同一个契约里，调用方分不清哪个是输入、哪个是输出。

`response_format` 固定为 `url`，不提供 base64 选项 —— 把大图塞进响应，会让上下文成本与传输成本同时失控。

这三条（改名映射、参数白名单、`response_format` 固定）在实现时要各自变成测试。

### `model` 不只是模型名，它是路由键

在 new-api 里，`model` 决定请求走哪个渠道、落到哪个上游实现。所以**凡是需要区分上游的能力都必须带 model** ——
搜索也不例外：火山的搜索、腾讯的搜索、秘塔的搜索，参数与返回都不一样，`model` 就是用来说「用哪一个」的。

因此搜索同样有画像，`kind: "search"` 就是一个正常模型：

```json
{
  "id": "volcengine-search",
  "object": "model",
  "created": 1762000000,
  "owned_by": "volcengine",
  "kind": "search",
  "execution": { "task": "sync", "transport": "search_query" },
  "input": {},
  "parameters": {
    "count":      { "min": 1, "max": 50, "default": 10 },
    "time_range": { "enum": ["day", "week", "month", "year"], "default": "month" }
  }
}
```

`search.query` 的入参因此和生成类工具同构：**model + 该模型声明的参数**。

| MCP 入参 | 上游字段 | 说明 |
|---|---|---|
| `model` | `model` | 选哪个搜索实现；缺省用默认搜索模型 |
| `query` | `query` | 原样透传，`site:` / `lang:` / `after:` 等算符保留 |
| `count` | `count` | 上限由画像约束 |
| `include_content` | `need_content` | 部分供应商才有，画像没声明就不接受 |
| … | 画像声明的其他参数 | 火山可能有时效范围，秘塔可能有学术库范围 |

**三种供应商三种参数，这正是画像存在的理由。** 如果所有搜索实现参数都一样，画像就是空转；
现实是各家的可选参数各不相同，而客户端不该为此写三套调用代码。

两件事仍然与生成类不同：

- **返回是结果数组，不是单个资产**。所以它不能复用「生成一个东西并把 URL 还给你」的返回契约，
  需要单独定义：结果项含标题、URL、摘要、发布时间、权威度等。
- **计费按成功请求**（HTTP 200 且结果非空），失败与空结果都不计费 —— 空结果重试是免费的，
  值得在服务端做一次查询改写后重试，而不是把空数组直接丢给调用方。

限流 5 QPS（账号级）由上游产生 429，MCP **不自行排队**：排队会把延迟转嫁给所有调用方。

### 新增一个模态要改什么

| 层 | 改动 |
|---|---|
| 契约 | `kind` 枚举加一个值 |
| 工具 | 加一个 `${kind}.${动词}` |
| 适配器 | 加一个 `transport` 实现 |
| 管理端 | 无改动 —— 表单从数据生成 |

**不需要改**：schema 结构、合并规则、权限模型、控制面板、`models.list` / `task.get`。

这就是四层分离的意义：**新增模态是加枚举，不是改结构**。反过来，如果每加一个模态都要动 schema、
动表单、动合并逻辑，那说明抽象放错了层。

## 9. 异步任务：协议还没准备好，先用应用层

MCP 在 2025-11-25 版引入了 Tasks（SEP-1686），看上去正是为「长任务」准备的。但实际状态是这样：

| 事实 | 影响 |
|---|---|
| 规范标注 experimental，"design and behavior may evolve" | 不敢作为依赖 |
| Go SDK v1.8.0 **完全没有实现**（`mcp` 包里搜不到任何 Task 定义） | 想用也用不了 |
| Python SDK 的 tasks API 已标 deprecated，注明「Tasks 已从核心规范移除，将以独立扩展回归」 | 方向已变 |
| 现在是 `ext-tasks` 独立扩展 | 客户端支持面还很窄 |

所以第一版**不用协议 tasks**，改用应用层工具：

```
image.generate(model = 异步模型)
  → { task_id: "…", status: "working" }        立刻返回，不等
task.get(task_id)
  → { status: "completed", outputs: [...] }    客户端自己轮询
```

**task_id 必须自包含**，因为服务无状态：任何副本收到 `task.get` 都要能处理，不能依赖「创建任务的那个实例」。

把 `model + 上游任务 ID` 编进 ID 就够了，不需要共享存储。但不要用可猜的明文拼接（`"model:id"` 会被伪造），
外面加一层 HMAC，密钥用 `api_key` 派生 —— 这样不同部署签出的任务 ID 互不通用，也不需要额外配置。

什么时候返回 task_id：

| 画像声明 | 行为 |
|---|---|
| `execution.task = async` | 立刻返回 task_id，不等上游 |
| `execution.task = sync` | 同步等待结果 |

声明决定**预期**，响应决定**事实**：如果标着 sync 的上游实际返回了任务 ID，照样把它包成 task_id 返回，
不要假装拿到了结果。

这里还有一条容易被忽略的：**MCP 客户端自己有超时**。同步模型如果跑 60 秒，某些客户端会先断开。
所以 sync 只用于确认快速的模型；对慢模型，哪怕上游是同步的，也建议画像里标成 async 走轮询 ——
超时发生在客户端那一侧，服务端再优雅也救不回来。

## 10. 管控层：MCP 覆盖优先

MCP 不是上游的镜像，是**管控层**。上游提供事实（有哪些模型、能不能调），
MCP 决定对外呈现什么、谁能用。合并时 **MCP 配置优先级最高**。

| 维度 | 谁说了算 | 说明 |
|---|---|---|
| 模型是否存在、能否调用 | 上游 | 事实，MCP 不能伪造 |
| 能力、参数、限制 | **MCP 覆盖** | 管理员显式配置最高优先 |
| 模型是否对外可见 | **上游 ∧ MCP** | 上游列了是前提；上游列了之后，MCP 的 `disabled` 才决定对下游是否开放 |
| 工具级权限 | **MCP** | 谁能调哪个工具 |

可见性是**两层 AND**，不是 MCP 单方面说了算：

| 上游 `/v1/models` | MCP `disabled` | 对下游 |
|---|---|---|
| 未列出 | 任意 | 不可见。配置里若还留着条目，进 `orphaned` 供清理 |
| 已列出 | 未禁用 | 可见，再按 `kind` 过滤 |
| 已列出 | 已禁用 | 不可见 |

判定只发生在 `Overlays.Apply`；之后的 `models.list` / `pickModel` / `PickDefault`
用的都是它输出的同一份列表，没有旁路。`disabled` 只回答「上游仍在、但对下游不开放」，
既不能让上游没列的模型出现，也不需要为上游下架做任何补偿。

### 合并顺序

```
上游 /v1/models → Normalize(kind 推导) → 协议模板「只补缺失键」 → 模型级覆盖「逐字段替换」 → 下架过滤 → 对外结果
```

四种动作，都归 MCP：

| 动作 | 效果 |
|---|---|
| 继承 | 没逐条配置的模型，自动继承内置基础模板或管理员协议模板（见下节） |
| 覆盖 | 上游或模板给的字段，MCP 配了就换成 MCP 的值 |
| 补充 | 上游没给的字段，MCP 填上 |
| 下架 | 上游有，但 MCP 侧不对外呈现：`disabled: [模型 ID]` |

MCP 没配的字段才回落到上游 —— 那是**默认值**，不是权威。

下架放在顶层 `disabled`，与 `models` / `settings` 平级 —— 它是管控动作，不是模型的一种能力，
塞进模型对象会污染画像。过滤发生在合并之前（`Overlays.Apply`），所以 `models.list`、
`pickModel`、`PickDefault` 三处自然都看不到它，不需要各写一遍判断。

```yaml
protocols:
  image:                          # kind=image 自动继承
    execution: { task: sync, transport: generations }
    input: { max_reference_images: 0 }
    parameters:
      size: { enum: ["1K", "2K"], default: "2K" }
  video:                          # kind=video 自动继承（经网关）
    execution: { task: async, transport: video_generations }
  ark-video:                      # 自定义协议族（直连火山），由 _protocol 显式引用
    execution: { task: async, transport: contents_tasks }
models:
  doubao-seedream-5-0-flash-260915:
    kind: image
    input: { max_reference_images: 3 }   # 盖掉 protocols.image 的 0
  doubao-seedance-2-5-260628:
    _protocol: ark-video                # 不用自动的 video 模板
settings:
  defaults: { image: doubao-seedream-5-0-flash-260915 }
disabled:
  - doubao-seedream-4-0-250828
```

### 协议族模板：让新模型不再裸奔

**内置基础模板**：`image` / `video` 两类在代码里自带兜底（`catalog.BuiltinProtocols`），
管理员完全不写 `protocols` 也生效：

| kind | task | transport | 端点 |
|---|---|---|---|
| `image` | `sync` | `generations` | `/v1/images/generations` |
| `video` | `async` | `video_generations` | `/v1/videos/generations` |

内置模板**只放确定无害的字段**：不给 `input.max_reference_images`（上游不声明 ≠ 不支持垫图，
写 0 会锁死图生图，§14 教训 3），不给 `parameters`（尺寸 / 时长枚举各上游不同，猜错会误杀合法调用）。
`video` 选 `video_generations` 是因为主线是经 new-api 网关；**直连火山 Ark 必须**用
`protocols.video` 覆盖成 `contents_tasks`，或给模型写 `_protocol: ark-video`。
管理员模板按字段与内置合并（只写 `transport` 不会冲掉内置 `task`），模型级覆盖仍然最高优先。

`protocols` 按 **`kind` 自动匹配**：上游新加一个图片/视频模型、配置中心没给它逐条画像时，
自动继承 `protocols.image` / `protocols.video` 里的 `execution` / `input` / `parameters`。
模板**只补缺失的键**，上游真声明过的字段以事实为准；模型级覆盖仍然最高优先，能盖掉继承值：

```
上游原始（含 Normalize 推导的 kind） < 协议模板（补缺） < 模型级覆盖（逐字段替换） < disabled（不对外呈现）
```

同一 `kind` 需要不同协议族时（经网关 `video_generations` vs 直连火山 `contents_tasks`），
用 `_protocol` 显式指定；它是管理字段，不会出现在 `models.list` 的画像里。模板也可以包含
`kind`：对上游推不出类型的新模型，配一条 `models.<id>._protocol` 就能补出类型，
让它从「不可见」变成可调用。

合并链收敛在 `catalog.Overlays.ApplyOne` 一个函数里，运行时、`/admin/state`、保存前校验
都调它 —— 不各写一份，才不会出现「面板显示一套、客户端拿到另一套」。
继承管理员模板的带 `source: protocol`、只继承内置的带 `source: builtin`、
逐条覆盖过的带 `source: mcp`；`/admin/drift.inherited` 列出继承管理员模板的模型，
只靠内置兜底的仍进 `unconfigured`（它们还需要管理员补 `input` / `parameters`）。

⚠️ 模板里的 `enum` / `default` 会深拷贝到每个模型，改一个模型不会污染模板或别的模型。
保存时模板先校验 `enum` 非空、`default` 在枚举内、`min ≤ max`、垫图上限非负 ——
模板写错会一次性污染所有继承它的模型，所以这是 `issues` 硬拦；
引用了不存在的协议族只给 `warnings`（运行时退化为无模板，不产生错误调用）。

⚠️ `PUT /admin/overlays` 对 `disabled` / `protocols` 做了旧客户端兼容：请求体**完全没有这些键**
时保留现值，不按全量替换清空。当前面板的 `save()` 已发全量
`{models, settings, disabled, protocols}`；兼容分支留给脚本、旧版页面等不带这些键的调用方 ——
否则照标准 PUT 语义，一次保存就会把下架列表和协议模板一起抹掉。清空要显式传 `[]` / `{}`。
`GET /admin/drift` 的 `disabled` 栏只列「上游仍在、确实生效」的下架项，失效的归入 `orphaned`。

**别把 `disabled` 当成「上游下架的补偿」**。`Overlays.Apply` 只遍历上游返回的列表，
上游不再列出的模型本来就自动消失 —— 既不需要配置，也不会因为覆盖层里还留着条目而复活。
所以按 `status = Retiring` 自动下架、按 ID 前缀匹配下架这类机制都不需要，
后者还会把刚上线的新版本一起误杀。`disabled` 的唯一用途是：**上游仍在列，
但本地因计费 / 质量 / 合规原因主动停用**。换版后遗留的配置条目交给
`/admin/drift.orphaned` 报告，它只供人清理，不参与可见性判定。

### 前期路线：配置中心是唯一能力来源

前期不去改 new-api / apiduo 的 `/v1/models`。上游只回答两件事：**有哪些模型、这把 key 能不能调**；
`kind` / `execution` / `input` / `parameters` 四层能力由 MCP 配置中心提供：协议模板兜底 + 逐条覆盖修正。
实测过的两家上游都撑不起这套信息：new-api 返回 5 个字段
（`id/object/created/owned_by/supported_endpoint_types`），火山 Ark 虽然返回 15 个字段，
但只有 8 个全覆盖，且没有参数枚举、垫图上限、同步异步这些生成侧真正要用的东西。

**边界要认**：配置中心是能力来源，**不是模型目录**。`Overlays.Apply` 只遍历上游返回的模型，
配了上游没有的 ID 不会凭空变出可调用的模型 —— 保存时给 `warnings`，运行时静默忽略。
所以「能不能调」始终归上游，配置中心改不了这个事实。

代价是**配置漂移变成高频故障**：Ark 的 ID 带日期版本（`doubao-seedream-5-0-flash-260915`），
55 条里 18 条已标 `Retiring`，上游换版或下架就会让覆盖层变成孤儿条目。
前期靠人工配置，就必须能自查：`GET /admin/drift`（服务身份鉴权，实时拉上游）
返回 `configured` / `inherited` / `unconfigured` / `orphaned` / `disabled` 与计数。`unconfigured` 只列仍裸奔且能识别产物类型的模型，
LLM / Embedding 不会混进来当噪音；上游拉不动时返回 502，不拿缓存冒充事实。

演进路径不变：上游将来在 `/v1/models` 里返回能力字段时，它成为 MCP **未配置时**的默认值，
配上就以 MCP 为准。前期不为此预留兼容代码 —— `catalog.Model` 是 map，上游加字段自动可见。

**保存时校验分级**。`PUT /admin/overlays` 返回 `issues`（阻断，HTTP 422）与 `warnings`
（不阻断，随 200 返回）两栏：

| 等级 | 包含 | 理由 |
|---|---|---|
| `issues` | `kind` 非法、参数约束矛盾、默认模型类型不符、默认模型被下架 | 会让运行时把请求打错工具 |
| `warnings` | 孤儿覆盖层条目、失效下架项、默认模型当前不在上游 | 运行时有安全回落（`Apply` 忽略 / `PickDefault` 回落第一个），不产生错误调用 |

判据只有一条：**拦的是「会让运行时做错事」，不是「配置不好看」**。把配置漂移也判 422，
等于拿保存那一刻的上游快照否决一份长期配置 —— 前期人工改配置频度高，一个遗留 ID
就会让管理员连别的字段都存不了。唯一保守的例外是上游本身拉不动：事实不可得时
连合并结果都验不了，仍然硬拦。

⚠️ **对称的一半**：上游**新增**一个能识别 `kind` 的模型时，它会自动出现在 `models.list`，
并可能被 `PickDefault` 选为默认模型。现在有协议模板兜底，它不再是「硬编码缺省裸奔」，
而是继承 `protocols.<kind>`；没有对应模板的模型仍落到硬编码缺省，列在
`/admin/drift.unconfigured`。若前期路线要求「既没模板也没逐条配置就不该可见」，
需要把 `models.list` 的默认过滤从 `KnownOnly` 收紧到「具备完整能力画像」——
这会改变对外契约，暂未实施，留作显式缺口。

**再补一道门槛：`kind` 推得出是前提**。`Normalize` 只认上游自己声明的
`supported_endpoint_types` / `modalities.output_modalities` / `domain`；三者都没有时
`kind` 保持未知，`KnownOnly` 把它过滤掉、`Filter` 不匹配、`pickModel` 也选不中 ——
这时上游列了也没法调用。所以「上游新增」有两种结局：火山 Ark 这类带结构化字段的上游，
新模型**自动可调用**，甚至可能因为 `PickDefault` 取第一个而成为默认模型；
new-api 这类只返回 5 个裸字段的上游，新模型通常**不可调用**，直到配置中心补上 `kind`
——`_protocol` 指向一份含 `kind` 的协议模板也能补出类型，不必逐条画像。

「自动」有两层，护栏只挡得住一层：显式设置 `settings.defaults` 能防止新模型被
**自动选中**（见 §8），但挡不住它**自动出现在 `models.list`**；而且 defaults 指向的模型
一旦下架，`PickDefault` 会回落到第一个，新模型仍可能被选中。真要完全隔离，用 `disabled`。

### 能力标识：四层分离

```json
{
  "id": "doubao-seedream-5-0-flash-260915",
  "object": "model",
  "created": 1762000000,
  "owned_by": "bytedance",
  "kind": "image",
  "execution": { "task": "async", "transport": "generations" },
  "input": { "max_reference_images": 3 },
  "parameters": {
    "size": { "enum": ["1K", "2K", "4K"], "default": "2K" },
    "n":    { "min": 1, "max": 4, "default": 1 }
  }
}
```

| 层 | 回答什么 | 与模态的关系 |
|---|---|---|
| `kind` | 产出什么 | 就是模态本身 |
| `execution` | 怎么跑：同步还是异步、请求什么形状 | **无关** —— 图片也能异步，视频也能同步 |
| `input` | 能喂什么素材 | 通用名：参考图 / 参考视频 / 提示词长度 |
| `parameters` | 能怎么调 | 通用结构：枚举与区间 |

关键在于第二行：`task` 和 `transport` **不是模态的属性**。

把它们塞进 `image` 里，等于宣称"只有图片才需要知道同步异步"，然后加视频时再复制一份
`video.task`、`video.transport`。放错层的东西，每扩展一次就要重贴一次，而且三份定义迟早不同步。

「能做什么」仍由 `input.max_reference_images` 表达，但它是**三态**而不是二值：

| 取值 | 含义 | 本地行为 |
|---|---|---|
| 键缺失 | 上限未知（上游没声明） | **不拦截**，交给上游判定 |
| `0` | 明确不支持垫图 | 传了参考图直接拒绝 |
| `N > 0` | 上限 N | 超出 N 张拒绝 |

早先把它当二值（缺省 0），在真实上游上直接锁死了图生图能力。

### 哪些信息该进画像，哪些不该

一条判据：**发送请求前必须知道的，写进画像；收到响应后能判断的，不要写。**

| 信息 | 进不进画像 | 理由 |
|---|---|---|
| 请求形状（`execution.transport`） | 进 | 不知道它就发不出正确的请求 |
| 上游返回 url 还是 base64 | 不进 | 收到响应看一眼就知道，MCP 自己转存即可 |
| 余额、渠道、价格、并发上限 | 不进 | 那是宿主的账，MCP 不复制 |

`transport` 默认 `generations`，也就是最常见的那种：`POST /images/generations`，`image` 统一发数组。

**它不是「一个 URL」，而是「一整套端点 + 编码 + 字段映射」。**

| transport | 生成 | 查询 | 编码 | 适用 kind |
|---|---|---|---|---|
| `generations`（默认） | `POST /images/generations` | `GET /tasks/{id}` | JSON | image |
| `edits_multipart` | `POST /images/edits` | `GET /tasks/{id}` | multipart | image |
| `video_generations` | `POST /videos/generations` | `GET /tasks/{id}` | JSON | video |
| `contents_tasks` | `POST /contents/generations/tasks` | `GET /contents/generations/tasks/{id}` | JSON | video |
| `search_query` | `POST /tools/search` | 无（同步返回结果） | JSON | search |

**为什么查询端点必须跟着生成端点走**：火山的任务键只认 `/contents/generations/tasks/{id}`，
new-api 的只认 `/tasks/{id}`。拿错端点时上游返回**空 body 的 404** —— 没有错误码也没有消息，
是这类问题里最难查的一种。所以四件事绑定在同一个协议族里，不允许分开配置。

`contents_tasks` 还多一层：火山没有 `prompt` / `image` 字段，提示词与素材都在 `content` 数组里按 `type` 区分：

```json
{
  "model": "doubao-seedance-…",
  "content": [
    { "type": "text", "text": "镜头缓缓拉出" },
    { "type": "image_url", "image_url": { "url": "https://…/fox.png" }, "role": "reference_image" }
  ],
  "ratio": "16:9", "resolution": "720p", "duration": 5
}
```

这层映射由协议族消化，调用方仍然只说 `prompt` / `reference_images` / `aspect_ratio`。
注意 `aspect_ratio` 在这里会改名成 `ratio` —— **参数名翻译也属于协议族**，
否则每接一个上游，客户端就要多记一套名字。

`edits_multipart` 有个副作用要单独说：它必须**先下载参考图**再作为文件上传。
这是本服务唯一会主动访问「调用方提供的第三方 URL」的路径，所以带出站防护
（只允许 http/https 与公网地址、限 20 MiB、限 30 秒、限 3 次重定向，见 `internal/upstream/fetch.go`）。
代价也要知道：**内网图床在这里会被拒**，那种场景应走 `generations`（让上游自己去取 URL）。

上游的 `image` 字段本身兼容**单值与数组**两种写法，所以不需要为此再分一档 ——
入口统一收成数组发出去，调用方不必为「只有一张图」写 `["url"]` 这种别扭语法。

**它是服务端字段**：管理端可见可改，但**不暴露为 MCP 工具参数**。调用方只需要说清要什么图，
不需要知道请求怎么发 —— 这也和「客户端只看到三个工具」是同一件事的两面。

### 为什么没有 `modes`

文生图和图生图的区别只有一个：**有没有垫图**。既然模式由垫图推导，再单独存一个
`["text_to_image","image_to_image"]` 就是同一事实的第二次表达 —— 而两份表达迟早会不一致：
「声明支持图生图，却把垫图上限设成 0」这种配置，在旧结构里完全合法。

删掉 `modes` 之后，这类矛盾**在结构上无法表达**：上限为 0 就是不支持垫图，也就不可能「支持图生图」。

用结构消灭非法状态，比用校验拦截非法状态便宜得多 —— 校验会漏（不同语言的库支持度不一样），结构不会。

同理不设 `tags` / `features` 这类自由文本数组：它们只能给人读，程序没法据此做任何判断。

机器可读定义在 [`packages/model-capabilities.schema.json`](packages/model-capabilities.schema.json)
（JSON Schema draft 2020-12，契约版本 4）—— **apiduo 与 MCP 两侧共用同一份**，不各写一套。

**缺省的语义分两层**，不要混：

- `image` 整个缺失 → 上游没给能力信息，**未知**，等 MCP 配置补
- `image` 存在但缺具体键 → `task` 按 `sync`；`max_reference_images` **保持未知**（不按 0 处理）

`source` 只在被 MCP 覆盖时出现（`"source": "mcp"`），省略即表示来自上游。

### 这些字段谁来填

结构固定，值不能靠猜。

| 来源 | 负责什么 |
|---|---|
| 协议族模板（§10，已实现） | 按 kind 自动补缺；同一协议族配一次，新模型自动继承 |
| LLM 辅助 | 模板推不出的字段，出草稿 + 字段依据 + 置信度 |
| 真实调用 | 尺寸、格式、参考图限制必须实测通过才算数 |

AI 可以自动生成这份结构，但边界是 **填值、不发明字段**。看到模型名里带 "4k" 就写
`size: 4096x4096` 是这类自动配置最典型的翻车方式：配置看着完整，调用时才失败，
而失败发生在用户那侧。

所以发布门槛是：**枚举值来自上游文档或真实响应，且最小成本的真实调用通过**。
自动发布只对受信供应商开放，其余走「草稿 → 人工确认 → 发布」。

⚠️ 内置模板只兜住了 `execution`。`input.max_reference_images` 仍为**未知**（不拦截）、
`parameters` 仍为空（标准参数直接放行，见 §14 教训 3）——这两类要管理员用
`protocols` 或逐条覆盖补。`settings.defaults` 只用于**选哪个 id**（`PickDefault`），
从不参与 `Merge`；默认模型的能力参数不会传给新模型。`ParamDefault` 定义了但目前没有调用方，
不要以为缺省值会自动注入。

内置 `video` 模板已解决 `kind = video` 的新模型带着 `transport = generations`
打到 `/v1/images/generations` 的风险。仍可选的收口：① `Generate` 里对
「kind 与 transport 明显不匹配」本地报错；② 收紧为「具备完整画像才可见」（§10 已记缺口）。
两者都是行为变更，当前未实施。

### 为什么 MCP 优先

管控层的价值在最终解释权。如果上游优先，管理端改的任何字段都可能被上游悄悄盖掉，
面板就退化成只读展示，管控不成立。

apiduo 将来在 `/v1/models` 里加能力判断，与这条不冲突：它成为 MCP **未配置时**的默认值。
配上就以 MCP 为准，没配就跟着上游走。

### 一条不能破的底线

MCP 可以隐藏模型，但不能把上游不存在的模型变成「可调用」。
强行暴露上游没有的模型，调用必然失败，而错误会出现在最不该出现的地方 —— 用户面前。

所以这类条目若出现在模型列表里，必须显式标注来源（`source: mcp`），控制面板同时给出告警。

## 11. 控制面板

这是 MCP 的核心界面，不是附属功能 —— 模型与权限的管控都在这里完成。

| 面板 | 能力 |
|---|---|
| 模型管控 | 上游模型列表 + 覆盖状态；逐字段覆盖能力/尺寸/限制；整条下架（后端已就绪） |
| 权限管控 | key 或分组 → 可用工具集、可调模型范围 |
| 差异视图 | `configured` / `unconfigured` / `orphaned` / `disabled` —— 后端 `/admin/drift` 已返回，前端待接 |
| 变更记录 | 谁在什么时候改了哪个字段，可回滚 |

### 表单不手写，从数据生成

管理端不需要为每个模型做一套界面。`/v1/models` 的能力标识本身就是表单描述：

| 数据形态 | 控件 | 例子 |
|---|---|---|
| `enum` | 取值胶囊 + 默认值下拉 | `quality: ["standard","hd"]` |
| `min` 或 `max`（**任一边即可**） | 最少 / 最多 / 默认三个数字框，缺的一边显示「不限」 | `n: {max: 2}` → 最少不限、最多 2 |
| 只有 `default` | 按默认值类型给输入框 | — |
| 什么都没给（`{}`） | 一个空输入框，标注「任意值 · 未设约束」 | — |
| `task`（枚举） | 单选 | `sync` / `async` |

`min` / `max` 只要求出现一边。真实上游经常只写 `{max: 2}`，早先要求两边都有才渲染范围控件，
结果被画成「默认值」文本框 —— 数字上限藏在一个文本输入里，管理员根本看不出它在限什么。

行内的区间提示是现算的（`范围 · 3–10` 会随输入变成 `范围 · 20–10`），因为数字是就地编辑、
不重建 DOM；提示若只在渲染时算一次，改完就会出现「输入框 20、旁边还写着 3–10」。

**空状态不是死路**。上游通常不给 `parameters` —— 实测那批 59 个模型里 53 个连 `kind` 都没有。
若「没声明」只显示一行灰字，这块面板就等于没有编辑能力。所以空状态给的是**可点的建议**：

- 按产物类型预置常见参数名（图片 `n/size/quality/aspect_ratio`，视频 `duration/resolution/n/fps`，
  检索 `count/top_k`），点一下带出形状与起点值，数字仍按上游实际能力改；
- 末尾一个输入框，自定义参数名回车即加（只收 `[a-zA-Z_][a-zA-Z0-9_]*`，避免写出上游不认的键）；
- 已经有参数时，卡片底部保留「再加」一行，卡片头也有「+ 添加参数」，两处都落到同一个输入框。

`input` 同理：`max_reference_images` / `max_reference_videos` / `max_prompt_chars` 三个已知键做成胶囊，
点一个加一行，起点 1 / 1 / 4000。不从 0 起是因为 0 在这里是**明确不支持**，不是「还没填」。

**入口要能被找到**。`parameters` 卡片在中栏最底部，笔记本上常要滚动才看得见。
所以顶部事实条做成了导航：点「参数 0 个」直接滚到那张卡并闪一下边框，不用先猜它藏在哪。

上游新上线一个模型 → 自动出现在列表里 → 表单按上述规则渲染并预填上游值。
没有这一步，接一个新模型就要改一次前端代码 —— 那和「为每个模型写一个 MCP 工具」是同一个错误。

### 存差异，不存快照

管理端保存的是**覆盖层**，只记录被改过的字段：

```yaml
models:
  doubao-seedream-5-0-flash-260915:
    kind: image
    input:
      max_reference_images: 2
    parameters:
      n: { max: 2 }
```

没被覆盖的字段继续跟随上游。若改成保存完整对象，上游加了新尺寸，用户永远看不到 ——
快照省事，代价是配置与事实永久脱节。

### 每个字段标来源

表单里每个字段旁标 `继承` / `已覆盖` / `新增`：

- `继承`：值来自上游，改动即变成覆盖
- `已覆盖`：上游有这个键，值被管理端改了，可一键恢复继承
- `新增`：上游根本没有这个键，是管理端补的 —— 没有「继承」可恢复，所以只给删除

区分后两者不是为了好看：对一条新增的参数，「恢复继承」和「删除」结果完全一样，
两个按钮并排出现只会让人以为它们有区别。判据是 `getIn(上游画像, 该字段路径)` 是否存在，
不手写、也不看覆盖层本身。

不标来源，管理员过两周就分不清某个值是上游给的还是自己改的，于是不敢动也不敢删，
覆盖层最后会烂在原地。

### 三栏等高：面板高度由视口决定，不由内容决定

早先是「整页滚动 + 左右 sticky」。看着能用，但窗口一矮就露馅：三块面板各按内容长，
底部参差，页面背景从下面漏出来一大片。sticky 救不了这件事 —— sticky 只在页面滚动时保持位置，
面板本身还是内容那么高。

改成 app shell：`.app` 高度 = `100dvh - 顶栏`，grid 单行 `align-items:stretch`，
三栏高度等于视口，滚动发生在**面板内部**。底部是一条水平基线，和 Finder / Mail 一样。

三个必须一起改的点，漏一个就出错：

1. **中栏得成为滚动容器**（`overflow-y:auto`），否则内容溢出会撑破面板。
2. **中栏的 flex 子项要 `flex:none`**。`#wp` 是 `display:flex` 纵向排列，子项默认 `flex-shrink:1`——
   高度不够时卡片会「缩」而不是「溢出」：参数行被挤成一团，看起来像渲染坏了。
3. **`scrollbar-gutter:stable`**：滚动条出现/消失时卡片宽度不跳。

窄屏（≤820）反过来：单列三块堆叠时如果还各自内滚，会变成三个小囚笼，所以整页滚。

滚动边缘用 `mask-image` 跟随滚动位置淡出：下方还有内容 → 底部淡出 20px；离开顶部 → 顶部淡出 10px；
滚到底就撤掉底部淡出，否则最后一张卡永远像被吃掉一截。这是 iOS 26 scroll edge effect 的 Web 近似，
比一条硬切边更像「内容还在继续」。

### 三栏的宽度跟着视口走，不是钉死的 296 / 396

高度改成视口决定之后，宽度还是写死的：`296px / 1fr / 396px`。这在 1512 的笔电上勉强能看，
到 1280 就露馅 —— 两侧吃掉 54%，中栏只剩 520，参数行的控件被挤成一列；
而 1900 以上又白白空着，多出来的宽度给不出去。

现在两侧栏用 `clamp()` 跟视口走，中间那段永远归中栏：

```css
.app{
  --rail-l:clamp(228px,15.8vw,300px);   /* 下界：放得下模型名 + 徽标 */
  --rail-r:clamp(288px,20.6vw,392px);   /* 上界：再宽也不会更好读 */
  grid-template-columns:var(--rail-l) minmax(0,1fr) var(--rail-r);
  max-width:1860px;                     /* 超宽屏不做成中间一条窄带 */
}
.row{ grid-template-columns:clamp(118px,9.6vw,150px) minmax(0,1fr) auto }
```

实测（中栏 = 真正能干活的地方）：

| 视口 | 左栏 | 中栏 | 右栏 |
|---|---|---|---|
| 2560 | 300 | 1100 | 392 |
| 1920 | 300 | 1100 | 392 |
| 1670 | 264 | 994 | 344 |
| 1440 | 228 | 847 | 297 |
| 1280 | 228 | 696 | 288 |
| 1181 | 228 | 597 | 288 |

1280 这一档中栏从 520 涨到 696（+34%）。行标签列同步收窄，否则窄屏下标签还要占 150。

**右栏可以收起来**（头部箭头 / `⌘\`）。JSON 是参照物不是常驻，收起来把 344px 全还给中栏，
只留一条 40px 的脊，点一下就回来；状态记在 `localStorage`，收着的人不用每次刷新重来。

这里有个必须一起改的联动：**保存主按钮在右栏底部**。面板一收，入口就跟着消失 ——
所以 `body.insp-off .topbar .top-save` 把它搬到顶栏。隐藏一个面板时，一定要检查
「只存在于这个面板里的操作」有没有别的入口，否则用户会以为功能坏了。

收起只在 ≥1181 生效：更窄时右栏本来就是底部抽屉，再收起没有意义。
脊上留一个小圆点，收起时看不见校验结果，至少让「有未保存改动 / 有冲突」看得见。

### 全局配置只占一条，不占一屏

协议模板是全局配置，却挂在每个模型工作区的最上面 —— 位置最贵，内容最少。
所以默认态只占一条 34px 的工具条（灰底、无投影），展开才升成卡片（投影 + 描边 + 内滚 `58vh`）。
判断标准很简单：**它是不是「每次进来都要改的东西」**。不是，就不该有卡片的视觉权重。

折叠态直接把生效值写进摘要：`image sync·generations`、`video async·video_generations`。
蓝底 = 这个协议族有覆盖层（悬停有说明），灰底 = 纯内置。多数时候管理员只想确认一眼值，
不该逼他先展开。

展开后**内置模板也渲染完整 JSON**（只读 `<pre>`），覆盖态才换成可编辑 textarea 并注明
「只写要改的键，没写的继续继承内置」。早先内置态不给 JSON，想看一眼就得先点「覆盖此模板」——
为了看而先制造一份覆盖层，顺序是反的。

顺带记一个 CSS 坑：选中态本来写的是 `background:currentColor; color:var(--surface)`，
结果得到白底白字。原因是 `currentColor` 在计算值阶段取**本元素最终**的 `color`，
同一规则里把 `color` 改成 surface，`currentColor` 就跟着变成 surface。改成在容器上定义
`--kd-ink` 并让按钮 `background:var(--kd-ink)`，两者才真正解耦。

### 校验放两道

前端按同一份 schema 即时校验，后端保存前再校验一次（不信任前端）。

但要注意校验的对象：**覆盖层本身不是合法模型对象**（它缺 `id`/`object` 等字段），拿完整 schema 去校验它必然报错。
正确的链路是：

```
读覆盖层 → 与上游条目合并 → 用完整 schema 校验「合并结果」→ 通过才落盘
```

也就是说，保证的是**结果正确**，不是**输入合法**。覆盖层只需要约束键名
（只能出现 `kind` / `execution` / `input` / `parameters`），值的合法性交给合并后的整体校验。

取消 `modes` 之后，原先最容易写错的那类配置（声明支持图生图却把垫图上限设成 0）已经无法表达，
剩下的校验都在参数层面：

- `enum` 不能为空，且 `default` 必须落在 `enum` 内
- `min` 不能大于 `max`
- `max_reference_images` 不能为负

**声明的约束必须真的拦得住调用**。覆盖层里写了 `parameters.duration: {min: 3, max: 10}`，
客户端用 `extra.duration = 99` 调 `video.generate` 就得当场拒绝 —— 只在 `models.list` 里展示、
调用时不管，这份画像就只是文档，越界值照样打到上游再吃 400（甚至按非法参数计费）。
工具层对 `extra` 的每个键按画像校验：有 `enum` 先比枚举，没有再看 `min` / `max`；
两样都没声明就放行（等价于「任意值」）。`standardParams`（size / duration / resolution 等）
仍然放行**未声明**的模型，避免把没画像的模型直接判死。

**能力层面的矛盾交给结构，参数层面的矛盾交给校验** —— 前者零成本且不会漏，后者必须写但只覆盖真正可能的错。

这也是 schema 必须放 `packages/` 由两侧共用的原因：apiduo 输出、MCP 合并、管理端表单，
三处引用同一份定义，才不会出现「前端允许、后端拒绝」。

**差异视图为什么必须存在**：管控层最大的风险是配置与事实脱节 —— 上游下架了模型，MCP 还在宣称它可用；
上游上了新模型，MCP 一直没配能力，客户端拿到的是残缺信息。没有差异视图，这类漂移只能等用户报错才发现。
后端提供 `GET /admin/drift`（见 §10），面板「连接 / 探针」页直接展示 unconfigured / orphaned / disabled 三类差异。

**权限如何定位到人**：MCP 不存用户表，也不该存。规则以 **key 的哈希**为索引 ——
管理员在面板录入需要管控的 key（只存哈希，不存明文），命中的按规则放行，未命中的走默认策略
（全开或全关，部署时定）。这样既能做 per-key 管控，又不需要知道 key 背后是谁。

要按 new-api 的分组管控，就得拿 admin 凭据去查 key 归属 —— 这会打破「只用用户 key」的前提，
因此作为可选能力：不配 admin 凭据就不启用。

**鉴权与存储**：管理端口独立监听、只在内网暴露，用服务身份的那把 `api_key` 校验，不建第二套账号。

这里要分清两类凭据：配置里的 `api_key` 是**部署者凭据**，代表「这台服务以谁的身份调上游」；
客户端带的是**用户令牌**。普通用户就算拿着自己的 key，也进不了控制面板 —— 两者不是同一类东西，
泄露影响面也不同：用户 key 泄露只损失该用户额度，服务 key 泄露等于交出管控面。

写操作先校验后落盘，失败保留旧值 —— 与热更新同一套规矩。
校验分级见 §10：`issues` 阻断并返回 422，`warnings` 照常保存并随 200 返回，前端应把两者分开呈现。
存储单机用一份 YAML，多副本挂共享卷；变更频率是「人工改配置」级别，不需要一开始就上数据库。

### 管理 API：先为 agent 写，再给人用

管理端不只是页面的后端。它还要被 codex / workbuddy 这类客户端直接调用 ——
它们没有上下文记忆、会盲目重试、会和别人并发改同一份配置。所以接口形状的判据
不是「页面需要什么」，而是：**一个只能看见 JSON 的调用方，怎样才能改对配置。**

四条性质，缺一条就不成立：

| 性质 | 做法 | 缺失时的后果 |
|---|---|---|
| 可发现 | `GET /admin/manifest` 给出端点、九种操作、错误码与常用流程 | agent 猜路径、猜字段，靠 422 试错 |
| 细粒度 | `POST /admin/ops` 是定址的小操作，不是整份文档替换 | 读-改-写一次就抹掉别人改的字段 |
| 并发安全 | 响应带 `revision`，写请求可带 `If-Match`，不符 409 且不落盘 | last-writer-wins，两个客户端互相看不见 |
| 可空跑 | `dry_run` 只校验并返回**叶子级** `changes` | 只有成/败两种结果，错了不知道错在哪 |

#### 九个操作，四个集合

覆盖层的全部状态就是四个集合。操作因此可以穷举，不需要「通用 JSON Patch」：

| 集合 | 操作 |
|---|---|
| `models.<id>` 补丁 | `model.patch` / `model.delete` |
| `settings.defaults.<kind>` | `default.set` / `default.delete` |
| `disabled[]` | `disabled.add` / `disabled.remove` |
| `protocols.<name>` | `protocol.patch` / `protocol.delete` |
| 整份覆盖层 | `overlays.replace`（只用于导入/迁移） |

`patch` / `template` 用 **RFC 7386 合并补丁**：对象递归合并，`null` 删键，数组整体替换。
用标准语义而不是自创规则，是因为 agent 对它有稳定预期 —— 自创规则会让它每次回来查文档。

九个操作全部**幂等**：同一组 ops 重发，状态与 `revision` 都不变（返回 `no_change: true`）。
所以不需要 `Idempotency-Key` —— 用结构消灭重复提交，比用缓存去重便宜。

一组 ops 要么全部生效，要么一条都不生效。批量改 53 个模型时这不是洁癖：
循环单发会在第 30 个失败，留下一个「改了一半、没人知道改到哪」的配置。

#### 端点

| 端点 | 用途 |
|---|---|
| `GET /admin/manifest` | 自描述清单（端点从路由表生成，不会与实现漂移） |
| `GET /admin/config` | 覆盖层 + `revision`，不拉上游，最便宜的起点 |
| `POST /admin/ops` | 批量原子操作；`dry_run` 只校验 |
| `GET /admin/models` | **投影列表**（不含参数约束）；`?kind=` 过滤、`?include_unknown=1` 含未声明类型、`?full=1` 仅导出用 |
| `GET /admin/models/{id}` | **完整约束** + 三份视图（上游 / 覆盖层 / 合并）+ 漂移分类 |
| `PATCH /admin/models/{id}` | 合并补丁，body 就是补丁本身 |
| `DELETE /admin/models/{id}` | 删除整条覆盖层（≠ 下架） |
| `PUT` / `DELETE /admin/settings/defaults/{kind}` | 设置 / 取消默认模型 |
| `POST` / `DELETE /admin/disabled/{id}` | 下架 / 恢复 |
| `PATCH` / `DELETE /admin/protocols/{name}` | 协议模板覆盖 / 回到内置 |
| `GET /admin/history` | 变更记录 |
| `POST /admin/mcp` | 管理端 MCP（下面单说） |
| `GET /admin/overlays`、`PUT /admin/overlays` | 兼容旧面板：全量读写。agent 不要用 PUT |

两条与管理端语义有关的细节：

- **`/admin/models` 包含已下架模型**，并显式标注 `disabled: true`。客户端列表要过滤，
  管理端不能过滤 —— 看不见就没法恢复。
- **列表是投影、详情才是全量**，切分见下一节。

#### 两段式读取：列表回答「是不是」，详情回答「是多少」

`GET /admin/models` 返回的是**投影**，不是完整画像：

| 列表给 | 列表不给 |
|---|---|
| `id` `kind` `source` `task` `transport` `disabled` `has_overlay` `max_reference_images` | `parameters.*` 的 `enum` / `min` / `max` / `default` |
| `params`：参数**名**列表 | 参数的**约束** |

切分线不是拍脑袋定的，它对应两个不同的问题：

- 「这个模型能不能做我要做的事」—— 列表能答（类型、同步/异步、垫图上限、有哪些参数名）；
- 「这个参数该填什么值」—— 只有详情能答（枚举、区间、默认值）。

把两件事分开，第一次请求就足够支撑「改不改」的决策，第二次只在你真要动手时才发。
少了 `params` 这一列，agent 连「支不支持 duration」都得再问一次 —— 两次就变成三次。

「两次」是**发现流程**的上界，不是规矩：用户直接点名某个模型时，已知 id 就跳过列表直取详情。
列表只用来筛候选，不该成为每次操作的必经仪式。

代价是两次请求 = 两次上游 `/v1/models` 往返（详情没法按 id 只取一个，上游没有这个接口）。
所以读路径加了 **30 秒缓存**（`-models-cache-ttl` 可调，0 关闭），响应里带
`upstream_age_ms`：0 表示本次刚拉，大于 0 就是这份列表的年龄。

能缓存 30 秒，是因为**缓存的不是合并结果**：`models` 原始列表的变更频率是渠道上下线（小时级），
而覆盖层改动每次现算。换句话说，缓存只影响「看到的上游有多新」，不影响「刚配的为什么没生效」——
后者在这个设计里根本不会发生。过期后拉取失败会如实报错，不会拿旧值兜底。

写校验**不走缓存**（`validateOverlays` 直连上游）：校验必须基于当下事实，而写是人工频率，
多一次往返换「不会用 30 秒前的列表判断模型是否存在」，很划算。

刚在上游加了渠道、要立刻确认时，用 `?refresh=1`（MCP 工具传 `refresh=true`）：
它只刷新这一次读，不清空缓存 —— 清空会让所有并发请求一起 miss，把小操作放大成一次惊群。

`/admin/state` 与 `/admin/models` **共用同一份缓存**。分成两套的话，
「面板显示 59 个、agent 列表显示 58 个」会变成常态；同一进程里不该有两个真相。
`state.upstream.age_ms` 把这份视图的年龄标出来。

`?full=1` 保留，作为人导出 / 调试的逃生舱，**agent 默认不要用**：
59 个模型 × 每个几千字节的 `parameters`，一次就能吃掉大半个上下文，其中九成字段它用不到。

列表响应自带指路：`{"projection": true, "detail": "/admin/models/{id}"}` ——
契约跟着响应走，而不是只写在文档里等人转述。

#### 错误是数据，不是一句话

`issues[]` 里的每一条都带 `path`（JSON Pointer，指到具体字段）与 `hint`（下一步怎么改）：

```json
{
  "error": {
    "code": "validation_failed",
    "message": "校验未通过，未保存",
    "hint": "按 issues[].path 改对应字段后重试；warnings 不会阻断保存"
  },
  "revision": "8fd8e31e8f82e03c",
  "issues": [
    {
      "path": "/models/doubao-seedream-5-0-flash-260915/parameters/n",
      "message": "parameters.n：min 不能大于 max",
      "hint": "min/max 只要求出现一边；两边都写时必须 min ≤ max"
    }
  ]
}
```

判据是**能否自我纠正**：「校验失败」这四个字，人看了要回来翻文档，agent 看了只能
原样重试一遍。而 `path` 指向哪一行、`hint` 给出可选值，它就能自己改对。

| code | 状态 | 含义 |
|---|---|---|
| `revision_mismatch` | 409 | 别人先改了；重新读再算，不要原样重发 |
| `validation_failed` | 422 | 操作合法但结果会让运行时做错事，按 `issues[].path` 改 |
| `invalid_op` | 400 | 操作名或字段写错，`error.allowed` 给出可用操作 |
| `invalid_request` | 400 | 请求形状不对 |
| `persist_failed` | 500 | 落盘失败，内存仍是旧值 |
| `upstream_unavailable` | 502 | 拉不到上游，管理端不拿缓存冒充事实 |

#### 管理端 MCP：把配置能力直接交给 agent

REST 已经够用，但让每个 agent 自己写 HTTP 胶水是重复劳动 —— 而本项目的前提就是
「agent 通过 MCP 使用能力」。所以同一个操作集另开一个 MCP 入口：

```json
{
  "mcpServers": {
    "apiduo-admin": {
      "type": "http",
      "url": "http://127.0.0.1:8081/admin/mcp",
      "headers": { "Authorization": "Bearer <服务身份 api_key>" }
    }
  }
}
```

| 工具 | 作用 |
|---|---|
| `config.get` / `config.models` / `config.model` / `config.drift` / `config.history` | 只读 |
| `config.apply` | 唯一的写工具：一组 ops + `dry_run` + `if_match` |

`config.apply` 的 `dry_run` **默认 true**。这是刻意的摩擦：agent 拿到新工具时最容易
直接开写，默认空跑保证第一次调用一定安全，要提交必须显式传 `dry_run=false`。
REST 侧相反，默认直接写 —— 会手写 HTTP 的调用方已经是显式的了。

工具内部还是走 `commit`，校验、版本、审计一条都不少。不存在「MCP 有后门」这种可能：
两条传输共用同一份操作实现，不可能出现一套能过、另一套过不了。

#### 变更记录

每次写入追加一行到 `<config>.audit.jsonl`，同时留最近 200 条在内存里：

```json
{"time":"2026-11-04T23:42:41+08:00","actor":"codex","source":"rest","outcome":"saved",
 "ops":[{"op":"model.patch","target":"doubao-seedream-mock"}],
 "revision_before":"515966e6d9fa134b","revision_after":"8fd8e31e8f82e03c","warnings":0}
```

只记「谁、什么时候、用哪些操作把版本从 A 推到 B」，**不记补丁内容**：
内容看 `changes` 或 git，日志再存一份等于复制配置，还会顺手把敏感字段带进文件。

`actor` 取 `body.actor` > `X-Admin-Actor` > `User-Agent`。被拒绝的写入也记
（`outcome: rejected`），因为「agent 在凌晨试了什么」和「它改成了什么」同样重要。

#### 面板上的变更记录

后端有了时间线，面板就必须把它显示出来 —— 否则「agent 半夜改了配置」这件事，
仍然只有会写 `curl` 的人才能发现。顶栏「变更」打开一条只读时间线：

- 每条一行：相对时间（超过半天回到绝对时刻）+ 结果胶囊（已保存 / 空跑 / 被拒 / 失败）
  + actor + 操作与目标 + 版本 `A → B`；`issues` / `warnings` / `经 MCP` 作为标签附在下方。
- **最新在最上面**。服务端按追加顺序正序返回，面板翻过来 —— 打开它的人问的是「刚才谁动了」。
- 只读，**不提供一键回滚**。回滚是一次新的写入，该走正常的编辑 + 保存，并留下它自己的记录；
  给个「撤销」按钮，等于允许两个人互相覆盖审计。
- 高度跟着条数走（`height:auto` + `max-height`）：5 条时不留一大片空白，多了才封顶内滚。

这里有个必须踩过才知道的坑：**自定义请求头的值只能是 ASCII**。最初把 `X-Admin-Actor`
写成「控制面板」，`fetch` 直接抛 `String contains non ISO-8859-1 code point`，
于是**所有** `/admin/*` 请求全挂（不只是变更记录这一处）。页面表现为「演示数据」，
看起来像鉴权失败，实际是请求根本没发出去。现在 wire 上是 `panel`，界面按 `HIST_ACTOR`
映射成人话，原始值留在 `title` 里。

外部改动**不打断编辑**：窗口回到前台时只取一次 `/admin/history?limit=1` 比对 `revision`，
不一致就把「变更」按钮点亮（小圆点 + 蓝色描边），不弹窗、不移焦点。点开面板才出现
「重新加载配置」，而且有未保存草稿时先问一句 —— 不做悄悄覆盖。

#### 刻意不做的

| 想加的 | 不加的理由 |
|---|---|
| `Idempotency-Key` 缓存 | 操作本身幂等，用结构消灭重复提交，比用缓存去重便宜 |
| 文件锁 / 分布式锁 | `If-Match` 已经能让调用方发现冲突；文件锁只能锁住单机，还会制造新的失败模式 |
| GraphQL / 通用 JSON Patch | 状态只有四个集合，九个操作就穷举完了，通用性带来的是学习成本 |
| 字段级权限 | 服务身份是部署者凭据，能改就是能改；细分权限需要第二套身份体系，与本项目前提冲突 |

## 12. 跑起来

```bash
go build ./... && go test ./...

# 最小配置（两项）
cat > config.yaml <<'EOF'
base_url: http://127.0.0.1:3000
api_key: sk-xxx
EOF

./mcpd -config config.yaml -addr :8080 -admin-addr :8081 \
  -ui prototype/admin.html -schema packages/model-capabilities.schema.json
```

MCP 入口 `POST http://127.0.0.1:8080/mcp`（Streamable HTTP，无状态）；
管理端 `http://127.0.0.1:8081/admin/*`，需 `Authorization: Bearer <服务身份 api_key>`。

打开 `http://127.0.0.1:8081/admin/ui`，在「调试台 → 连接 / 探针」填入服务身份 key 即可接真实数据；
也可用 `file://` 直接打开页面并加 `?api=http://127.0.0.1:8081`（CORS 只放行 localhost / 127.0.0.1 / `null` 来源）。
`-ui` 为空则不托管页面，`-schema` 为空则 `/admin/schema` 返回 503。

管理端口有两组端点。**配置读写**的完整清单见 §11（agent 直接读 `GET /admin/manifest`），
这里只列调试与静态资源：

| 端点 | 用途 |
|---|---|
| `GET /admin/state` | 一次返回服务端配置、上游归一化模型、覆盖层、`revision`、**后端合并结果**、漂移；上游失败时分项标 `ok=false`，不拿缓存冒充事实 |
| `GET /admin/schema` | 下发 `packages/model-capabilities.schema.json` 原文，前后端共用同一份契约 |
| `POST /admin/debug/mcp` | 进程内透传一条 JSON-RPC 到真实 MCP handler，回传原始请求 / 响应 / HTTP 状态 / 耗时 |
| `GET /admin/ui` | 托管 `prototype/admin.html`（每次读盘，改页面无需重启） |
| `GET /admin/healthz` | 健康检查：`base_url`、脱敏 key、`config_error`、`revision`、各类计数 |

除 `/admin/ui` 与 `/admin/healthz` 外全部走 Bearer 服务身份。

客户端配置：

```json
{
  "mcpServers": {
    "apiduo": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer sk-xxx" }
    }
  }
}
```

### 本地联调（不花钱）

`testdata/mock_upstream.py` 是一个假上游，覆盖了五种协议族各自的端点：
`/v1/models`、`/v1/images/generations`、`/v1/images/edits`（multipart）、`/v1/videos/generations`、
`/v1/contents/generations/tasks`（含查询）、`/v1/tools/search`、`/v1/tasks/:key`。

它对请求体是**严格校验**的：火山那条要求 `content` 数组且有 `model`，edits 那条要求真正的
multipart 文件 part。所以「mock 通过」至少能证明形状没拼错，而不是什么都收：

```bash
python3 testdata/mock_upstream.py 3999 &
# 配置里 base_url 指向 http://127.0.0.1:3999 即可跑通全部工具
```

### 管理端调试台：真实数据 / 真实调用 / Schema 自动填充与转换

`prototype/admin.html` 不再只是 mock 原型：`boot()` 读 `GET /admin/state`，`persist()` 走
`PUT /admin/overlays`（**全量提交所有模型的草稿**，后端校验后才落盘）。连不上服务时回落种子数据，
但顶栏、横幅、调试台全程标注「演示数据」，保存只在本地预览 —— 不把演示值冒充事实。

调试台四个页签：

| 页签 | 做什么 |
|---|---|
| 连接 / 探针 | 填服务身份；健康检查 / 状态快照 / 配置漂移 / 契约定义；**前后端合并一致性比对** |
| MCP 调用 | 从真实 `tools/list` 拿 `inputSchema` → 自动生成参数表单（enum→下拉、type→输入、default→预填）→ 转成 `arguments` JSON → 发起真实调用；表单与 JSON 可双向回填；右侧看原始报文与耗时，**生成类结果直接在报文上方出缩略图**（图片 / 视频，点击「打开原图」） |
| Schema 转换 | 粘贴模型对象 / `/v1/models` 响应 / 任意 JSON Schema → 按 `GET /admin/schema` 的契约做字段白名单与校验 → 生成**只含差异**的覆盖层并自动填充当前模型草稿；反向导出模型 JSON 或 JSON Schema |
| 记录 | 本次会话的调用历史，点任意一条回看完整请求与响应 |

两条红线：

- **计费双闸**：`image.generate` / `video.generate` / `search.query` 默认不发。前端要显式勾选确认，
  后端 `/admin/debug/mcp` 还强制要求 `X-Debug-Allow-Cost: yes`，缺一返回 428。新增计费工具时必须同步
  `internal/admin/debug.go` 的 `costTools` 白名单。
- **只填值，不发明字段**：转换只搬契约声明的 `kind` / `execution` / `input` / `parameters`，
  忽略的字段会在转换报告里列出；`max_reference_images` 缺失保持「未声明」，绝不补 0。
  保存的是与上游的**差异**，与上游相同的叶子自动剔除。

调试台同时把页面的 `mergeDeep` 结果与 Go 侧 `catalog.Merge` 的输出逐字段比对 ——
配置页最危险的状态是「页面显示一套、客户端拿到另一套」，这条断言能在保存前发现它。

#### 上游不声明产物类型时怎么办（真实上游实测）

聚合网关常常只在 `/models` 返回 `id` / `owned_by` / `supported_endpoint_types`，不给 `kind`。
实测 `newapi-bff.oneapis.cn`：**59 个模型里只有 6 个 `gpt-image-*` 能靠 `image-generation`
识别为图片，其余 53 个（含全部视频模型）是 `unknown`**，而 `KnownOnly` 会把它们从
`models.list` 里过滤掉 —— 客户端根本看不到。

处理流程（页面已内置，无需手写 YAML）：

1. 侧栏多出「未声明 N」一档，列出这些模型（「全部」里不含它们，与客户端可见性一致）；
2. 选中后标题下方出现「声明产物类型 → 图片 / 视频 / 搜索」，写入草稿；
3. 保存后 `kind` 落进覆盖层，模型立刻进入 `models.list`；
4. 若配置里定义了 `protocols.<kind>` 模板，`execution` / `input` / `parameters` 会自动继承，
   不必逐条填写。

⚠️ 模板选择按**合并后的 kind**，不是上游原始 kind（`catalog.ApplyOne` 的 `protocolFor`）。
早先读上游 kind，导致「只补了 kind」的模型拿不到模板，`Transport()` 退回默认 `generations` ——
视频模型会被打到 `/images/generations` 上。`TestOverlayKindSelectsProtocolTemplate` 守住这条。

#### 一次真实图片生成的完整证据（2026-11-04）

`gpt-image-2.5-sunburst`，提示词「一只戴针织帽的橘猫，柔和棚拍光，纯净浅灰背景，写实质感」：

- 调用路径 `/admin/debug/mcp` → 真实 MCP handler → 上游 `/v1/images/generations`；
- 返回 `structuredContent.outputs[0].url`，产物 1086×1448，`status=completed`；
- 报文里带 `usage`（input 27 / output 199 tokens），可直接核对计费口径；
- 调试台把该 URL 渲染成缩略图，无需再去 JSON 里翻链接。

调试调用超时为 **5 分钟**（与上游客户端一致）：图片、视频都是慢操作，2 分钟会把视频任务
误判成失败。

本地零消耗联调：先起 `python3 testdata/mock_upstream.py 3999`，配置里 `base_url` 指向
`http://127.0.0.1:3999`，页面连上后所有探针与工具调用都只打本机假上游。

#### 手改配置文件也会热更新（原来那条已知问题已修）

旧版本里 `base_url` / `api_key` 走 `config.Store` 的 mtime 热更新，但覆盖层是在 `admin.New`
时读入的一份快照，只有 `PUT /admin/overlays` 会更新它 —— 手改 `models` 必须重启。

根因不是缓存，是**同一份文件有两个读者**。现在 `admin.Handler` 不持有配置，读写都经过
`config.Store`：页面保存与手改文件是同一条路径，区别只是谁触发写入。
覆盖层变更后同样会失效模型缓存（`store.OnReload`），不需要额外的 `Reload()` 钩子。

`TestReloadPicksUpOverlayEdits` 与 `TestReplaceOverlaysPreservesHumanFile` 守住这两条：
改文件 2 秒内生效；写回不重排、不删注释。

### 当前实现状态

| 能力 | 状态 |
|---|---|
| 配置两项 + 热更新（失败保留旧值、变更后失效缓存） | ✅ |
| 模型画像拉取 / 覆盖层合并 / 校验（合并结果） | ✅ |
| `models.list` / `image.generate` / `video.generate` / `search.query` / `task.get` | ✅ |
| 参数白名单、参考图上限、enum/区间本地拦截 | ✅ |
| 异步：自包含 HMAC task_id（含协议族）+ `task.get` | ✅ |
| 五种协议族：generations / edits_multipart / video_generations / contents_tasks / search_query | ✅ |
| 出站防护：参考图下载限协议、限公网、限大小、限时长、限重定向 | ✅ |
| 管理端 API（GET/PUT overlays，校验后落盘） | ✅ |
| 覆盖层热更新：与部署配置同一个 `Store`，手改文件即生效 | ✅ |
| 写回保留注释、键序与部署配置；空段删键 | ✅ |
| 覆盖层版本号（内容哈希）+ `ETag` + `If-Match` 冲突检测（409） | ✅ |
| 细粒度管理 API：九个幂等操作 + 批量原子 + `dry_run` + 叶子级 `changes` | ✅ |
| 结构化校验：`issues[].path` + `hint`，可按字段定位 | ✅ |
| 自描述 `GET /admin/manifest`（端点从路由表生成，含错误码与常用流程） | ✅ |
| 管理端 MCP：`config.get/models/model/drift/history/apply`，写工具默认空跑 | ✅ |
| 变更记录 `<config>.audit.jsonl` + `GET /admin/history` | ✅ |
| 面板适配 `revision`（保存带 If-Match，409 提示刷新而不是盲目重发） | ✅ |
| 变更记录面板（谁在什么时候动了配置 · 外部改动只点亮按钮，不打断编辑） | ✅ |
| 公网边界加固（身份三态 / CORS 不鉴权 / 公开端点脱敏 / 约束兜底 / CSP） | ✅ |
| 三栏宽度随视口缩放（`clamp` 侧栏）+ 右栏可收起（`⌘\`，40px 脊） | ✅ |
| 校验分级（`issues` 阻断 / `warnings` 放行） | ✅ |
| 整条下架 `disabled` + 漂移自查 `GET /admin/drift` | ✅ |
| 协议族模板 `protocols`（按 kind 补缺 / `_protocol` 显式指定 / 漂移 `inherited`） | ✅ |
| 内置基础模板（image / video 开箱即用，不配 `protocols` 也生效） | ✅ |
| 管理端 UI（配置编辑 / 脏态 / 校验 / 差异视图 / 调试台） | ✅ 见 `prototype/admin.html` |
| 三栏 app-shell：面板等高、各自内滚、滚动边缘淡出、窄屏回落整页滚 | ✅ |
| 参数 / 输入上限可增删改：空状态给建议、自定义参数名、删除、来源分「新增 / 已覆盖」 | ✅ |
| `extra` 参数按画像校验 enum / min / max（越界本地拒绝，不打上游） | ✅ |
| 前端协议模板表单（折叠看生效值 / 展开看完整 JSON / 一键覆盖 / 自定义增删 / 即时校验 / 随保存提交） | ✅ |
| 未声明产物类型的模型可见、可声明，保存后进入客户端列表 | ✅ |
| 生成类产物在调试台内直接预览（图片 / 视频 + 打开原图） | ✅ |
| 管理端调试台（真实 state / drift / schema / MCP 调用 + 原始报文 + 调用记录） | ✅ |
| JSON Schema 自动填充 + 模型 JSON / Schema 双向转换（共用一份契约） | ✅ |
| 前后端合并一致性断言 | ✅ |
| 图片：火山 Ark + new-api 真实联调（文生图 / 图生图） | ✅ |
| 视频：火山 `contents_tasks` 真实联调 | ⚠️ 端点与请求体已验证到位（返回 `ModelNotOpen` 而非参数错误），**账号未开通视频模型**，未跑通完整生成 |
| 视频：new-api 形态真实联调 | ⬜ 只对接过 mock |
| 搜索：真实上游联调 | ⬜ 只对接过 mock |
| `edits_multipart` 真实联调 | ⬜ 只对接过 mock（经网关不需要它，只有直连 OpenAI 原厂才用） |
| 限流、审计、调用记录 | ⬜ 未做 |
| 提示词改写（`/v1/chat/completions`） | ⬜ 未做 |

### 容器镜像

每次发版同时产出一个镜像，**前端页面与能力契约已经打进去**——部署时不需要额外挂文件，
也不存在「二进制能跑、管理页 503」这种半残状态：

```bash
docker run -d --name mcpd \
  -p 8080:8080 -p 8081:8081 \
  -e BASE_URL=https://your-newapi.example.com \
  -e API_KEY=sk-xxx \
  ghcr.io/apiduo/mcp-for-apiduo:0.0.2
```

镜像里是什么、为什么是这个布局：

| 路径 | 用途 | 对应 flag 默认值 |
|---|---|---|
| `/app/mcpd` | 静态二进制（`CGO_ENABLED=0`） | — |
| `/app/prototype/admin.html` | 管理页 | `-ui prototype/admin.html` |
| `/app/packages/model-capabilities.schema.json` | 能力契约 | `-schema packages/...` |

三条路径与二进制的默认值**逐字对齐**，所以镜像里不传 `-ui` / `-schema` 也能直接工作 ——
这是「前后端打进同一个镜像」真正成立的条件，不是把文件 COPY 进去就完事。

基础镜像是 `distroless/static`（`nonroot`）：

- **不用 scratch**：没有 CA 证书，服务调 HTTPS 上游会直接失败；
- **不用 alpine**：`RUN apk add` 会让多架构构建引入 QEMU，慢且脆；
- 代价是没有 shell，`docker exec sh` 进不去，排障靠 `docker logs` 与 `/healthz`。

多架构（`linux/amd64` + `linux/arm64`）不需要 QEMU：镜像里没有任何 `RUN`，
只是把 CI 里已经交叉编译好的二进制装进去，所以构建是秒级。

要挂配置文件也可以，此时把配置目录挂成可写，审计日志 `<config>.audit.jsonl` 才落得下来：

```bash
docker run ... -v "$PWD:/app/conf" ghcr.io/apiduo/mcp-for-apiduo:0.0.2 -config /app/conf/config.yaml
```

包可见性由**组织策略**决定，不是仓库可见性：`apiduo` 组织已允许创建公开包
（组织下已有 `public` 的容器包），所以首次推送 `ghcr.io/apiduo/mcp-for-apiduo` 应当直接就是公开的。

推送后用一条命令核验（`gh` 需有 `read:packages`）：

```bash
gh api /orgs/apiduo/packages/container/mcp-for-apiduo --jq .visibility   # 期望 public
```

万一仍是 private，只能去包设置页改一次：`GITHUB_TOKEN` 对包可见性的 PATCH 恒 404 ——
这是 REST 路由限制，不是权限没给够，别在 workflow 里加这一步（只会得到一条「假绿」的
`continue-on-error`）。

### 发版（push main 即发版）

`.github/workflows/release.yml`：推到 `main` → 自增 patch（`v0.0.1` → `v0.0.2`）→ 跑 `go vet` + `go test`
→ 交叉编译五个平台到 `dist/` → 冒烟断言二进制**自报版本**等于发布版本 → 打 annotated tag → 建 Release
（附 `SHA256SUMS`）。

- **版本号只活在 tag 里**，不写回任何文件。要 minor / major 就人工打 tag，下一次自增以它为基线。
- 不想发版的提交（纯文档、注释）在提交信息里写 `[skip release]`，整场跳过；漏了就 `workflow_dispatch` 补跑。
- 顺序是「先出产物、再打 tag」：构建或冒烟失败不会留下有 tag、没产物的半成品；Release 步骤幂等，可重跑。
- 五个平台：`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`。

### 公网边界：一次对抗审查的四个结论（2026-11-05）

线上跑起来之后做了一轮对抗测试，四个真问题，全部是「每一层单独看都合理，
接起来就漏」的那一类。

**1. `/mcp` 的身份回落可被任意头触发（严重）**

公网 nginx 的判据是「`Authorization` 非空」，服务端的判据是「能解析成 `Bearer <token>`」。
两者中间的差值就是洞：`Authorization: x` 能穿过 nginx，而服务端把「解析不出来」
当成「没带」，回落到配置里的服务身份 —— 匿名调用 `models.list` 直接返回了上游模型列表，
换成 `image.generate` 就是花部署者的额度。

第一性原理：**「没带凭证」和「带了坏凭证」是两件事**，前者可以回落到单租户身份，
后者必须 401。`bearer()` 现在返回 `(token, present)`，调用方无法再把两者混为一谈；
nginx 侧也改成 `^Bearer[ \t]+\S`。两层各说各的，所以要两层都改。

**2. CORS 把自家面板挡在门外（严重，功能）**

线上面板打开正常、读得到数据，但**一保存就 403**。原因是 CORS 中间件把所有
「非本机 Origin」一律拒绝，而面板跑在 `https://mcp.apiduo.cn`：
浏览器对**同源**的 POST/PUT 同样会带 `Origin`（GET 不带），于是每个写请求都被自己挡掉。

第一性原理：**CORS 不是鉴权**。凭证是 sessionStorage 里的 Bearer，跨站页面读不到，
也过不了预检；非白名单来源根本不需要 403 —— 它没有凭证，只会拿到 401。
现在 CORS 只做一件事：给白名单来源补响应头。请求本身照常处理。

**3. 公开探活会吐密钥片段与内网服务名**

`/admin/healthz` 是免鉴权的，却返回了 `api_key: "sk-5***30"` 和
`base_url: "http://apiduo:3000/v1"`。掩码不是脱敏 —— 前 4 位 + 后 2 位足够确认
「这把 key 属于哪一批」，内网服务名则是拓扑信息。公开端点现在只回答
`ok / version / config_ok / revision / 计数`。

**4. 「声明的约束必须拦得住调用」只做到了一半**

`search.query` 的 `extra` 只检查键是否存在，不校验 enum/min/max ——
配置里写的 `count ≤ 10` 在这个入口上等于一句注释。
另外 `n` 在模型没声明 `max` 时会原样透传，一次 `n=1000` 就能把额度打光。
现在两个入口共用 `checkExtra`；未声明上限时 `n ≤ 8`。

顺手补的几处（都不改变对外契约）：服务端 API key 比较改常量时间；
`http.Server` 补 `ReadTimeout` / `IdleTimeout`（不设 `WriteTimeout`，否则 5 分钟的
同步生成会被掐断）；审计记 `X-Real-IP`（只在直连方是本机时采信，否则永远是 127.0.0.1）；
审计文件超过 8 MiB 裁到最近 500 行；`/admin/ui` 下发 CSP / `X-Frame-Options` / `nosniff`，
vhost 加 HSTS。

## 13. 文档

- [`docs/NewAPI图片MCP扩展中心解决方案.md`](docs/NewAPI图片MCP扩展中心解决方案.md) — V1.0 完整目标方案（含控制面与扩展中心形态，属于进阶阶段）

## 14. 真实上游给的六个教训

用火山 Ark 的真实接口对打之后，暴露出的都不是「写错了」，而是「想当然了」：

**1. base_url 的版本前缀不能猜**
火山是 `/api/v3`，new-api 是 `/v1`。早先把 base_url 当纯域名、路径硬拼 `/v1/models`，
请求直接打到 `/api/v3/v1/models` → 404。
现在：base_url 是 API 根，路径相对它拼；只给域名时自动补 `/v1`，给了路径就原样相信、不改写。

**2. `kind` 的缺省值不能是 image**
火山 `/models` 返回 55 个模型，只有 6 个是 ImageGeneration，其余是 LLM / VLM / Embedding。
缺省成 image 会让它们全部混进图片工具，默认模型更可能选中一个语言模型 —— 然后对着图片端点发请求。
现在：缺省是「未知」，由 `Normalize` 从上游声明的结构化字段推导，推不出来就保持未知、不参与任何生成类工具。

目前支持的三类证据，按强弱排序：

| 证据 | 来源 | 例 |
|---|---|---|
| `supported_endpoint_types` | new-api 系列 | `["image-generation", "openai"]` → image |
| `modalities.output_modalities` | 火山 Ark 等 | `["video"]` → video |
| `domain` | 火山 Ark | `ImageGeneration` → image |

注意 `supported_endpoint_types` 里混装了两类信息：**产物类型**（`image-generation`）和**协议类型**
（`openai` / `gemini`）。只有前者能推导产物，后者推不出来 —— 所以映射必须是白名单，
不能"看到 openai 就当 OpenAI 模型、就能生图"。

**3. 「未声明」不等于「不支持」**
火山不提供 `max_reference_images`。早先按缺省 0 处理，直接把 seedream 的图生图锁死了 ——
而它明明声明了 `task_type: [text_to_image, image_to_image]`。
同理，模型没有 `parameters` 画像时，`size` / `n` 这类标准参数必须仍然放行，否则模型等于不可用。

**4. 一个 transport 对应一整套端点，不是一个 URL**
火山的视频生成与查询都在 `/contents/generations/tasks` 之下（查询是 `…/{id}`），
new-api 是 `/videos/generations` + `/tasks/{id}`。只按生成端点区分 transport，
`task.get` 会拿着火山的任务键去打 `/tasks/{id}` —— 空 body 的 404。

现在 transport 是**协议族**：生成端点、查询端点、编码方式、字段映射四件绑在一起（见 §10），
而且**协议族名随 task_id 一起签名传递**。查询时不回查模型列表：模型可能在任务跑完前就下架了，
那时再去查就查不到该用哪个端点。

**5. 网关会把上游差异抹平，这是「直连网关」的主要理由**

原本以为 `gpt-image` 的图生图必须走 `/images/edits`（multipart），所以准备了 `edits_multipart` 这个 transport。
实测走 new-api 时，同一个 `image` 数组 + `/images/generations` 就被接受了，
而且 `usage.input_tokens_details.image_tokens = 198`（文生图时这个值是 0）——
证明参考图真的被上游编码，不是静默忽略。

结论：`edits_multipart` 只在**直连 OpenAI 原厂**时才需要，通过网关时是多余的一档。
这也是「优先接网关、次选直连原厂」的实际理由。

**6. 验证方法本身也会骗人**

第一次验证火山视频时，我拿一个未开通的视频模型调 `video.generate`，收到 `ModelNotOpen`，
于是判定「路径对了」。这是**假阳性**：那次 overlay 因为管理端的一个 bug 根本没存进去，
请求实际打的是 `/images/generations`（在火山上也是有效端点），而它对一个未开通的模型
同样返回 `ModelNotOpen`。

两处修正：
- **上游错误带上实际方法与路径**：`upstream 404 ModelNotOpen (POST /contents/generations/tasks)`。
  一眼能看出是「打错了地方」还是「参数不对」—— 在此之前这两种情况长得一模一样。
- **管理端校验补上归一化**：它就是那个让 overlay 悄悄存不进去的 bug。
  校验用未归一化的模型，`kind` 是空串，于是「默认 video 模型」被判成类型不符。

教训：**用一个「注定失败」的调用去证明「路径对了」，本身就不成立** ——
失败的原因可能有很多个，而你只看到了其中一个。

六条的共同点：**错误都表现为功能静默缺失，而不是报错**。
这也是为什么必须拿真上游打一遍 —— mock 会把所有假设都验证成"对的"。

## 15. 不做什么

- 不做渠道调度、模型映射、额度与计费 —— 全部留在 new-api。
- 不建第二套身份体系：没有注册、没有票据、没有委托凭证。
- 不为每个新模型写一个 MCP 工具：固定工具集 + 模型发现。
- 不把图片 Base64 塞进协议响应：转存后返回受控 URL。
- 不在 MCP Server 里保存上游供应商密钥。
