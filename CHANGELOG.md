# 变更记录

## v1.26.8（当前）

**本版 = 第四份 bug 清单（12 条）的落地修复，严重 3/3、一般 9/9 全修。**

### 严重

* **1 面板加载被反复打断，最终永久白屏** —— `tick()` 每 2 秒只要 `!panelLoaded` 就再发一次
  `loadUrl`，**不判上一次是否还在进行**。低端机上 WebView 冷启动 + 面板大 HTML/JS 普遍
  超过 2 秒，于是从第 2 拍起每 2 秒把正在进行的加载打断重来，30 次（≈60 秒）后
  `loadTries` 用尽 → 永久空白页，只能手点「重载面板」。新增 `panelLoading` 在途标志，
  `onPageFinished` / `onReceivedError` 里清位。
* **2 余额解析非周期套餐不钳制** —— `packageRemainUsed` 的 Cycle 分支把 remain 钳到
  `[0,size]`，非 Cycle 分支没钳（函数注释却承诺了）。上游脏数据（`CapacityRemain > Size`）
  会高估 credits → 选号权重与 `credit_floor` 判定失真、面板显示"剩余 > 面额"。
* **3 用量窗口起点按绝对时间取整** —— `time.Now().Truncate(time.Hour)` 是按「公元 1 年起
  的绝对时长」截断，UTC+05:30 / +05:45 / +12:45 这类非整点偏移时区会截到本地 :30/:45，
  而桶按本地小时存 → 当前小时的桶被排除，「近 N 小时」永远少最新一小时。
  改成先用 `time.Date` 构造本地整点。

### 一般

* **4 前台服务 `onTimeout` 里不退出服务** —— 两个重载都只 `setState` + `armAlarm`，
  而 `onTimeout` 的语义是「你自己停下来，几秒内不停就 ANR / 被打死」。补 `stopSelf(startId)`
  （服务是 `START_STICKY`，停掉后 alarm / JobScheduler 照常拉回）。
* **5 降级重试吃掉一次轮转名额** —— 内容拦截降级重试走 `continue` 会让 `i` 自增；
  `MaxRotate==1` 时循环直接结束，而这条分支从不给 `lastErr` 赋值 → 末端落默认
  503 `no_healthy_account`，把"请求内容被拦（应 400 `content_blocked`）"报成"服务端没号"。
  改成 `i--` 不占名额。
* **6 两个模型缺默认档位** —— `deepseek-v4-flash` / `hy4-preview-x` 补 `defaultEffort: "high"`
  （表头注释与其它三档模型都是 high），否则 `/v1/models` 这两个模型没有
  `reasoning_default_effort`。
* **7 优惠时段允许 24 点** —— `promoClock` 校验是 `h > 24`，`"24:00"` 合法并返回 1440；
  上游下发 `start:"24:00"` 时 `st > ed` → 走跨午夜分支对多数窗口恒真 → 优惠被判"全天生效"。
  上限改 `h > 23`。
* **8 请求归档停机时静默丢事件且不计数** —— `close()` 置位与 `enqueue()` 检查之间有窗口，
  事件可能写进没人读的通道：既不落盘也不进 `DroppedWrites`（指标显示"没丢"）。
  加 `enqMu` 读写锁让「检查 + 入队」与 close 互斥，停机窗口内的事件计入丢弃。
* **9 全量余额刷新是同步扇出** —— 其它 `*_all` 都是「起 goroutine 立刻回 200」，只有
  `balance_all` 同步等（并发 3 × 每号 30s，46 个号最坏 ≈ 7 分钟）。改异步 + 前端轮询
  overview，toast 不再谎称"已刷新完成"。
* **10 `GatewayService.stop()` 停不掉服务** —— 只 `stopService`、没置 `userStop`，
  `onDestroy` 里 `if (!userStop)` 会重挂 alarm + JobScheduler，5 分钟内自己回来。
  按 `ACTION_STOP` 分支补齐（`userStop` + 取消 alarm/job）。
* **11 `login.sh` 在非交互环境静默退出** —— `set -e` 下 `read` 遇 EOF 直接终止脚本、
  只留半截输出。改成显式判返回值并给错误说明。
* **12 非 JSON 数据帧被当「空流」** —— `writeFrame` 对非 JSON 帧原样透传但**不计数**；
  整条流都是这种帧（心跳/纯文本兼容形态）时被判空流 → 补写错误帧 + 返回
  `errEmptyStream` → handler 记 502 假失败。改成成功转发即计一帧。

### 测试

`upstream 203 / server 90 / pool 184 / panel 27 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**688 通过 0 失败**；`gofmt -l` 为 0。

## v1.26.7（当前）

**新增任务 `WorkBuddy_Valorant`：体验「无畏契约拼豆专家」（+100 分 +5 能，截止 11-15）。**

* **判据与 expert_5 同源但目标不同**：那个是「任意 5 个专家各用一次」，这个是**指定
  某一个专家**。走完整链：accept → 召唤链（`web_element_click` + `expert_summon_click`
  + `expert_summoned`）→ 真实桌面 chat（`X-Expert-Id`，拿服务端 `requestId`）→
  `expert_actual_use` + chat 链 → 回读进度 → 达标即领奖。
* **国服/国际服分开**（`valorantExpertIDs`）：
  * 国服已知 id `ex_xQHHD1u5Jkua`（`agent_name=valorant-perler-expert`，version 1.2.1）；
  * 国际服 id 未实测，**留空**表示只能按名称匹配（`无畏契约` / `valorant` / `perler` / `拼豆`），
    绝不复用国服 id —— 跨域 id 打过去只会白跑一次且不留痕迹；
  * 市场列表里都找不到时明确报错，不静默用兜底 id。
* **任务码也可能按域不同**：`WorkBuddy_Valorant` 查不到时按 `valorant` / `perler`
  在任务列表里兜底匹配，命中后用它的真实 code 做 accept / claim。
* 新增 3 个单测：动作已注册且非尝试型、国服 id 固定 / 国际服必须留空、名称兜底匹配
  （英文名、中文名、无关列表都要给出正确结论）。

### 测试

`upstream 203 / server 90 / pool 184 / panel 27（+3） / session 20 / redisstore 6 /
reqlog 7 / auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**688 通过 0 失败**；
`gofmt -l` 为 0。

## v1.26.6（当前）

**本版 = 第三份 bug 审查（19 条）的落地修复，P0 7/7、P1 7/7、P2 5/5 全修。**

### P0（功能直接失效）

* **1 `run.sh` 的 restart 分支必然失败** —— `"$0" stop` 依赖可执行位，而脚本本身没有 x 位、
  工作区还在 sdcard（noexec），`bash run.sh restart` 直接 `command not found`，
  stop/start 都没跑。改成 `bash "$0" stop`（watchdog.sh 早就是这么调的，只有这一行漏了）。
* **2 任务扫描结果混进空账号行** —— `items` 按 `len(states)` 预分配，被禁用账号与 ctx
  取消的分支用 `continue` 跳过，留下 `{"uid":"","nickname":""}` 零值条目。收尾统一过滤
  （`schoolVouchers` 早就这么做，这里漏了）。
* **3 手动领奖后池内积分不刷新** —— 自动领奖路径都调了 `refreshCreditsAsync`，
  手动这条漏了；用户点「领取」拿到奖励但面板积分不动，读成"领了没到账"。
* **4 旧网关进程的退出回调覆盖新进程状态** —— 看门狗先 SIGTERM 旧进程再退避 2s 拉新的，
  而 Go 侧优雅停机最长 5s：旧进程的 `waitFor` 可能在新进程 `setState(RUNNING)` **之后**
  才返回，把状态和 `lastExit` 一起覆盖成「已停止 / rc=143」，而 7863 其实正在服务。
  现在整个回调先判 `proc != me` 就直接返回。
* **5 连登日志打出 `昵称(昵称(uid8))`** —— 上一轮的批量改写脚本把已经包在
  `logfmt.Label(...)` 里的 `a.UID` 又替换了一次（嵌套两层）。
* **6 导入账号的 `expires_at` 单位假设** —— 只按毫秒解释，导出工具给**秒**时
  `/1000` 得到 1970 年，既非 0 也进不了兜底 → `NeedsRefresh` 恒真，每个 chat 请求前
  都先打一次 refresh。改成按量级判单位（>1e11 才算毫秒），并对"明显过期"兜底。
* **7 前端按 UTC 解析日桶日期** —— `new Date("2026-09-16")` 按规范解析成 UTC 零点，
  西半球时区整体早一天，而柱子位置用的是本地解析（`parsePointTime` 补了 `T00:00:00`）
  —— 标签与柱子错开一天。日桶标签与到期日都改成本地解析/格式化。

### P1（状态与语义不一致）

* **8 `/status` 的「冷却中」汇总与行内字段口径不一致** —— 汇总用 `!healthy`（四维或门），
  行内只取 until/breakerUntil 两维：仅因连败降权的号在汇总里算 cooling、行内却
  `cooling=false`。补上 `degradeUntil`，并把降权期并入"取更晚截止"的比较（`cool_kind=degraded`）。
* **9 只有小程序路径的等待可中断** —— `runAutoAll` / `runChat5` / `runExpertBatch` /
  `runTemplateUse` / `taskAcceptAll` / 队列路径的 18 处裸 `time.Sleep` 全部换成
  `sleepAB(uid, d)`，断连后不再把整条流水线跑完还占着 per-account 锁。
* **10 粘性绑定失效时不删 Redis 镜像** —— 慢路径 `delete(r.entries, key)` 漏了 `DelBind`，
  重启后 `LoadFromStore` 把旧绑定读回来，会话又被钉到不可用的号上（要等 TTL 到期）。
* **11 凌晨触发的余额耗尽冷却只到当天 04:00** —— 签到时点是 09:00/21:00，04:00 时余额
  仍是 0：账号 04:00 回池必然再撞一次 402，然后被推到次日 04:00。改成 **09:00 前触发 →
  当天 09:00**（下一次签到点），其余时段仍是次日 04:00。
* **12 上游断流且 0 有效帧时被归类成「空流」** —— `validFrames == 0` 的判断排在
  `readErr != nil` 之前，后者成了死代码；排障时分不清"上游没吐数据"与"连接被打断"。
  调换顺序（读错误优先）。
* **13 `RefreshHeaders` 直读 `a.RefreshToken`** —— 同文件其它头都走加锁读取器，
  这一处是唯一直读；调用方一旦传池内的 `*auth.Auth` 就是数据竞争。改 `RefreshTokenValue()`。
* **14 `trunc()` 按字节截断** —— 中文昵称被切成半个字符（非法 UTF-8）。改用 `logfmt.Truncate`。

### P2（显示 / 健壮性）

* **15 「积分构成」首次进入用错 `package_detail_limit`** —— 先开首页让到期卡片拉过
  packages（写 `lastPackages` 但不写 `lastDetailLimit`），再切过去走缓存分支就用默认 5。
  缓存分支也解析 limit。
* **16 `usage.json` 同 key 重复条目互相覆盖** —— 命中已有 key 时改成累加，不再静默丢数据。
* **17 每条请求都整体复制最近记录切片** —— `append([]Event{e}, r.recent...)` 在持锁状态下
  搬运十几 KB。改成环形写，`Snapshot` 时倒序输出（对外契约不变）。
* **18 非流式序列化失败时流水行仍记 200** —— 改成先 marshal 再写，失败记 500 + HTTPError。
* **19 `.fixc.py` 的按行兜底替换是嵌套 `Label` 的源头** —— 一次性脚本已删除。

### 测试

`upstream 203 / server 90 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**685 通过 0 失败**；`gofmt -l` 为 0。

bug #11 改了硬冷却的落点（09:00 前 → 当天 09:00），5 个断言随之更新为
「hour ∈ {4, 9}」并注明理由 —— 这是行为变更，不是把测试改绿。

## v1.26.5（当前）

**本版 = 第三份 bug 清单（19 条）的落地修复，P1 7/7、P2 9/9、P3 3/3 全修。**

### P1（会丢数据 / 让功能静默失效）

* **1 落盘失败后状态不再重试** —— `saveLocked()` 改成返回 `error`，flusher 与 `Flush`
  失败时**回置 dirty**。此前先清脏标记再落盘，一次瞬时失败（磁盘满 / rename 跨设备 /
  权限）之后要等到下一次状态变更才重试，而 APK 场景进程随时被杀 —— 冷却截止、熔断计数、
  成本账本就永久丢了。`usage.go` 早就按这个范式修过，pool 侧没对齐。
* **2 面板 `writeJSON` 吞掉序列化错误** —— 面板那份是漏改的副本（server 包已按 B11 修过）：
  marshal 失败仍 200 + 0 字节，前端 `r.json().catch(()=>({}))` 把它吞成 `{}`，
  页面「没报错但什么都没变」。
* **3 模型级阻塞判定不分域** —— 新增 `ModelBlockedForRealm`，handler 传入本次解析出的
  `realm`。此前一个 `global:xxx` 请求即使所有 global 号都被该模型冷却挡住，也会因为
  某个 CN 号「没被冷却」而判为未阻塞 → 503 `no_healthy_account`，客户端按「服务端过载」
  无限重试，而正确处置是换模型（400 `model_unavailable`）。
* **4 末端「等级闸门」兜底用全池判定** —— 改成 `ModelDeniedForAllAccountsInRealm`，
  与 `/v1/models` 那边的分域口径一致。
* **5 积分包两套 remain/size 口径** —— `CreditPackages` 删掉内联 switch，改用
  `packageRemainUsed()`（补回「remain > size 钳制」与「CycleUsed 反算 remain」两条），
  面板「积分构成」与「账号余额」不再互相矛盾。
* **6 `scheduler.New` 漏了 GrowthHours 默认值** —— 其余五个槽位都有、唯独它没有，
  空值时 `nextFire(now, nil)` 返回零时间被跳过，成长任务**永远不排程且无日志**。
* **7 单任务入口没有 ctx / 中止标记** —— `accountTaskAuto` 补 `clearAborted` +
  `watchAbort(r.Context().Done(), uid)`：客户端断开后长流水线（mp 45~55s/条 × 最多 10 条）
  尽快退出并释放 per-account 锁，不再让用户重试全撞 409。

### P2（行为不一致 / 边界错误）

* **8 web 域请求的 Origin/Referer 硬编码 CN 域** —— 新增 `webOrigin(a)`，profile 与
  ClaimReward 都改用它。国际版号此前因来源域与目标域不一致被拒（global 号昵称永远
  同步不上、领奖走 Web 降级路径失败）。
* **9 `ClaimRewardMP` 用类型断言** —— 改 `errors.As`（任何一层 `%w` 包装都会让断言
  失败 → 400 时不再降级到 Web 领奖端点）。
* **10 登录完成时 `expiresIn` 缺失把过期时间写成「现在」** —— `NeedsRefresh` 恒真 →
  每个 chat 请求都先打一次 refresh。改成 `ExpiresIn > 0` 才写。
* **11 签到账号间节流不可取消** —— 新增 `RunCheckinNowCtx(ctx)`，调度器 `runBatch`
  传 ctx，面板手动入口走 `RunCheckinNow()`（Background）。`sleepCtx(Background, …)`
  的 `return` 死代码随之消失。
* **12 跳过名单 `Until` 零值永久跳过** —— 零值视为已到期并清条目（手改 / 旧版本写入 /
  写入中途断电都会留下这种条目，7 天自动放回的承诺会失效）。
* **13 单号签到把幂等「已签到」当错误文案回前端** —— 改回「今天已签到」，其余错误照常回显。
* **14 手动全量签到日志文案不符** —— 删掉「（含猫猫旅行）」，改为提示旅行是独立排程。
* **15 前端读取后端不存在的 `file_error`** —— 该分支恒为 undefined（后端删文件失败
  直接 500 且不出池），删掉。
* **16 暂停中的账号在冷却期显示「解冻」** —— 按钮优先级改成先判 `paused`（「恢复选号」），
  再判 `frozen`。此前点一下会把暂停状态和全部冷却一起清掉。

### P3

* **17 领奖后不刷新池内积分** —— 新增 `refreshCreditsAsync()`，领奖成功后异步拉一次
  `UserResourceDetailedWithExpiry` 写回池内，面板不用再等 5 分钟。
* **18 面板自动加载的重试次数永不重置** —— 成功加载时 `loadTries = 0`。
* **19 连登管家日志用完整 uid** —— 统一 `logfmt.Label(uid, nick)`，按 uid8 grep 不再漏行。

### 测试

`upstream 203 / server 90 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**685 通过 0 失败**；`gofmt -l` 为 0。

三个「全禁用」排程用例补了 `GrowthDisabled: true` —— bug #6 修好之后成长排程有了默认
时点（01:00），不关它这些用例的「不应排程」断言自然不成立。

## v1.26.4（当前）

**本版 = Bug 清单（B1–B15）的落地修复，15 条全修。**

### 高

* **B1 打 tag 时发布流程必定失败** —— CI 从 `cmd/server/main.go` 里 grep `appVersion`
  取版本，而该值在 v1.26.3 之后是 `dev`（真版本由 `-ldflags -X` 注入）：`TAG=1.26.4`、
  `SRC=dev`，一致性断言必然 `exit 1`，Release 永远建不出来；"断言二进制含版本串"
  也是 grep `dev`，任何二进制都命中，等于没校验。改成两个 job 都读仓库根 `VERSION`，
  构建补 `-ldflags "-X main.appVersion=$(VERSION)"`，并自动识别 Go 模块目录。
* **B2 点"停止"后，排队中的重启任务会把网关重新拉起** —— `restartGateway` 投递给静态
  单线程 `boot` 的任务只检查 `alive()`：旧进程已被 `killProc` 杀掉 → 判定"没有存活进程"
  → 直接 `startGateway()`。此时 Service 已 `stopSelf()`，新进程没人看门狗、没 wakelock、
  没 alarm，成了占着 7863 的孤儿。现在 `boot` 任务执行前检查 `userStop`，
  `startGateway()` 入口也加了一道。

### 中

* **B3 硬冷却被模型级 429 缩短并降级** —— `CooldownSoftForModel` 的"无重置时间"分支
  是**赋值** `e.until = now.Add(d)` 且无条件 `coolKind = CoolSoft`：硬冷却到次日 04:00
  的号会被 6004 砍成 10 分钟、提前回池继续撞 402，而 `reviveCoolingLocked` 只认
  `CoolHard`，状态机此后错位。补上与 `CooldownSoftRate` 相同的守卫，并改成"只延长"。
* **B4 配置写空串启动失败，"空值回落默认"是死代码** —— `ParseDuration("")` 先返回
  invalid duration，后面那段 `if x == "" { 默认值 }` 永远走不到。四处（`degrade_cooldown`
  / `degrade_cooldown_max` / `breaker_cooldown` / `breaker_cooldown_max`）的回落全部
  **提到** ParseDuration 之前，与 `soft_rate_max` 口径一致（空回落、非法报错）。
* **B5 面板 api_key 注入靠 400ms 硬延时** —— 写完 `localStorage` 睡 400ms 就 reload，
  低端机首帧慢时 reload 抢在写入生效前 → 面板 401，点"重载面板"又是同一条竞速。
  改成 `evaluateJavascript` 回调里**回读校验**，确认值一致才 reload；失败重试 3 次
  （每次 300ms），仍失败在状态条明说"自动登录失败"。
* **B6 常驻图层是 check-then-act** —— `installOverlay` 加 `synchronized`：主线程与看门狗
  线程都会经 `holdWake()` 走到这里，最坏情况加两个 1×1 悬浮窗、静态字段只记住后一个，
  前一个再也 `removeView` 不掉（WindowManager token 泄漏）。
* **B7 日志配额按字符算** —— `line.length()` 是 UTF-16 字符数，中文日志每字符写 3 字节
  UTF-8，`LOG_PUMP_MAX=2MB` 实际能落 6MB；而翻代判断用的是真实字节，两处口径不一致，
  2MB/4MB 边界行为会跳变。改成按实际写出的 UTF-8 字节数累计。
* **B8 降级构建不更新 versionCode，装机不升级** —— 无 aapt2 时复用上一版清单只改
  `versionName`，`versionCode` 还是旧的，覆盖安装会被系统当成"版本没变"。
  新增 `patches/patch_axml_version.py`：解析 AXML 的 resource map，定位
  `android:versionCode`（0x0101021b）的 int 属性**原地改写 + 回读校验**，失败即非 0 退出
  （宁可不产出，也不产出半正确的包）。本版实测：`versionCode` 从 **21 → 12604**。
* **B9 `.tmp/` 里的构建产物被推到公开仓库** —— tar 排除清单补 `./.tmp`，根 `.gitignore`
  补 `.tmp/`（原有 `*.tmp` 只匹配后缀，匹配不到目录）。

### 低

* **B10 `appLog` 翻代失败后日志无界增长** —— `renameTo` 的返回值此前没看，失败时仍往
  原文件追加，512KB 上限失效。现在检查返回值，失败则原地截断重写并记一条原因。
* **B11 `writeJSON` 吞掉 marshal 错误** —— 序列化失败仍 `WriteHeader(200)` 再写 0 字节，
  客户端拿到"200 空响应"。改成 marshal 成功后才写状态码，失败回 500 并记日志。
* **B12 面板里点外链会在 WebView 里跳走** —— 覆写 `shouldOverrideUrlLoading`：
  只有面板地址放行，其余交系统浏览器。此前一跳出去 `panelLoaded` 仍是 true，
  `tick()` 不会再把面板拉回来。
* **B13 加权抽签的 `maxCredits` 基准与注释不一致** —— 截断用全集 max、抽签用子集 max，
  子集里最高 credits 的号被 `minPickGap` 排除时基准变小、credits 权重被放大。
  改成把全集 `maxCredits` 传进 `pickWeighted`，两阶段基准统一。
* **B14 上游不发 `[DONE]` 时被丢弃的 tool_call 没有日志** —— `dropTruncatedToolCalls`
  返回丢弃数，`Aggregate` 有丢弃时打一条 WARN（带 id / model / finish_reason / saw_done）。
* **B15 算不出速率却打成 `0.0tok/s`** —— 同一列同一语义（无观测）不能有两种表示，
  改成保持 `-`，与 `toks` 缺失同口径。

### 测试

`upstream 203 / server 90 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**685 通过 0 失败**；`gofmt -l` 为 0。

## v1.26.3（当前）

**本版 = ISSUES-2026-10-19.md（19 个 issue）的落地。**
P0 1/1、P1 5/5、P2 10/10 全部关闭；P3 三个合集（17/18/19）里挑了高价值项修完，
剩余项在文末「未做」列明。

### P0

* **ISSUE-01 示例配置与 Docker 镜像开箱启动失败** —— `config.example.json` 的
  `listen: ":7863"` + `api_key: "CHANGE_ME"` 正好撞上 v1.26.1 加的守卫（非回环 +
  占位密钥 → 拒绝启动），照文档"三步启动"必然 EXIT=1。改法：
  ① 示例配置改成 **`127.0.0.1:7863` + 空 key**（裸机默认安全，直接能跑）；
  ② 新增 `docker/entrypoint.sh`：容器里没有显式配置时自动生成随机 `WB2A_API_KEY`
  并把监听面设为 `0.0.0.0:7863`，`ENTRYPOINT` 改走它。
  实测：示例配置启动成功，`/healthz` 返回 503（无账号）而不是崩溃。

### P1

* **ISSUE-02 凭据散落** —— ① 签名口令文件优先读 **App 私有目录**
  `$PREFIX/etc/wb2api.kspass`（0600 真生效），工作区 `.kspass` 降为兜底，已迁移；
  ② `push_github.sh` 的 tar 排除 `.kspass`，扫描规则新增"疑似口令文件内容"；
  ③ token 优先取 `GITHUB_TOKEN` 环境变量，走文件时打警告。
* **ISSUE-03 `state.json` 损坏静默清零** —— 解析失败时另存 `state.json.corrupt-<unix>`
  并打 **ERR** 日志再以空状态启动（此前静默 return，5 秒后 flusher 把空状态写回，
  冷却/熔断/成本账本/credits 全部不可恢复）。
* **ISSUE-04 全量签到无节流** —— 加 `checkinAccountDelay = 800ms`（与 travel/activity
  同口径），46 号部署不再 92 个请求连发把自己打进 WAF 冷却。
* **ISSUE-05 版本三处不一致** —— 新增仓库根 **`VERSION`** 作为单一版本源：
  `rebuild.sh` 与 `build_apk.sh` 都从它注入 `-X main.appVersion=`，`appVersion` 从
  `const "1.13.0-panel"` 改成可注入的 `var`；`build_apk.sh` 校验清单 `versionName`
  与 VERSION 一致，不一致直接失败。
* **ISSUE-06 匿名可读面** —— `/healthz` 匿名只返回 `{"ok":…,"service":…}`；
  计数与 `realm_servable` 只在带正确 Bearer 时返回（运维脚本不受影响），
  完整数据本来就在要鉴权的 `/status`。实测：匿名 `{"ok":false,"service":"workbuddy2api"}`，
  带 key 才有 healthy/total。

### P2

* **ISSUE-07 `max_body_mb: 0` 契约不可达** —— 改 `*int` 三态：`null`=默认 64、
  `0`=**真的不限制**、负值=报错。另两项（`idle_weight_per_hour` / `header_timeout_seconds`）
  的注释改成"0 = 回落默认"，不再声称可关闭。
* **ISSUE-08 导入伪造一年有效期** —— 缺 `expires_at` 时写 `now+1h` 并打日志
  （此前写 `now+365d`，token 过期后永不刷新，只能等 12153 三振禁用）。
* **ISSUE-09 auth 读访问器用写锁** —— `AccessTokenValue`/`DomainValue`/
  `RefreshTokenValue`/`Realm` 全部改 `RLock`（写方仍是 `Lock`）。
* **ISSUE-10 SSE `data:` 口径不一致** —— 抽出 `ssePayload()`，统计层与透传层共用；
  `data:{...}` 形态不再"成功但 0 token"。
* **ISSUE-11 长任务裸 sleep** —— 新增 `internal/panel/cancel.go`：按账号登记取消标记 +
  `sleepAB()` 可中断睡眠；`accountTaskAutoAll` 在 ctx 结束时给账号打标记，
  mp 事件节流（45~55s/条）与 accept 回读等待都改成可中断，断连后尽快放锁。
* **ISSUE-12 归档查询全量扫描** —— 文件名按 (日期, 序号) **数值倒序**读，凑够 limit 即停；
  最终结果仍按事件时间排序（mtime 失真/同秒轮转都不影响正确性）。
* **ISSUE-13 热点 O(N)** —— 未做（见「未做」）。
* **ISSUE-14 列模型用了会改选号状态的 `Pick`** —— global 分支改成
  `AvailableUIDsForRealm()[0]` + `AuthByUID`，与 CN 分支对齐；看一眼 `/v1/models`
  不再影响后续 LRU 与闲置权重。
* **ISSUE-15 成长排程未接线 + 死路由** —— 选了「接线」：`main.go` 补
  `sch.SetGrowthHook(pn.RunGrowthQueueOnce)`；`panel.routes()` 注册
  `POST /panel/api/tasks/scan_all`、`POST /panel/api/tasks/run_queue`、
  `GET /panel/api/queue`、`GET /panel/api/school/vouchers`（纯 API，不依赖已下线的 UI）。
* **ISSUE-16 测试工程化** —— 部分做：node 探针改成真跑 `node --version`（Android 上
  LookPath 找得到但执行不了，此前 15 个前端测试全红）。夹具路径与 `-race` 见「未做」。

### P3（选做）

* **ISSUE-17** 部分：travel / 签到的 `time.Sleep` 改 `sleepCtx`。
* **ISSUE-18** 全部：`patch_rt0.py` 的 `already-patched` 措辞改准确；
  `probe_max_tokens.py` 示例地址改 `127.0.0.1`；`Dockerfile` 补 `cmd/trial`；
  `.dockerignore` 补 `*.apk`/`*.idsig`/`*.keystore`。
* **ISSUE-19** 部分：`login.sh` 落盘 auth 显式写 `realm`。

### 未做（明确记录，不假装完成）

* **ISSUE-13**（可用集短 TTL 缓存、`ModelLockView` 聚合）—— 需要先有 CPU profile 基线，
  盲改容易引入一致性问题。
* **ISSUE-16** 的夹具路径改造与 CI `-race` job —— `-race` 需要 cgo/交叉工具链；
  夹具路径改动面大（十几个测试文件）。
* **ISSUE-17 其余**：`validUID`/`validImportUID` 去重、`runExpertBatch` 失败计数、
  `panel.models` 上游超时、`ring` 按频道分配、`exploreLast` TTL、流内 error 帧喂 WAF、
  时区口径统一 —— 都是"改对了没人看得出来、改错了要排查半天"的类型，单独排期更稳。
* **ISSUE-19 其余**：`portUp` 外部实例 PID 校验、保活开关 UI、`networkSecurityConfig`、
  `onTimeout` SDK 断言、`cmd/signin` 的 `isAlready` 判定、`cmd/credit` 口径复用 ——
  多数要动 Android 清单（当前构建链无法重编二进制清单）。

### 测试

`upstream 203 / server 90 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**685 通过 0 失败**；`gofmt -l` 为 0。

## v1.26.2（当前）

**本版 = 第三份代码审查报告（CODE-REVIEW-REPORT.md）的落地修复。**
4 条 P0、8 条 P1、8 条 P2 全部处理，脚本类 4 条修 3，观察项 4 条修 3。
其中 P0-1/P0-2/P0-3 都是「静默把健康号弄坏」这一类，用户侧表现是"明明有号却 503"。

### P0（4/4）

* **P0-1 客户端断连被记成「账号连败」，健康号 5 次即降权** —— Agent 场景频繁取消长任务时，
  `r.Context()` 取消落进「网络层抖动」分支 → `NoteFailures` 连喂 5 次 → 账号降权 10 分钟。
  现在传输层错误分支**最先**判 `r.Context().Err() != nil`：不换号、不罚号，只记
  `OutcomeInterrupted` 并终止轮转。
* **P0-2 12153 单次即永久禁用，绕过三振规则** —— 聊天主路径与 token 刷新路径原本直接
  `Disable`，而保活路径早就改成「连续 3 次才判死」。现在两处都改 `NoteSessionDead`，
  第三次才禁用并打 ERR 日志。一次网络抖动不再把号打进冷宫。
* **P0-3 短冷却覆盖长冷却：零积分账号 10 分钟后回池继续撞 402** —— `Cooldown` 与
  `CooldownSoftRate` 此前无条件改写 `until` 与 `coolKind`。现在两条入口都加**单调性**：
  未到期的 `CoolHard` 不被 `CoolSoft` 缩短或降级，冷却只允许延长。
* **P0-4 `auth_dir` 配错时启动把 state.json 清空** —— `filepath.Glob` 对不存在的目录
  不报错、只返回空列表，于是「扫到 0 个账号」被当成「账号全被删了」，credits / 冷却 /
  熔断计数 / 成本账本一次性清零并落盘。`SyncToDir` 现在遇到「扫到 0 个但池内非空」
  直接 WARN 并跳过剔除。

### P1（8/8）

* **P1-5** 已核实**不是缺陷**：`candsAll` 在代码里是成本分层**之后**的集合
  （`cands = cands[:0]` 后只回填 `ws` 的元素），LRU 兜底不会捞到收费号。未改动。
* **P1-6** 等权重洗牌的「并列」判定只比 `ws[0]`：权重 `[5,3,3,1,1,1]` 时判成"无并列"→
  不洗牌 → 字典序截断饿死靠后账号。改成对**全体**权重判重。
* **P1-7** 用量小时桶永不按时间折叠（只在 >40 万桶时触发）→ 每 30s 全量重写 `usage.json`
  的 O(n) 写放大。补「每小时最多折叠一次」的触发。
* **P1-8** `flush` 在写盘**之前**就清 dirty，失败后不再重试（磁盘满/权限/rename 跨设备
  即静默丢一整天用量）。改成失败时把 dirty 放回去。
* **P1-9** `Auth.Nickname` 真实数据竞争（写加锁、读全裸）→ 新增 `NicknameValue()`（读锁），
  并把 server/pool/scheduler 里 55 处直读改成走访问器；`auth.mu` 换 `sync.RWMutex`。
* **P1-10** `/v1/chat/completions` 请求体无上限 → 新增 `server.max_body_mb`（默认 **64**，
  0 = 不限制保留旧语义），超限回 **413 request_too_large**。
* **P1-11** 余额刷新每号一个 goroutine（46 号 = 46 路并发打上游）→ 加 `sem=3` 并发闸门。
* **P1-12** 并发保存配置会互相覆盖同一个 `path.tmp` → `saveConfig` 加包级互斥。

### P2（8/8）

* **P2-13** 空模型名写进 `modelCooldowns[""]` 会让 `modelExempt()` 恒真、带偏 `/healthz`
  的 `ServableForRealm`。`CooldownSoftForModel` 补守卫（但**不整函数 return**——无重置
  时间那条路本来就是账号级软冷却，早期改法会把正常冷却一起关掉，已修正）。
* **P2-14** `SaveAtomic` 静默丢弃 auth 文件里的未知键 → 先读回原文件再覆盖已知键。
* **P2-15** `accountRemove` 先出池再删文件，删失败则账号重启后"复活" → 改成先删文件、
  成功才出池，失败回 500 且保留账号。
* **P2-16** 全仓 `gofmt -l` 归零（此前 import.go 等 15 个文件不合格式）。
* **P2-17** 三处文档注释错位（`RestoreFromSnapshot` 贴在 `Acquire` 上、`Acquire` 贴在
  `startFlusher` 上、`Add` 贴在 `load` 上）已归位。
* **P2-18** 删死代码 `tierDeniedLocked`。
* **P2-19** 模型目录缓存过期瞬间的惊群 → 拉取路径加互斥 + 锁内二次检查。
* **P2-20** 启动兜底日志打错错误对象（打的是原始的 not-exist，不是 WriteDefault 的失败原因）。

### 脚本（3/4）

* **S-21** 签名口令以 `pass:xxx` 进 argv（`ps` 可读）→ 改 `--ks-pass file:` 临时文件（0600，
  退出即删）。
* **S-22** `patch_source.sh` 的退出码检查是死代码（`set -e` 下 python 非零直接终止脚本，
  后面的 `[ $? -eq 0 ]` 永远执行不到）→ 改成 `if ! python3 ... then` 包住并给出友好提示。
* **S-23** 密钥/token 进日志：`run.sh` 改打**指纹**（sha256 前 8 位）；`cfg_val` 的键名
  改走 `sys.argv`（不再拼进 python 源码）；`login.sh` 不再打 token 前缀。
* **S-24** `push_github.sh` 的 `mask()` 改走环境变量 + python（不再把 token 拼进 sed 表达式，
  也不让 sed 命令行进 `ps`）；`git fetch` 补认证头（仓库转私有后不会静默失败）。

### 观察项（3/4）

* **观察项 2** `checkListenKey` 挪到 listen 归一化**之后**，并对空 `listen` 给出明确报错
  （原来空值会先被当成"对外地址"要求密钥，随后以 `missing port` 失败，信息错位）。
* **观察项 3** `reqlog` 加 `closed` 标志，`Close` 后 `enqueue` 直接丢弃（不再往死通道里塞）；
  同时把「配置上启用」与「当前还能写」拆成 `configured()` / `enabled()`，`Close` 之后
  `ReadArchive` 与 `Snapshot` 仍正常工作。
* **观察项 4（未改）** `LoadBinds` 的 SCAN+逐 key GET 在绑定数上百时可能超时并返回部分
  结果（启动期粘性恢复静默缺失）。改成 MGET 需要重写批处理与错误语义，留待下一轮。
* **观察项 1（未改）** `X-Domain` 头传的是完整 URL 还是纯域名需要实测上游行为，不盲改。

### 测试

`upstream 203 / server 90 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**685 通过 0 失败**。

顺带修了两处测试自身的问题：
- `TestCooldownHardDoesNotAdvanceSoftStreak` 原本断言"软冷却覆盖硬冷却"（正是 P0-3 的
  病灶），改成断言新的单调语义。
- `TestChatSessionDeadDisables` 原本断言"单次 12153 即禁用"，改成断言三振才禁用。
- `frontend_test.go` 的 node 探针改成真跑一次 `node --version`：Android 上 `LookPath`
  能找到但执行不了（私有目录里的二进制需 linker 代跑），此前 15 个前端测试全红。

## v1.26.1（当前）

**本版 = 第二份代码审查报告（CODE-REVIEW-2026-10-19.md）的落地修复。**
7 条高危全修、9 条中危全修、6 条低危全修。其中 **H1 是上一版我自己引入的真 bug**
（防重入标记写在值类型 map 的副本上，等于没写），本版改成指针 map 才真正生效。

### 高危（7/7）

* **H1 面板 OAuth 轮询的「认领」标记从未写回** —— `logins` 是 `map[string]loginSession`
  （**值类型**），`sess.consumed = true` 只改了栈上副本，defer 里复位同样写副本。
  上一版加的防重入因此形同虚设。本版把 map 改成 `map[string]*loginSession`，
  改的就是 map 里那一个（并发两路 poll 现在只有一路进完成路径）。
* **H2 `run.sh stop` 1 秒后无条件 `kill -9`** —— 服务端要 `Flush()` + `Shutdown(5s)`
  排空在途 SSE，1 秒就强杀会把最后一次落盘抢走（冷却时间/计数/用量丢一截）。
  改成最多等 8 秒，超时才强杀，并区分「优雅退出 / 强制终止」两种提示。
* **H3 「重启网关」不等旧进程退出** —— `destroy()` 是异步的，代码把「句柄清空」当成
  「进程已死」，新进程必然 `bind: address already in use` 退出，只能等看门狗 10s 一拍。
  新增 `restartGateway()`：先捕获旧句柄 → 杀掉 → **后台线程** `waitFor(4s)` → 再拉起
  （不阻塞主线程，避开 ANR 窗口）；`onStartCommand` 的 restart 分支改走它，且不再
  被后面的 `if (!alive())` 判成「外部实例」。
* **H4 面板首屏加载失败后永不重试** —— 原来固定 1500ms 硬加载，而 `onPageFinished`
  连 WebView 错误页都会回调 → 被记成「已注入」，之后再也不试。改成由 tick 里真实
  的端口探测驱动（`panelLoaded` 只在真落在面板地址上才置真），并补 `onReceivedError`
  把失败交回 tick 重试（上限 30 次）。
* **H5 Redis 异步写按 QPS 堆积 goroutine** —— 改成**有界队列 + 固定 worker 池**：
  入队成功即保证执行（`Close` 排空），队列满（1024）则丢弃并计数（`Dropped()`），
  关停后提交的直接丢弃。不再有「请求数 × 等待时间」个 goroutine。
* **H6 `login.sh` 路径穿越 + token 文件权限** —— uid 加白名单（`[A-Za-z0-9_-]`，
  长度 ≤64）后再拼路径；落盘改 `os.open(..., 0o600)` + tmp→rename，与 Go 侧
  `SaveAtomic` 的 0600 口径一致。
* **H7 默认「全网卡监听 + 空密钥」** —— 三道一起改：
  ① `Default().Listen` 从 `":7863"` 改成 **`127.0.0.1:7863`**（默认只给本机）；
  ② `normalize()` 新增 `checkListenKey()`：非回环监听 + 空/占位密钥（`test_key`/
  `CHANGE_ME`）**直接拒绝启动**并给出生成命令；
  ③ `config.example.json` 与 APK 内模板改成 `CHANGE_ME`，`GatewayService` 的
  随机 key 替换同步覆盖两个占位符；报错文案不再教用户 `cp config.example.json`。

### 中危（9/9）

* **M1** `rebuild.sh test` 透传 `go test` 的退出码（原来是 `tail` 的 `$?` + 无条件 `exit 0`）。
* **M2** `patch_source.sh` 第 3 步的锚点跟随 `$PREFIX`，并在替换后断言
  `wb2apiResolvConfPath()` 真的被调用（否则 exit 1）——自定义 PREFIX 下不再静默失效。
* **M3** 面板 `tick()` 加可见性闸门：`onStop` 停、`onStart` 恢复（`tickQueued` 防重复投递），
  后台不再每 2 秒发 socket connect。
* **M4** `ReviveJobService.onStartJob` 的网络探测与写日志挪到后台线程，返回 `true`
  由 `jobFinished` 收尾（主线程不再做磁盘 IO）。
* **M5** 会话粘性命中加续期节流（`minTouchInterval = 5s`）：5 秒内的重复命中不再拿写锁、
  不再打一次 Redis 写。
* **M6** `watchdog.sh` 外部实例分支重置 `fail=0`（否则外部实例一挂就按历史次数退避）。
* **M7** `apply_panel_mobile.sh` 的 `re.sub` 改用 `lambda m: block`（CSS 里的反斜杠
  不会再被当转义模板解释）。
* **M8** APK 降级路径的版本串正则改成通用的 `x.y.z`（任意位数），并支持
  `OLD_VERSION` 环境变量消歧 —— 升到 `2.0.0` / `1.100.0` 不会再中断构建。
* **M9** 签名口令不再硬编码：`WB2API_KS_PASS` 环境变量 → `$WS/.kspass`（已 gitignore）→ 报错。

### 低危（6/6）

* **L1** `versionCode` 由 `versionName` 派生（`major*10000+minor*100+patch`），不再手工同步。
* **L2** `BootReceiver` 去掉 `QUICKBOOT_POWERON`（非受保护广播，exported=true 下任何应用
  都能触发前台服务启动）。
* **L3** 去掉 `MIXED_CONTENT_ALWAYS_ALLOW`（面板只连 127.0.0.1）。
* **L4** `task_common.py` 的 `AUTHS` 改成 `WB2A_AUTH_DIR` → 脚本同级 `../auths` → 旧路径兜底。
* **L5** `tasksScanAll` 加 4 路并发闸门并尊重请求取消（不再按账号数无上限起 goroutine）。
* **L6** `restartRequiredFields` 改成**真差异**：只有值真的变了才提示需要重启
  （此前恒返回全量清单，改个热生效字段也喊重启，狼来了）。

### 测试

`upstream 203 / server 89 / pool 184 / panel 24 / session 20 / redisstore 6 / reqlog 7 /
cmd-server 全绿`，0 FAIL。
配置默认值变更导致 3 个断言同步更新（`listen` 默认值、`WriteDefault` 生成值、
目录错误文案）。

## v1.26.0（当前）

**本版 = 第三方代码审查报告（CODE-REVIEW.md）的落地修复。** 4 条高危全修，10 条中危修 10，
9 条低危修 8（L1 保留，见下），并新增 6 个回归测试。

### 高优先级（全部修复）

* **H1 非流式把上游 error 帧吃成「假 200 成功」**（`internal/upstream/sse.go`、`internal/server/handler.go`）
  * `Aggregate` 现在识别 `{"error":...}` 帧并返回 `*upstream.ErrorFrame`，不再合成
    `content:""` + `finish_reason:"stop"` 的空成功响应。
  * handler 用 `FrameKind` 分类后走与流式路径**同一套**账号处置：6004 冷却、内容审核 400、
    11158 等级负缓存；请求级错误（内容拦截/上下文超限/参数非法/图片无效）直接终止轮转，
    不再伪装成 503「服务端过载」让客户端无限重试。
  * 后果链被切断：此前一个正在限流的号会被 `NoteSuccess` 洗成健康号、11102 负缓存被清、
    粘性把会话钉死在它身上，后续每轮继续失败。
* **H2 Activity 销毁后 tick 仍在跑 → `RejectedExecutionException` 崩掉整个进程**（`MainActivity.java`）
  * `onDestroy` 补 `ui.removeCallbacksAndMessages(null)`（先停自投递，再关线程池），
    补 `web.destroy()`（WebView 持有 Activity，不 destroy 就永久泄漏）。
  * `probePort()` 的 `probe.execute()` 包 try/catch：线程池已关时吞掉，绝不让主线程崩
    —— 主线程崩会把同进程的 `GatewayService` 一起带走。
* **H3 `startGateway()` 无互斥 → 两个 Go 进程、孤儿占着 7863**（`GatewayService.java`）
  * `startGateway()` / `killProc()` 改 `synchronized`，入口加 `if (alive()) return;` 二次检查；
    `selfHeal` 退避醒来后再查一次 `alive()`。三条路一起堵住「主线程 + 看门狗同时拉起」，
    也就堵住了「P1 变孤儿 → 界面永久显示『运行中（外部）』→ 点停止杀不掉」。
* **H4 成本账本把「上游没给 credit」记成「免费」，6 小时把该模型流量压到一个号**（`handler.go`）
  * `usageCreditTotal` 增加存在性校验（`usage["credit"]` 不存在或非数字 → `ok=false`），
    与流式路径的 `*float64` 判存在性口径对齐。

### 中优先级（10/10）

* **M1** 一键完成全部：HTTP 超时 5min → **15min**（mp 9 条 × ~50s ≈ 8min + expert_5 等，
  原来必然 504，且超时后后台还持着 per-account 锁 → 重试全撞 409）。
* **M2** `StreamHint` 区分「写客户端失败」与「读上游失败」：新增 `errClientWrite` /
  `errUpstreamRead` 两个哨兵。上游中途 RST 现在**补一帧 error + [DONE] 收尾**再返回，
  客户端不再拿到悬挂流；handler 不再把上游故障记成「人走了」，改为 `NoteFailures` + 502 观测。
* **M3** `patch_source.sh` 加 `set -eu` 并检查 rt0 补丁的 python 退出码（此前锚点未命中被静默吞掉，
  之后编出的二进制 argv 错位、启动即退且无提示）。
* **M4** `build_apk.sh` 的 GOROOT 源码替换改为 `trap ... EXIT INT TERM HUP` 还原（此前中途打断
  会把 GOROOT 永久停在「未打 rt0 补丁」状态）；缺 `.orig-dsh` 时前置报错。
* **M5** 复用上一版 `classes.dex` 前校验 `apk-src/java/**` 指纹（`.java-src-hash`），
  源码改过就**直接失败**，不再产出「新版本号 + 旧 Java 代码」。
* **M6** 面板请求体上限：`ServeHTTP` 统一 `MaxBytesReader(32MB)`，JSON 接口再收紧到 1MB
  （`limitJSONBody`），导入文件 `io.LimitReader` + 413。
* **M7** 单任务入口与「一键全部」共用 `taskDone()`（已领取 **或** 已达标未领都跳过），
  不再在「已达标未领」时把整条动作链重跑一遍。
* **M8** `models()` 的 `hiddenTier` 现在真的自增；全部模型被等级闸门隐藏时返回
  **200 + 空列表 + 说明**，而不是误导性的 503「请先添加账号」。
* **M9** 重新注册 `GET/POST /panel/api/tasks/skip`（跳过名单 + 尝试型开关的唯一入口），
  并把 409 文案里「任务中心勾选」改成实际的 API 调用方式。
* **M10** `loginPoll` 加 `consumed` 认领标记：并发轮询不再重复执行完成路径
  （`Pool.Add` / `SaveAtomic` / 签到 / 余额刷新各跑两遍）。

### 低优先级（8/9）

* **L2** `function.name` 为空串不再占住「首片已出」名额（否则真名会被后续分片删掉）。
* **L3** `repairFrame` 在分配合成 `tool_calls` index 之前先并入本帧的真实 index，
  避免合成调用与真实调用撞 index 被当成「同 index 后续分片」。
* **L4** SSE 接受 `data:{...}`（冒号后无空格）。多行 `data:` 合并仍按行处理（见「已知限制」）。
* **L5** `ensureConfig` 读 assets 循环到 EOF（此前单次 `read()` 超 8KB 就截成半截 JSON）。
* **L6** `push_github.sh` 建仓探测的 `curl` 加容错（`set -eu` 下网络抖动会无自述地中断）。
* **L7** `probe_active.py` 读 auth 文件显式 `encoding="utf-8"`。
* **L8** `task_common.py` 兜住 `{"data": null}`（新号无连登数据）。
* **L9** `ReenableIfCredits` 的注释从「调用方必须已持锁」改成「本函数自己取锁，别持锁调用」
  ——原注释会诱导后人写出自死锁代码。

### 已知限制（本版未改）

* **L1** 上游吐若干帧后直接断连（无 `finish_reason`、无 `[DONE]`）时，正文的 `finish_reason`
  仍是 `stop`。OpenAI 协议里没有「半截」这个语义，强行改成 `length` 是撒谎；本版只保证
  **客户端能看出是上游断流**（error 帧 `upstream_read_error`），不改写正文的收尾原因。
* **L4 的多行 `data:`**：一个事件里多条 `data:` 行仍未按空行合并（按行当独立事件）。
  上游实测未出现该形态；如出现需把读循环改成「按空行分帧」。

### 打包

* `build_apk.sh` 新增 **无 aapt2 降级路径**：探测 `aapt2 version` 失败时，复用上一版 APK 的
  清单与资源，只重编 `classes.dex` 与 `.so`，并把 AXML 字符串池里的版本串**等长就地改写**。
* `build_apk.sh` 的 `R8`/`SIGNER` 支持双路径探测（私有工具目录 → 系统包目录）。
* `javac` 的 `gen/.../R.java` 改为按存在与否决定（降级路径下没有它，Java 源码只引用 `android.R.*`）。

### 测试

新增回归：`internal/upstream/sse_errorframe_test.go`（5 个：error 帧不再假成功、正常流不受影响、
`data:` 无空格、上游断流补 error+[DONE]、客户端写失败可识别）、
`internal/server/usagecredit_test.go`（1 个：credit 缺失/为 null/为字符串都不记账）。

全绿：upstream **203**、server **90**、pool **184**、panel **24**（+15 node 依赖 SKIP），0 FAIL。

## v1.25.0

* **rc=141（SIGPIPE）根治完成 —— Java 侧修复这次真正编进 dex 了**：
  * `GatewayService.pump()`：超限后只把管道**读空**、不再落盘、**绝不关读端**（循环只在 EOF 结束）；
    日志轮转阈值 4MB → **2MB**（与 pump 上限对齐，彻底消灭 2~4MB 死区）；
    `exitMeaning` 补 `rc=141` 人话解释。
  * Go 侧 `signal.Notify(SIGPIPE)` 兜底继续生效（管道被关也只返回 EPIPE，进程不死）。
  * 验证：新 `classes.dex` = 43,616 字节，内含 `rc=141` / `EPIPE` / `SIGPIPE` / `LOG_PUMP_MAX`；
    APK 内 `versionName` 已改为 **1.25.0**（AXML 字符串池按 UTF-16LE 原位同长度替换）。

## v1.24.9

* **修「看门狗拉不起网关，rc=141（信号 13 = SIGPIPE）」**
  * **根因（代码级）**：`GatewayService.pump()` 把网关 stdout 灌进 `server.log` 时，写满 **2MB
    就 `break`** 跳出 try-with-resources → BufferedReader 被关闭 → **管道读端消失**；而 Go 对
    stdout/stderr 写 EPIPE 是**直接以 SIGPIPE 退出** → exit 141。更毒的是重启路径：`bytes` 从
    `logFile.length()` 起算，文件早已 >2MB → **第一行就 break** → 每次拉起几秒内必死；而日志
    轮转要到 **4MB** 才做 → **2~4MB 区间就是"怎么拉都 rc141"**。
  * **本版已生效（进 APK 的 `.so`）**：`cmd/server/main.go` 用 `signal.Notify(SIGPIPE)` 接管该
    信号 —— 管道被关也只返回 EPIPE、进程继续跑（日志丢了也不能让服务死）。**实测**：把网关
    stdout 接到"读 1 字节就关"的管道（老代码必死），进程 **5s / 8s 后仍存活** ✓✓。
  * **源码已修、待随下一版编进 dex**：`pump()` 超限后只把管道**读空**、不再落盘、**绝不关读端**
    （循环只在 EOF 结束）；日志轮转阈值 4MB → **2MB**；`exitMeaning` 补 `rc=141` 人话解释。
    本版采用**零编译打包**：直接替换 APK 内的 `.so` 后重签，`classes.dex` 未变。
  * APK 内 `versionName` 仍显示上一版（AXML 字符串池未改写），release 与功能都是 v1.24.9。

## v1.24.8

* **修「换境外代理后 CN 模型报 503 no_healthy_account」的误诊**（用户实测：hint 里明明
  `healthy=3`、`realm_healthy=1`，却报"没有可用号"）。真相是**传输层失败**：号选出来了，
  但请求连不上上游（`copilot.tencent.com` / `codebuddy.cn` 是境内服务，境外出口连不上），
  网关一个 HTTP 响应都没拿到，末端于是退回默认的 `no_healthy_account` —— 语义完全错，
  把人引去查账号。
  * 新增末端分支：`code == no_healthy_account && lastErr != nil && 非上游 *Error` →
    **502 + `upstream_unreachable`**；`message` 带上游 base 与实际错误（折叠空白 + 截断），
    `gateway_hint` 明确写「cn 上游是境内服务：换了代理/VPN 出口就用境内出口跑 cn 模型，
    或改用 `global:` 模型配 global 号」，并保留池子实况。
  * 顺带把 hint 里的 `realm_candidates` 改名 `realm_healthy`（它没算积分保底，不能宣称"候选"）。
  * 测试：新增 `internal/server/transport_err_test.go`（域→base 映射、传输层文案折叠/截断）；
    `TestChatTransportErrorDoesNotPenalize` 断言更新为 502 + `upstream_unreachable` + 文案含
    上游 base。全绿：server **89 PASS**、pool 184、upstream 198、panel 24。

## v1.24.7

* **修「`global:` 模型列表里留着、一发就 503」**（用户贴出的自解释 hint 直接定位）：
  hint 显示 `healthy=3, model_cooled(gpt-6-astra)=2, realm=global | 第三个号 ok` ——
  两个 **global 号**早被判定"等级不支持该模型"，而池里还有一个 **CN 号**等级未知，于是
  v1.24.4 的「全池」判据放过了它；可 `global:` 前缀的模型**永远选不到 CN 号**（选号带
  realm 谓词），客户端挑中必然失败。
  * 等级闸门改为**分域判定**：新增 `ModelDeniedForAllAccountsInRealm(model, realm)` ——
    CN 条目只看 CN 号、global 条目只看 global 号；该域没有账号 → 不下结论（保守不变）。
    `/v1/models` 与面板模型页两条路径都改用它。
  * 503 hint 增加域信息：逐号标出 `uid/realm`，并给出 `realm_candidates=N`；该域一个候选
    都没有时明确写 `no candidate in realm global (cross-realm accounts are not selectable for it)`。
  * 测试：`internal/pool/modeltier_realm_test.go`（global 域两号已知不支持 → 隐藏；CN 域
    等级未知 → 不隐藏；无账号的域 → 不下结论；两域事实互不影响）。
    全绿：pool **184 PASS**、upstream 198、server 87、panel 24。
  * 实测：临时实例打 `global:gpt-6-astra`，hint 中出现
    `realm_candidates=0; no candidate in realm global (cross-realm accounts are not selectable for it)` ✓。

## v1.24.6

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
  `files/gateway/resolv.conf` 传进去的。曾出现过源码里「读该环境变量」的
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
* **APK 打包降级**：`android.jar` 缺失时，aapt2 的框架表改用系统 `framework-res.apk`、
  **复用上一版 `classes.dex`**（Java 源码零改动），并跳过 javac/d8 与看门狗自检。副作用：APK 的
  `compileSdkVersion` 元数据记为 36（跟着设备框架）而非 35，其余（minSdk 24 / targetSdk 34）不变。
  ⚠ 改 `apk-src/java/**` 之前必须先补回 `android.jar`（脚本已用源码指纹挡住这种误用）。
* **Go 工具链 rt0 补丁修复**（不影响产物）：工具链重装后丢了 rt0 补丁，`go` 全线报
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
  为了不依赖 PIL/ImageMagick，自写了纯标准库缩图器 `patches/make_icons.py`
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
  细节见 `AGENT.md`。
