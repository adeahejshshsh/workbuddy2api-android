#!/usr/bin/env python3
"""W6-H-02 / W6-L-11 的源码级回归断言（纯文本检查，不需要 JDK / android.jar）。

为什么不做成单测：判定链全在 Android 框架里（WebView 回调、HttpURLConnection、
应用私有目录），宿主 JVM 跑不起来。能防的回归是「又变成无条件注入」——
所以断言直接盯 onPageFinished 的函数体：注入动作必须留在 verifyThenInject 之后，
且失败分支必须 return（不注入）。

用法：  python3 tests/android/check_webpanel_key_semantics.py
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
    """取 signature 所在函数的函数体（从签名处按花括号配平）。找不到返回 ""。"""
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


panel = read("WebPanelActivity.java")
trust = read("PanelTrust.java")
iid = read("InstanceId.java")
main = read("MainActivity.java")

print("== W6-H-02 注入前实例认证 ==")

# ① onPageFinished 里不许再出现注入动作：它只能"先校验"。
finished = body(panel, "public void onPageFinished(WebView v, String url)")
check(bool(finished), "WebPanelActivity.onPageFinished 存在")
check("localStorage.setItem" not in finished,
      "onPageFinished 不许直接写 localStorage（必须经过实例校验）")
check("verifyThenInject" in finished,
      "onPageFinished 必须调 verifyThenInject，而不是直接 injectKey")

# ①-b 面板侧契约（W3-04）：密钥不再进 localStorage。
#     app.js 的 authKey() 优先级是 window.__WB2A_ADMIN_KEY > sessionStorage > 旧值迁移，
#     所以注入必须走 window 变量；而 reload 会丢 window 变量，所以还要写 sessionStorage
#     （仅内存、单标签页，不落盘）。两处注入（WebPanelActivity 与 MainActivity 的
#     fallback WebView）必须同口径，否则修一处漏一处。
for name, src in (("WebPanelActivity", panel), ("MainActivity", main)):
    check("__WB2A_ADMIN_KEY" in src, "%s 必须注入 window.__WB2A_ADMIN_KEY" % name)
    check("sessionStorage.setItem" in src, "%s 必须写 sessionStorage（扛 reload）" % name)
    check("localStorage.setItem" not in src,
          "%s 不许再写 localStorage（明文落盘、永不过期）" % name)

# ①-c W3-02：自举令牌也要注入，否则「设置管理员密钥」弹窗拿不到令牌。
check("__WB2A_BOOTSTRAP_TOKEN" in panel,
      "WebPanelActivity 必须注入 window.__WB2A_BOOTSTRAP_TOKEN（W3-02 一次性令牌）")
check("bootstrapToken" in panel,
      "令牌必须从 AdminKey.bootstrapToken() 读（0600 文件，用后即删）")

# ② 注入只能由 verifyThenInject 触发，且失败分支必须不注入。
verify = body(panel, "private void verifyThenInject(final WebView v)")
check("PanelTrust.verify" in verify,
      "verifyThenInject 必须走 PanelTrust.verify（唯一判定入口）")
check("injectKey(" in verify, "verifyThenInject 在校验通过后调 injectKey")
check(re.search(r"if\s*\(!ok\)", verify) is not None,
      "verifyThenInject 必须有 if (!ok) 分支")
check(re.search(r"if\s*\(!ok\)[\s\S]{0,500}?return;", verify) is not None,
      "校验失败分支必须在注入前 return（失败关闭）")
check("PanelTrust.message" in verify, "失败时要按原因提示用户（不能静默）")
# 注入入口点必须是**受控的**：校验通过分支、用户显式确认分支（含它派生出的
# 「填该实例密钥」子流程）、以及 injectKey 内部的重试自调用。把这些函数体剥掉后
# 若还有 injectKey( 调用，就是绕过校验的后门。
stripped = panel
for sig in ("private void verifyThenInject(final WebView v)",
            "private void confirmTrustAndInject(final String actualId)",
            "private void askKeyOverride(final String actualId)",
            "private void injectKey(WebView v)",
            "private void injectKey(final WebView v, final int attempt)"):
    b = body(stripped, sig)
    if b:
        stripped = stripped.replace(sig + " " + b, "void removed_" + str(len(sig)) + "() {}", 1)
check("injectKey(" not in stripped,
      "injectKey 只允许在 verifyThenInject / confirmTrustAndInject / askKeyOverride / 自身重试里被调用")
trust_dialog = body(panel, "private void confirmTrustAndInject(final String actualId)")
check("injectKey(" in trust_dialog,
      "手动放行必须走 confirmTrustAndInject（带确认对话框），而不是直接注入")
check("AlertDialog.Builder" in trust_dialog,
      "手动放行前必须弹确认框（把风险讲清楚，用户自己决定）")

# 「每次都弹、只对本次会话有效」是本轮要修的用户可见问题：确认结果必须按
# (端口, 实例标识) 持久化，且 id 一变自动失效（端口可能换主，id 换不了）。
check("PanelTrust.rememberTrust" in trust_dialog,
      "用户确认「信任这个实例」后必须持久化 (port, instance_id)，否则下次打开又弹一遍")
check("PanelTrust.forgetTrust" in trust_dialog,
      "选「取消」时必须清掉旧的信任标记（否则会一直沿用上一次的错误放行）")
verify_src = body(panel, "private void verifyThenInject(final WebView v)")
check("actualId" in verify_src,
      "校验失败时要拿到对面报出的实例标识（区分「第三方占端口」与「另一个同类实例」）")

# 两个实例的 admin_api_key 通常不同：注入了本应用那份只会 401。必须有覆盖入口，
# 且覆盖值优先于 config.json。
inj_src = body(panel, "private void injectKey(final WebView v, final int attempt)")
check(inj_src.index("PanelTrust.keyOverride") < inj_src.index("PanelApi.key"),
      "注入用的密钥必须先取 keyOverride（用户为该实例手输的那份），再回落 config.json")
check("PanelTrust.rememberKey" in body(panel, "private void askKeyOverride(final String actualId)"),
      "手输的密钥必须落盘记住（否则「填了也只对本次会话有效」）")

# 写入必须**回读确认**：旧实现在写入前就置 injected=true，写失败与成功长得一样，
# 表现就是「每次打开都要手输密钥」。
inj = body(panel, "private void injectKey(final WebView v, final int attempt)")
check("onReceiveValue" in inj and "got.contains(key)" in inj,
      "注入后必须回读 sessionStorage 确认写入成功（bug-04 同口径）")
# reload 现在发生在两阶段校验之后：JS 写入回读确认 → 服务端确认这把 key 被接受。
# 关键不变量是「写入确认成功之前不许 reload」；reload 本身挪进了 confirmKeyAccepted。
check("confirmKeyAccepted" in inj,
      "写入确认成功后必须再走服务端 key 校验（不能只凭 JS 写入就当登录成功）")
confirm_src = body(panel, "private void confirmKeyAccepted(final WebView v, final String key)")
check("keyAccepted" in confirm_src and "v.reload()" in confirm_src,
      "只有服务端确认接受 key 之后才 reload（否则端口上是另一个实例时照样弹密钥门）")
check("keyRejected" in confirm_src,
      "服务端明确拒绝 key 时不能 reload，要引导用户填该实例自己的密钥")
check("PanelTrust.keyOverride" in body(panel, "private void injectKey(final WebView v, final int attempt)"),
      "注入密钥必须用 keyOverride（用户为该实例手输的那份），不能只用 config.json")
check(not re.search(r"injected\s*=\s*true;[\s\S]{0,200}?evaluateJavascript", inj),
      "不许在 evaluateJavascript 之前就置 injected=true")
check("attempt < 2" in inj, "写入未确认时要重试（不是弹一次 toast 就放弃）")

# 状态 chip：把「有没有自动登录」写在界面上，而不是只弹一次 toast。
check("statusChip" in panel and "已自动登录" in panel,
      "顶栏必须常驻显示「已自动登录 / 未自动登录」状态")
check("未自动登录" in panel, "失败态要给出原因并指引手动放行")

# ③ 判定本体：必须比对 instance_id，且不跟随重定向、不把自己的 id 发出去。
check("InstanceId.matches(expect" in trust,
      "PanelTrust 必须用 InstanceId.matches 比对对方报出的 id")
check("setInstanceFollowRedirects(false)" in trust,
      "探测不许跟随重定向（30x 可能是占位进程在导流）")
check("instance_id" in iid and "instance_id" in trust,
      "契约字段名必须是 instance_id（Go 侧 /panel/api/instance 同名字段）")
check("X-WB2API-Instance" not in trust and "?id=" not in trust,
      "不许把自己的 instance_id 放进请求再让对方回显（回显可伪造）")
check("[0-9a-fA-F]{32}" in iid and "parseId" in iid,
      "响应解析必须只接受 32 hex（HTML 错误页/空值一律当失败）")

# ④ instance_id 的生成与存放：32 hex、应用私有目录、原子写。
check('"instance_id"' in iid, "InstanceId.FILE_NAME 必须是 instance_id")
check('"gateway/data"' in iid or "gateway/data" in iid,
      "instance_id 必须落在 files/gateway/data（与网关工作目录一致）")
check("new byte[16]" in iid and "Character.forDigit" in iid,
      "instance_id 必须是 16 字节随机数的 32 位 hex")
check("AtomicFile" in iid, "instance_id 写入必须原子（半截串会让校验永远失败）")

# ⑤ MainActivity 的 fallback 注入路径也要门禁（nativeMode=false 时才走）。
minject = body(main, "void injectKey()")
check("PanelTrust.verify" in minject, "MainActivity.injectKey 也必须先过 PanelTrust.verify")
check("panelTrusted" in main, "MainActivity 必须记录「已通过校验」状态")

print()
print("== W6-L-11 外链交系统浏览器 ==")
check("shouldOverrideUrlLoading" in panel, "WebPanelActivity 必须实现 shouldOverrideUrlLoading")
route = body(panel, "private boolean routeExternally(String url)")
check("ACTION_VIEW" in route, "非面板 URL 必须用 ACTION_VIEW 交给系统浏览器")
check("isPanelUrl(url)" in route, "面板内部 URL 必须放行（否则网页面板自己点不动）")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
