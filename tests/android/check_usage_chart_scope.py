#!/usr/bin/env python3
"""issue #205 的源码级回归断言（纯文本，不需要 JDK / android.jar）。

现象：用量页只有 24H 有柱状图，3D/7D/30D 恒空白。

根因在**客户端**：`applyChart` 按 scope 二选一（24H 取 hour、其余取 day）且没有回落；
而服务端日桶只由「满 7 天的小时桶折叠」产生（internal/usage hourlyKeep=7d），
3D/7D 窗口永远够不到日桶 —— 数据都在 series 里，只是被筛掉了。

这个脚本盯四件事：
  1. 不许再出现「按 scope 二选一」的一次性筛选（那是 #205 的形状）；
  2. >24H 必须把小时点按日期聚合，且服务端日桶要覆盖同一天（权威 + 覆盖更早历史）；
  3. 聚合后只剩一天时必须退回实际小时点（刚装几小时看得到那几根，而不是一根）；
  4. 空状态文案不许再把「日桶要等 7 天」描述成设计；默认档保持 72 小时。

用法：  python3 tests/android/check_usage_chart_scope.py
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
    """剥掉 // 行注释：修复说明里会引用旧写法，注释不能骗过反向断言。"""
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


ui = strip_comments(read("ScreenUsage.java"))
code = strip_comments(read("UsageBuckets.java"))

print("ScreenUsage：取点规则已外置，不再自己按 scope 二选一")
chart = body(ui, "private void applyChart(")
if not chart:
    fails.append("ScreenUsage.applyChart() 找不到")
check('wantHour ? "hour".equals' not in ui,
      "不存在 `wantHour ? \"hour\" : \"day\"` 这种一次性筛选（#205 的根因形状）")
check("UsageBuckets.bucket(" in chart, "applyChart 委托给 UsageBuckets.bucket()")
check("shortLabel(p.t, res.hourly)" in chart, "标签按结果里的 hourly 口径走（不是自己再判一次）")

print("UsageBuckets：>24H 小时点按日期聚合成日柱")
bucket = body(code, "public static Result bucket(")
check("dayKey(p.t)" in bucket, "按日期键聚合（dayKey）")
check("acc.requests += p.requests" in bucket and "acc.tokens += p.tokens" in bucket
      and "acc.credits += p.credits" in bucket,
      "请求 / TOKENS / 积分三项都累加（缺一项切换指标就空）")
check("byDay.put(d, p)" in bucket, "服务端日桶覆盖同一天（权威聚合 + 覆盖 7 天以前）")

print("数据不足时退回小时点")
check("r.pts.size() <= 1" in bucket and "r.pts.addAll(hoursPts)" in bucket,
      "聚合后只剩一天时退回实际小时点（刚装几小时看到的是那几根）")

print("文案与默认档")
check("更早的区间按天统计" not in code, "空状态不再把「日桶要等 7 天」说成设计")
check("暂无数据" in ui, "空状态改成中性的「该区间暂无数据」")
check("private int usageHours = 72;" in ui,
      "默认档保持 72 小时（issue 评论区明确要求：用「有图」解决空图，不要改默认档）")

print("dayKey 实现")
dk = body(code, "public static String dayKey(")
check("substring(0, 10)" in dk, "取 t 的前 10 个字符作为日期键")
check('return ""' in dk, "非法输入返回空串（不抛异常、不造桶）")

if fails:
    print("\n失败 %d 项：" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("\n全部通过")
