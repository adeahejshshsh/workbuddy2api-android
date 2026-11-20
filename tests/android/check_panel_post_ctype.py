#!/usr/bin/env python3
"""「无 body 的 POST 被平台写成表单编码」的源码级回归断言（纯文本，不需要 JDK）。

线上症状：原生控制台点「签到」直接弹
    签到失败：HTTP 415：unsupported_media_type：面板写接口…
根因不在按钮，在传输层：Android 的 HttpURLConnection 给 POST 恒挂一个
EMPTY_REQUEST_BODY（OkHttp HttpEngine 的 hasRequestBody() 为真就补默认类型），
于是 body 为 null 的请求到了服务端，Content-Type 是
`application/x-www-form-urlencoded` —— 被 W3-03 的跨站写闸门按「CORS 简单请求」
判 415。修法是：写请求一律自己声明 application/json，空参数也发 `{}`。

断言盯 req() 的函数体，所以「又改回 if (body != null)」会立刻红：
  1) 写分支必须按方法判定（isReadMethod），不是按 body 是否为 null；
  2) 空参数必须补 {}；
  3) 写分支必须显式设 application/json；
  4) 不允许再出现 `if (body != null) {` 这种「没参数就不发 body」的形状。

用法：  python3 tests/android/check_panel_post_ctype.py
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
    """剥掉 // 行注释：修复说明里会写到旧形状的字面量，注释不能骗过断言。"""
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


api = read("PanelApi.java")
code = strip_comments(api)

req = body(code, "private static void req(")
if not req:
    fails.append("PanelApi.req() 找不到（断言无从下手）")

print("PanelApi.req：写请求一律自己声明 Content-Type")
check("isReadMethod(method)" in req,
      "写分支按方法判定（isReadMethod），不按 body 是否为 null")
check("body == null ? new JSONObject() : body" in req,
      "空参数补 {}：没有参数不等于没有 body")
check('conn.setRequestProperty("Content-Type", "application/json")' in req,
      "写分支显式声明 application/json")
check("conn.setDoOutput(true)" in req, "写分支真的会写 body")
check("if (body != null) {" not in req,
      "不允许再出现 if (body != null) —— 那正是 415 的形状")

print("PanelApi 其它入口：")
check('"POST", path, body' in body(code, "static void post(") or
      '"POST", path, body' in body(code, "static void postSlow("),
      "post / postSlow 走同一个 req()（单点修复，新入口自动覆盖）")
check("body == null ? new JSONObject() : body" in body(code, "private static void req("),
      "postSlow 传 null 时同样被 req() 兜住")
upload = body(code, "static void upload(")
check('"Content-Type", "multipart/form-data; boundary="' in upload,
      "上传入口自己声明 multipart/form-data（不受本次改动影响）")

print("isReadMethod 覆盖范围：")
irm = body(code, "private static boolean isReadMethod(")
check('"GET".equals(method)' in irm and '"HEAD".equals(method)' in irm,
      "GET / HEAD 不带 body")
check('"POST"' not in irm, "POST 必须被当成写请求（会写 body）")

if fails:
    print("\n失败 %d 项：" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("\n全部通过")
