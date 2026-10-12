#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# 把本工作区推到 GitHub（可选：建 Release 并上传 APK）。
#
#   bash push_github.sh            # 只推代码
#   bash push_github.sh release    # 推代码 + 建/更新 Release(v<版本>) + 上传 APK
#
# token 从 /sdcard/public_repo 读（40 位 ghp_…），只在命令行里用，
# **不会**写进任何文件、也不会进仓库（推之前还会全量扫一遍高危串）。
# 暂存目录 $PREFIX/tmp/ghrepo 会保留（复用它的 .git 才能累加历史，不用每次强推）。
set -eu
export GIT_TERMINAL_PROMPT=0      # 绝不交互式问账号密码（问不到会挂住）

OWNER="${OWNER:-adeahejshshsh}"
NAME="${NAME:-workbuddy2api-android}"
BRANCH="${BRANCH:-main}"
WS="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
TOKEN_FILE="${TOKEN_FILE:-/sdcard/public_repo}"
STAGE="$PREFIX/tmp/ghrepo"
APK="$WS/WorkBuddy2API.apk"

[ -f "$TOKEN_FILE" ] || { echo "❌ 找不到 token 文件: $TOKEN_FILE"; exit 1; }
TOKEN=$(tr -d ' \r\n' < "$TOKEN_FILE")
[ -n "$TOKEN" ] || { echo "❌ token 为空"; exit 1; }

VER=$(grep -o 'android:versionName="[^"]*"' "$WS/apk-src/AndroidManifest.xml" | head -1 | sed 's/.*="//; s/"//')
echo "== 目标仓库: $OWNER/$NAME    版本: v$VER"

echo "== 1/5 暂存到 $STAGE（排除密钥/运行数据/日志/成品 APK）"
mkdir -p "$STAGE"
# 清掉上一轮的普通文件，但**必须留住 .git**（留住才能累加历史、不用每次强推）
[ -d "$STAGE/.git" ] && find "$STAGE" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
# 用 tar 拷（--exclude 排除工作区里可能存在的 .git，别把暂存库的 .git 冲掉）
tar -C "$WS" \
    --exclude=./.git \
    --exclude=./workbuddy2api-panel/.git \
    --exclude=./workbuddy2api-panel/data \
    --exclude=./workbuddy2api-panel/auths \
    --exclude=./workbuddy2api-panel/config.json \
    --exclude=./apk-src/wb2api.keystore \
    --exclude=./WorkBuddy2API.apk \
    --exclude=./WorkBuddy2API.apk.idsig \
    --exclude=./.panel-ip \
    -cf - . | tar -C "$STAGE" -xf -
[ -d "$STAGE/.git" ] && echo "  （沿用已有 .git，历史可累加）"

echo "== 1.5/5 敏感串扫描（命中就中止）"
TOKEN="$TOKEN" STAGE="$STAGE" python3 - <<'PY'
import os, re, sys, pathlib
stage = pathlib.Path(os.environ["STAGE"]); tok = os.environ["TOKEN"]
pats = [(re.compile(re.escape(tok)), "GitHub token 本人"),
        (re.compile(r"gh[pousr]_[A-Za-z0-9]{20,}"), "疑似 GitHub token"),
        (re.compile(r"github_pat_[A-Za-z0-9_]{20,}"), "疑似细粒度 token"),
        (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"), "私钥"),
        (re.compile(r'"api_key"\s*:\s*"(?!\s*")(?!test_key)[^"]{8,}"'), "真实 api_key"),
        (re.compile(r"BEGIN (RSA|OPENSSH|EC) PRIVATE"), "私钥头")]
hits = []
for p in stage.rglob("*"):
    if not p.is_file() or ".git/" in str(p): continue
    if p.stat().st_size > 8_000_000: continue
    try: t = p.read_text(errors="ignore")
    except Exception: continue
    for rx, why in pats:
        if rx.search(t): hits.append(f"{p.relative_to(stage)}  ← {why}")
if hits:
    print("❌ 暂存内容里发现高危串，已中止推送：")
    for h in hits[:20]: print("   ", h)
    sys.exit(1)
print("   OK 没扫到 token/私钥/真实 api_key")
PY

echo "== 2/5 提交"
cd "$STAGE"
[ -d .git ] || git init -q -b "$BRANCH"
git config user.name "$OWNER"
git config user.email "$OWNER@users.noreply.github.com"
git add -A
if git diff --cached --quiet; then echo "  （内容无变化，跳过提交）"; else
  git commit -qm "WorkBuddy2API Android 移植版 v$VER"
  echo "  已提交 $(git rev-parse --short HEAD)"
fi

echo "== 3/5 仓库（不存在就建，公开）"
CODE=$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
       "https://api.github.com/repos/$OWNER/$NAME")
if [ "$CODE" = "200" ]; then echo "  已存在"; else
  curl -sS -m 30 -o /dev/null -w '  建仓 http=%{http_code}\n' -X POST \
    -H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json" \
    -d "{\"name\":\"$NAME\",\"description\":\"WorkBuddy2API 安卓自托管移植版：网关 APK + 看门狗 + 保活 + 手机端面板\",\"private\":false,\"has_issues\":true,\"has_wiki\":false,\"has_projects\":false}" \
    "https://api.github.com/user/repos"
fi

echo "== 4/5 推送"
URL="https://$TOKEN@github.com/$OWNER/$NAME.git"
mask() { sed "s#$TOKEN#***#g"; }   # git 的输出可能带 token，一律抹掉再打出来
set +e
OUT=$(git push "$URL" "$BRANCH:$BRANCH" 2>&1); rc=$?
set -e
if [ $rc -ne 0 ]; then
  printf '%s\n' "$OUT" | mask | tail -4
  echo "  普通推送不行（远端有本地没有的东西），改强推："
  set +e
  OUT=$(git push --force "$URL" "$BRANCH:$BRANCH" 2>&1); rc=$?
  set -e
  if [ $rc -ne 0 ]; then
    printf '%s\n' "$OUT" | mask | tail -4
    echo "❌ 推送失败"; exit 1
  fi
fi
echo "  已推送 → https://github.com/$OWNER/$NAME"

[ "${1:-}" = "release" ] || { echo "（要发 Release 就加参数 release）"; exit 0; }

echo "== 5/5 Release v$VER + 上传 APK"
OWNER="$OWNER" NAME="$NAME" VER="$VER" APK="$APK" TOKEN="$TOKEN" python3 - <<'PY'
import json, os, pathlib, urllib.request, urllib.error

o, r, ver = os.environ["OWNER"], os.environ["NAME"], os.environ["VER"]
tok, apk = os.environ["TOKEN"], pathlib.Path(os.environ["APK"])
tag = "v" + ver
api = f"https://api.github.com/repos/{o}/{r}"

def call(url, method="GET", data=None, ctype="application/json", raw=False):
    req = urllib.request.Request(url, method=method, data=data)
    req.add_header("Authorization", "Bearer " + tok)
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("User-Agent", "wb2api-publish")
    if data is not None: req.add_header("Content-Type", ctype)
    try:
        with urllib.request.urlopen(req, timeout=120) as f:
            b = f.read()
            return f.status, b, f.headers
    except urllib.error.HTTPError as e:
        return e.code, e.read(), e.headers

body = f"""安卓自托管移植版第一版（APK v{ver}）。

**装完就能用**：一台手机 = 一个 OpenAI 兼容网关 `http://127.0.0.1:7863/v1`（局域网换成手机 IP）。

### 相对上游多出来的
- **看门狗四层**：进程级（10s tick，退避 2/4/8/16/32/60s）/ 健康级（`/healthz` 45s 无响应判僵死）/
  系统级（AlarmManager 每 5 分钟兜底）/ 保活级（partial wakelock —— 不持它锁屏后 CPU 一睡，
  线程和 Go 定时器全冻住，这就是"只有前台才生效"的真因）
- **状态按事实判定**：后台线程探 127.0.0.1:7863，端口在服务就是「运行中」；没起来第三行直接写明原因
- **前台服务 `specialUse`**：`dataSync` 在 Android 15+ 有 6 小时/天上限，对 7×24 网关是致命的
- **面板手机端适配**：导航换行铺满（7 个 tab 全可见）、触控目标 ≥40px、
  固定列宽网格改可收缩（不再整页横滚）、弹层改底部抽屉
- **壳与网页同一套配色**（抄面板 `:root` 暗色令牌）+ 加载进度线

### 安装
1. 下载下方 `WorkBuddy2API.apk` 安装（覆盖安装即升级，账号与 api_key 保留）
2. 打开 App → 顶栏点「省电白名单」允许
3. 国产 ROM 再放行：自启动 / 省电策略无限制 / 任务列表锁定后台

### 说明
- 包名 `com.dsh.wb2api`，minSdk 24 / targetSdk 34；本包用作者自备密钥签名
- 不含任何账号凭据、签名密钥与本机运行数据（见 `.gitignore`）
- 移植细节（撞过的墙 / 验证记录 / 踩过的坑）都在仓库 `PORTING.md`
"""

st, b, _ = call(api + "/releases/tags/" + tag)
rel = None
if st == 200:
    rel = json.loads(b); print(f"  Release {tag} 已存在，复用")
else:
    payload = json.dumps({"tag_name": tag, "name": f"WorkBuddy2API Android v{ver}",
                          "body": body, "draft": False, "prerelease": False}).encode()
    st, b, _ = call(api + "/releases", "POST", payload)
    if st not in (200, 201):
        print("  ❌ 建 Release 失败:", st, b[:300]); raise SystemExit(1)
    rel = json.loads(b); print(f"  已建 Release {tag}")

# 已有同名 asset 先删（幂等）
name = apk.name
st, b, _ = call(api + f"/releases/{rel['id']}/assets")
for a in json.loads(b) if st == 200 else []:
    if a["name"] == name:
        call(api + f"/releases/assets/{a['id']}", "DELETE"); print("  删掉旧 asset，准备重传")
url = rel["upload_url"].split("{")[0] + "?name=" + name
data = apk.read_bytes()
st, b, _ = call(url, "POST", data, ctype="application/vnd.android.package-archive")
if st not in (200, 201):
    print("  ❌ 上传 APK 失败:", st, b[:300]); raise SystemExit(1)
a = json.loads(b)
print(f"  ✅ 上传成功 {a['name']}  {a['size']} 字节")
print(f"  {rel['html_url']}")
PY
