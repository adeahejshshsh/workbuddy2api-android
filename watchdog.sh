#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# CLI 侧看门狗（run.sh 那套用）：
#   · 7863 有人正常服务（本脚本起的，或外部实例如 App 里那份）→ 不插手
#   · 进程在但 /healthz 连续 MAXFAIL 次不通 → 判僵死，杀掉重拉
#   · 进程没了且服务不通 → 立刻拉起（带退避）
#   启动: nohup bash watchdog.sh [检查间隔秒=20] >/dev/null 2>&1 &
#   停止: kill "$(cat workbuddy2api-panel/data/watchdog.pid)"
#   日志: workbuddy2api-panel/data/watchdog.log
set -u
DIR="$(cd "$(dirname "$0")" && pwd)"
DATA="$DIR/workbuddy2api-panel/data"
LOG="$DATA/watchdog.log"
INTERVAL="${1:-20}"
MAXFAIL=3
mkdir -p "$DATA"
# 单实例锁（bug B17）：此前无条件覆写 pid 文件，手工多跑一次（或 nohup 被重复
# 执行）就有两条看门狗同时 stop/start 同一个网关，表现为「服务反复重启、日志里
# 成对的 stop/start」。
if [ -f "$DATA/watchdog.pid" ]; then
  OLD_WD="$(cat "$DATA/watchdog.pid" 2>/dev/null || true)"
  if [ -n "$OLD_WD" ] && [ "$OLD_WD" != "$$" ] && kill -0 "$OLD_WD" 2>/dev/null; then
    echo "看门狗已在运行 pid=$OLD_WD（要重启先 kill 它）"
    exit 0
  fi
fi
echo $$ > "$DATA/watchdog.pid"
say() { echo "[$(date '+%F %T')] $*" >> "$LOG"; }
say "看门狗启动 pid=$$ 间隔=${INTERVAL}s"

fail=0
noted=0     # 外部实例只提示一次，别刷屏
while :; do
  # 锚定 argv[0]：松散写全会匹配到"命令行里提到这个路径"的无关进程（测出来的坑）
  p=$(pgrep -f '^[^ ]*/workbuddy2api/server( |$)' | head -1)
  code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:7863/healthz 2>/dev/null)
  code=${code:-000}      # curl 失败自带 000，别再叠加一个

  if [ "$code" = "200" ] || [ "$code" = "503" ]; then      # 503=服务在但没绑账号，也算活着
    if [ -z "$p" ]; then
      # 端口有主但不是本脚本起的那个 → 复用，绝不抢
      [ "$noted" = 0 ] && { say "7863 上是外部实例（http=$code），复用不干预"; noted=1; }
      fail=0   # 外部实例在正常服务：历史失败计数必须清零，否则它一挂就要按
               # 旧计数退避（最长白等 30s 才拉起）
    else
      [ "$fail" -gt 0 ] && say "已恢复正常 http=$code pid=$p"
      fail=0; noted=0
    fi
  elif [ -n "$p" ]; then                                   # 进程在但不通：容忍 MAXFAIL 次
    fail=$((fail + 1))
    say "健康检查无响应 http=$code（pid=$p，第 $fail/$MAXFAIL 次）"
    if [ "$fail" -ge "$MAXFAIL" ]; then
      say "判定僵死 → 杀掉重拉"
      bash "$DIR/run.sh" stop >> "$LOG" 2>&1
      sleep 1
      bash "$DIR/run.sh" start >> "$LOG" 2>&1
      fail=0
    fi
  else                                                     # 进程没了且不通：立刻拉（退避）
    fail=$((fail + 1))
    say "进程不存在且服务不通 → 拉起（第 $fail 次）"
    bash "$DIR/run.sh" start >> "$LOG" 2>&1
    [ "$fail" -gt 1 ] && sleep $((fail > 4 ? 30 : fail * 2))
  fi
  sleep "$INTERVAL"
done
