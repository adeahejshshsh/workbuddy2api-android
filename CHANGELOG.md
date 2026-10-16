# 变更记录

## v1.24.6（当前）

* **让「503 no_healthy_account」自己讲清原因**（用户实测：反复 503，而 hint 只有一句
  "check /status"，等于没说）。现在这句 hint 会带上**池子实况 + 每个号的状态与剩余时间**：

  ```
  ... no healthy account available in pool; check /status or retry later
      [pool: total=3 healthy=1 cooling=1 degraded=1 paused=0 disabled=0 in_flight_full=0
       model_cooled(gpt-6-astra)=1 realm=cn | u1a2b3c4:cooling 2m30s, e5f6a7b8:degraded 8m, c9d0e1f2:ok]
  ```

  域不可服务时照旧前置 `realm cn has no servable account (cn=..., global=...)`。
  * 顺带核实「在途名额泄漏」的怀疑：**没有泄漏** —— 轮转循环外有 `defer` 统一 `Release`、
    循环内提前释放走 `releaseHeld()`（handler.go:593-605 / 883），所以这类 503 属真实瞬态
    （号在冷却 / 连败降权 / 满载），会自愈；现在能一眼看出是哪个维度、还要等多久。
  * 验证：起临时实例（7999，独立 state）实打一次 chat，返回体的 `gateway_hint` 里确实出现
    `[pool: total=0 healthy=0 cooling=0 degraded=0 paused=0 disabled=0 in_flight_full=0
    model_cooled(gpt-6-astra)=0 realm=cn | ]` ✓（用户 7863 全程未碰）。

## v1.24.5

* **修「模型等级不支持，却被报成 503」**（用户实测："你改成 503 了"）：
  v1.24.2 把 11158 归类成"模型级避让"后，**第二次请求就不会再打到上游了** —— 该模型已被
  (账号,模型) 负缓存避开，末端 `lastErr` 为空、拿不到任何上游原文，于是退回默认的
  `503 no_healthy_account`，把"你的等级不够"读成了"服务端过载"。
  * `BlockModelBackoff` 的 reason 分开记：11102「模型不存在」 vs 新增
    `upstream.ModelNotAllowedReason = "11158 model not allowed for account type"`；
    `BlockModelClear` 两种前缀都清（6004 有自己的上游重置墙钟，仍不动）。
  * 末端两处收紧：① `modelBlock.Reason` 以 `11158` 开头 → `code` 换成精确的
    **`model_not_allowed_for_account_type`**（仍 400、明确不可重试）；② 全池被等级闸门
    判定"用不了该模型"但本轮一个上游请求都没打 → 直接给精确 code + 400，不再退回笼统 503。
  * 测试：`internal/pool/modelblock_reason_test.go`（11158 reason 保留 / 两个号都挡才报模型级
    阻塞 / 清 11102+11158 两种前缀 / 6004 不被误清 / 负缓存与等级事实组合判定）。
    全绿：pool **183 PASS**、upstream 198、server 87、panel 24。

## v1.24.4

* **修「账号用不了的模型照样显示在模型列表里」**：上游模型目录（CN `/console` 与 global `/v2`
  同构）**不提供任何权限/等级字段**（`upstream.ModelInfo` 全字段核对过；账号 profile 只有
  uid/nickname），此前网关把平台全量目录直接透出 → 免费档账号照样看到 `gpt-6-astra` 这类
  只对高等级开放的模型，挑中必失败（还要吃一条 503）。
  * **做法（从报文里学事实）**：11158 报文里明确写着
    `model [gpt-6-astra] is not allowed for user type [personal_free_user]` →
    据此记下 (账号→等级) 与 (等级→该模型不可用)，落盘到与 `state.json` 同目录的
    `model_tier.json`（独立文件，不动 state schema）。
  * **只在事实确凿时隐藏**：当且仅当**池内每个账号**都能被判定"用不了该模型"，
    才从 `/v1/models` 与面板模型页摘掉它；任一账号判不出来（等级未知且无该号自己的记录）
    → 保持可见。宁可多显示一个，也不把一个其实能用的模型藏起来。
  * **面板**：模型页新增开关「显示等级不支持的模型」（`GET /panel/api/models?all=1`
    保留并打 `tier_denied` 标记），提示栏显示「已按账号等级隐藏 N 个」。
  * 测试：`internal/pool/modeltier_test.go` —— 无事实不隐藏 / 只有部分账号等级未知不隐藏 /
    池内全部判得出来才隐藏 / 别的等级的事实不影响本池 / 落盘与重启恢复。
    全绿：pool **181 PASS**、upstream 198、server 87、panel 24。

## v1.24.3

* **修「点了解冻没效果」**（用户实测）：`Pool.Revive`（面板「解冻」按钮，`POST
  /panel/api/accounts/{uid}/revive`）清了 disabled / paused / 账号冷却 / 软限流退避 /
  模型级冷却 / sessionDeadFails / fails / retryCount / 熔断 —— **偏偏漏了连败降权的两个字段**
  `consecutiveFails` 与 `degradeUntil`（`internal/pool/entry.go` 第 245/249 行）。
  而 `entry.healthy()` 在 `degradeUntil` 未到期时恒为 false → 面板提示「已解冻」、号却依然
  不可选（healthy 计数仍是 0，账号行仍挂「连败降权 · 剩余 xx」）。这两个字段还会落盘
  （`persist.go`），**重启网关也甩不掉**，只能等 10 分钟 ~ 2 小时自然过期。
  * 修复：Revive 与 NoteSuccess 口径对齐，一并清 `consecutiveFails` / `degradeUntil`
    （Revive 的语义本就是「运维无条件恢复」）。
  * 回归测试 `internal/pool/revive_degrade_test.go`：连败到阈值 → 断言 healthy=0 →
    Revive → 断言字段清零且 healthy=1。**撤掉修复该测试即 FAIL**（已验证），pool 包 179 PASS / 0 FAIL。

## v1.24.2

* **修「API 返回给 agent 的 JSON 里满屏转义符」**：上游错误体本身就是一份 JSON 文档，
  此前按「原文透传」整段塞进 `error.message` → 客户端看到的是
  `"message":"{\"code\":11158,\"msg\":\"...\"}"` 这种一坨反斜杠。现在把 body 压成
  **一句话**（新增 `internal/upstream/errbrief.go`：优先中文 `displayMsg`，非 JSON 体
  折叠空白 + 按 rune 截断），并把 `code` / `requestId` / 本地化文案改成**并列结构化字段**：
  ```json
  {"error":{
    "message": "当前账号类型暂不支持该模型。 ｜ model [gpt-6-astra] is not allowed for user type [personal_free_user]（上游 code=11158）",
    "type": "api_error", "code": "model_not_allowed_for_account_type",
    "upstream_code": "11158", "upstream_request_id": "3c1e...",
    "upstream_display": "当前账号类型暂不支持该模型。", "gateway_hint": "..."}}
  ```
  原文信息一个不丢，转义符一个不留；`content_blocked` / `image_invalid` / `bad_params`
  三条早退路径同样改用摘要。
* **11158「该模型不允许当前账号类型」单列**：以前落进通用错误 → 客户端按 503 无限重试。
  现在：该号对该模型走 (账号,模型) 负缓存避让、继续轮转别的号；全池都不行时返回
  **400 + `code=model_not_allowed_for_account_type`**（不可重试语义），文案说清"账号等级不支持"。
* **选号失败时 hint 带域信息**：请求的 realm 无可用账号时附加
  `realm global has no servable account (cn=true, global=false)` —— 单域部署里
  「池子没号」与「你要的这个域没号」一眼可分，不再都报一句笼统的 no_healthy_account。
* 测试：新增 `internal/upstream/errbrief_test.go`（真实 11158 体 / 嵌套形态 / HTML 体 /
  截断 / 误判防护）。全绿：上游包 **319 PASS**、server **102 PASS**、面板 **37 PASS**。

## v1.24.1

* **修「装完新版后调用任何模型都 503，跟着连败降权」** —— v1.24.0 的回归，真凶定位到字节级：
  APK 里的 Go 进程跑在 `com.dsh.wb2api` 下，**读不到 DSH 的 `$PREFIX`**；Java 侧
  （`GatewayService`）是靠环境变量 **`WB2API_RESOLV_CONF`** 把它自己写的
  `files/gateway/resolv.conf` 传进去的。本机 Go 工具链被重装后，源码里「读该环境变量」的
  定制补丁丢了，而 `patch_source.sh` 只补回了「硬编码路径替换」→ v1.24.0 的 `.so` 固定去读
  DSH 私有路径 → **App 内 DNS 全挂 → 上游全失败 → 全 503**，随后三个号被连败降权
  （阈值 3 次；降权时长 10 分钟 ~ 2 小时，见 `defaultDegradeCooldown`）。
  * 证据：两版 `.so` 对照 —— v1.23.0（好用）含 `WB2API_RESOLV_CONF` **1 处**，
    v1.24.0（坏）**0 处**；两版其余 DNS 字符串一致。
  * 修复：`dnsclient_{android,unix}.go` 的 resolv.conf 路径改为
    **环境变量优先 → APK 固定私有路径 → DSH `$PREFIX` → `/etc/resolv.conf`**。
  * 该补丁已**写进 `patches/patch_source.sh`**（幂等），重装工具链后再跑一次即可复原，不会重演。
  * 实验验证：`WB2API_RESOLV_CONF` 指向坏 DNS 文件 → 解析按预期失败；指向好文件 → 立刻成功。

## v1.24.0

* **合并上游 main `6b31db6`**（本地此前停在 `8461419`，净变化 6 文件 +295/−4）：
  * **修线上 11133**：出站请求体里 `function.name` 为空（含纯空白）的 `tool_call` 一律剔除
    —— 上游对这类条目直接判 `11133 model_param_invalid`，带不带配对结果都会被拒。
    新增 `internal/upstream/tool_pairing.go`(+30) 与配套测试(+91)
  * **`prompt_cache_key` 回退**：没有 `conversation_id` 的客户端（Anthropic / Claude Code 这类）
    此前会退化成「每账号一个常量 key」，同账号所有客户端抢同一个前缀缓存槽；现在回退到会话 key
  * **积分到期提醒卡片默认可见**：修 issue #137/#145 的「卡片 hidden → 检查按钮也看不见 → 死锁」，
    进页面即用 `/overview` 快照渲染（零上游请求），「检查」降级为逐包明细的增强入口
  * 新增 `internal/panel/frontend_test.go`(+103)
* **配置表单 ⇄ CFG_MAP 一致性**：v1.23.0 删掉配置页「成长任务自动执行 / 执行时点」两行后，
  `CFG_MAP` 里还留着这两个键，被上游的 `TestConfigFormMatchesCFGMap` 抓出来 → 一并清掉
* **APK 打包降级**（环境侧）：`$PREFIX/lib/wbapi-tools/` 整个被重装抹掉，`android.jar` 无法离线重建
  （SDK 的 stub jar）→ 本次 aapt2 的框架表改用系统 `framework-res.apk`、**复用上一版 `classes.dex`**
  （Java 源码零改动，dex 仍为 44,060 字节），并跳过 javac/d8 与看门狗自检。副作用：APK 的
  `compileSdkVersion` 元数据记为 36（跟着设备框架）而非 35，其余（minSdk 24 / targetSdk 34）不变。
  ⚠ 下次改 `apk-src/java/**` 前必须先补回 `android.jar`。
* **本机 Go 工具链修复**（环境侧，不影响产物）：工具链被重装后丢了 rt0 补丁，`go` 全线报
  `go <自身路径>: unknown command`；用 `patches/patch_rt0.py` 补回 `$GOROOT/bin/{go,gofmt}` 与
  `pkg/tool/android_arm64/*`，再 `bash patches/patch_source.sh` 重打源码补丁后恢复。

## v1.23.0

* **任务中心整个下线**：面板侧边栏入口、整个视图（扫描待办 / 执行全部待办 / 并发 /
  跳过名单 / 开学季券码）、对应前端逻辑（`groupItems` / `renderQueue` / `qrowHTML` / 队列轮询 /
  跳过名单）与后端路由（`/panel/api/tasks/scan_all|run_queue|queue|skip`、
  `/panel/api/school/vouchers`）**全部删除**；成长任务队列的**每日自动执行钩子**
  （`sch.SetGrowthHook`）也一并摘掉 —— 网关不会再自己去跑成长任务。
  * 连带清理：队列/券码相关 CSS（约 40 行）、券码弹窗 HTML、窄屏媒体查询里的死选择器
    （顺手补回被误删的 `.acts, .savebar { flex-wrap: wrap; }`）、配置页里已失效的
    「成长任务自动执行 / 执行时点」开关（钩子摘掉后它已经是死开关）。
  * **保留**：账号详情里的任务弹窗（全部接受 / 领取 / 一键完成可自动任务）与
    `/panel/api/accounts/{uid}/tasks/*` 路由 —— 那是账号页的功能，不属于任务中心。
  * Go 侧 `taskcenter.go` / `taskgate.go` **文件保留**（不再接线，编译时被链接器丢弃）：
    要恢复功能，从 `v1.22.1` 取回 `index.html` / `app.js` / `panel.go` / `main.go` 的对应片段即可。

## v1.22.1

* **拆掉「日志」芯片**：动作条恢复成 4 个胶囊（重启网关 / 复制密钥 / 保活设置 / 重载面板），
  日志入口与日志页（`openLog` / `tailOf` / `mask` / 复制按钮）一并删除，横向滚动容器也撤了 ——
  界面回到 v1.20.0 的样子。
  * **后端日志照旧写**（不占界面、不额外耗电）：网关 stdout/stderr →
    `files/gateway/data/server.log`（超 4MB 翻代），App 事件 → `files/logs/app.log`（超 512KB 翻代）。
    要看得走 adb：`adb shell run-as com.dsh.wb2api cat files/gateway/data/server.log | tail -80`。

## v1.22.0

* **任务中心只做"能做得了的"** —— 以前「执行全部待办」里混着一批**上游未证实能点亮**的任务
  （`Sequential_Tasks_4~7` 预留项、`black_cat` 夜猫子），跑完不完成、还天天出现在待办里，
  看起来就是"任务中心做了没用"。这次按"能做就做完、做不了就砍掉"改：
  * **尝试型任务默认不排**：动作表里本来就有 `Attempt`（"上游未证实可脚本化，跑了可能不点亮"）字段，
    但以前**只用来显示一句提示**、不拦队列。现在它是真门控，任务中心新增
    「包含尝试型任务」勾选（默认关 = 只做有把握的）。顺带把 `Sequential_Tasks_4~7`
    这批注释写着"预留/判据待解锁验证"的动作标成尝试型。
  * **反复无效的自动跳过**：某个 (账号,任务码) **连续 3 次**执行后进度没动或报错，
    就把它砍出待办并落盘 `data/task_skip.json`（**7 天后自动放回**，避免永久误判），
    不再无限重试白烧上游额度；真有一次进展则计数清零。
  * **不做静默过滤**：扫描结果里每个被跳过的任务都给一行 + **写明原因**
    （本版无自动化动作 / 尝试型 / 反复无效 + 最后一次失败原因），标题栏显示
    「N 项待办，M 项已跳过」。单任务与「一键完成」路径同样遵守该名单（尝试型直接 409 说明原因）。
  * 任务中心新增 **「清空跳过名单」** 一键重试；新接口 `GET/POST /panel/api/tasks/skip`
    （GET 看名单与开关，POST 改 `allow_attempt` / `clear`）。
  * 队列里无效项的提示带上计数：`（连续无效 1/3）`…`（连续 3 次无效 → 已跳过：不再出现在待办里…）`。

## v1.21.0

* **App 加详细日志：一页看全 + 一键复制。** 以前日志其实一直在写
  （`files/gateway/data/server.log`），但手机上没有入口、没有复制按钮 —— 等于没有日志。
  * 顶栏新增 **「日志」芯片**：**① App 事件**（系统什么时候冻我/杀我、前台服务有没有被拒
    启动、alarm 精不精确、JobScheduler 有没有触发、常驻图层有没有挂上）
    + **② 网关日志尾部**（任务中心执行了什么、上游回了什么，全在里面）。
  * **「复制全部」** 一键复制，且**自动脱敏**（`Bearer …` / `wb2api-…` / `sk-…` 一律打码），
    可以直接贴给作者。
  * App 事件单独落盘 `files/logs/app.log`（超 512KB 翻代，跨重启保留）；网关日志超 4MB 翻代；
    日志页只读**尾部**（512KB / 600 行，走 `RandomAccessFile`），不整读，不会 OOM。
  * 动作条改**横向可滚**：芯片再多也不会被窄屏裁掉。
* **「执行过了，但还是未完成」现在有判据**（面板）：队列单项结果会直说进度有没有动 ——
  `✅ 进度 +1` / `⚠️ 进度未变化：上游没有计入这次行为（可能未到结算时间），建议过几分钟再扫一次`
  / `✓ 已领取` / `，还差 N 次`。以前只印一个进度快照，"跑完进度没动"和"没事可做"看起来一模一样。
  * 实测（临时实例 + 坏 token 假账号）：`level=err`、message 带账号名与上游 401 ✓

## v1.20.0

* **「任务中心 → 执行全部待办」点了没效果？现在它会告诉你为什么。** 以前队列路径把每个
  账号的拉取错误**静默吞掉**，无待办时统一回一句「全部账号没有待办任务」，前端还弹成**绿色 ok**
  —— 拉取失败看起来跟"没事可做"一模一样，这就是"点了没效果"的来源。
  * 服务端新增诊断账 `queueDiag`：分别统计 **拉取失败（带账号名 + 上游报错）/ 每日锁定 /
    已领取 / 本移植版暂无自动化动作 / 不参与账号（停用·global·企业版）**，拼成人话回给前端。
  * 响应新增 `level`：`err`（确实出问题 → 前端弹红）与 `ok`（单纯没事可做）分开。
  * 面板同步：「扫描待办」把每个账号的 `growth_error` 渲染成**红行 + 原因**
    （以前完全不显示，扫描"看着啥事没有"）；错误行支持自定义标题，上游回 HTML/换行压平成一行。

## v1.19.0

* **治"启动一会就连不上、必须把 App 拉回前台"**。这个"拉回前台就好"是关键判据：进程没死，
  是被**冻结**了（被杀的话看门狗 10 秒就拉回来了；被冻则连看门狗线程一起停摆）。
  * **常驻图层**：1×1、全透明、不接收触摸的悬浮窗。系统按进程**可见性**分级 —— 有可见窗口的
    进程不会被判成缓存态，也就不会被 app-freezer 冻住。需授予「显示在其他应用上层」，
    没授权就静默跳过（功能照旧，只是可能再被冻）。
  * **Wi-Fi 高性能锁**：屏灭后 Wi-Fi 省电/休眠会掐掉从电脑连过来的连接（127.0.0.1 不受影响）。
  * **冻结自检**：看门狗发现心跳断了 30s 以上 → 记下「系统冻了我 N 秒（HH:mm 恢复）」，
    顶栏显示「冻过 N」。没 root 时这是唯一能拿到"谁冻的、冻多久"的证据。
* **「保活设置」按钮**（原「省电白名单」）：缺哪补哪，按 省电白名单 → 常驻图层 → 精确闹钟
  的顺序引导，一次点一下；顶栏实时显示 `省电✓ 图层✓`。

## v1.18.0

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
