#!/usr/bin/env python3
"""日志着色的一致性断言（纯文本检查，不需要 JDK / android.jar）。

背景：App 的原生日志页此前用 `tv.setText(String)` 一把梭，整页只有一种颜色；
网页面板早就有「级别色 + 频道徽标」。补上之后最怕的是**两边判据漂开** ——
同一条网关日志在网页上标红、在 App 里不标，等于同一份数据有两套事实。

所以这里同时钉住两侧：
  * `internal/panel/app.js` 的 `loadLogs()` 正则
  * `apk-src/.../LogLevel.java` 的判据
  * `ScreenLogs.java` 是否真的把级别/频道映射到了主题令牌

行为级验证在 `tests/android/LogLevelSelfCheck.java`（17 项，宿主 JVM 直接跑）。

用法：  python3 tests/android/check_log_color.py
退出码：0 全过；1 有失败。
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "apk-src/java/com/workbuddy/api"
APPJS = ROOT / "workbuddy2api-panel/internal/panel/app.js"

fails = []


def read(p, label):
    if not p.exists():
        fails.append("找不到 %s（%s）" % (p, label))
        return ""
    return p.read_text(encoding="utf-8")


def body(src, signature):
    i = src.find(signature)
    if i < 0:
        return ""
    j = src.find("{", i + len(signature))
    if j < 0:
        return ""
    depth = 0
    for k in range(j, len(src)):
        c = src[k]
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return src[j:k + 1]
    return src[j:]


def check(cond, msg):
    if cond:
        print("  OK   %s" % msg)
    else:
        fails.append(msg)
        print("  FAIL %s" % msg)


js = read(APPJS, "网页面板 app.js")
lv = read(SRC / "LogLevel.java", "级别判据")
logs = read(SRC / "ScreenLogs.java", "原生日志页")

print("== 日志着色（网页 ↔ 原生） ==")

# ① 两侧判据必须同一套词。`err` 是必需项：后端有 27 处 `ERR:` 前缀，
#    而 `i` 标志只能解决大小写，解决不了 ERR 与 ERROR 的拼写差异。
ERR_WORDS = ["error", "err", "失败", "错误"]
WARN_WORDS = ["warn", "冷却", "熔断"]

m = re.search(r"const lvl = /([^/]+)/i\.test\(e\.text\).*?/\s*([^/]+)/i\.test", js)
check(m is not None, "app.js 里能找到 lvl 的两条正则")
if m:
    err_alt, warn_alt = m.group(1).split("|"), m.group(2).split("|")
    check(err_alt == ERR_WORDS,
          "app.js 错误分支必须是 error|err|失败|错误（实得 %s）" % "|".join(err_alt))
    check(warn_alt == WARN_WORDS,
          "app.js 警告分支必须是 warn|冷却|熔断（实得 %s）" % "|".join(warn_alt))

of = body(lv, "public static int of(String text)")
for w in ERR_WORDS:
    check('"%s"' % w in of, "LogLevel.of 必须判 %r" % w)
for w in WARN_WORDS:
    check('"%s"' % w in of, "LogLevel.of 必须判 %r" % w)

# ② 原生页必须真的用上判据，而不是又退回单色。
check("LogLevel.of" in logs, "ScreenLogs 必须调 LogLevel.of（不许自己再写一套）")
# 关键是**调用点**：方法留着但改回 setText(String) 的话，页面照样是单色的 ——
# 这条断言就是防那种「看起来改过」的半成品。
check("setText(buildLogText(" in logs,
      "ScreenLogs 必须 setText(buildLogText(...))，不许退回单色字符串")
check("ForegroundColorSpan" in logs, "ScreenLogs 必须用 ForegroundColorSpan 上级别色")
check("BackgroundColorSpan" in logs, "ScreenLogs 必须用 BackgroundColorSpan 画频道徽标")

# ③ 徽标只在「全部」频道下画（与网页 logCh === 'all' 同一条件）
build = body(logs, "private CharSequence buildLogText")
check('"all".equals(logCh)' in build,
      "频道徽标必须只在 logCh=all 时画（与网页 logCh === 'all' 一致）")

# ④ 三种频道各有自己的色，不能都落到同一个分支
for ch in ["task", "chat", "sys"]:
    check('"%s".equals(ch)' % ch in logs, "频道 %r 必须有自己的配色分支" % ch)

# ⑤ 时间戳要压暗，否则正文和戳子同色，仍然「一片一个颜色」
check("Theme.INK3" in build, "时间戳必须用 INK3 压暗（正文取 INK2）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过（%d 项）" % 19)
