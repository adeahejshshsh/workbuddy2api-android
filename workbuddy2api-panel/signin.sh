#!/usr/bin/env bash
# 用法：服务器/容器内可直接 ./signin.sh（Docker 镜像里 chmod 755 后执行）；
#      本机（Android）请用 `bash signin.sh` —— /sdcard 是 noexec 挂载，
#      直接 ./ 执行会在读 shebang 之前就 Permission denied。
#      shebang 保持 /usr/bin/env bash 是为了 Docker（alpine + apk add bash）可用。
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

# 二进制定位（bug-03）：Docker 镜像里是 ./signin_bin（COPY /out/signin_bin /app/），
# 本机 rebuild.sh 的产物是 $PREFIX/lib/workbuddy2api/signin —— 原先只会现场
# `go build`，而本机 go 不在 PATH（在 $PREFIX/lib/go/bin），set -e 下直接退出。
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
BIN=""
for c in ./signin_bin "$PREFIX/lib/workbuddy2api/signin"; do
    [ -x "$c" ] && { BIN="$c"; break; }
done
if [ -z "$BIN" ]; then
    echo "build signin_bin ..."
    export PATH="$PREFIX/lib/go/bin:$PATH"   # 本机 go 的落点（bug-03）
    go build -o ./signin_bin ./cmd/signin
    BIN=./signin_bin
fi

exec "$BIN" "${1:-auths}"
