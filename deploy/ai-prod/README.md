# mcp-for-apiduo · ai-prod 部署

域名 **[mcp.apiduo.cn](https://mcp.apiduo.cn)**（公开）· 主机 `210.121.44.245` · 容器名 `mcp-for-apiduo`

## 拓扑

```
公网 → nginx :443 (mcp.apiduo.cn)
         ├── /mcp        → 127.0.0.1:39090 → 容器 :8080 → apiduo:3000 (new-api, apiduo_default 网络)
         ├── /playground → 同上（客户调试台，公开静态页）
         └── /healthz    → 同上
       /admin/*  → 127.0.0.1:39091（同域放行；页面公开，读写要 key）
```

- 容器与 `apiduo`（new-api）同在 `apiduo_default` 网络，用容器名直连，不走公网、也不依赖宿主回环端口。
- 端口一律绑 `127.0.0.1`：公网入口只有 nginx（管理端也是，公网只能经 nginx 进）。
- 配置在 `/www/data/mcp-for-apiduo/config.yaml`（属主 65532，容器以 nonroot 运行）；覆盖层与审计日志也写在这里。

## 日常操作

```bash
# 看状态 / 日志
ssh -i ~/.ssh/id_ed25519_ops root@210.121.44.245 'cd /opt/mcp-for-apiduo && docker compose ps && docker logs --tail 50 mcp-for-apiduo'

# 升级：改 .env 里的版本号（镜像 tag 不带 v），再重建
ssh ... 'cd /opt/mcp-for-apiduo && sed -i "s/^MCP_IMAGE_TAG=.*/MCP_IMAGE_TAG=0.0.8/" .env && docker compose pull -q && docker compose up -d'

# 回滚：把 .env 指回上一个版本即可（.env.bak 是上一次升级前的值）
ssh ... 'cd /opt/mcp-for-apiduo && sed -i "s/^MCP_IMAGE_TAG=.*/MCP_IMAGE_TAG=0.0.7/" .env && docker compose pull -q && docker compose up -d'

# 管理面板（不对外，走隧道）
ssh -i ~/.ssh/id_ed25519_ops -N -L 39091:127.0.0.1:39091 root@210.121.44.245
# 然后浏览器开 http://127.0.0.1:39091/admin/ui ，服务 key 在服务器 /root/.mcp-apiduo-token
```

> ⚠️ `/opt/mcp-for-apiduo/` 下的 `docker-compose.yml` 与 nginx 片段是**本目录的副本**，
> 不是软链。改本目录之后必须同步过去并 `docker compose up -d` —— 2026-11-05 就踩过：
> 只改了仓库里的 `command`（加 `-trust-proxy`），服务器上的副本没变，参数等于没加，
> 审计日志里的来源依旧全是 Docker 网关地址。

## 管理端为什么放开了（一次有意的偏离）

服务设计上管理端「只在内网暴露」，本部署**没有遵守**这条 —— 2026-11-05 部署者裁定同域放行，
理由是运维便利。准确的边界因此变成：

| 路径 | 鉴权 | 说明 |
|---|---|---|
| `/admin/ui` | 无 | 纯静态 HTML（约 200KB），不含任何数据 |
| `/admin/healthz` | 无 | 探活 |
| `/playground` | 无 | 客户调试台，公开静态页；用访问者自己填的 Key 调 `/mcp`，不碰服务身份 |
| 其余 `/admin/*` | **Bearer 服务 key** | 没有 key 一律 401；面板打开后要在「连接 / 探针」里填 key |

所以「能打开页面」不等于「能改配置」。`/playground` 同理：它只是一张表单，
Key 填在浏览器里、走 `/mcp` 的 Bearer，页面本身不持有任何凭据。

升级后确认一下两个页面都在：

```bash
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' https://mcp.apiduo.cn/playground
curl -s https://mcp.apiduo.cn/ | grep -o '"playground":"[^"]*"'
```
但必须承认：**拿到 key 的人可以改配置，也能调生成工具花钱**。
用这个 key 的地方还有 new-api 的令牌列表（名称 `mcp-apiduo`），泄露后两步都要处理：
`docker exec apiduo` 里删令牌 + 重新生成 → 更新 `/www/data/mcp-for-apiduo/config.yaml` → 重启容器。

## 为什么公网 `/mcp` 强制要 Authorization

服务在**没有** `Authorization` 时会回落到配置里的服务身份 key（单租户内网用）。
公网入口如果不管，任何人匿名调用都在花我们的 new-api 额度。所以 nginx 在 `/mcp`
上直接拦空头 → 401，并回 `WWW-Authenticate: Bearer`。

调用方带自己的 new-api key 即可；管理端仍然只认服务身份 key，且只在回环可达。

## 证书与 DNS

- 证书：`acme.sh`（Let's Encrypt，HTTP-01 webroot），装在
  `/www/server/panel/vhost/cert/mcp.apiduo.cn/`，续期自动 reload。
- DNS：阿里云 `apiduo.cn`，A 记录 `mcp → 210.121.44.245`，TTL 600。
- 重放整条链路（幂等）：`bash /opt/mcp-for-apiduo/nginx/apply-https.sh`
  它会依次做 DNS 预检 → ACME 通道自检 → 签发 → 投 :443 → reload → 五项断言
  （含「匿名 401」「/admin 404」两条反向断言）。

## 变更记录

| 日期 | 变更 |
|---|---|
| 2026-11-05 | 首次部署 0.0.3；建专用令牌 `mcp-apiduo`（new-api tokens 表 id 13）；DNS + 证书落地 |
| 2026-11-05 | 管理端改为同域放行（`/admin/*` 反代 39091），验证矩阵加「无 key 401 / 带 key 200」 |
| 2026-11-05 | 升级 0.0.4（面板新增「变更记录」时间线）；`.env.bak` 留 0.0.3 供回滚 |
| 2026-11-05 | 升级 0.0.5（三栏宽度随视口缩放 + 右栏可收起）；`.env.bak` 留 0.0.4 供回滚 |
| 2026-11-05 | 对抗审查后 0.0.6/0.0.7：身份三态、CORS 不再当鉴权、探活脱敏、CSP、`-trust-proxy`；nginx `/mcp` 判据收紧为 `^Bearer[ \t]+\S` |
| 2026-11-05 | 0.0.8：客户调试台 `/playground` 上线（镜像内页面 + nginx 放行）；上游模型列表 30s 缓存、CLI 包纳入发版测试。升级顺序：先切容器并直连验页面，再 reload nginx |
