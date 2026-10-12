#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# workbuddy2api 启停脚本（Android / DeepSeek Harness 移植版）
#   用法: bash run.sh [start|stop|restart|status|log|panel]
set -u

PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
APP_DIR="/storage/emulated/0/新建文件夹/workbuddy2api/workbuddy2api-panel"
BIN_DIR="$PREFIX/lib/workbuddy2api"
BIN="$BIN_DIR/server"
LOG="$APP_DIR/data/server.log"
PIDF="$APP_DIR/data/server.pid"
PORT=7863

cfg_val() { python3 -c "
import json,sys
try: print(json.load(open('$APP_DIR/config.json')).get('$1',''))
except Exception as e: print('')
"; }

api_key() { cfg_val api_key; }

running_pid() {
  if [ -f "$PIDF" ]; then
    p=$(cat "$PIDF" 2>/dev/null)
    [ -n "$p" ] && kill -0 "$p" 2>/dev/null && { echo "$p"; return; }
  fi
  # 锚定 argv[0]：否则会匹配到"命令行里正好提到这个路径"的无关进程（比如正在跑 run.sh 的 shell）
  pgrep -f '^[^ ]*/workbuddy2api/server( |$)' | head -1
}

case "${1:-start}" in
  start)
    if [ -n "$(running_pid)" ]; then echo "已在运行 (pid $(running_pid))"; exit 0; fi
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
      echo "api_key=$(api_key)"
      echo "日志: $LOG"
    else
      echo "启动失败，日志尾部:"; tail -20 "$LOG"
    fi
    ;;
  stop)
    p=$(running_pid)
    if [ -n "$p" ]; then kill "$p" 2>/dev/null; sleep 1; kill -9 "$p" 2>/dev/null; echo "已停止 pid=$p"; else echo "未在运行"; fi
    rm -f "$PIDF"
    ;;
  restart) "$0" stop; sleep 1; "$0" start ;;
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
    echo "API : http://127.0.0.1:$PORT/v1  (api_key=$(api_key))"
    ;;
  *) echo "用法: bash run.sh [start|stop|restart|status|log|panel]"; exit 1 ;;
esac
