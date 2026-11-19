#!/usr/bin/env python3
"""W6-M-08 / W6-M-09 / W6-M-14 的源码级回归断言（纯文本 + 标准库 XML，不需要 JDK）。

三条都是「构建/安装面」的配置项，回归的表现是「代码没变、暴露面又回去了」：
  · CI 没写 permissions → GITHUB_TOKEN 拿到仓库默认权限；
  · 基础镜像用浮动 tag → 什么都没改却构建行为变了；
  · 明文开关全局放开 → 任意 URL 都能走 http。

用法：  python3 tests/android/check_build_hygiene.py
退出码：0 全过；1 有失败。
"""
import re
import sys
import xml.etree.ElementTree as ET
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

fails = []


def check(cond, msg):
    if cond:
        print("  OK   %s" % msg)
    else:
        fails.append(msg)
        print("  FAIL %s" % msg)


print("== W6-M-08 CI 最小权限 + action 版本 ==")
ci_path = ROOT / ".github/workflows/ci.yml"
ci = ci_path.read_text(encoding="utf-8") if ci_path.exists() else ""
check(bool(ci), "ci.yml 存在")
check(re.search(r"(?m)^permissions:\s*$", ci) is not None, "必须有顶层 permissions 块")
check(re.search(r"(?m)^\s+contents:\s*read\s*$", ci) is not None,
      "permissions 必须是 contents: read（最小权限）")
check(re.search(r"permissions:[\s\S]{0,80}?write", ci) is None,
      "permissions 里不许出现 write")
check("actions/checkout@v4" in ci, "checkout 必须显式写 major tag（@v4）")
check("actions/setup-go@v5" in ci, "setup-go 必须显式写 major tag（@v5）")
check(re.search(r"uses:\s*\S+@(main|master)\b", ci) is None,
      "不许用 @main / @master 这类浮动引用")
check(re.search(r"uses:\s*\S+@v\d", ci) is not None,
      "每个 action 都必须钉到 major tag（@vN）")

print()
print("== W6-M-09 基础镜像固定 + 构建上下文收窄 ==")
df = (ROOT / "workbuddy2api-panel/Dockerfile").read_text(encoding="utf-8")
froms = [l.strip() for l in df.splitlines() if l.strip().startswith("FROM ")]
check(len(froms) >= 2, "Dockerfile 至少有两个 FROM 阶段")
check(bool(froms) and froms[0].startswith("FROM golang:1.24-alpine3.20"),
      "构建镜像必须固定到 alpine 小版本（golang:1.24-alpine3.20）")
check(len(froms) > 1 and re.match(r"FROM alpine:3\.20\.\d+(@sha256:[0-9a-f]{64})?(\s|$)", froms[1]) is not None,
      "运行镜像必须固定到 patch 版本（alpine:3.20.N）")
check(all("@sha256:" in f for f in froms),
      "两个基础镜像都必须钉 digest（tag 可以被上游重新推送）")
check(not any(re.match(r"FROM golang:1\.24-alpine(\s|$)", f) for f in froms),
      "不许再用会随 alpine 漂移的 golang:1.24-alpine")
di = (ROOT / "workbuddy2api-panel/.dockerignore").read_text(encoding="utf-8")
check("/trial" in di, ".dockerignore 必须排除 12.5MB 的 trial 二进制")
check("dist/" in di, ".dockerignore 必须排除 dist/ 构建产物")

print()
print("== W6-M-14 networkSecurityConfig ==")
mf = (ROOT / "apk-src/AndroidManifest.xml").read_text(encoding="utf-8")
check('android:networkSecurityConfig="@xml/network_security_config"' in mf,
      "清单必须引用 @xml/network_security_config")
check('android:usesCleartextTraffic="true"' not in mf,
      "清单不许再全局放开 usesCleartextTraffic")

nsc_path = ROOT / "apk-src/res/xml/network_security_config.xml"
check(nsc_path.exists(), "res/xml/network_security_config.xml 必须存在")
if nsc_path.exists():
    tree = ET.parse(str(nsc_path))
    root = tree.getroot()
    base = root.find("base-config")
    check(base is not None and base.get("cleartextTrafficPermitted") == "false",
          "base-config 必须默认禁止明文")
    domains = []
    for dc in root.findall("domain-config"):
        allowed = dc.get("cleartextTrafficPermitted") == "true"
        for d in dc.findall("domain"):
            domains.append((d.text, allowed))
    mapping = dict(domains)
    check(mapping.get("127.0.0.1") is True, "127.0.0.1 必须允许明文（面板/探针都用它）")
    check(mapping.get("localhost") is True, "localhost 必须允许明文（WebView 可能规范化成它）")
    check(all(v is False for n, v in mapping.items() if n not in ("127.0.0.1", "localhost")),
          "除回环外的域名不许放开明文")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
