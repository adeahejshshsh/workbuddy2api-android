#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# 重新编译 workbuddy2api（Android / DeepSeek Harness 移植版）
#   用法: bash rebuild.sh           # 编译全部 cmd/*
#         bash rebuild.sh test      # 跑 go test（需要 node，用于面板 JS 冒烟）
set -u

PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
export GOROOT="$PREFIX/lib/go"
export PATH="$GOROOT/bin:$PREFIX/bin:$PATH"
export GOPATH="$HOME/go"
export GOCACHE="$HOME/.cache/go-build"
export GOMODCACHE="$GOPATH/pkg/mod"

# 关键移植项（已用 go env -w 持久化，这里再显式兜底）：
#   -toolexec=/system/bin/linker64  → Go 无法直接 exec app 私有目录里的工具，
#                                     借系统 linker 代跑（argv[1] 即工具绝对路径）
export GOFLAGS="-toolexec=/system/bin/linker64"
export CGO_ENABLED=0          # 无系统 libc 头/链接器可用路径，禁 CGO
export GOTOOLCHAIN=local      # 不尝试联网切换工具链
export GOSUMDB=off
export GOPROXY="file://$PREFIX/lib/workbuddy2api/goproxy,off"   # 本地模块代理(离线)

REPO="/storage/emulated/0/新建文件夹/workbuddy2api/workbuddy2api-panel"
OUT="$PREFIX/lib/workbuddy2api"
mkdir -p "$OUT"
cd "$REPO" || exit 1

if [ "${1:-build}" = "test" ]; then
  echo "== go test ./... =="
  go test -buildvcs=false ./... 2>&1 | tail -30
  exit 0
fi

echo "== 编译 cmd/* =="
for c in server signin login credit trial; do
  [ -d "./cmd/$c" ] || continue
  printf "  %-8s " "$c"
  if go build -buildvcs=false -trimpath -ldflags="-s -w" -o "$OUT/$c" "./cmd/$c" 2>"$PREFIX/tmp/e_$c.log"; then
    echo "OK  $(du -h "$OUT/$c" | cut -f1)"
  else
    echo "FAIL"; tail -6 "$PREFIX/tmp/e_$c.log"
  fi
done
echo "== 产物 =="
ls -la "$OUT"
echo
echo "提示: 改了 runtime/net 等标准库源码后如需彻底重编: go build -a"
