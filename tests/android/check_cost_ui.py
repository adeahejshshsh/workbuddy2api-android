#!/usr/bin/env python3
"""余额刷新设置 + 首页耗电提示条的源码级断言（issue #194 / #177 第 2 条）。

为什么不做成单测：这两项全在 Android 视图层与 SharedPreferences 上，宿主 JVM 跑不起来。
能防的回归是「控件被删掉 / 又写回 0 当关闭 / 提示条变成弹窗」。

用法：  python3 tests/android/check_cost_ui.py
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "apk-src/java/com/workbuddy/api"

fails = []


def read(name):
    p = SRC / name
    if not p.exists():
        fails.append("找不到 %s" % p)
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


def strip_comments(src):
    out = []
    for line in src.splitlines():
        i = line.find("//")
        out.append(line if i < 0 else line[:i])
    return "\n".join(out)


def check(cond, msg):
    if cond:
        print("  OK   %s" % msg)
    else:
        fails.append(msg)
        print("  FAIL %s" % msg)


svc = read("PanelService.java")
settings = read("ScreenSettings.java")
console = read("Console.java")
main = read("MainActivity.java")

print("== issue #194 余额刷新间隔 ==")

check("balanceMinutes" in body(svc, "static final class Settings"),
      "Settings 必须带 balanceMinutes 字段")
check("balanceEnabled" in body(svc, "static final class Settings"),
      "Settings 必须带 balanceEnabled 字段")
check('sch.optInt("balance_refresh_minutes"' in svc,
      "loadSettings 必须读 schedule.balance_refresh_minutes")
check('sch.optBoolean("balance_refresh_enabled"' in svc,
      "loadSettings 必须读 schedule.balance_refresh_enabled")

save = body(svc, "void saveSettings(Settings st")
check('put("balance_refresh_minutes"' in save,
      "saveSettings 必须写 balance_refresh_minutes")
check('put("balance_refresh_enabled"' in save,
      "saveSettings 必须写 balance_refresh_enabled")
# 「关闭」必须走 enabled=false。把分钟数写 0 会被 config 校验回落成 5（config.go:1501），
# 用户以为关了、实际还是 5 分钟一次 —— 这条断言就是钉这个坑。
# 只看「语句里有没有 0」不够：三元表达式 `x ? n : 0` 也会写 0 却匹配不上字面量。
# 直接要求那条写入语句**逐字**受 balanceMinutes>0 保护 —— 关掉开关时不动分钟数。
check("if (st.balanceMinutes > 0) sch.put(\"balance_refresh_minutes\", st.balanceMinutes);" in save,
      "分钟数写入必须受 balanceMinutes>0 保护（关闭只能改 enabled，不能写 0）")

card = body(settings, "private View balanceCard")
check(bool(card), "ScreenSettings 必须有 balanceCard")
for label in ["5 分钟", "10 分钟", "30 分钟", "60 分钟", "关闭"]:
    check(label in card, "余额刷新档位必须含「%s」" % label)
check("不需要重启" in card or "立即生效" in card,
      "卡片必须说明保存后立即生效（SetBalanceInterval 热生效）")

print()
print("== issue #177 第 2 条 首页提示条 ==")

build = body(console, "private LinearLayout buildCostBanner")
check(bool(build), "Console 必须有 buildCostBanner")
# 断言**赋值点**而不是方法定义：方法留着但不挂到视图树上，等于没实现。
check("costBanner = buildCostBanner();" in console,
      "提示条必须被构建并挂到 Console 根视图（只留方法定义不算）")
check("AlertDialog" not in build and "Sheet." not in build,
      "提示条必须是内联文字，不是开屏弹窗/Sheet")
check("常驻网关会阻止设备休眠" in build,
      "文案必须说清「阻止设备休眠」这件事")
# 裸 View（圆点）的 LayoutParams 必须是固定 dp 尺寸。WRAP_CONTENT 的 View 会取满
# 父容器的 AT_MOST 尺寸 —— 6dp 圆点会被拉成铺满整页的巨大椭圆（用户实测截图）。
check(re.search(r"LinearLayout\.LayoutParams dlp = new LinearLayout\.LayoutParams\(\s*"
                r"Theme\.dp\(a, 6\),\s*Theme\.dp\(a, 6\)\)", build) is not None,
      "提示条的圆点必须用固定 6dp（WRAP_CONTENT 会把它拉成整页椭圆）")
# 只钉「圆点那一处」：同一方法里 TextView/胶囊用 WRAP_CONTENT 是正常的，不能一刀切。
check("box.addView(dot, dlp)" in build,
      "圆点必须用显式 dlp 挂上去（不能让它自己 WRAP_CONTENT 撑满）")
check("PREF_COST_BANNER" in console,
      "关闭状态必须落 SharedPreferences（关一次永久生效）")
refresh = body(console, "void refreshCostBanner")
check("ST_RUNNING" in refresh and "ST_EXTERNAL" in refresh,
      "只有网关真在跑时才显示（停止态不该挂着耗电提示）")
check('"accounts".equals(screen)' in refresh,
      "只在首页显示，别在日志/设置页也占一行")
check("console.refreshCostBanner()" in main,
      "MainActivity.onStart 必须刷新提示条（从后台回来状态可能变了）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过（%d 项）" % 19)
