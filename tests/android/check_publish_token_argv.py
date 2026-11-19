#!/usr/bin/env python3
"""W6-H-05 / W6-M-04 的源码级回归断言（纯文本，不需要 JDK）。

两条都在 push_github.sh 里，且都是「脚本自己已经写了规矩、实现却没遵守」：
  1. 52-54 行写着「argv 不可信」，188/191 行却把 PAT 放进 `curl -H` 的 argv；
  2. 敏感串扫描给 >8MB 的文件开了后门，而仓库里正好有个 12.5MB 的 ELF。

用法：  python3 tests/android/check_publish_token_argv.py
退出码：0 全过；1 有失败。
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SH = ROOT / "push_github.sh"

fails = []


def check(cond, msg):
    if cond:
        print("  OK   %s" % msg)
    else:
        fails.append(msg)
        print("  FAIL %s" % msg)


src = SH.read_text(encoding="utf-8") if SH.exists() else ""
if not src:
    fails.append("找不到 %s" % SH)
lines = src.splitlines()

print("== W6-H-05 token 不许进 argv ==")

# ① 任何 `-H "Authorization...` 都是把凭据摊在 /proc/<pid>/cmdline 里。
bad_h = [ln for ln in lines if re.search(r'-H\s+["\']Authorization', ln)]
check(not bad_h, "push_github.sh 不许用 -H 传 Authorization（argv 可被 ps 读到）")
if bad_h:
    for ln in bad_h:
        print("       %s" % ln.strip())

# ② 认证头必须走 stdin：--config - + printf。
check("--config -" in src, "必须用 curl --config - 从 stdin 读认证头")
check("ghcurl()" in src, "必须把「带认证头的 curl」收敛成一个函数（避免下次又写回 -H）")
fn = src[src.find("ghcurl()"):]
fn = fn[:fn.find("}") + 1] if "}" in fn else fn
check("printf" in fn and "Authorization: Bearer %s" in fn,
      "ghcurl 必须用 printf 生成 header 行（token 只出现在参数里，不进 argv）")

# bash 函数没有提升：定义必须出现在第一次调用之前（3/5 建仓那一步就用了）。
def_line = next((i for i, l in enumerate(lines) if l.startswith("ghcurl()")), -1)
uses = [i for i, l in enumerate(lines) if "ghcurl " in l and not l.startswith("ghcurl()")]
check(def_line >= 0 and uses and def_line < min(uses),
      "ghcurl 必须先定义后使用（否则脚本跑到 3/5 才 command not found）")

# ③ 所有 curl 调用的 argv 里不许直接出现 $TOKEN（printf 那行除外）。
bad_tok = [ln for ln in lines
           if re.search(r"\bcurl\b", ln) and "$TOKEN" in ln and "printf" not in ln]
check(not bad_tok, "curl 的命令行里不许出现 $TOKEN")
if bad_tok:
    for ln in bad_tok:
        print("       %s" % ln.strip())

# ④ 反向断言：文件里必须还有真正的认证（别为了「过检查」把认证删了）。
check("Authorization: Basic $B64" in src, "git 的认证头（gauth）必须保留")

print()
print("== W6-M-04 大文件也要扫 ==")

check("8_000_000: continue" not in src,
      "扫描不许再对 >8MB 的文件 continue（仓库里那个 12.5MB ELF 就是这样漏掉的）")
check("CHUNK" in src and "OVERLAP" in src,
      "大文件必须分块读（CHUNK/OVERLAP），块间留重叠避免模式跨块漏报")
check("def scan_big(" in src, "必须有专门的大文件扫描函数")
check("p.stat().st_size" in src and re.search(r"why_list\s*=\s*scan_big\(p\)", src) is not None,
      "按 size 分派：小文件整读、大文件分块，两条路都要扫")
check("无法读取（扫描未完成）" in src,
      "读不动时必须报错（把「没扫」当「干净」是最坏的一种通过）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
