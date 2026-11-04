#!/usr/bin/env bash
# mcp.apiduo.cn 一键落地（幂等；DNS 未就绪会拒绝执行）
#
# 用法（在 ai-prod 上跑，root）：
#   bash /opt/mcp-for-apiduo/nginx/apply-https.sh
#
# 做什么：DNS 预检 → ACME 通道自检 → acme.sh 签发（HTTP-01 webroot）
#        → install-cert（含续期 reloadcmd）→ 投 :443 vhost → nginx -t + reload
#        → 跑验证矩阵（含「管理端不对外」这条反向断言）
set -euo pipefail

HOST="${HOST:-mcp.apiduo.cn}"
IP_EXPECT="${IP_EXPECT:-210.121.44.245}"
VHOST_DIR=/www/server/panel/vhost/nginx
CERT_DIR="/www/server/panel/vhost/cert/$HOST"
ACME=/root/.acme.sh/acme.sh
NGINX=/www/server/nginx/sbin/nginx
SRC_DIR=/opt/mcp-for-apiduo/nginx

echo "== 1) DNS 预检：$HOST 必须已指向 $IP_EXPECT =="
GOT=""
for R in dns13.hichina.com dns14.hichina.com 8.8.8.8 1.1.1.1 223.5.5.5; do
  GOT=$(dig +short "$HOST" @"$R" 2>/dev/null | grep -E '^[0-9]+\.' | head -1 || true)
  [ "$GOT" = "$IP_EXPECT" ] && break
done
if [ "$GOT" != "$IP_EXPECT" ]; then
  echo "❌ DNS 未就绪：$HOST -> '${GOT:-<空>}'（期望 $IP_EXPECT）—— 先加 A 记录再跑本脚本"
  exit 2
fi
echo "✅ $HOST -> $GOT"

echo "== 2) ACME 通道自检（挑战文件必须能经本 vhost 取回）=="
mkdir -p /www/wwwroot/acme/.well-known/acme-challenge
echo "preflight-$$" > /www/wwwroot/acme/.well-known/acme-challenge/preflight
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" http://127.0.0.1/.well-known/acme-challenge/preflight || true)
[ "$CODE" = "200" ] || { echo "❌ 挑战路径返回 $CODE（应 200）—— 检查 $VHOST_DIR/$HOST.conf 的 ^~ location"; exit 3; }
echo "✅ 挑战路径 200"

echo "== 3) 签发证书（已存在则跳过）=="
if [ -d "/root/.acme.sh/${HOST}_ecc" ]; then
  echo "· 证书已存在，跳过签发"
else
  "$ACME" --issue -d "$HOST" --webroot /www/wwwroot/acme --keylength ec-256
fi
mkdir -p "$CERT_DIR"
"$ACME" --install-cert -d "$HOST" --ecc \
  --key-file       "$CERT_DIR/privkey.pem" \
  --fullchain-file "$CERT_DIR/fullchain.pem" \
  --reloadcmd      "$NGINX -s reload"
echo "✅ 证书就位：$CERT_DIR"

echo "== 4) 投 :443 vhost（含 -proxy.inc）并 reload（幂等）=="
SSL_CONF="$VHOST_DIR/$HOST-ssl.conf"
INC_CONF="$VHOST_DIR/mcp-for-apiduo-proxy.inc"
[ -f "$INC_CONF" ] || cp -f "$SRC_DIR/mcp-for-apiduo-proxy.inc" "$INC_CONF"
INC_ADMIN="$VHOST_DIR/mcp-for-apiduo-admin.inc"
[ -f "$INC_ADMIN" ] || cp -f "$SRC_DIR/mcp-for-apiduo-admin.inc" "$INC_ADMIN"
if [ ! -f "$SSL_CONF" ]; then
  cp -f "$SRC_DIR/$HOST-ssl.conf" "$SSL_CONF"
  echo "· 已写入 $SSL_CONF"
else
  echo "· $SSL_CONF 已存在，跳过"
fi
# ⚠️ `nginx -t && nginx -s reload` 写成一行是错的：set -e 对 AND-OR 列表首命令不生效，
#    -t 失败会被吞掉，脚本会照样打印「✅ 完成」。必须分两行。
"$NGINX" -t
"$NGINX" -s reload
echo "✅ nginx reloaded"

echo "== 5) 验证矩阵 =="
R="--resolve $HOST:443:127.0.0.1"
# reload 是异步的：老 worker 还会服务片刻，立刻断言会拿到旧配置
for _ in $(seq 1 20); do
  [ "$(curl -sk -o /dev/null -w '%{http_code}' $R "https://$HOST/healthz")" = "200" ] && break
  sleep 0.5
done

S80=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOST" http://127.0.0.1/healthz)
S443=$(curl -sk -o /dev/null -w '%{http_code}' $R "https://$HOST/healthz")
ROOT=$(curl -sk $R "https://$HOST/" | grep -o '"mcp":"/mcp"' | head -1)
TOKEN=$(cat /root/.mcp-apiduo-token 2>/dev/null || true)
ADMIN_UI=$(curl -sk -o /dev/null -w '%{http_code}' $R "https://$HOST/admin/ui")
ADMIN_ANON=$(curl -sk -o /dev/null -w '%{http_code}' $R "https://$HOST/admin/config")
ADMIN_AUTH=$(curl -sk -o /dev/null -w '%{http_code}' $R -H "Authorization: Bearer $TOKEN" "https://$HOST/admin/config")
INIT_BODY='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}'
# 匿名必须 401：没有这道闸，服务会回落到内置服务 key（等于对全网开放额度）
ANON=$(curl -sk -o /dev/null -w '%{http_code}' $R -H 'Content-Type: application/json' \
        -H 'Accept: application/json, text/event-stream' \
        -H 'MCP-Protocol-Version: 2025-06-18' \
        -d "$INIT_BODY" "https://$HOST/mcp")
INIT=$(curl -sk $R -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
        -H 'Accept: application/json, text/event-stream' \
        -H 'MCP-Protocol-Version: 2025-06-18' \
        -d "$INIT_BODY" "https://$HOST/mcp" | grep -o '"serverInfo"' | head -1)

printf '%-34s %s\n' ":80 /healthz → 301"        "$S80"
printf '%-34s %s\n' ":443 /healthz"             "$S443"
printf '%-34s %s\n' ":443 / （服务说明）"        "${ROOT:-<缺失>}"
printf '%-34s %s\n' ":443 /admin/ui → 200"      "$ADMIN_UI"
printf '%-34s %s\n' ":443 /admin/config 无 key → 401" "$ADMIN_ANON"
printf '%-34s %s\n' ":443 /admin/config 带 key"  "$ADMIN_AUTH"
printf '%-34s %s\n' ":443 /mcp 匿名 → 401"      "$ANON"
printf '%-34s %s\n' ":443 /mcp 带 key → init"   "${INIT:-<无 serverInfo>}"

if [ "$S80" = "301" ] && [ "$S443" = "200" ] && [ -n "$ROOT" ] && \
   [ "$ADMIN_UI" = "200" ] && [ "$ADMIN_ANON" = "401" ] && [ "$ADMIN_AUTH" = "200" ] && \
   [ "$ANON" = "401" ] && [ -n "$INIT" ]; then
  echo "✅ 全部通过：https://$HOST/mcp"
else
  echo "❌ 有断言未通过，见上表"
  exit 4
fi
