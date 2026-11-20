#!/usr/bin/env python3
"""issue #197 的源码级回归断言（纯文本，不需要 JDK / android.jar）。

需求：原生账号池页的概览条要有「所有账号积分合计」，口径照网页端 app.js 的
sCredits——同一个池子两个界面各算各的，迟早对不上。

这个脚本盯的就是「口径三条」和「渲染位置」：
  1. 不限量哨兵必须排除（credits_total == -1 或 credits >= 1e12），否则一个
     不限量号就能把总额撑到天文数字；
  2. 分母只累加 credits_total > 0（0 = 未知，不是零额）；
  3. 合计行必须挂在 hero 卡片里（跟四个 KPI 同一张卡），不是另起一张卡或塞进
     会被挤扁的第五列。

用法：  python3 tests/android/check_credits_sum.py
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
    """剥掉 // 行注释：注释里会写到旧写法/反例，不能骗过正向或反向断言。"""
    out = []
    for line in src.splitlines():
        i = line.find("//")
        out.append(line if i < 0 else line[:i])
    return "\n".join(out)


def body(src, signature):
    """取 signature 所在函数的函数体（花括号配平）。找不到返回 ""。"""
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


src = read("ScreenAccounts.java")
code = strip_comments(src)

print("creditsSummary：口径与网页端 sCredits 对齐")
summ = body(code, "static String[] creditsSummary(")
if not summ:
    fails.append("ScreenAccounts.creditsSummary() 找不到")
else:
    check("isUnlimited(s)" in summ, "不限量哨兵先排除（不进分子也不进分母）")
    check("Math.max(0, s.credits)" in summ, "分子累加 credits，负值钳到 0（防脏数据把合计算小）")
    check("if (s.creditsTotal > 0) tot += s.creditsTotal;" in summ,
          "分母只累加 credits_total > 0（0 = 未知，不是零额）")
    check("counted == 0" in summ, "全部账号都不限量时不能显示 0/0")
    check('"积分剩余 / 总额"' in summ, "有总额时的标签就是 issue 里写的「积分剩余 / 总额」")
    check("accs == null || accs.isEmpty()" in summ, "空池返回 null（整行不画），不能画出 0")

print("isUnlimited：两种哨兵都要认")
unl = body(code, "static boolean isUnlimited(")
check("s.creditsTotal == -1" in unl, "credits_total == -1 判为不限量")
check("s.credits >= 1000000000000L" in unl, "credits >= 1e12 判为不限量")

print("渲染位置：合计行挂在 hero 卡片里")
render = body(code, "private void renderAccounts(")
check("creditsSummary(o.accounts)" in render, "renderAccounts 调用 creditsSummary")
check("hero.addView(line, dlp)" in render and "hero.addView(Theme.kpi(a, creditSum[0], creditSum[1]" in render,
      "合计行加进 hero（与四个 KPI 同卡），不另起卡片")
check("row.addView(Theme.kpi(a, String.valueOf(o.total)" in render,
      "原有四个 KPI 未被改动（第一个仍是「账号」）")

if fails:
    print("\n失败 %d 项：" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("\n全部通过")
