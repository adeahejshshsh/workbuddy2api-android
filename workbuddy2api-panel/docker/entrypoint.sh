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

# 只在「配置里没有有效 key」时生成，并**写回 config.json**（bug #3）。
# 此前每次容器启动都重新生成并 export —— 环境变量在 applyEnv 里覆盖配置文件，
# 于是 docker restart / compose up / 宿主重启都会换掉 key：客户端、面板
# localStorage、手机壳里注入的 key 全部失效，而挂载的 config.json 里的 key
# 完全不生效（注释里"重启不会重新生成"与实现相反）。
if [ -z "${WB2A_API_KEY:-}" ]; then
  if grep -q '"api_key"[[:space:]]*:[[:space:]]*"[^"]\+"' /app/config.json 2>/dev/null; then
    echo "[entrypoint] 沿用 config.json 里已有的 api_key"
  else
    KEY="sk-$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    if command -v python3 >/dev/null 2>&1; then
      python3 - "$KEY" <<'PY'
import json, sys
p = "/app/config.json"
try:
    with open(p, encoding="utf-8") as f:
        c = json.load(f)
except Exception:
    c = {}
c["api_key"] = sys.argv[1]
with open(p, "w", encoding="utf-8") as f:
    json.dump(c, f, ensure_ascii=False, indent=2)
PY
    else
      # 无 python：分两种情况（报告 #24）—— 键存在则替换，键**不存在**则插入。
      # 此前只做替换：而进入本分支的前提正是「grep 没匹配到非空 api_key」，
      # 键缺失时 sed 什么都不改 → 下面的校验必然失败 → 每次都退回 env 注入，
      # key 随重启变化，注释承诺的「写入后重启不变」不成立。
      if grep -q '"api_key"' /app/config.json 2>/dev/null; then
        sed -i "s|\"api_key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"|\"api_key\": \"$KEY\"|" /app/config.json
      else
        # 在首个 { 后插入一行（GNU sed 的 \n 在替换串里有效）
        sed -i "0,/^{/s|{|{\n  \"api_key\": \"$KEY\",|" /app/config.json 2>/dev/null \
          || printf '{\n  "api_key": "%s"\n}\n' "$KEY" > /app/config.json
      fi
    fi
    # 写回失败（只读挂载等）时退回 env：至少能起来，但 key 会随重启变 —— 明确告警。
    if grep -q "\"api_key\"[[:space:]]*:[[:space:]]*\"$KEY\"" /app/config.json 2>/dev/null; then
      echo "[entrypoint] config.json 里没有 api_key，已生成并写入：$KEY"
      echo "[entrypoint] 记下来：客户端与面板都要用它（之后重启不会再变）"
    else
      export WB2A_API_KEY="$KEY"
      echo "[entrypoint] ⚠ config.json 不可写（只读挂载？），改用环境变量注入 api_key：$KEY"
      echo "[entrypoint] ⚠ 这样每次重启都会换 key，请把 config.json 挂成可写或显式设 WB2A_API_KEY"
    fi
  fi
fi

if [ -z "${WB2A_LISTEN:-}" ]; then
  WB2A_LISTEN="0.0.0.0:7863"
fi
# 用户显式提供了 WB2A_API_KEY 才 export（env 优先于文件，这是 applyEnv 的既有语义）
if [ -n "${WB2A_API_KEY:-}" ]; then export WB2A_API_KEY; fi
export WB2A_LISTEN

exec /app/wb2api -config /app/config.json "$@"
