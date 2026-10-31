#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# 把本工作区推到 GitHub（可选：建 Release 并上传 APK）。
#
#   bash push_github.sh "(fix) 修好X"           # 只推代码，提交标题用给的那句
#   bash push_github.sh "(feat) 加Y" release    # 推代码 + 建/更新 Release + 上传 APK
#
# 提交标题约定（本项目自 v1.35.1 起）：**必须**带类型前缀，一眼能看出改了什么性质：
#   (fix) 修 bug  (feat) 新功能  (doc) 文档  (chore) 杂务/脚本  (perf) 性能
#   (refactor) 重构  (test) 测试  (security) 安全/指纹对齐
# 不给标题时按 (chore) 兜底并带时间戳 —— 但请尽量显式给，日志才可读。
#
# token 从 /sdcard/public_repo 读（40 位 ghp_…），只在命令行里用，
# **不会**写进任何文件、也不会进仓库（推之前还会全量扫一遍高危串）。
# 暂存目录 $PREFIX/tmp/ghrepo 会保留（复用它的 .git 才能累加历史，不用每次强推）。
set -eu
# 参数解析：release 可出现在任意位置，其余视作提交标题。
DO_RELEASE=0
MSG=""
for arg in "$@"; do
  case "$arg" in
    release) DO_RELEASE=1 ;;
    *) [ -n "$MSG" ] && MSG="$MSG $arg" || MSG="$arg" ;;
  esac
done
export GIT_TERMINAL_PROMPT=0      # 绝不交互式问账号密码（问不到会挂住）

OWNER="${OWNER:-adeahejshshsh}"
NAME="${NAME:-workbuddy2api-android}"
BRANCH="${BRANCH:-main}"
WS="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
TOKEN_FILE="${TOKEN_FILE:-/sdcard/public_repo}"
STAGE="$PREFIX/tmp/ghrepo"
APK="$WS/WorkBuddy2API.apk"

# token 来源（issue ISSUE-02）：优先环境变量（不落盘、不进外部存储），
# 其次才是 /sdcard 上的文件。用完在退出时 unset。
if [ -n "${GITHUB_TOKEN:-}" ]; then
  TOKEN="$GITHUB_TOKEN"
  echo "  使用 GITHUB_TOKEN 环境变量"
else
  [ -f "$TOKEN_FILE" ] || { echo "❌ 找不到 token：设 GITHUB_TOKEN 或写 $TOKEN_FILE"; exit 1; }
  TOKEN=$(tr -d ' \r\n' < "$TOKEN_FILE")
  echo "  ⚠ token 来自 $TOKEN_FILE（外部存储，建议改走 GITHUB_TOKEN 环境变量）"
fi
[ -n "$TOKEN" ] || { echo "❌ token 为空"; exit 1; }

# 取不到要明确报错（bug B19 的机制其实不成立：脚本是 set -eu 且**没有** pipefail，
# 管道的退出码取 sed，所以 grep 找不到不会让脚本退出 —— 实测会带着空版本号一路跑
# 下去，最后打出 `版本: v` 并用空 tag 建 Release）。这里补显式判空。
VER=$(grep -o 'android:versionName="[^"]*"' "$WS/apk-src/AndroidManifest.xml" | head -1 | sed 's/.*="//; s/"//' || true)

# Release 附件统一用**带时间后缀**的产物（2026-10-27 起）：同名稳定副本
# WorkBuddy2API.apk 留在工作区供本地安装，但上传用 WorkBuddy2API-v<ver>-<ts>.apk ——
# 事后能一眼核对「这个 release 到底是哪次构建」。找不到带时间戳的就退回稳定名。
for cand in "$WS/WorkBuddy2API-v$VER"-*.apk; do
  [ -f "$cand" ] || continue
  APK="$cand"   # glob 按字典序展开，时间戳定长(YYYYMMDDHHMM) → 最后一个即最新
done
[ -n "$VER" ] || { echo "❌ apk-src/AndroidManifest.xml 里没找到 versionName"; exit 1; }
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
    --exclude=./workbuddy2api-panel/signin \
    --exclude=./apk-src/wb2api.keystore \
    --exclude=./WorkBuddy2API.apk \
    --exclude=./WorkBuddy2API.apk.idsig \
    --exclude=./.panel-ip \
    --exclude=./.kspass \
    --exclude=./.tmp \
    --exclude='./__pycache__' \
    --exclude='*/__pycache__' \
    --exclude='*.pyc' \
    --exclude='./*-REVIEW*.md' \
    --exclude='./REVIEW-*.md' \
    --exclude='./CODE-REVIEW*.md' \
    --exclude='./BUG-REVIEW*.md' \
    --exclude='./ISSUES-*.md' \
    --exclude='./审查报告*.md' \
    --exclude='./代码审查报告*.md' \
    --exclude='./缺陷*.md' \
    --exclude='./*BUG清单*.md' \
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
        # 占位符（test_key / CHANGE_ME）不算密钥：它们是示例配置里的固定串。
        (re.compile(r'"api_key"\s*:\s*"(?!\s*")(?!test_key\b)(?!CHANGE_ME\b)[^"]{8,}"'), "真实 api_key"),
        (re.compile(r"BEGIN (RSA|OPENSSH|EC) PRIVATE"), "私钥头"),
        # 口令文件本体（issue ISSUE-02）：即便 .gitignore 挡住了提交，暂存目录里
        # 也不该出现明文口令。
        (re.compile(r"(?m)^\s*(wb2api\d{4}|sk-[A-Za-z0-9_-]{16,})\s*$"), "疑似口令文件内容")]
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
echo "  标题: ${MSG:-（未给标题，按 (chore) 兜底）}"
cd "$STAGE"
[ -d .git ] || git init -q -b "$BRANCH"
git config user.name "$OWNER"
git config user.email "$OWNER@users.noreply.github.com"
git add -A
echo "  推送日志（$(git diff --cached --name-only | wc -l) 个文件）："
git diff --cached --stat --format='' | tail -n 8 | sed 's/^/    /'
if git diff --cached --quiet; then echo "  （内容无变化，跳过提交）"; else
  # 标题带类型前缀（(fix)/(feat)/(doc)/…），正文写清这次推了什么。
  TITLE="${MSG:-"(chore) 同步 v$VER 构建（$(date +%Y-%m-%d\ %H:%M)）"}"
  git commit -qm "$TITLE" -m "版本: v$VER
构建: ${BUILD_STAMP:-$(date +%Y%m%d%H%M)}
变更文件:
$(git diff --cached --stat --format='' | tail -n 5)"
  echo "  已提交 $(git rev-parse --short HEAD)"
fi

echo "== 3/5 仓库（不存在就建，公开）"
# set -eu 下命令替换里的 curl 非零会直接触发 errexit，把「3/5 建仓」这步无自述地
# 中断（网络抖动 / DNS 超时 = curl 6/28）。这里显式容错并降级成空码。
CODE=$(curl -sS -m 30 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
       "https://api.github.com/repos/$OWNER/$NAME" 2>/dev/null) || CODE=""
if [ "$CODE" = "200" ]; then echo "  已存在"; else
  curl -sS -m 30 -o /dev/null -w '  建仓 http=%{http_code}\n' -X POST \
    -H "Authorization: Bearer $TOKEN" -H "Accept: application/vnd.github+json" \
    -d "{\"name\":\"$NAME\",\"description\":\"WorkBuddy2API 安卓自托管移植版：网关 APK + 看门狗 + 保活 + 手机端面板\",\"private\":false,\"has_issues\":true,\"has_wiki\":false,\"has_projects\":false}" \
    "https://api.github.com/user/repos"
fi

echo "== 4/5 推送"
# 掩码（review S-24）：此前把 token 拼进 sed 表达式，token 里的 #/& 会破坏表达式，
# 而且 sed 的命令行本身进 ps（等于又泄漏一次）。改成走环境变量 + python 替换：
# 环境变量只对同 uid 可读，比 argv 安全。
mask() {
  MASK_TOKEN="$TOKEN" python3 -c 'import os,sys
t = os.environ.get("MASK_TOKEN", "")
d = sys.stdin.read()
sys.stdout.write(d.replace(t, "***") if t else d)'
}
# 认证方式（踩过）：把 token 写进 URL（https://TOKEN@github.com/...）会被 GitHub 重定向到
# 带斜杠的规范地址，而**重定向会把 URL 里的 auth 丢掉** → push 报
# "could not read Username for 'https://github.com'"。改用「无凭据 URL + Authorization 头」，
# http.extraHeader 会跟着重定向一起走（实测 push 成功）。
B64=$(printf 'x-access-token:%s' "$TOKEN" | base64 | tr -d '\n')
PURL="https://github.com/$OWNER/$NAME.git"
gpush() { git -c http.extraHeader="Authorization: Basic $B64" push "$@" "$PURL" "$BRANCH:$BRANCH"; }
# 暂存目录（$STAGE）可能被环境清理成"全新历史"：先 fetch 远端，把本轮提交接到远端之后，
# 保证是快进推送（不丢远端历史、也不依赖强推）。
set +e
# fetch 也带认证头（review S-24）：仓库一旦转私有，裸 fetch 会静默失败。
git -c http.extraHeader="Authorization: Basic $B64" fetch -q "$PURL" "$BRANCH" 2>/dev/null; frc=$?
set -e
if [ $frc -eq 0 ] && git rev-parse --verify -q FETCH_HEAD >/dev/null 2>&1; then
  if ! git merge-base --is-ancestor FETCH_HEAD HEAD 2>/dev/null; then
    git reset --soft FETCH_HEAD
    # 参数必须写成 -c name=value（**一个** argv）。曾写成 `-c user.name "$OWNER"`：
    # bash 把它分成 4 个参数，git 于是把 owner 当子命令 →
    # "git: 'adeahejshshsh' is not a git command"；而这里原来挂着 `|| true`，
    # 失败被静默吞掉 —— 后果是 reset 已把本轮提交挪走、新提交没建出来，
    # push 推的还是旧 HEAD（"Everything up-to-date"），脚本却打印「已推送」。
    if ! git -c user.name="$OWNER" -c user.email="$OWNER@users.noreply.github.com" \
         commit -qm "${TITLE:-WorkBuddy2API Android 移植版 v$VER}"; then
      echo "❌ 接到远端历史后重新提交失败，已中止（暂存目录 $STAGE 保留便于排查）"
      exit 1
    fi
    echo "  已把本轮提交接到远端 $BRANCH 之后（保留远端历史 → 快进）"
  fi
fi
# 普通推送先重试 3 次：TLS 层偶发 "unexpected eof while reading" 是网络抖动，
# 重试就能过。强推会覆盖远端历史（误用一次不可恢复），所以只在「重试仍失败
# 且确认是快进关系」时才用——不是快进关系就停下让人看。
# PR 流程：`BRANCH=feat/x bash push_github.sh "msg"` 把本轮提交挂到同名分支上推，
# 不动 main —— 之后的合并走 PR（CI 绿了再合）。main 保持原来的行为：直接在 main 上提交推送。
if [ "$BRANCH" != "main" ]; then
  git checkout -q -B "$BRANCH" 2>/dev/null || true
  echo "  分支模式：推到 $BRANCH（不碰 main，合并走 PR）"
fi
push_ok=0
for attempt in 1 2 3; do
  set +e
  OUT=$(gpush 2>&1); rc=$?
  set -e
  [ $rc -eq 0 ] && { push_ok=1; break; }
  printf '%s\n' "$OUT" | mask | tail -3
  [ $attempt -lt 3 ] && { echo "  第 $attempt 次推送失败，2 秒后重试…"; sleep 2; }
done
if [ $push_ok -ne 1 ]; then
  # 只有「远端是本地的祖先」（快进）时强推才等价于正常推送；否则拒绝。
  set +e
  git -c http.extraHeader="Authorization: Basic $B64" fetch -q "$PURL" "$BRANCH" 2>/dev/null
  if git rev-parse --verify -q FETCH_HEAD >/dev/null 2>&1 \
     && git merge-base --is-ancestor FETCH_HEAD HEAD 2>/dev/null; then
    echo "  重试 3 次仍失败；已确认是快进关系 → 强推"
    OUT=$(gpush --force 2>&1); rc=$?
  else
    echo "  ⚠ 远端有本地没有的提交（非快进）。已停下，不覆盖远端历史。"
    echo "     先在本机 git pull/合并，或确认要丢弃远端改动后再手工强推。"
    rc=1
  fi
  set -e
  if [ $rc -ne 0 ]; then
    printf '%s\n' "$OUT" | mask | tail -4
    echo "❌ 推送失败"; exit 1
  fi
fi
echo "  已推送 → https://github.com/$OWNER/$NAME"

[ "$DO_RELEASE" = "1" ] || { echo "（要发 Release 就加参数 release）"; exit 0; }

echo "== 5/5 Release v$VER + 上传 APK"
OWNER="$OWNER" NAME="$NAME" VER="$VER" APK="$APK" TOKEN="$TOKEN" WS="$WS" python3 - <<'PY'
import json, os, pathlib, time, urllib.request, urllib.error

o, r, ver = os.environ["OWNER"], os.environ["NAME"], os.environ["VER"]
tok, apk = os.environ["TOKEN"], pathlib.Path(os.environ["APK"])
tag = "v" + ver
api = f"https://api.github.com/repos/{o}/{r}"

def call(url, method="GET", data=None, ctype="application/json", raw=False, tries=3):
    # 网络抖动要重试（2026-10-23 实测）：4MB 的 APK 上传在移动网络下会被
    # `[Errno 103] Software caused connection abort` 打断，一次失败就让整个
    # Release 半成品（release 建好、asset 空）。HTTPError 是服务端答复，不重试；
    # URLError/超时这类传输层错误才重试。
    last = None
    for i in range(tries):
        req = urllib.request.Request(url, method=method, data=data)
        req.add_header("Authorization", "Bearer " + tok)
        req.add_header("Accept", "application/vnd.github+json")
        req.add_header("User-Agent", "wb2api-publish")
        if data is not None: req.add_header("Content-Type", ctype)
        try:
            with urllib.request.urlopen(req, timeout=300) as f:
                b = f.read()
                return f.status, b, f.headers
        except urllib.error.HTTPError as e:
            return e.code, e.read(), e.headers
        except Exception as e:      # URLError / socket.timeout / ConnectionReset
            last = e
            if i + 1 < tries:
                wait = 3 * (i + 1)
                print(f"  ⚠ 上传/请求中断（{e}），{wait}s 后重试（{i+1}/{tries-1}）")
                time.sleep(wait)
    raise SystemExit(f"❌ 重试 {tries} 次仍失败: {last}")

# Release 说明直接取 CHANGELOG.md 里对应版本那一节 —— 一处维护，别两处写
sec, grab = [], False
try:
    txt = (pathlib.Path(os.environ["WS"]) / "CHANGELOG.md").read_text(encoding="utf-8")
    for ln in txt.splitlines():
        if ln.startswith("## v" + ver):
            grab = True; continue
        if grab and ln.startswith("## "):
            break
        if grab:
            sec.append(ln)
except Exception as e:
    print("  （读 CHANGELOG 失败，Release 说明退回简版）:", e)
changes = "\n".join(sec).strip()

body = (changes or f"APK v{ver}") + """

---

### 安装
1. 下载下方 `WorkBuddy2API-v<版本>-<构建时间>.apk` 覆盖安装即升级（账号与 api_key 保留）
2. 打开 App → 顶栏点「省电白名单」允许
3. 国产 ROM 再放行：自启动 / 省电策略无限制 / 任务列表锁定后台
4. 客户端接入：Base URL `http://<手机IP>:7863/v1`，Key 用面板里的（点「复制密钥」）

### 说明
- 包名 `com.dsh.wb2api`，minSdk 24 / targetSdk 34；本包用作者自备密钥签名
- 不含任何账号凭据、签名密钥与本机运行数据（见 `.gitignore`）
- 移植细节（撞过的墙 / 验证记录 / 踩过的坑）都在仓库 `AGENT.md`
"""

st, b, _ = call(api + "/releases/tags/" + tag)
rel = None
if st == 200:
    rel = json.loads(b); print(f"  Release {tag} 已存在，复用")
    if rel.get("body") != body:                      # 说明以 CHANGELOG 为准，同步一份
        call(api + f"/releases/{rel['id']}", "PATCH", json.dumps({"body": body}).encode())
        print("  已同步 Release 说明（取自 CHANGELOG.md）")
else:
    payload = json.dumps({"tag_name": tag, "name": f"WorkBuddy2API Android v{ver}",
                          "body": body, "draft": False, "prerelease": False}).encode()
    st, b, _ = call(api + "/releases", "POST", payload)
    if st not in (200, 201):
        print("  ❌ 建 Release 失败:", st, b[:300]); raise SystemExit(1)
    rel = json.loads(b); print(f"  已建 Release {tag}")

# 旧附件先清（幂等）：附件名带构建时间戳，每次都不一样，只按同名删会让重发
# 累积出多个 APK。按前缀清掉本仓所有 WorkBuddy2API*.apk 附件。
name = apk.name
st, b, _ = call(api + f"/releases/{rel['id']}/assets")
for a in json.loads(b) if st == 200 else []:
    if a["name"].startswith("WorkBuddy2API") and a["name"].endswith(".apk"):
        call(api + f"/releases/assets/{a['id']}", "DELETE")
        print(f"  删掉旧 asset {a['name']}，准备重传")
url = rel["upload_url"].split("{")[0] + "?name=" + name
data = apk.read_bytes()
st, b, _ = call(url, "POST", data, ctype="application/vnd.android.package-archive")
if st not in (200, 201):
    print("  ❌ 上传 APK 失败:", st, b[:300]); raise SystemExit(1)
a = json.loads(b)
print(f"  ✅ 上传成功 {a['name']}  {a['size']} 字节")
print(f"  {rel['html_url']}")
PY
