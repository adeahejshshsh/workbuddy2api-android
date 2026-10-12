# WorkBuddy2API · Android 自托管移植版

把 [workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（上游作者
[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，MIT）打包成**能直接装在
手机上常驻运行**的安卓 App：内置网关二进制 + 管理面板 + 看门狗 + 保活。

* 包名 `com.dsh.wb2api`，minSdk 24 / targetSdk 34，当前 **v1.22.1**（versionCode 11）
* 下载：见 [Releases](../../releases) —— 直接装 APK，覆盖安装即升级（账号与 api_key 保留）
* 一台手机 = 一个 OpenAI 兼容网关：`http://127.0.0.1:7863/v1`（局域网把 127.0.0.1 换成手机 IP）

## 装完怎么用

1. 装 APK → 打开 → 顶栏点 **「保活设置」**，按提示依次点完三项
   （省电白名单 → 显示在其他应用上层 → 闹钟与提醒）
2. 国产 ROM 再放行三处：**自启动 / 省电策略无限制 / 任务列表锁定后台**（各家叫法不同）
3. App 内就是上游那套管理面板：绑 CodeBuddy 账号 → 「全部签到 / 保活」等任务照原样用
4. 客户端接入：Base URL `http://<手机IP>:7863/v1`，API Key 用面板里的 key（点「复制密钥」）

## 目录结构

```
WorkBuddy2API.apk        现成安装包（也从 Releases 下）
apk-src/                 App 源码（无 Gradle：AndroidManifest + java + res + assets + test）
  java/com/dsh/wb2api/   MainActivity（面板壳）/ GatewayService（网关+看门狗）/ BootReceiver
  test/                  WatchdogSelfCheck（纯 JVM 自检：退避序列）
patches/
  build_apk.sh           一键重打包（aapt2 + javac + d8 + apksigner）
  make_icons.py          纯标准库缩图器 → 五档 mipmap 图标（本机没有 PIL/ImageMagick）
  panel-mobile.css       面板手机端补丁（≤760px 生效）
  apply_panel_mobile.sh  把上面的 CSS 幂等注入面板 index.html
workbuddy2api-panel/     上游服务端源码（Go，已内含手机端 CSS 补丁；未含 .git）
run.sh / rebuild.sh      命令行侧：起停服务 / 重编 Go 服务端
watchdog.sh              命令行侧看门狗（外部实例只提示、不抢端口）
PORTING.md               移植报告：撞过的墙、验证记录、踩过的坑（建议先看这个）
```

## 从源码构建

**App（APK）** —— 不需要 Android Studio / Gradle，但需要这些工具（Termux 里都能装/取到）：

```bash
# aapt2、javac 17、android.jar（API 35，来自 platform-35_r01.zip）、
# d8（在 r8.jar 里）、apksigner、zip、keytool
bash patches/build_apk.sh          # 产出 ./WorkBuddy2API.apk，约 8 秒
```

脚本做的事：把 Go 服务端编成 `lib/arm64-v8a/libwb2api.so`（**不打** rt0 补丁，见下）→
编译 java + 跑自检 → d8 出 dex → 组包 → 签名。签名需要自备 keystore（本仓库不含签名密钥）。

**服务端（Go）**：

```bash
bash rebuild.sh                    # 编 cmd/* 到 $PREFIX/lib/workbuddy2api/
bash patches/apply_panel_mobile.sh # 改了面板 CSS 之后要重跑（面板是 go:embed 进二进制的）
```

## 移植撞到的墙（详见 PORTING.md）

| 墙 | 绕法 |
|---|---|
| 安卓 10+ 禁止 app 从自己私有目录 execve（SELinux） | 二进制打进 `lib/arm64-v8a/`，交给系统解到 nativeLibraryDir；带 x 位直接 exec，否则 `/system/bin/linker64` 代跑 |
| `/storage/emulated/0` 是 noexec | 只放源码；可执行文件一律走 `$PREFIX` |
| Termux 里 linker 代跑时 argv[0] 被顶掉一个槽位 | 给 Go runtime 的 `asm_arm64.s` 打 rt0 源码补丁（`rebuild.sh` 侧用 patched、`build_apk.sh` 侧用 unpatched） |
| Go 纯 resolver 读不到安卓的 DNS 属性 | App 把系统 DNS 写进 `gateway/resolv.conf`，用 `WB2API_RESOLV_CONF` 指给 Go |

## App 里多出来的东西（相对上游）

* **看门狗五层**：进程级（10s tick，退避 2/4/8/16/32/60s）/ 健康级（`/healthz` 45s 无响应判僵死）/
  系统级（AlarmManager 兜底，能精确就精确）/ **JobScheduler 周期兜底**（系统托管、
  `setPersisted` 跨重启仍在）/ 保活级（**partial wakelock**，否则锁屏 CPU 一睡，
  线程和 Go 定时器全冻住 —— 这就是"只有前台才生效"的真因）
* **被杀诊断**：网关退出码翻成人话写进状态条第三行（`rc=137` = 被强杀 SIGKILL /
  内存不足或系统清后台），顶栏显示「系统重启 N」= App 进程被系统杀过几次 —— 再被杀不用猜
* **防冻结三件套**：常驻图层（1×1 全透明悬浮窗，让系统不把我判成缓存态 → 不进 app-freezer，
  治"过一会儿连不上、拉回前台就好"）+ Wi-Fi 高性能锁（局域网场景）+ 冻结自检（顶栏「冻过 N」）
* **`stopWithTask="false"`**：任务列表划掉不等于停服务（只有通知里的"停止"才真停）
* **任务中心只做"能做得了的"**：未证实可点亮的**尝试型**任务默认不排（可勾选打开），
  反复执行仍无进展的 (账号,任务码) **自动跳过**（7 天后自动放回，可一键清空）；
  跳过的项照样列出来 + 写明原因，不做静默过滤
* **任务中心不再"点了没效果"**：没待办时说明原因（拉取失败 / 每日锁定 / 已领 / 本版无自动化
  动作 / 已跳过），拉取失败在扫描结果里是**红行 + 上游报错**，而不是一句绿色的"没有待办任务"
* **日志落盘（无界面入口）**：网关 stdout/stderr → `files/gateway/data/server.log`、
  App 事件（被冻/被杀/被拒启动/兜底触发）→ `files/logs/app.log`；要看用 adb 取，不占界面
* **状态判定按事实**：后台线程探 `127.0.0.1:7863`，端口在服务就是「运行中」；没起来就在第三行
  直接写明原因（不再靠猜中文状态串）
* **前台服务用 `specialUse`**：`dataSync` 在 Android 15+ 有 6 小时/天上限，对 7×24 网关是致命的
* **面板手机端适配**：导航换行铺满、触控目标 ≥40px、可收缩网格（不再整页横滚）、弹层改底部抽屉
* **壳与网页同一套配色**：直接抄面板 `:root` 的暗色令牌

## 许可与致谢

* 上游服务端与面板：[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（MIT，见 `workbuddy2api-panel/LICENSE`）
  / 本移植基于 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)
* 本仓库的安卓移植部分（`apk-src/`、`patches/`、`run.sh` 等）同样以 MIT 发布，保留上游版权声明
* 本仓库**不含**任何账号凭据、签名密钥与本机运行数据（见 `.gitignore`）
