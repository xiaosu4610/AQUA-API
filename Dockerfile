# AQUA-API 多阶段构建：前端产物内嵌进 Go 二进制，最终镜像只含一个可执行文件。
#
# 为什么分三段：
#   1) 前端构建需要 Node，运行期完全用不到；
#   2) 后端用纯 Go（CGO_ENABLED=0）静态编译，运行期不需要 gcc 与 glibc；
#   3) 最终镜像用 alpine，体积最小、攻击面最小。
#
# 构建：docker build -t aqua-api:local .
# 运行：docker run -d --name aqua -p 8787:8787 -e AQUA_APP_KEY=<主密钥> -v $PWD/data:/data aqua-api:local

# ---------- 1. 前端构建 ----------
FROM node:20-alpine AS web
WORKDIR /src/web
# 先只拷贝依赖清单：依赖不变时可复用缓存层，改代码不会重装依赖
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---------- 2. 后端构建 ----------
FROM golang:1.27-alpine AS build
WORKDIR /src
# 同理：先拉依赖，再拷源码
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用第 1 段的前端产物覆盖占位目录，go:embed 才能把界面打进二进制
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/aqua ./cmd/aqua

# ---------- 3. 运行 ----------
FROM alpine:3.20
# ca-certificates：访问 HTTPS 上游必需；tzdata：日志时间与支付签名的时间戳依赖时区
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 aqua \
    && mkdir -p /data && chown aqua:aqua /data
COPY --from=build /out/aqua /usr/local/bin/aqua

# 以非 root 运行：容器逃逸时拿到的也只是普通用户
USER aqua
WORKDIR /data

# 容器内必须监听 0.0.0.0，否则宿主无法访问
ENV AQUA_SERVER_LISTEN=0.0.0.0:8787 \
    AQUA_DATABASE_DSN=/data/aqua.db \
    AQUA_SERVER_MODE=release

VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["aqua"]
