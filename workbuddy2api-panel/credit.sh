#!/usr/bin/env bash
# 用法：服务器/容器内可直接 ./credit.sh（Docker 镜像里 chmod 755 后执行）；
#      脚本所在卷禁止执行时请用 `bash credit.sh`（否则 ./ 会在读 shebang 之前就
#      Permission denied）。shebang 保持 /usr/bin/env bash 以便 Docker 可用。
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
# 而按安装前缀部署时产物在 $PREFIX/lib/workbuddy2api/ —— 仓库里**没有** ./credit，
# 所以原先的 `exec ./credit` 在那种布局下必然 "No such file or directory"。
PREFIX="${PREFIX:-$HOME/.local}"
BIN=""
for c in ./credit "$PREFIX/lib/workbuddy2api/credit"; do
    [ -x "$c" ] && { BIN="$c"; break; }
done
[ -n "$BIN" ] || { echo "❌ 找不到 credit 二进制（先 go build -o credit ./cmd/credit 并装到上述路径）" >&2; exit 1; }

if [[ "${1:-}" == "-json" ]]; then
    exec "$BIN"
fi
exec "$BIN" -pretty
