#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# 把 panel-mobile.css 注入上游面板 index.html（幂等）
#   bash patches/apply_panel_mobile.sh [面板仓库路径]
# 有标记 → 原地替换（便于改完 CSS 再跑一次）；无标记 → 插到 </style> 之前。
# 想还原成上游原样：git -C <仓库> checkout internal/panel/index.html
set -eu

REPO="${1:-/storage/emulated/0/新建文件夹/workbuddy2api/workbuddy2api-panel}"
CSS="$(cd "$(dirname "$0")" && pwd)/panel-mobile.css"

REPO="$REPO" CSS="$CSS" python3 - <<'PY'
import os, pathlib, re, sys

repo = pathlib.Path(os.environ["REPO"])
css = pathlib.Path(os.environ["CSS"]).read_text(encoding="utf-8")
html_p = repo / "internal/panel/index.html"
html = html_p.read_text(encoding="utf-8")

BEG = "/* ══ DSH-MOBILE-PATCH-BEGIN（勿手改此块，改 patches/panel-mobile.css 后重跑脚本）══ */"
END = "/* ══ DSH-MOBILE-PATCH-END ══ */"
block = f"{BEG}\n{css}{END}\n"

if BEG in html:
    # 替换模板里的反斜杠会被 re 当转义解释（CSS 里的 content: "\201C" 会直接
    # 抛 bad escape，或内容被悄悄吃掉）。用 lambda 让 block 原样插入。
    html2 = re.sub(re.escape(BEG) + r".*?" + re.escape(END) + r"\n?", lambda m: block, html, flags=re.S)
    action = "已更新（替换旧块）"
elif "</style>" in html:
    html2 = html.replace("</style>", block + "</style>", 1)
    action = "已注入（插在 </style> 之前）"
else:
    sys.exit("找不到 </style>，面板结构变了？")

if html2 == html:
    print("内容无变化（已是该版本）")
else:
    html_p.write_text(html2, encoding="utf-8")
    print(f"{action}：{html_p}  {len(html)} → {len(html2)} 字节")

# 自检：标记在不在、有没有把 </style> 顶掉
t = html_p.read_text(encoding="utf-8")
ok = t.count(BEG) == 1 and t.count(END) == 1 and t.count("</style>") >= 1 and t.count("<style>") >= 1
print("自检：", "OK 标记唯一且 style 闭合" if ok else "❌ 结构异常")
sys.exit(0 if ok else 1)
PY
