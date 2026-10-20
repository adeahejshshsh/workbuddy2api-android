#!/usr/bin/env python3
"""把一段新的版本小节前置到 CHANGELOG.md，并保证只有一个「（当前）」。

为什么需要这个脚本：
    之前每次发版都是手写一段 Python 内联合并，逻辑是「新版本写 ## vX（当前）」+
    「原文件从上一个版本处拼接」——**但从来没人把上一版的「（当前）」去掉**。
    结果 v1.26.0 到 v1.27.6 连续 17 个小节全标着「（当前）」，
    读 changelog 的人根本分不清哪个才是当前版本。

用法：
    python3 patches/changelog_prepend.py .cl.md          # 前置
    python3 patches/changelog_prepend.py .cl.md --dry-run # 只看结果不写

输入文件要求：
    以 `## vX.Y.Z` 开头（可带或不带「（当前）」），后面是正文。
    脚本会把它的标题规范化成 `## vX.Y.Z（当前）`，并清掉所有旧小节的「（当前）」。

注意：**规范化是幂等的**（对已是正确状态的文件再跑一次不会改内容），
但「前置」本身不幂等 —— 拿同一个 .cl.md 跑两次会插入两份同样的小节。
"""
import re
import sys

PATH = "CHANGELOG.md"
HEADING = re.compile(r"^## (v[0-9]+(?:\.[0-9]+)*)(（当前）)?\s*$")


def normalize(text: str) -> str:
    """只保留第一个版本小节的「（当前）」，其余全部去掉。"""
    lines = text.split("\n")
    seen = False
    for i, line in enumerate(lines):
        m = HEADING.match(line)
        if not m:
            continue
        ver, cur = m.group(1), m.group(2)
        if not seen:
            seen = True
            lines[i] = f"## {ver}（当前）"
            continue
        if cur:
            lines[i] = f"## {ver}"
    return "\n".join(lines)


def main() -> int:
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    dry = "--dry-run" in sys.argv[1:]
    if len(args) != 1:
        print(__doc__)
        return 2

    new = open(args[0], encoding="utf-8").read().rstrip("\n")
    if not HEADING.match(new.split("\n", 1)[0]):
        print(f"❌ {args[0]} 的第一行必须是 `## vX.Y.Z`，实际是：{new.split(chr(10),1)[0]!r}")
        return 1

    old = open(PATH, encoding="utf-8").read()
    # 保留一级标题（# 变更记录），只把版本小节正文拼到它后面。
    # 注意：这里必须把 H1 原样留着 —— 早期版本把它剥掉却没写回，
    # 结果文件第一行直接变成 `## vX（当前）`，丢了大标题。
    title = "# 变更记录"
    body = old
    if body.startswith("# "):
        nl = body.find("\n")
        title = body[:nl].rstrip()
        body = body[nl + 1:].lstrip("\n")

    merged = title + "\n\n" + new + "\n\n" + body.rstrip("\n") + "\n"
    merged = normalize(merged)

    if dry:
        head = "\n".join(merged.split("\n")[:6])
        print(head)
        print(f"... 共 {len(merged.splitlines())} 行（dry-run，未写入）")
        return 0

    open(PATH, "w", encoding="utf-8").write(merged)
    cur = [l for l in merged.split("\n") if HEADING.match(l) and "（当前）" in l]
    print(f"✅ 已前置；当前版本小节：{cur[0] if cur else '(无)'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
