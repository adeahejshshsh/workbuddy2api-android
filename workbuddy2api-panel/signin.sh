#!/usr/bin/env bash
# 用法：服务器/容器内可直接 ./signin.sh（Docker 镜像里 chmod 755 后执行）；
#      本机（Android）请用 `bash signin.sh` —— /sdcard 是 noexec 挂载，
#      直接 ./ 执行会在读 shebang 之前就 Permission denied。
#      shebang 保持 /usr/bin/env bash 是为了 Docker（alpine + apk add bash）可用。
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
if [ ! -x "$BIN" ]; then
    echo "build signin_bin ..."
    go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
