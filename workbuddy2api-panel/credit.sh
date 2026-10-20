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
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
