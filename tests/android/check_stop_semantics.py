#!/usr/bin/env python3
"""issue #176 的源码级回归断言（纯文本检查，不需要 JDK / android.jar）。

为什么不做成单测：这几条语义全部落在 Android 框架里（SharedPreferences、
Service 生命周期、Handler 队列），宿主 JVM 跑不起来 —— tests/android 下那两个
SelfCheck 只能验纯计算（退避序列、IP 判定）。能防的回归是「关键调用被删掉/
又改回无条件」，所以按函数体做包含性断言，每条对应 issue 里的一个根因。

用法：  python3 tests/android/check_stop_semantics.py
退出码：0 全过；1 有失败。
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
    """取 signature 所在函数的函数体（从签名处按花括号配平）。

    找不到签名返回 ""；这样断言会以「找不到 X」的形式红，而不是静默通过。
    """
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


def ok(msg):
    print("  OK   %s" % msg)


def check(cond, msg):
    if cond:
        ok(msg)
    else:
        fails.append(msg)
        print("  FAIL %s" % msg)


gw = read("GatewayService.java")
console = read("Console.java")
main = read("MainActivity.java")

print("== issue #176 停止语义 ==")

# ① 通知栏「停止」必须落盘 user_stop，与 stop(Context) 同语义。
#    只写 service_up 的话 BootReceiver / ReviveJobService 会认为「用户没停过」。
stop_branch = body(gw, "ACTION_STOP.equals")
check("persistUserStop" in stop_branch,
      "ACTION_STOP 分支必须调 persistUserStop（否则 user_stop 永远写不进去）")

stop_static = body(gw, "public static void stop(Context c)")
check("persistUserStop" in stop_static,
      "stop(Context) 必须与 ACTION_STOP 共用 persistUserStop")

# ④ 用户明确要停 = 不等优雅排空，直接强杀；否则 3s 延时强杀的 daemon 线程
#    会跟着进程一起消失，Go 子进程活过 SIGTERM 就成占着 7863 的孤儿。
check("killProc(true)" in stop_branch,
      "ACTION_STOP 必须 killProc(true)（直接强杀，不留孤儿窗口）")
hard_kill = body(gw, "synchronized void killProc(boolean hard)")
check("destroyForcibly" in hard_kill,
      "killProc(boolean hard) 的 hard 路径必须 destroyForcibly")

# ⑤ 用户停过之后，划掉任务列表不许重挂复活链。
removed = body(gw, "public void onTaskRemoved(Intent root)")
check(re.search(r"if\s*\(\s*userStop\s*\)", removed) is not None,
      "onTaskRemoved 必须先判 userStop 再 armAlarm/schedule")

# ② 「打开 App」不再撤销停止标记；否则 user_stop 在任何路径上都不可能是 true。
oncreate = body(main, "protected void onCreate(Bundle")
check('putBoolean("user_stop", false)' not in oncreate,
      "MainActivity.onCreate 不许无条件清 user_stop")
check("GatewayService.userStopped(this)" in oncreate,
      "MainActivity.onCreate 必须按 userStopped() 决定是否自动启动")

# ③ Console 必须能渲染「已停止」终态；它此前全文不引用 GatewayService。
check("GatewayService.statusOf()" in console,
      "Console 必须读 GatewayService.statusOf()（此前零引用，一切失败都画「启动中」）")
check("showTerminal" in console, "Console 必须有 showTerminal 终态页")
fail_body = body(console, "public void fail(String msg)")
check("ST_STOPPED" in fail_body,
      "Console.fail 必须在停止态跳过重试、直接进终态页")
check("if (!quiet) toast" in fail_body,
      "Console.fail 的 quiet 分支必须保留（非连接类错误仍按 quiet 决定是否提示）")

# ⑥ Console 有自己的 Handler，Activity 销毁必须显式清，否则旧链会覆盖新终态页。
check("void dispose()" in console, "Console 必须有 dispose()")
ondestroy = body(main, "protected void onDestroy()")
check("console.dispose()" in ondestroy,
      "MainActivity.onDestroy 必须调 console.dispose()（清 Console 自己的 Handler）")

# ④ 复用态不能只看「端口能 accept」：孤儿进程占着端口但不应答 HTTP。
watch = body(gw, "void watchLoop()")
check("httpUp()" in watch,
      "watchLoop 的 external 分支必须探 httpUp()（否则死端口被当成健康实例）")
check("extStale" in gw, "GatewayService 必须记录 extStale（复用态不应答轮数）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过（%d 项）" % 15)
