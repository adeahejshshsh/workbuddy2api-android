#!/usr/bin/env bash
# 用法：服务器/容器内可直接 ./credit.sh（Docker 镜像里 chmod 755 后执行）；
#      本机（Android）请用 `bash credit.sh` —— /sdcard 是 noexec 挂载，
#      直接 ./ 执行会在读 shebang 之前就 Permission denied。
#      shebang 保持 /usr/bin/env bash 是为了 Docker（alpine + apk add bash）可用。
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制升级: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"

# 二进制定位（bug-03）：Docker 镜像里产物就在当前目录（COPY /out/credit /app/credit），
# 而本机 rebuild.sh 把产物放到 $PREFIX/lib/workbuddy2api/ —— 仓库里**没有** ./credit，
# 所以原先的 `exec ./credit` 在手机上必然 "No such file or directory"。
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
BIN=""
for c in ./credit "$PREFIX/lib/workbuddy2api/credit"; do
    [ -x "$c" ] && { BIN="$c"; break; }
done
[ -n "$BIN" ] || { echo "❌ 找不到 credit 二进制（先跑 bash rebuild.sh）" >&2; exit 1; }

if [[ "${1:-}" == "-json" ]]; then
    exec "$BIN"
fi
exec "$BIN" -pretty
