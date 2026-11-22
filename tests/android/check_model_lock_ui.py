#!/usr/bin/env python3
"""模型锁池 / 账号可用性在**原生端**可见性的回归断言（纯文本，不需要 JDK）。

背景（用户反馈）：
  · 网页面板有「模型锁池」表，原生账号池页完全没有 —— 上游按（账号, 模型）限流时，
    聊天只回 no_healthy_account，用户分不清「这个模型不能用」还是「号都挂了」；
  · 账号卡此前只有一个 8dp 小圆点表示状态，「这个号现在能不能用」要凑近看颜色。

这个脚本盯四件事：
  1. /panel/api/overview 的 model_locks 被解析进 ModelLock（九个字段一个不能少）；
  2. ScreenAccounts 在 hero 卡片之后画出锁池卡片，且**空态也要画**（不画等于还是看不到）；
  3. 行内必须给出「可用 x/y」+ 状态标签 + 解锁倒计时/原因；
  4. 账号卡用彩色标签显示可用性，而不是只有小圆点。

用法：  python3 tests/android/check_model_lock_ui.py
退出码：0 全过；1 有失败。
"""
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


def strip_comments(src):
    out = []
    for line in src.splitlines():
        i = line.find("//")
        out.append(line if i < 0 else line[:i])
    return "\n".join(out)


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


svc = strip_comments(read("PanelService.java"))
acc = strip_comments(read("ScreenAccounts.java"))

print("PanelService：解析 model_locks")
check('optJSONArray("model_locks")' in svc, "overview 解析 model_locks 数组")
for field in ("model", "realm", "total", "servable", "locked", "state",
              "unlock_at", "fully_unlock_at", "reason"):
    check('optString("%s")' % field in svc or 'optInt("%s")' % field in svc,
          "ModelLock 读取字段 %s" % field)
check("usable()" in body(svc, "static final class ModelLock"),
      "ModelLock 提供 usable()（判据是 servable > 0，与选号口径一致）")

print("ScreenAccounts：锁池卡片")
render = body(acc, "private void renderAccounts(")
check("lockCard(col, o)" in render, "renderAccounts 调用 lockCard")
check(render.find("lockCard(col, o)") > render.find("col.addView(hero"),
      "锁池卡片画在 hero 概览之后（先看总盘子，再看为什么不可用）")
lock = body(acc, "private void lockCard(")
if not lock:
    fails.append("ScreenAccounts.lockCard() 找不到")
check('"全部可选"' in lock and "没有模型级限流" in lock,
      "空态必须画出来（不画等于用户还是看不到这张表）")
check('"可用 " + m.servable + "/" + m.total' in lock,
      "每行给出「可用 x/y」（与网页面板同口径）")
check("m.stateText()" in lock, "每行给出状态标签（全部锁定 / 无号空闲 / 部分锁定）")
check("unlockText(m.unlockAt" in lock, "每行给出最早解锁时间")
check("fullyUnlockAt" in lock, "全部锁定行还要给「全部解锁」时间")
check("m.reason" in lock, "上游原文（冷却原因）要透出")
check('"locked".equals(m.state) ? Theme.BAD' in lock,
      "状态配色：locked=红 / starved=黄 / partial=蓝")

print("账号可用性：标签而不是只有小圆点")
check("Theme.tag(a, s.statusText(), dotColor)" in acc,
      "账号卡用彩色标签显示可用性（色弱/强光下小圆点不可辨）")
check("disabledReason" in body(acc, "private View accountCard("),
      "禁用原因占用副行（比重复一遍状态更有用）")

print("时间解析（Go 的 RFC3339 带数字偏移与小数秒）")
ms = body(acc, "private static long rfc3339Ms(")
check("y < 2000" in ms, "年份 < 2000 视为无效（Go 零值是 0001-01-01）")
check("offMin" in ms and "+/-" or "offMin" in ms, "解析数字时区偏移（+08:00）")
check("TimeZone.getTimeZone(\"UTC\")" in ms, "先按 UTC 组装再减偏移")

if fails:
    print("\n失败 %d 项：" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("\n全部通过")
