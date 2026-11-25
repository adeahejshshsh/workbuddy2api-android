#!/usr/bin/env bash
# 用法：服务器/容器内可直接 ./signin.sh（Docker 镜像里 chmod 755 后执行）；
#      脚本所在卷禁止执行时请用 `bash signin.sh`（否则 ./ 会在读 shebang 之前就
#      Permission denied）。shebang 保持 /usr/bin/env bash 以便 Docker 可用。
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

# 二进制定位（bug-03）：Docker 镜像里是 ./signin_bin（COPY /out/signin_bin /app/），
# 按安装前缀部署时产物是 $PREFIX/lib/workbuddy2api/signin —— 原先只会现场
# `go build`，而 go 不在 PATH（在 $PREFIX/lib/go/bin），set -e 下直接退出。
PREFIX="${PREFIX:-$HOME/.local}"
BIN=""
for c in ./signin_bin "$PREFIX/lib/workbuddy2api/signin"; do
    [ -x "$c" ] && { BIN="$c"; break; }
done
if [ -z "$BIN" ]; then
    echo "build signin_bin ..."
    export PATH="$PREFIX/lib/go/bin:$PATH"   # Go 装在安装前缀下时的落点（bug-03）
    go build -o ./signin_bin ./cmd/signin
    BIN=./signin_bin
fi

exec "$BIN" "${1:-auths}"
