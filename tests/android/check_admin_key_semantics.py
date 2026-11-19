#!/usr/bin/env python3
"""W6-H-03 / W6-M-01 / W6-M-11 的源码级回归断言（纯文本，不需要 JDK）。

三条互相独立的根因，共用一个脚本是因为它们都围着「凭据怎么显示、怎么自举」：
  1. 出厂 admin_api_key 为空 → 面板回退用客户端 api_key 鉴权（管理面等于对客户端开放）；
  2. 配置页把两把 key 明文摊在屏幕上；
  3. access token 输入框不掩码。

用法：  python3 tests/android/check_admin_key_semantics.py
退出码：0 全过；1 有失败。
"""
import json
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


admin = read("AdminKey.java")
main = read("MainActivity.java")
boot = read("BootReceiver.java")
console = read("Console.java")
cfg_activity = read("ConfigActivity.java")
accounts = read("ScreenAccounts.java")

print("== W6-H-03(a) 首启自动生成 admin_api_key ==")

check("admin_api_key" in admin, "AdminKey 必须处理 admin_api_key")
check("SecureRandom" in admin and "new byte[16]" in admin,
      "admin_api_key 必须是 16 字节随机数的 32 hex（不是 UUID/时间戳）")
check("AtomicFile" in admin, "写配置必须原子（半截 JSON 会让网关起不来）")
check("ADMIN_KEY" in admin and "replaceFirst" in admin,
      "键已存在但为空/占位时必须原地替换值（其余配置一个字都不动）")
check("lastIndexOf('}')" in admin,
      "缺键时按「最后一个 } 前插入」处理（和 ensureConfig 的 bug #4 同一套逻辑）")
check("已生成" in admin or "已补写" in admin,
      "生成后必须落日志（用户在 logcat/面板里能看到发生了自举）")
check("局域网" in admin and "唯一防线" in admin,
      "启动日志必须说明「面板已暴露到局域网，admin key 是唯一防线」")

# 首次安装的顺序问题：配置还不存在时，必须由我们在 start 之前落一份带 admin key 的
# 模板；否则网关会先落一份 admin 为空的模板并立刻用它起来 —— 那个窗口就是 H-03 的洞。
check(re.search(r"return\s+bootstrapFromAssets\(c, cfg\)", admin) is not None,
      "首次安装必须由 AdminKey 先落模板（不能等 ensureConfig 写完再由补偿任务补）")
check('getAssets().open("config.json")' in admin,
      "落模板必须读 assets/config.json（不要自己手写一份 schema 会漂移的配置）")
check("needsRestart" in admin and "needsRestart" in main,
      "「刚生成」与「需要重启才生效」必须分开：首次安装落盘后不用重启")

# 启动顺序：ensure 必须在 GatewayService.start 之前（Go 只在启动时读配置）。
oncreate = body(main, "protected void onCreate(Bundle")
i_ensure = oncreate.find("AdminKey.ensure")
i_start = oncreate.find("GatewayService.start(this)")
check(i_ensure >= 0, "MainActivity.onCreate 必须调 AdminKey.ensure")
check(i_ensure >= 0 and i_start >= 0 and i_ensure < i_start,
      "AdminKey.ensure 必须在 GatewayService.start 之前（否则网关读到的是空 key）")
check("InstanceId.ensure" in oncreate, "MainActivity.onCreate 必须调 InstanceId.ensure")
check("scheduleAdminKeyFixup" in oncreate,
      "首启模板刚落盘时 ensure 还读不到配置，必须有一次性补偿重试")

boot_body = body(boot, "public void onReceive(Context c, Intent i)")
check("AdminKey.ensure" in boot_body, "BootReceiver 自启路径也必须先补 admin_api_key")
check("InstanceId.ensure" in boot_body, "BootReceiver 自启路径也必须先补 instance_id")
check("AdminKey.ensure" in console, "Console 的「启动网关」按钮也要补（幂等）")

print()
print("== W6-H-03(c) 复制密钥区分两把 key ==")
copy_body = body(main, "void copyApi()")
check("Admin Key" in copy_body, "copyApi 必须单独列出 Admin Key")
check("API Key（给下游客户端）" in copy_body or "API Key（给下游" in copy_body,
      "copyApi 必须标明 API Key 是给下游客户端的")
check("别外发" in copy_body or "本机面板" in copy_body,
      "copyApi 必须提示 admin key 只在本机面板用")

print()
print("== W6-H-03(d) 出厂模板与 listen 不被打断 ==")
cfg = json.loads((ROOT / "apk-src/assets/config.json").read_text(encoding="utf-8"))
check("admin_api_key" in cfg, "assets/config.json 必须带 admin_api_key 字段（空值待自举）")
check(cfg.get("listen") == ":7863",
      "listen 必须保持 \":7863\"（局域网是用户明确要的用法，不能改成 127.0.0.1）")
check("networkSecurityConfig" in (ROOT / "apk-src/AndroidManifest.xml").read_text(encoding="utf-8"),
      "清单必须挂 networkSecurityConfig（W6-M-14 的 LAN 说明成立的前提）")

print()
print("== W6-M-01 配置页密钥掩码 ==")
check("isSecretKey" in cfg_activity, "ConfigActivity 必须识别密钥类键")
check("TYPE_TEXT_VARIATION_PASSWORD" in cfg_activity, "密钥输入框必须用密码变体（掩码）")
check("revealToggle" in cfg_activity and "显示" in cfg_activity,
      "必须有「显示 / 隐藏」切换（否则用户没法核对密钥）")
row_body = body(cfg_activity, "private View row(final JSONObject owner, final String key, Object value)")
check("isSecretKey(key)" in row_body, "row() 里必须按 isSecretKey 分派")
check("secretInput(" in row_body, "密钥键必须渲染成 secretInput（掩码），不能是 textInput")

print()
print("== W6-M-11 access token 输入框掩码 ==")
token_line = [l for l in accounts.splitlines() if "access token" in l]
check(bool(token_line), "ScreenAccounts 里能找到 access token 输入框")
check("secretField" in accounts, "access token 必须用 secretField（掩码）")
dialog = body(accounts, "private void apiKeyDialog()")
check("secretField" in dialog and "field(\"access token" not in dialog,
      "apiKeyDialog 的 token 输入框必须走 secretField，不能是明文 field")

print()
if fails:
    print("失败 %d 项" % len(fails))
    for f in fails:
        print("  - %s" % f)
    sys.exit(1)
print("全部通过")
