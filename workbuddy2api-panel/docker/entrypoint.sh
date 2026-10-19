#!/bin/sh
# entrypoint.sh 容器启动兜底（issue ISSUE-01）
#
# 为什么需要：
#   1) config.example.json 的 listen 是 127.0.0.1（裸机默认安全），容器里必须改成
#      0.0.0.0 才能被外部访问；
#   2) 非回环监听必须有 api_key（config.normalize 的守卫），而示例里的 api_key 是空的。
# 所以这里在**没有显式配置**时生成一个随机 key，并把监听面改成对外。
# 用户显式设了 WB2A_API_KEY / WB2A_LISTEN 时一律尊重。
set -e

if [ -z "${WB2A_API_KEY:-}" ]; then
  WB2A_API_KEY="sk-$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  echo "[entrypoint] 未提供 WB2A_API_KEY，已生成随机密钥：$WB2A_API_KEY"
  echo "[entrypoint] 记下来：客户端与面板都要用它（重启容器不会重新生成，除非不挂载配置）"
fi
export WB2A_API_KEY

if [ -z "${WB2A_LISTEN:-}" ]; then
  WB2A_LISTEN="0.0.0.0:7863"
fi
export WB2A_LISTEN

exec /app/wb2api -config /app/config.json "$@"
