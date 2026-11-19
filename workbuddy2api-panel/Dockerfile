# syntax=docker/dockerfile:1
# 基础镜像必须满足 go.mod 的 `go 1.24`（审查 E2）：Go 1.21+ 会强制最低版本，
# 用 1.23 构建直接报 "go.mod requires go >= 1.24"。此前是 1.23，镜像链实际不可用。
#
# W6-M-09：tag 固定到 alpine 小版本 + 钉住 manifest digest。
# `1.24-alpine` 会随 alpine 大版本漂移（昨天能构建、今天基础库换了就是「什么都没改却
# 起不来」），而只写 tag 时上游还能重新推送同一个 tag。digest 取自 Docker Hub
# （2026-11 查）：docker buildx imagetools inspect golang:1.24-alpine3.20
FROM golang:1.24-alpine3.20@sha256:9f98e9893fbc798c710f3432baa1e0ac6127799127c3101d2c263c3a954f0abe AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 版本号（bug 13）：镜像此前只注入 -s -w，main.appVersion 保持兜底值 "dev"，
# /status 与面板左上角全显示 vdev，排障/升级判断没有依据。
# 优先级：构建参数 APP_VERSION → 上下文里的 VERSION → git tag → dev。
# 用法：docker build --build-arg APP_VERSION=$(cat VERSION) .
ARG APP_VERSION=""
# 构建源改用国内镜像（上游 PR #230）：默认 proxy.golang.org 在国内常被墙。
ENV GOPROXY=https://goproxy.cn,direct
RUN VER="${APP_VERSION:-$(cat VERSION 2>/dev/null || git describe --tags --abbrev=0 2>/dev/null || echo dev)}" \
 && VER="${VER#v}" \
 && LDF="-s -w -X main.appVersion=$VER" \
 && echo "构建版本: $VER" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDF" -o /out/wb2api ./cmd/server \
 # 注：本仓没有 cmd/signin（上游有，这里没搬），此前这行让 `docker build` 必然失败
 # （审查 E2）。签到改由 /panel/api 与调度器驱动，不需要独立二进制。
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial ./cmd/trial

# 运行镜像同样固定小版本 + digest：`alpine:3.20` 是浮动 tag，3.20.x 的 busybox/libc
# 变化会直接改变运行期行为（本镜像的 HEALTHCHECK 用的就是 busybox wget）。
# 3.20 分支当前 patch 是 3.20.10，写死它而不是 3.20.3，避免钉在一个带已知 CVE 的旧包上。
FROM alpine:3.20.10@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
# 本仓没有 signin_bin（审查 E2）：上游那个二进制依赖 cmd/signin，这里没搬。
# 少这一行不会报错，但留着会让 build 在 COPY 阶段直接失败（找不到 /out/signin_bin）。
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial /app/trial
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
# 首次启动兜底（issue ISSUE-01）：容器内必须对外监听，且必须有 key。
COPY docker/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/livez || exit 1
ENTRYPOINT ["/app/entrypoint.sh"]