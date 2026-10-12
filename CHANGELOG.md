# 变更记录

## v1.18.0（当前）

* **图标换成 WorkBuddy 新图**（800×800 源图 → 五档 mipmap 48/72/96/144/192 全做齐）。
  本机没有 PIL/ImageMagick，所以自写了纯标准库缩图器 `patches/make_icons.py`
  （面积平均 + 预乘 alpha，半透明边缘不发黑）。
* **修一个差点白干半天的构建坑**：`build_apk.sh` 里 `aapt2 compile --dir res`（相对路径）
  编译的是 `$PREFIX/build/apk/res` —— 一份 15:52 的老副本。症状是"换了图标没变化、
  新增资源目录不生效"，而且**一声不响**（`set -e` 也救不了"成功编译了错误的目录"）。
  现已改成绝对路径 + 构建前清副本 + 存在性断言。
* **被杀防治再加一层 + 把"谁杀的"记下来**：
  * 第五层兜底 `ReviveJobService`（JobScheduler，15 分钟周期、`setPersisted`，
    系统托管、重启手机仍在）—— alarm 与 job 是两套独立调度机制，互为补位。
  * `stopWithTask="false"` 显式声明 + 补 `onTaskRemoved`：任务列表被划掉不再等于停服务，
    并会重挂 alarm/job；只有通知里的"停止"才真停。
  * alarm 能精确就精确（`SCHEDULE_EXACT_ALARM` + `canScheduleExactAlarms()`），
    Doze 里 5 分钟不再被拖成 ~9 分钟；未授权则自动回落，不会崩。
  * **诊断**：网关退出码翻成人话写进状态条第三行（`rc=137` = 被强杀 SIGKILL /
    内存不足或系统清后台，`rc=143` = SIGTERM 正常停止……），并跨 App 重启保留；
    顶栏新增「系统重启 N」= App 进程被系统杀过几次。**7863 再被杀，界面直接给出证据。**

## v1.17.0

* **状态判定改为按事实**（修一个我自己的设计错误）：上一版把状态词从中文状态串里猜
  （`st.contains("未启动") → 已停止`），于是运行态的 `"直接 exec pid=1234"` 猜不出来、
  服务没起来时又永远停在"未启动" → **界面不论如何都显示"已停止"**。现在服务导出机器可读的
  `statusOf()` / `reasonText()`，UI 用后台线程探 `127.0.0.1:7863` 判定，不在跑时**第三行直接写原因**。
* **前台服务类型 `dataSync` → `specialUse`**：`dataSync` 在 Android 15+ 有 6 小时/天上限，
  对 7×24 自托管网关是致命的。补 `onTimeout()` 兜底（被回收时立刻挂 5 分钟 alarm）。
* `startForegroundCompat()` 不再吞异常；`buildNotification()` 去掉主线程上的网络探针（ANR 隐患）。

## v1.16.0

* **面板手机端适配**：导航换行铺满（7 个 tab 全可见，之前 3 个 off-screen）、触控目标 ≥40px、
  固定列宽网格改可收缩（不再整页横滚）、弹层改底部抽屉、窄屏密度收紧。
  改动只在 `≤760px`，桌面端一行未动；补丁在 `patches/panel-mobile.css`（幂等注入）。
* **App 外壳重做**：配色抄面板 `:root` 暗色令牌、三行状态条、4 个胶囊 chip、2dp 加载进度线，
  WebView 关掉 `loadWithOverviewMode`（会把整页缩小，是"没适配"观感的来源）。

## v1.15.0

* **保活**：服务在跑就持 partial wakelock —— 不持它锁屏后 CPU 一进 suspend，
  看门狗线程和 Go 定时器全被冻住，症状正是"只有 App 在前台才生效"。
* 新增「省电白名单」入口（申请忽略电池优化，关掉 Doze 掐网络）。

## v1.14.0

* **看门狗**：进程级（10s tick，退避 2/4/8/16/32/60s）+ 健康级（`/healthz` 45s 无响应判僵死）
  + 系统级（AlarmManager 每 5 分钟兜底）+ 开机/升级自启（BOOT_COMPLETED / MY_PACKAGE_REPLACED）。
* 命令侧同名逻辑 `watchdog.sh`（遇外部实例只提示、绝不抢端口）。

## v1.13.0 及之前

* 安卓移植打通：Go 服务端编成 `lib/arm64-v8a/libwb2api.so` 由前台服务拉起；
  绕过 noexec / SELinux execve / Go resolver 读不到安卓 DNS / linker 代跑的 rt0 argv 偏移。
  详见 `PORTING.md`。
