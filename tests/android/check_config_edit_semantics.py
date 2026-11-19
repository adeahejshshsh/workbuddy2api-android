#!/usr/bin/env python3
"""W6-M-02 / W6-M-13 的源码级回归断言（纯文本，不需要 JDK）。

  · W6-M-02：配置页的编辑值原先只在失焦时写回，直接点「保存」提交旧值；
  · W6-M-13：lanCache 只算一次就永不失效，Wi-Fi 换网后界面一直显示旧 IP。

两条都是「看起来能用、边界上出错」的类型，只能靠函数体断言守住。

用法：  python3 tests/android/check_config_edit_semantics.py
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


cfg = read("ConfigActivity.java")
gw = read("GwConfig.java")

print("== W6-M-02 保存前必须取到最新编辑值 ==")

check("commits" in cfg, "ConfigActivity 必须有「控件 → 提交动作」登记表")
check("IdentityHashMap" in cfg,
      "登记表要用 IdentityHashMap（EditText 没重写 equals，按身份登记才不会被合并）")
save = body(cfg, "private void saveAll()")
check("clearFocus()" in save, "saveAll 必须先 clearFocus（触发一次失焦提交）")
check("commits.values()" in save, "saveAll 必须遍历登记表逐个提交")
m_focus = save.find("clearFocus()")
m_iter = save.find("commits.values()")
m_save = save.find("saveConfig")
check(m_focus > 0 and m_iter > m_focus and m_save > m_iter,
      "顺序必须是 clearFocus → 逐个提交 → saveConfig（提交在保存之后等于没改）")

row = body(cfg, "private View row(final JSONObject owner, final String key, Object value)")
check(row.count("commits.put(et, commit)") >= 3,
      "三种可编辑控件（数字 / 字符串 / 数字数组）都必须登记提交动作")
check("final Runnable commit" in row or "Runnable commit" in row,
      "每个输入框要有自己的 commit 闭包（共享一个会把值写串）")
check("commits.clear()" in body(cfg, "private void render()"),
      "render() 重建时必须清空登记表（旧控件会把值写回已废弃的 owner）")

print()
print("== W6-M-13 lanCache 必须会过期 ==")

check("LAN_CACHE_TTL_MS" in gw, "GwConfig 必须定义缓存 TTL")
check(re.search(r"LAN_CACHE_TTL_MS\s*=\s*\d+", gw) is not None, "TTL 必须是具体毫秒值")
lan = body(gw, "static String lanIPv4()")
check("elapsedRealtime()" in lan,
      "必须用 elapsedRealtime 判过期（currentTimeMillis 会被改表/时区影响）")
check(re.search(r"now\s*-\s*lanCacheAt\s*<\s*LAN_CACHE_TTL_MS", lan) is not None,
      "命中缓存前必须判 TTL（原实现只判 lanCache != null → 永不失效）")
check("lanCacheAt = now" in lan, "算完必须记下时间戳")
check("invalidateLanCache" in gw, "要留一个主动作废入口（网络变化回调可以调）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
