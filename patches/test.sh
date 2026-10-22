#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# test.sh 只跑「改动涉及的包」，全量留给发版前。
#
# 为什么：全量 11 个包 ≈ 15s（编译另算），而多数改动只碰 1-2 个包；
# 每次无脑全量是纯浪费。用法：
#   bash patches/test.sh            # 只跑 git 改动涉及的包
#   bash patches/test.sh all        # 全量（发版前跑）
#   bash patches/test.sh server pool  # 指定包
set -uo pipefail

WS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
P="$WS/workbuddy2api-panel"
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
export GOROOT="$PREFIX/lib/go"
export PATH="$GOROOT/bin:$PATH"
export GOFLAGS="-toolexec=/system/bin/linker64 -buildvcs=false"

ALL_PKGS="server upstream panel pool scheduler auth session reqlog usage redisstore credithist"

pick() {
  if [ "${1:-}" = "all" ]; then echo "$ALL_PKGS"; return; fi
  if [ $# -gt 0 ]; then echo "$*"; return; fi
  # 从 git 改动推包名：internal/<pkg>/... → <pkg>；cmd/... → 特殊处理
  local changed
  changed=$(cd "$WS" && git diff --name-only HEAD 2>/dev/null | sed -n 's#.*workbuddy2api-panel/internal/\([a-z]*\)/.*#\1#p' | sort -u)
  if [ -z "$changed" ]; then echo "$ALL_PKGS"; return; fi
  echo "$changed"
}

pkgs=$(pick "$@")
echo "跑: $pkgs"
fail=0
for pkg in $pkgs; do
  if ! ( cd "$P" && go test -c -o "$PREFIX/tmp/$pkg.test" "./internal/$pkg" >/dev/null 2>&1 ); then
    echo "  $pkg 编译失败"; fail=1; continue
  fi
  out=$(cd "$P/internal/$pkg" && /system/bin/linker64 "$PREFIX/tmp/$pkg.test" 2>&1 | tail -1)
  printf "  %-11s %s\n" "$pkg" "$out"
  [ "$out" = "PASS" ] || fail=1
done
# cmd/server 只在配置类改动时需要
if [ "${1:-}" = "all" ]; then
  if ( cd "$P" && go test -c -o "$PREFIX/tmp/cmdserver.test" ./cmd/server >/dev/null 2>&1 ); then
    out=$(cd "$P/cmd/server" && /system/bin/linker64 "$PREFIX/tmp/cmdserver.test" 2>&1 | tail -1)
    printf "  %-11s %s\n" "cmd/server" "$out"
    [ "$out" = "PASS" ] || fail=1
  fi
fi
exit $fail
