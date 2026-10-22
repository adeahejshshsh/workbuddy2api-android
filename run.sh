#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# workbuddy2api 启停脚本（Android / DeepSeek Harness 移植版）
#   用法: bash run.sh [start|stop|restart|status|log|panel]
set -u
# SELF 绝对路径（bug #9）：restart 里原本用 bash "$0"，而 $0 是**调用时的相对路径**
# （如 run.sh），从别的目录执行（cd /tmp && bash ~/run.sh restart）就会找不到脚本，
# restart 静默只跑一半。
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
APP_DIR="/storage/emulated/0/新建文件夹/workbuddy2api/workbuddy2api-panel"
BIN_DIR="$PREFIX/lib/workbuddy2api"
BIN="$BIN_DIR/server"
LOG="$APP_DIR/data/server.log"
PIDF="$APP_DIR/data/server.pid"
# PORT 在 cfg_val 定义之后按 config.json 的 listen 推导（报告 #11）：写死 7863 时，
# 面板里把 listen 改掉后 status/panel 给出的地址全是错的。
PORT_FALLBACK=7863

# 取配置值：键名走 sys.argv（review S-23），不再拼进 python 源码 ——
# 键名里带引号会让整段变成 SyntaxError，而且拼接本身就容易被当成注入面。
cfg_val() { python3 - "$APP_DIR/config.json" "$1" <<'PY'
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding="utf-8")).get(sys.argv[2], ""))
except Exception:
    print("")
PY
}

api_key() { cfg_val api_key; }

# 从 listen（如 ":7863" / "127.0.0.1:8080"）取端口；取不到回落 7863。
# 与 APK 壳的探针、面板 URL 同源，改 listen 后这里自动跟上。
PORT=$(cfg_val listen | sed 's/^.*://' | tr -dc '0-9')
[ -n "$PORT" ] || PORT="$PORT_FALLBACK"

# key_fp 只打密钥指纹（sha256 前 8 位）：完整 api_key 会进 server.log，
# 再经 log 镜像进面板日志环 —— 等于把凭据写进日志文件（review S-23）。
key_fp() {
  k=$(api_key)
  [ -n "$k" ] || { echo "(未配置)"; return; }
  printf '%s' "$k" | sha256sum | cut -c1-8
}

running_pid() {
  if [ -f "$PIDF" ]; then
    p=$(cat "$PIDF" 2>/dev/null)
    # 必须校验命中进程的命令行（bug B18）：pid 会被系统复用，或 pid 文件是别的实例
    # 留下的 —— 只 kill -0 就会把无关进程当成"网关已在运行"，start 直接 exit 0，
    # 实际服务根本没起来。
    if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then
      if tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null | grep -q 'workbuddy2api/server'; then
        echo "$p"; return
      fi
    fi
  fi
  # 锚定 argv[0]：否则会匹配到"命令行里正好提到这个路径"的无关进程（比如正在跑 run.sh 的 shell）
  pgrep -f '^[^ ]*/workbuddy2api/server( |$)' | head -1
}

case "${1:-start}" in
  start)
    # 只取一次 pid（报告 #25）：两次调用之间进程可能退出，会打印空的 pid。
    CUR_PID="$(running_pid)"
    if [ -n "$CUR_PID" ]; then echo "已在运行 (pid $CUR_PID)"; exit 0; fi
    [ -x "$BIN" ] || { echo "缺少二进制: $BIN （先跑 rebuild.sh）"; exit 1; }
    mkdir -p "$APP_DIR/data" "$APP_DIR/auths"
    cd "$APP_DIR" || exit 1
    # 注意: 二进制必须在可执行分区($PREFIX); 工作区 /sdcard 是 noexec
    nohup "$BIN" -config config.json >>"$LOG" 2>&1 &
    echo $! > "$PIDF"
    sleep 2
    p=$(running_pid)
    if [ -n "$p" ]; then
      echo "已启动 pid=$p  http://127.0.0.1:$PORT/panel/"
      echo "api_key 指纹=$(key_fp)（完整值见 config.json）"
      echo "日志: $LOG"
    else
      echo "启动失败，日志尾部:"; tail -20 "$LOG"
    fi
    ;;
  stop)
    p=$(running_pid)
    if [ -n "$p" ]; then
      # 优雅停机预算：服务端收到 SIGTERM 后先 Flush，再 srv.Shutdown(5s) 等在途
      # 请求收尾，之后才执行 defer 里的 Close。原来 sleep 1 就 kill -9，等于把
      # 最后一次 Flush 抢走 —— 冷却时间/成功计数/用量记录丢一截，重启后表现为
      # 「刚冷却的号又能用」「用量少了一截」。这里最多等 8 秒，超时才强杀。
      kill "$p" 2>/dev/null
      i=0
      while [ "$i" -lt 80 ] && kill -0 "$p" 2>/dev/null; do
        sleep 0.1; i=$((i+1))
      done
      if kill -0 "$p" 2>/dev/null; then
        kill -9 "$p" 2>/dev/null
        sleep 0.3
      fi
      # 只有确认已退出才删 pid 文件（bug #8）：kill -9 之后进程仍可能活着
      # （僵尸/无权限），此时删掉 pid 就再也定位不到它了 —— 之后的 status/stop
      # 只剩 pgrep 兜底，还会打印「未在运行」，误导性极强。
      if kill -0 "$p" 2>/dev/null; then
        echo "⚠ 已发 SIGKILL 但 pid=$p 仍存活（僵尸/无权限），保留 $PIDF 便于排查"
      else
        rm -f "$PIDF"
        echo "已停止 pid=$p"
      fi
    else
      echo "未在运行"
      rm -f "$PIDF"   # 残留 pid 文件：清掉
    fi
    ;;
  # 必须用 bash 起子进程（bug #1）：工作区在 sdcard（noexec）且脚本本身没有 x 位，
  # 直接执行 "$0" 会报 command not found / Permission denied，stop 和 start 都没跑。
  restart) bash "$SELF" stop; sleep 1; bash "$SELF" start ;;
  status)
    p=$(running_pid)
    if [ -n "$p" ]; then
      echo "运行中 pid=$p"
      curl -sS -m 5 "http://127.0.0.1:$PORT/healthz" 2>/dev/null | head -c 400; echo
    else
      # 本脚本没起服务 ≠ 端口没人服务（可能是 App 里那份）—— 别误报"未运行"
      code=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" 2>/dev/null)
      code=${code:-000}
      if [ "$code" = "200" ] || [ "$code" = "503" ]; then
        echo "本脚本未起服务，但 $PORT 上有实例在服务（http=$code）→ 外部实例，别抢"
        curl -sS -m 5 "http://127.0.0.1:$PORT/healthz" 2>/dev/null | head -c 400; echo
      else echo "未运行"; fi
    fi
    ;;
  log) tail -n "${2:-40}" "$LOG" ;;
  panel)
    echo "面板: http://127.0.0.1:$PORT/panel/"
    echo "API : http://127.0.0.1:$PORT/v1  (api_key 指纹=$(key_fp)，完整值见 config.json)"
    ;;
  *) echo "用法: bash run.sh [start|stop|restart|status|log|panel]"; exit 1 ;;
esac
