#!/usr/bin/env python3
"""issue #177 的源码级回归断言（纯文本检查，不需要 JDK / android.jar）。

为什么不做成单测：这些全部落在 Android 框架里（PowerManager.WakeLock、
WifiManager.WifiLock、AlarmManager），宿主 JVM 跑不起来。能防的回归是
「又把强度调回去」—— 每条断言对应 issue 里的一个耗电源。

用法：  python3 tests/android/check_wake_semantics.py
退出码：0 全过；1 有失败。
"""
import json
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



def strip_comments(src):
    """剥掉 // 行注释，断言只看代码。

    必须做这一步：修复说明里会写「不能再套 if (!wake.isHeld())」，
    而字面量检查会把注释当成代码 —— 反向断言（「不许出现 X」）会被自己的
    注释绊红，正向断言也可能被注释骗绿。先剥注释，判据才落在真实代码上。
    """
    out = []
    for line in src.splitlines():
        # 只处理行注释：本仓没有把 // 放进字符串字面量的习惯，够用。
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


gw = read("GatewayService.java")
cfg = ROOT / "apk-src/assets/config.json"

print("== issue #177 耗电强度 ==")

# ① PARTIAL_WAKE_LOCK 必须带超时。无参 acquire() 会被系统耗电归因直接点名。
hold = body(gw, "void holdWake()")
check("WAKE_TIMEOUT_MS" in gw, "必须定义 WAKE_TIMEOUT_MS")
check(re.search(r"wake\.acquire\(\s*WAKE_TIMEOUT_MS\s*\)", hold) is not None,
      "holdWake 必须 wake.acquire(WAKE_TIMEOUT_MS)，不许无参 acquire()")
check(re.search(r"wake\.acquire\(\s*\)", strip_comments(hold)) is None,
      "holdWake 里不许残留无参 acquire()")

# ①-b W6-H-01：带超时的锁必须**无条件**续期，不能被 isHeld() 挡住 ——
#     带超时的锁在到期前 isHeld() 一直是 true，条件永不成立，续期一次都不会发生。
check(re.search(r"if\s*\(\s*!?\s*wake\.isHeld\(\)\s*\)", strip_comments(hold)) is None,
      "holdWake 不许用 isHeld() 守卫 acquire（带超时的锁会永不续期）")

# ①-c W6-H-01：必须有调用点在续期。只断言函数体写法不够 —— 上一版就是
#     「函数写得对、但只有 onStartCommand 调过一次」，红线绿而功能坏。
# 必须剥注释再判：修复说明里会写「只在 onStartCommand 调过一次 holdWake()」，
# 不剥的话注释本身就是一次命中，删掉真实调用也照样绿 —— 这条断言第一次写就踩了。
check("holdWake();" in strip_comments(body(gw, "void watchLoop()")),
      "watchLoop 每轮必须调 holdWake() 续期（否则 30s 后锁释放，看门狗被冻）")

# ② Wi-Fi 锁降到 WIFI_MODE_FULL。HIGH_PERF 让射频永不降档，是最贵的一项。
wifi = body(gw, "void holdWifi()")
first_mode = re.search(r"createWifiLock\(\s*WifiManager\.(WIFI_MODE_\w+)", wifi)
check(first_mode is not None and first_mode.group(1) == "WIFI_MODE_FULL",
      "holdWifi 首选必须是 WIFI_MODE_FULL（不许首选 HIGH_PERF）")

# ⑤ 5 分钟兜底 alarm 必须真的能到期：不许每轮重臂。
watch = body(gw, "void watchLoop()")
check("alarmArmedAt" in watch,
      "watchLoop 的重臂必须按 alarmArmedAt 节流，不许每轮 armAlarm")
check("ALARM_MS / 2" in watch,
      "重臂间隔必须是 ALARM_MS/2（< ALARM_MS，兜底 alarm 才能到期）")
arm = body(gw, "static void armAlarm(Context c)")
check("alarmArmedAt = SystemClock.elapsedRealtime()" in arm,
      "armAlarm 成功下发后必须记 alarmArmedAt")

# ④ balance_refresh_minutes：每轮对池内每个账号发一次真实上游额度查询，默认 5 分钟太密。
try:
    c = json.loads(cfg.read_text(encoding="utf-8"))
    m = c.get("schedule", {}).get("balance_refresh_minutes")
    check(isinstance(m, int) and m >= 30,
          "assets/config.json 的 balance_refresh_minutes 必须 >= 30（当前 %r）" % (m,))
except Exception as e:  # noqa: BLE001
    fails.append("config.json 解析失败：%s" % e)
    print("  FAIL config.json 解析失败：%s" % e)

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过（%d 项）" % 9)
