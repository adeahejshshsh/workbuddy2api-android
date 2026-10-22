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
  # 必须校验命令行（bug #16）：pid 会被系统复用，只 kill -0 会把无关进程当成
  # "看门狗已在运行"直接 exit 0，实际没有任何看门狗在跑、网关挂了没人拉。
  if [ -n "$OLD_WD" ] && [ "$OLD_WD" != "$$" ] && kill -0 "$OLD_WD" 2>/dev/null \
     && tr '\0' ' ' < "/proc/$OLD_WD/cmdline" 2>/dev/null | grep -q 'watchdog.sh'; then
    echo "看门狗已在运行 pid=$OLD_WD（要重启先 kill 它）"
    exit 0
  fi
fi
echo $$ > "$DATA/watchdog.pid"
say() { echo "[$(date '+%F %T')] $*" >> "$LOG"; }
say "看门狗启动 pid=$$ 间隔=${INTERVAL}s"

# curl 不可用（没装 / 被 SELinux 拦）时**不能**把「探不到」当成「网关挂了」（报告 #10）：
# 那会让看门狗每 10s 累加一次失败，60s 后把完全健康的网关 stop + start，无限循环。
# 探测能力缺失是**环境问题**，降级为「只按进程存活判断」。
# 端口与 run.sh 同源（报告 #8/#11）：写死 7863 时，面板里改了 listen，
# 这里的探针打的是一个没人监听的端口 → 判「僵死」并周期性杀健康网关。
WD_DIR="$(cd "$(dirname "$0")" && pwd)"
PORT=$(python3 - "$WD_DIR/workbuddy2api-panel/config.json" <<'PY' 2>/dev/null
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding="utf-8")).get("listen", "").rsplit(":", 1)[-1])
except Exception:
    print("")
PY
)
PORT=$(printf '%s' "$PORT" | tr -dc '0-9')
[ -n "$PORT" ] || PORT=7863

HAVE_CURL=1
command -v curl >/dev/null 2>&1 || HAVE_CURL=0
[ "$HAVE_CURL" = 0 ] && say "WARN: 未找到 curl —— 健康探测降级为仅进程存活判断（不会因探测失败重启网关）"

fail=0
noted=0     # 外部实例只提示一次，别刷屏
while :; do
  # 锚定 argv[0]：松散写全会匹配到"命令行里提到这个路径"的无关进程（测出来的坑）
  p=$(pgrep -f '^[^ ]*/workbuddy2api/server( |$)' | head -1)
  if [ "$HAVE_CURL" = 1 ]; then
    code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/healthz 2>/dev/null)
    code=${code:-000}    # curl 失败自带 000，别再叠加一个
  else
    # 无探测能力：有进程就当活着（503/000 无法区分，宁可不动）
    [ -n "$p" ] && code=200 || code=000
  fi

  if [ "$code" = "200" ] || [ "$code" = "503" ]; then      # 503=服务在但没绑账号，也算活着
    if [ -z "$p" ]; then
      # 端口有主但不是本脚本起的那个 → 复用，绝不抢
      [ "$noted" = 0 ] && { say "$PORT 上是外部实例（http=$code），复用不干预"; noted=1; }
      # 清零前先留痕（BUG-20）：否则「本实例失败 → 外部实例接管 → 本实例恢复」
      # 这条真实路径在 watchdog.log 里没有任何恢复记录，只剩早先的失败行。
      [ "$fail" -gt 0 ] && say "外部实例接管（http=$code），本实例失败计数 $fail 清零"
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
      noted=0   # 自愈后复位外部实例提示（bug-16）：否则之后切回本进程实例不再提示"已恢复"
    fi
  else                                                     # 进程没了且不通：立刻拉（退避）
    fail=$((fail + 1))
    say "进程不存在且服务不通 → 拉起（第 $fail 次）"
    bash "$DIR/run.sh" start >> "$LOG" 2>&1
    [ "$fail" -gt 1 ] && sleep $((fail > 4 ? 30 : fail * 2))
  fi
  sleep "$INTERVAL"
done
