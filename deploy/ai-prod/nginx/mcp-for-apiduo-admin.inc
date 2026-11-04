# 管理端反代片段 —— 后端固定 127.0.0.1:39091（只绑回环，公网只能经这里进）。
#
# 边界不在 nginx，在服务自己：
#   - /admin/ui、/admin/healthz 是 Public（页面与探活），无需 key；
#   - 其余 /admin/* 一律要求 Authorization: Bearer <服务 key>，否则 401。
# 页面是纯静态 HTML，不含任何数据；所有读写都要 key，所以「能打开页面」不等于「能改配置」。
proxy_pass http://127.0.0.1:39091;
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header Connection        "";

# 管理端会提交整份覆盖层（JSON，几十 KB 量级），与 /mcp 一样留 8m 余量。
client_max_body_size 8m;
proxy_read_timeout 120s;
proxy_send_timeout 120s;
