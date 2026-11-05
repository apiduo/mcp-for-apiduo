# 运行时镜像：Go 静态二进制 + 管理页 + 能力契约，一个镜像自包含。
#
# 为什么不逐层 build：
#   - 二进制在 CI 里已经交叉编译过了（CGO_ENABLED=0），镜像只做 COPY，
#     不做编译 —— 多架构构建因此不需要 QEMU，速度是秒级。
#   - 前端页面与 schema 是运行时依赖（-ui / -schema 的默认值就指向这两个路径），
#     不打进来就会得到「管理页 503」。
#
# 为什么用 distroless 而不是 scratch / alpine：
#   - scratch 没有 CA 证书，服务调 HTTPS 上游直接失败；
#   - alpine 要 RUN apk add，多架构构建会引入 QEMU（慢且脆）；
#   - distroless/static 自带 CA 与 tzdata，没有 shell，攻击面最小。
FROM gcr.io/distroless/static-debian12:nonroot

# buildx 会按目标平台自动注入；普通 docker build 需显式 --build-arg。
# 构建产物在 dist/ 里按 GOOS-GOARCH 命名（dist/mcpd-linux-amd64）。
ARG TARGETARCH

WORKDIR /app

COPY dist/mcpd-linux-${TARGETARCH} /app/mcpd

# 与 flag 默认值对齐：不传 -ui / -schema 也能正常托管。
COPY prototype/admin.html /app/prototype/admin.html
COPY prototype/playground.html /app/prototype/playground.html
COPY packages/model-capabilities.schema.json /app/packages/model-capabilities.schema.json

# nonroot（uid 65532）。二进制与静态文件都是只读挂载，不需要写权限。
USER nonroot:nonroot

EXPOSE 8080 8081

# 不给 -config 默认值：镜像默认走环境变量（BASE_URL / API_KEY），
# 直接 docker run -e ... 就能起。要挂配置文件就自己追加 -config。
ENTRYPOINT ["/app/mcpd"]
