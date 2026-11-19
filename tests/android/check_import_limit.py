#!/usr/bin/env python3
"""W6-H-04 的源码级回归断言（纯文本，不需要 JDK）。

根因是两条叠加：读取端没有字节上限（`ByteArrayOutputStream` 无界，峰值 ~2.5×），
选择器又给的是 `*/*`（误选 300MB 视频即 OOM）。OOM 被外层 catch(Throwable) 吞掉时
堆已经碎了，崩的是同一进程里的 GatewayService。所以断言要同时盯住「上限」与「MIME」。

用法：  python3 tests/android/check_import_limit.py
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


accounts = read("ScreenAccounts.java")
main = read("MainActivity.java")

print("== W6-H-04 导入上限 ==")

check("IMPORT_MAX_BYTES" in accounts, "ScreenAccounts 必须有导入字节上限常量")
check(re.search(r"IMPORT_MAX_BYTES\s*=\s*32L?\s*\*\s*1024\s*\*\s*1024", accounts) is not None,
      "上限必须是 32MB（32L * 1024 * 1024）")

imp = body(accounts, "void importUri(android.net.Uri uri)")
check(bool(imp), "importUri 存在")
check(re.search(r"total\s*>\s*IMPORT_MAX_BYTES", imp) is not None,
      "读取循环里必须累计 total 并与上限比较（provider 报的 SIZE 可能撒谎）")
# 上限判断必须在写缓冲之前：否则最后一字节仍然进了堆。
m_cap = imp.find("total > IMPORT_MAX_BYTES")
m_write = imp.find("bos.write(buf, 0, n)")
check(m_cap > 0 and m_write > 0 and m_cap < m_write,
      "超限判断必须在 bos.write 之前（先写再判等于没限）")
check(re.search(r"total\s*>\s*IMPORT_MAX_BYTES[\s\S]{0,200}?return;", imp) is not None,
      "超限必须立刻 return（中止导入，不再继续读）")
check("try (java.io.InputStream in" in imp,
      "InputStream 必须 try-with-resources（旧写法 read 抛异常时句柄永不关闭）")
check("in.close()" not in imp,
      "不该再出现裸 in.close()（try-with-resources 已经负责关闭）")
check("OpenableColumns.SIZE" in imp,
      "能先按 provider 报的 SIZE 拒掉就不要读（300MB 视频连 8KB 都不用读）")

print()
print("== W6-H-04 选择器 MIME 收窄 ==")
req = body(main, "void requestImportFile()")
check('setType("application/json")' in req,
      "原生导入选择器必须 setType(application/json)")
check('setType("*/*")' not in req, "原生导入选择器不许再用 */*")

chooser = body(main, "public boolean onShowFileChooser(WebView v,")
check('setType("application/json")' in chooser,
      "WebView 的 onShowFileChooser 兜底也必须 application/json")
check('setType("*/*")' not in chooser, "onShowFileChooser 兜底不许再用 */*")

# 整个 MainActivity 里都不该再有 */*（防以后又复制粘贴回去）
check(main.count('setType("*/*")') == 0, "MainActivity 全局不许再出现 setType(\"*/*\")")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
