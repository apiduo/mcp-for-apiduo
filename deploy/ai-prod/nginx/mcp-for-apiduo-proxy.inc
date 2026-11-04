# mcp-for-apiduo 公共反代片段 —— 被 mcp.apiduo.cn-ssl.conf 的 location 引用。
# 后端固定回环 127.0.0.1:39090（容器端口只绑回环）。
proxy_pass http://127.0.0.1:39090;
proxy_http_version 1.1;
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

# ⚠️ 必须显式清空：照抄 WebSocket 模板写 upgrade 会让每个请求都声明要升级，
#    Go 的 http.Server 于是不复用连接。MCP 的 Streamable HTTP 是普通 HTTP
#    （可能返回 SSE 流），不需要 Upgrade 语义。
proxy_set_header Connection        "";

# MCP 请求体是 JSON，正常很小；留点余量给带 base64 的调试报文。
client_max_body_size 8m;

# 视频生成是异步的，但 /mcp 的调用本身不等待上游完成任务，30s 足够；
# 留 120s 给调试台里同步生成图片的场景。
proxy_read_timeout 120s;
proxy_send_timeout 120s;
