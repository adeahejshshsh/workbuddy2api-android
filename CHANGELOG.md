# 变更记录

## v1.36.0（当前）

**本版 = 从 kk-cao 的 fork 里拣回 2 条（上游 2026-10-21 停更后它又推了 7 个提交）。**

### 已合并

* **业务码归一（关键修复）** —— kk-cao `ba71bf25` 的核心。上游这条错误路径实际发的是
  `{"code":11‑-128,...}`：**数字里带连字符，不是合法 JSON**；日志/终端又把它渲染成
  `11‑-128`。我们 v1.31.0 的通道校验检测用的是字面子串比较 + 要求合法 JSON，
  **在线上从未生效过**（真实报文一律落 `ErrAccountFault` → 健康账号被硬禁用）。
  - 新增 `normalizeBusinessCode`（只保留数字），三种写法归一到同一个键；
  - `hasBusinessCode` 改为「JSON 结构遍历（归一比较）+ 原文正则兜底」
    （`"code"\s*:\s*"?([0-9][0-9\x{2011}-]*)"?`）；
  - `isChannelRefused` 改用归一比较；
  - `IsAccountRestricted` 补**排除软限流文案** —— 11140 也承载模型级限流
    （`The model provider is rate-limiting requests.`），那时账号健康，
    误禁用的代价远高于一次短冷却。
* **11102 负缓存 UI** —— kk-cao `b7bcfb9f`。`model_unavailable` 是确定性负缓存
  （6h 起、指数退避），重试无意义；此前显示「预计 X 后重试」既误导又刷屏
  （一次目录差异能堆二十多行）。改为「该账号无此模型（不重试）」+ 多条折叠成一条汇总，
  模型名放 title。

### 未合并（需要决策或纯 UI）

| kk-cao 提交 | 内容 | 判断 |
|---|---|---|
| `ba71bf25` 二 | 出站指纹对齐官方安装包：补 `X-Private-Data: true`、UA 版本 CN 5.8.3 / intl 5.8.4、CLI 2.161.0，**平台段不再按 realm 切换**（intl 也用字面量 `WorkBuddy`） | **建议做**，但与现有注释「intl 用 `WorkBuddy AI`」冲突（我们当初的逆向结论），属指纹策略变更，等确认 |
| `9a4a4a87` | global 目录探测统一 desktop UA，CN 保留 IDE 探测 | 与上条同族，一起做 |
| `aa452c55` | 目录探测 UA 版本号跟到官方当前版 | 同上 |
| `c515ae1c` | 面板可查看/修改目录探测 UA | 依赖上条，纯 UI |
| `cbefb1bb` | 客户端版本字段改「左 CN / 右国际版」成对排列 | 纯 UI |
| `9214bf72` | `rateLimitMeta` 未声明 `detail` 导致账号池表格空白 | **不需要** —— 我们 v1.29.0 起就是 `let detail;` 声明好的（`app.js:311`） |

### 测试

新增/改写 `TestClassifyChannelRefusedVsAccountRestricted`（真实报文形态 → `ErrChannelRefused`、
渲染形态也命中、11140 不误判成通道、内容审核变体仍归 `ErrContentBlocked`、
带限流文案的 11140 不算账号限制）与 `TestNormalizeBusinessCode`（三种写法归一）。
`server 97 / upstream 229 / panel 54 / pool 188 / scheduler 65 / auth 29 /
session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52`，**768 通过 / 0 失败**。

## v1.35.0

**本版 = 第二十八份审查报告（CODE-REVIEW-bugs.md，23 条）：22 条为真、1 条误报。
真条目里 #1/#4/#5/#7/#12/#13/#15/#16/#17/#18/#19/#20 都与我最近几版的改动直接相关。**

### 误报

* **#2「导入与登录对 global 号的判定依据不一致」** —— 不成立。v1.29.1 起登录侧写的是
  `auth2.ResolveRealm(realmIfExplicit(realm), tok.Domain)`：**未显式给 --realm 时同样按
  domain 推断**，与导入侧 `auth.ResolveRealm("", acc.Domain)` 完全同口径。
  唯一差异是「cockpit 导入无法表达用户显式指定的 realm」，而导出文件里本就没有 realm
  字段 —— 属能力边界，不是缺陷。

### 配置与装配

* **#1 `dns_servers` 永不生效**（我 v1.32.0 引入）—— `up := upstream.New()` 在 149 行已构造
  Dialer，`SetDNSServers` 到 178 行才调。已把注入移到 `New()` 之前，注释与实现一致。

### upstream

* **#3 顶层数组目录被判失败** —— 信封解析先 `Unmarshal` 成 map，数组直接报错，让
  `resolveGlobalModelsArray` 的数组分支不可达。改为先看首字符是否为 `[`，是则跳过信封。
* **#4 `isChannelRefused` 三个字面量完全相同** —— 三行等价于一行，ASCII 变体永远匹配不到。
  去重成「U+2011 + ASCII」两种真实写法。
* **#5 `X-Root-Request-ID` 未过 `HeaderSafe`** —— 与同函数另外两个会话头不同口径，
  原值含 CR/LF 时在写请求阶段才报错。已补。
* **#6 `doJSON` 把「200 + 空 body」判成 parse failed** —— `/v2/report`、`appearance/set`、
  `claim-gift`、`claim-compensation` 成功回空体时被记成失败。空体按成功返回 `nil, nil`。
* **#7 `dropReasoningContentParts` 写出 `"content": []`**（我 v1.30.0 移植）—— 带 `tool_calls`
  的消息被写成空数组。改为**删除 content 键**。
* **#8 `model.json` 每次请求重写** —— `Output == 0` 时仍 `modelCatalogPut`，而该模型永远过不了
  第 3 级（要求 `MaxOutputTokens > 0`）→ `/v1/models` 每次请求落一次盘。只在 `Output > 0` 时回写。
* **#9 `repairFrame` 合成帧失败回落整帧原文** —— 多帧拆分后任一帧 marshal 失败会把原始 payload
  再发一遍（客户端收到重复内容）。合成帧改传空 fallback = 跳过该帧。
* **#10 `sanitizeFeatures` 有 3 条完全重复** —— `x-anthropic-hdr` ×2、`11‑-128` ×2。已去重。

### server

* **#11 `/v1/models` 负缓存对并发失效** —— 锁内二次检查只看新鲜缓存、不看 `lastFail`，
  上游失败后排队在锁上的 N 个请求会逐个再打一次。锁内补负缓存判断。
* **#12 单任务回读失败仍回 `ok:true`**（我 v1.28.5 改动附近）—— `progress_after=""` 与
  `"3/5"` 不等，前端判成「有进展」弹绿色。后端补 `progress_unknown`，前端优先认它。
* **#13 池实况串被 hint 覆盖** —— `hintOf` 整段替换，前面拼好的
  `[pool: total=… healthy=…]` 丢失。改为「具体原因 + 池实况」拼接。

### panel

* **#14 `panelModelEntry` 输出 `"supported_efforts": null`** —— 与 `/v1/models` 的「省略字段」
  不一致。改为空值不写，未命中静态表时用 mi 侧非空值兜底。
* **#15 `taskByCode` 大小写敏感** —— 与 `autoActionFor` 的 `EqualFold` 分叉：上游改大小写时
  单任务入口判「没有此任务」而队列扫描仍能命中。已统一 `EqualFold`。
* **#16 `autoActionIndex` 大小写敏感** —— 同上，导致排序时该码恒落 `1<<20`（排最后）。已统一。
* **#17 备注弹窗不回填**（我 v1.32.0 引入）—— 三元两边都是 `''`，改备注等于盲写。
  按钮加 `data-remark`，初值从它取。
* **#18 前端 `AUTO_TASKS` 精确匹配** —— 与后端忽略大小写分叉，上游改大小写后按钮永远渲染成
  「接受」。新增 `autoTaskLabel()` 做忽略大小写查表。
* **#19 切「自定义」不重置区间**（我 v1.29.1 附近）—— `!st.from || !st.to` 恒假（初始化时已赋值），
  注释承诺的重置从不生效。改为「从非 custom 切到 custom 时重置」。
* **#20 配置页缺 10 个键** —— 补齐 `blackcat_hours/enabled`、`growth_hours/enabled`、
  `streak_hours/enabled`、`request_archive_enabled/days` 的**表单控件 + CFG_MAP**
  （`TestConfigFormMatchesCFGMap` 会强制两者同步）。`request_archive_max_mb`、
  `server.read_timeout`、`upstream.dns_servers` 因是数值上限/时长串/数组，表单无法安全表达，
  保留手工编辑并在页面提示里写清。

### apk-src（Java）

* **#21 `panelLoading` 永不复位** —— 只在 `onPageFinished`/`onReceivedError` 复位，
  渲染进程崩溃或页面被回收时两个回调都不来 → 之后不再自动加载。加 `panelLoadAt` 时间戳，
  超过 30s 视为「在途但已死」。
* **#22 通知把「已停止」显示成「启动中」** —— 改为三态（运行中 / 已停止 / 启动中）。
* **#23 `appLog` 无同步** —— 四处并发调用，`f.length()` 判断与 `renameTo` 之间无互斥会重复翻代
  并丢日志。方法加 `synchronized`。

### 脚本与容器

* **#24 `entrypoint.sh` 无 python3 时 key 随重启变化** —— 进入该分支的前提正是「配置里没有
  非空 api_key」，而 sed 只做替换、键缺失时什么都不改 → 校验失败 → 每次退回 env 注入。
  改为「键存在则替换，不存在则插入」。
* **#25 `run.sh` 两次调用 `running_pid`** —— 两次之间进程退出会打印空 pid。只取一次。

### 测试

`server 96 / upstream 228 / panel 54 / pool 188 / scheduler 65 / auth 29 /
session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52`，
**766 通过 / 0 失败**；`app.js` 通过 `node --check`；`run.sh` 与 `entrypoint.sh` 通过 `bash -n`。

## v1.34.0

**本版 = 第二十七份审查报告（审查报告.md，8 条）：6 条为真、2 条误报。
真条目里 #4/#5/#8 是我最近几版引入或改了一半的。**

### 误报（附证据）

* **#1「三条指纹改写规则是空操作」** —— 不成立（这是该说法第三次出现）。用编译后的二进制
  打出每条 `sanitizeRewrites` 的**首个不同字节位置**：
  `pair0 lenL=56→lenR=61 firstDiff=46`（`l CLI for Claude` → `l CLI tool for C`）、
  `pair1 firstDiff=0`（`Main branc` → `Default br`）、
  `pair2 firstDiff=47`（`ex CLI, a termin` → `ex CLI tool, a t`）、
  `pair3 firstDiff=3`（`To give feedb` → `To provide fe`）—— 四条都在真改。
  报告把常量（= 被替换的**原文**）读成了「已经改好的形态」，是渲染/阅读错位。
* **#3「非 data 行不计有效帧导致误报空流」** —— 不该改。心跳/注释不是内容；
  整条流只有 `: ping` 时按空流处理（补 error 帧 + 502）是**刻意行为**，
  `TestStreamEmptyFramesCase/只有注释行` 钉住了它。我按报告改过一次，测试当场变红，
  已撤回并把「为什么不计」写进注释。

### 真条目

* **#2 夜猫子 target=0 被当成已达标** —— `0 >= 0` 成立 → `BlackcatNeed` 返回 0 →
  `runBlackCat` 回「进度已达标，无需补足」，一次对话都不发。按 `runMPMiniChatTask`
  的口径补 `target<=0 → 1`，**并且返回值改用归一化后的 target**（第一版只改判据、
  返回值仍用 `t.Target`，等于没修 —— 自己抓到并改正）。
* **#4 非流式缓存命中率恒 100%**（我 v1.31.0 引入）—— `cacheTokensFromUsage` 在
  `okM==false` 时把 `m` 的零值当 miss=0 上报 → `hit/(hit+0)=100%`，缓存告警永不触发。
  改为：miss 缺失时按 `prompt-hit` 推导（与流式侧同口径）；**推不出来就 `ok=false`**
  （不参与命中率统计，宁可不统计也不报假 100%）。
* **#5 `inReqRange` 忽略预设**（我 v1.29.1 引入）—— 预设模式不回写 `st.from/st.to`，
  只读 from/to 会让「近 7 天 / 全部历史」永远按初始的「今天 00:00 → 现在」筛；
  先选过自定义再切预设，旧区间还会继续生效。改为按预设现算窗口，与 `trangeQuery` 同口径。
* **#6 冷却标签只认 `hard_credit`** —— `cool_remaining_sec` 已是三个截止的最大值，
  再拿 `bl`/`dg` 去比恒为假 → 熔断/降权永远显示「限流冷却」。改为按后端
  `cool_kind` 直接映射（hard_credit/breaker/degraded/soft_rate）。
* **#7 日志高亮正则缺 `i`** —— 后端日志前缀是 `WARN:` / `ERR:`（大写），
  两个正则都没有 `i` 标志 → 所有 WARN/ERR 行按普通行渲染。已加 `i`。
* **#8 `credits_ok` 只写不读**（我 v1.28.2 引入）—— 余额查询失败时后端不下发 `credits`
  只给 `credits_ok:false`，前端判 `r.credits >= 0`（undefined >= 0 为 false）→
  「· 积分 N」整段消失，用户分不清「积分 0」与「查询失败」。改为三态：
  查询失败显示「· 积分查询失败」，有值显示积分，未知不显示。

### 测试

新增 `TestCacheTokensFromUsageMissDerivation`（hit+miss / hit+prompt 推导 /
推不出时 ok=false / miss 反推 hit 四态）。
`server 97 / upstream 228 / panel 54 / pool 188 / scheduler 65 / auth 29 /
session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52`，
**767 通过 / 0 失败**（42 秒）；`app.js` 通过 `node --check`。

## v1.33.0

**本版 = 合并上游 `#196 补偿先查后领` + `#205 任务补领清扫`（同一族，一起改省得动两次状态机）。**

### #196 补偿「先查后领」

* 新增 `upstream.CompensationStatus(a)`：`POST /billing/meter/compensation-status`，
  解析 `active` / `claimed` / `credit_num` / `end_time`（`end_time` 缺失回落 `validity_period`）。
  `active=false` 是「当前无补偿活动」的正常 200，**不算错误**。
* 连登管家改为「查 → 判 → 领」三态：
  - 查询失败 → **显式 WARN**（旧实现把「没有 / 已领 / 领取失败」压成同一个静默错误，
    带截止日的补偿会**静默过期**）；
  - 有活动且未领 → 领取，日志带上截止时间与可领积分；
  - 已领 / 无活动 → 静默跳过（正常态）。

### #205 任务补领清扫

* 新增 `Panel.ClaimSweepOnce(ctx)`：并发 3 扫全池，同时拉**默认任务列表 + 小程序任务列表**，
  对每个「已达标未领」（`Claimable && !Claimed`）的任务按口径补领
  （mp 码走 `ClaimRewardMP`，其余走 `ClaimReward`），按 code 去重。
* 门控：`global` 与企业上下文（`IsEnterpriseSession`）直接跳过 —— 上游对这两类没有成长体系。
* 新增接口 `POST /panel/api/tasks/claim_sweep`，返回 `{accounts, claimed, failed}`；
  任务族互斥复用调度器的锁（`TryBeginTask("claim_sweep")`），重复触发回 409。
* 与 #204 的分工：#204 是「执行动作前先补领一次」，#205 是**独立清扫** ——
  任务进度异步计分，达标那一刻往往没人点按钮，奖励会挂到下次执行同一任务（可能几天后）。

### 本轮仍未合并

| 上游 PR | 内容 | 说明 |
|---|---|---|
| **#158** | 按地区独立配置轮询/顺序耗尽调度 | ~300 行调度器 + 池策略重构 |
| **#213** | 联网搜索端点 `POST /v1/search/messages` | 新客户端 + 新路由 + 记账 |

### 测试

`server 96 / upstream 228 / panel 54 / pool 188 / scheduler 65 / auth 29 /
session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52`，
**766 通过 / 0 失败**（44 秒）。

## v1.32.0

**本版 = 继续合并上游能力（2 条）：`#144 dns_servers`、`#178 账号备注`。**
每条都先加判据/字段、再接行为、最后全量测试。

### #144 出站 DNS 可指定

* 新增 `upstream.dns_servers` 配置（`["1.1.1.1", "223.5.5.5"]`，空 = 系统默认）。
* 实现：`upstream.SetDNSServers()` 注入 → `newDialer()` 挂自定义 `net.Resolver`
  （`PreferGo: true`）。解析用的拨号器与出站拨号器**分开**：`Resolver.Dial` 若复用
  出站 dialer 会形成递归（解析目标地址本身又要解析）；DNS 服务器按 IP 给，只建 UDP 连接。
* 多个服务器按序尝试，全部失败才报错（错误里保留最后一个）。
* 已进重启清单（Dialer 在启动时构造）。

### #178 账号备注

* `pool.entry` 新增 `remark` 字段：**纯本地标注**，不参与选号/路由/上游请求；
  持久化在 `state.json`（`stateAccount.remark`，omitempty），跨重启保留。
* `Pool.SetRemark(uid, text)` + `Status.Remark` 透出（面板/状态接口可见）。
* 面板 API：`POST /panel/api/accounts/{uid}/remark`（body `{"remark":"…"}`，空串=清除）。
* 面板 UI：账号行显示「备注：…」（强调色），行内新增「备注」按钮 —— 用
  `data-uid` + **委托监听**实现（严格 CSP 禁内联 `onclick`，用属性绑定会被拦）。
  点击弹 `prompt` 输入，保存后刷新账号列表。

### 本轮仍未合并

| 上游 PR | 内容 | 说明 |
|---|---|---|
| **#196** | 补偿先查后领 + 倒计时 | 需要新增补偿状态查询端点封装与面板入口 |
| **#205** | 任务补领清扫 | 周期幂等清扫链路（与 #204 的单次补领不同） |
| **#158** | 按地区独立配置轮询/顺序耗尽调度 | ~300 行调度器 + 池策略重构 |
| **#213** | 联网搜索端点 `POST /v1/search/messages` | 新客户端 + 新路由 + 记账 |

### 测试

`server 96 / upstream 228 / panel 54 / pool 188 / scheduler 65 / auth 29 /
session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52`，
**766 通过 / 0 失败**（46 秒）；`app.js` 通过 `node --check` 语法校验。

## v1.31.0

**本版 = 继续合并上游能力（P1 全部 + P2/P3 的一部分，共 6 条），每条都带判据与测试。**
上游已于 2026-10-21 停止维护，此后无上游更新。

### P1（正确性，防误罚号）

* **#233 `11‑-128` 通道校验分流** —— 新增 `ErrChannelRefused`：`11‑-128` +
  `request illegal` 是「上游认为请求不是官方客户端发出的」，**账号本身健康**
  （官方客户端用同一账号正常），此前会落 `ErrAccountFault` 并把**健康账号硬禁用**。
  判据必须 **code + 文案双条件**：`11‑-128` 还承载内容审核变体
  （`blocked by security policy`，归 `ErrContentBlocked`），按 code 一刀切会误判。
  同时把 `11140` 的判定从「只看 `request illegal` 文案」收紧成 `IsAccountRestricted`
  （code 11140 + 文案），并在 `applyErrorPolicy` 里对「有文案但 code 不是 11140」直接
  不罚账号。末端映射 400 `channel_refused`。
* **#162 企业切换会话门控** —— 新增 `auth.IsEnterpriseSession()`：账号是企业版，或
  AccessToken 的 JWT payload 带 `token_source=enterprise_switch`（浏览器在企业空间下
  完成 OAuth 时签发）。这类会话的上游行为是「**读得见、记不上账**」：任务列表/额度查询
  正常，但 `/v2/report` 返回 200 却被**静默丢弃**，任务进度永不跳动、连登/签到无效。
  已门控调度器的行为类任务（活跃上报 / 连登 / 旅行 / 夜猫子）与面板「一键完成全部」；
  **保活、余额刷新、签到、任务列表扫描不门控**（它们有显式错误或读端点可用）。

### P2

* **#225 `models.hidden` 模型黑名单** —— 新增 `models.hidden` 配置：裸名=两域都藏，
  `"cn:x"`/`"global:x"`=只藏该域。生效点：`/v1/models` 列表过滤 + chat **点名拒绝**
  （400 `model_unavailable`，不换号不罚号）。改动需重启（Handler 构造时快照）。
* **#220 `cn.chat_base` / `cn.billing_base`** —— 国内版上游 base 可覆盖（此前只有 global 段），
  空则回落内置默认；已加入重启清单。

### P3

* **#223 出站代理走环境变量** —— `newTransport` 加 `Proxy: http.ProxyFromEnvironment`
  （`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`）；未设时返回 nil = 直连，行为零变化。
* **#230 Dockerfile 构建源** —— 加 `ENV GOPROXY=https://goproxy.cn,direct`。

### 本轮未合并（下一批）

| 上游 PR | 内容 | 状态 |
|---|---|---|
| **#196** | 补偿**先查后领** + 倒计时 | 未做（我们有裸 `ClaimCompensation`） |
| **#205** | 任务补领清扫（周期幂等补「已达标未领」） | 未做 |
| **#178** | 账号备注 + 到期卡片折叠 | 未做（要加 pool entry 字段 + 持久化 + 面板） |
| **#144** | `dns_servers` 可指定 | 未做 |
| **#158** | 按地区独立配置轮询/顺序耗尽调度 | 未做（~300 行，调度器重构） |
| **#213** | 联网搜索端点 `POST /v1/search/messages` | 未做（新功能） |
| **#232** | 裸模型名跨域回退 | **不做**（与我们 `400 realm_required` 方案冲突，会双重解释） |

### 测试

新增 `TestClassifyChannelRefusedVsAccountRestricted`：`11‑-128` → `ErrChannelRefused`、
`11140` 不误判成通道、`11‑-128` 的内容审核变体仍归 `ErrContentBlocked`、
只有文案缺 code 时不算账号限制。
全量 **766 通过 / 0 失败**（server 96 / upstream 228 / panel 54 / pool 188 /
scheduler 65 / auth 29 / session 21 / reqlog 10 / usage 16 / redisstore 7 / cmd-server 52）。

## v1.30.0

**本版 = 合并上游能力（6 条）+ 协议改为「保留所有权利」+ 上游免责声明全部搬入 NOTICE.md。**
上游已于 2026-10-21 声明停止维护，此后不再有上游更新；本次按优先级挑「不修就会掉号/必失败」
的条目先合，其余在下方「未合并」里列明。

### 一、合并的上游能力

| 上游 PR | 内容 | 落地位置 |
|---|---|---|
| **#202** | 软冷却到期后不再透出过期 `reason` | `pool/state.go`：只有 `Cooling` 或 `Disabled` 时才回填 reason。此前面板会出现「可用」与历史「429 rate limit」并排 |
| **#203** | 模型级 6004 **双时钟** | `panel/app.js` `rateLimitMeta`：`until < resetAt` 时主文字同时明示「网关 X 起重试」与「上游称 Y 恢复」，倒计时跟随 until（与实际掉框一致） |
| **#199** | 缓存命中率**对称推导** | 流式 `CacheTokens` 与非流式新增 `cacheTokensFromUsage`：hit 为 0/缺失但 miss 可信时按 `prompt-miss` 反推（上游「接近全命中时回 hit=0 + miss=3」不再被误判成整段未命中）；新增 `upstream.UsagePromptTokens` |
| **#159** | 剥离 `reasoning_text` 片段 | `upstream/payload.go` 新增 `dropReasoningContentParts`，接在主改写管线里。Codex 经 responses→chat 转换会把思考片段透传进来，上游 content 白名单不含它 → 400 11101 |
| **#185** | `6021 queue.full` 渠道级满载 | 新增 `ErrUpstreamBusy`：`Classify` 在 429 兜底**之前**判 `queue.full`；末端映射 503 `upstream_busy`；`applyErrorPolicy` 零动作（不冷却/不熔断）。此前按限流处置会逐号软冷却，实测 2 秒绞杀整池 |
| **#217** | 同账号 refresh **单飞** | `Client.RefreshToken` 按 UID 分桶单飞，并发调用共享首个结果。上游对 refreshToken 反重放，并发第二发会带已消费的 RT 撞 12153 并整链作废（账号从此恒 401，只能人工重登）—— 这条是**保号手段**，不只是省流量 |

### 二、协议变更

* **`LICENSE` 改为「保留所有权利（All Rights Reserved）」** —— 未经书面许可不得复制、分发、
  修改、商用、二次打包或以「公益服/低价中转/卡密」名义对外服务，也不得移除声明。
  例外：`workbuddy2api-panel/` 属第三方，仍依其原始 MIT 授权（MIT 全文保留在 `NOTICE.md`）。
* **上游免责声明全部搬入 `NOTICE.md`（必读）**：
  - 上游来源与 MIT 授权原文
  - **使用声明（必读）**（反对批量注册/售卖/加壳卡密分发，5 条）
  - **免责声明**（仅供学习研究，风险自担）
  - **停止维护声明（2026-10-21）**（上游禁反代/精力有限/非法转卖 + AI 辅助维护披露）
  - 本项目额外声明（保留所有权利、不含敏感数据、风险自担、不提供担保与支持）
* `README.md` 的许可段落同步改写。

### 三、未合并（明确列出，避免"看起来全合了"）

| 上游 PR | 内容 | 为什么没合 |
|---|---|---|
| **#225** | `models.hidden` 模型黑名单 | 需要新增 livecfg 快照字段 + 列表过滤 + chat 点名拒绝三处联动，属响应契约变更 |
| **#178** | 账号备注 + 到期卡片折叠 | 需要 pool entry 新字段 + 持久化 + 面板交互 |
| **#196** | 补偿状态可观测化（先查后领 + 倒计时） | 1475 行、10 个文件，涉及任务补偿状态机 |
| **#205** | 任务补领清扫 | 719 行，新增周期清扫链路（与 #204 的「先补领再跳过」不同，后者我们 v1.28.9 已自研） |
| **#213** | 联网搜索端点 `POST /v1/search/messages` | 新客户端 + 新路由 + 记账，是**新功能**而非缺陷修复 |

### 四、测试

新增 `TestDropReasoningContentParts`（思考片段剔除 + 只剩思考的消息整条丢弃）、
`TestClassifyQueueFull`（429+queue.full → ErrUpstreamBusy；普通 429 仍 ErrSoftRate）。
`upstream 227 / server 96 / panel 54 / pool 188`，**全部 0 失败**（19 秒）。

## v1.29.1

**本版 = 第二十六份审查报告（BUG-REVIEW.md，22 条）。21 条为真、1 条误报（#1）。
真条目里 #2/#4/#10/#17/#22 是我最近三版引入或改了一半的。**

### 误报（附证据）

* **#1「四条 sanitizeRewrites 左右值相同、改写层空转」** —— 不成立。用编译后的二进制打出每组 hex：
  `pair0 L=…"CLI for Claude"` → `R=…"CLI tool for Claude"`、`pair1 Main→Default`、
  `pair2 Codex CLI→Codex CLI tool`、`pair3 give→provide`，四条都真的在改；
  `TestSanitizeRewritesAllFire`（断言左右不同 + 写了必须生效）也一直绿。
  报告读到的应是渲染弄花的视图 —— 这个坑我自己也踩过一次（v1.28.9）。

### 功能错误

* **#2 `inReqRange` 未定义**（我 v1.29.0 引入）—— 归档关闭时 `loadLogs` 调它抛
  ReferenceError，整页固定显示「日志读取失败」。补上定义（用 `trangeState('reqRange')` 的
  from/to 筛内存记录），删掉从未使用的 `reqRangeState` 声明，并加「调用了但没定义」的守卫用例。
* **#3 `#view-models` 的标题栏整段丢失** —— 只剩孤立的 `</span></h3>`，该页没有标题、
  `.box > header` 样式不生效。补回 `<header><h3>模型与档位…</h3>`。
* **#4 `session_sticky.enabled` 不在重启清单** —— 它只在启动时决定是否创建 Router，
  面板保存后提示「已保存并立即生效」，实际要重启。补进清单。
* **#5 `cmd/login` 的 realm 恒为 cn，domain 推断分支不可达** —— 不传 `--realm` 登录国际版账号
  会被写成 `realm=cn`，网关按 CN 域路由。改为：显式 `--realm` 优先，否则交给 `ResolveRealm`
  按 domain 推断（新增 `realmExplicit` 标记与 `realmIfExplicit`）。
* **#8 登录/导入自动签到后不记「今日已签」** —— 账号行一直显示「签到」。补 `NoteCheckinDone`
  （含「今天已签到」形态）。
* **#9 11158 的 hint 被通用文案覆盖** —— `GatewayHint(ErrModelBlocked)` 是「后端没有这个模型」，
  与「等级不支持」相反。已定性为 `model_not_allowed_for_account_type` 时不再覆盖 hint。
* **#10 `cmd/credit` 非周期分支仍缺 `used` 钳制** —— v1.28.9 只钳了 `remain`，
  补 `used > size` 与 `remain = size - used` 回算。

### 逻辑与一致性

* **#11 一键完成整批 accept** —— 面板单任务入口早按 20 条分片，一键完成整批发；
  任务多时第一步整批被拒。改为 20 条分片。
* **#14 队列扫描 panic 时返回空文案** —— 命名返回值保持零值，前端弹空 toast。
  在 recover 里回填 msg/level。
* **#15 `apiEnvelope.Code` 是 int** —— 上游把业务码写成 `"11102"` 时整个 Unmarshal 失败，
  按 code 分类的策略全失效。新增 `flexInt`（数字/字符串都吃，非数字按 0）。
* **#17 `hasBusinessEnvelope` 只认紧凑 `"code":`** —— `"code" :` 会被判成 WAF 拦截页，
  走 IP 级 fail-fast。改为只认字段名。
* **#19 `loginPoll` 不校验 TTL** —— 超过 15 分钟的 state 仍可被轮询完成。补校验。
* **#22 `covered` 只在回读成功时登记**（我 v1.28.9 改的）—— 回读失败时 Buddy_App 与
  Buddy_App_QQ 共用的那次上报不被标记，紧接着的会再报一遍。改为动作跑过就登记。

### 已知限制（本轮记录未改）

* **#7** `parseGlobalModelNames` 不支持顶层数组（要动两处解析入口）
* **#12** `/v1/models` 在 global 探测中静默少一半（要给接口加 `probing` 字段）
* **#13** promo 时区解析后未使用（需整条链路带 zone）
* **#16** `upstreamBaseForRealm` 硬编码 base（配置要透到 hint 层）
* **#18** 服务端 requestId 只认紧凑 `"id":"`（改动落在 SSE 扫描热路径）
* **#20** `skipBlocked` 每条过期记录落一次盘（写放大族）
* **#21** 非流式先记账再写响应（调整顺序会影响成功路径的统计时机）

### 测试

新增 `TestAppJSInReqRangeDefined`（app.js 里「调用了但没定义」的守卫）。
`upstream 225 / server 96 / panel 54 / pool 188`，**全部 0 失败**（20 秒）。

## v1.29.0

**本版 = 第二十五份审查报告（代码审查报告.md，22 条）。22 条全部为真，
其中 4 条（#1/#4/#17/#22）是我最近三版引入的。**

### 高

* **#1 特征表被注释吞掉一行** —— 我上一版补 `cc_version` 时那行少了换行，注释把
  `"cc_entrypoint",` 整行吞进同一行 → 特征表里没有它。只带 `cc_entrypoint` 的正文
  `hasFingerprint()` 返回 false，整条净化被跳过、指纹原样出站。已拆成两行并补测试。
* **#2 导入把「已过期超过 24h」的 token 改写成 now+1h** —— `NeedsRefresh(10m)` 判据是
  `now+10m >= ExpiresAt`，伪造成未来时间后前 50 分钟**恒不刷新**，每次 chat 都拿过期
  access token 打上游（401/12153，三振还禁用账号）。改为写 0（未知），与登录路径同口径。
* **#3 看门狗两条 continue 绕过 `armAlarm`** —— 一次性 `setExactAndAllowWhileIdle` 只有
  每轮重臂才维持「每 5 分钟一次」；停在「外部实例复用」或反复自愈态时第 3 层自启失效。
  把重臂提到两条分支之前。
* **#4 队列绑了 HTTP 请求 ctx**（我 v1.28.4 改的）—— `startGrowthQueue` 是 `go` 之后立即
  返回，handler 一写完响应 net/http 就 cancel ctx；派发循环里 `select` 两分支同时就绪时
  **随机**命中 `ctx.Done()`，剩下的账号全被标成 skipped。表现为「点一次只跑前几个账号」。
  改为 `context.Background()`（定时任务入口本来就传 Background）。
* **#5 单域失败时把原因丢掉** —— `fetchErrs` 只在 `out` 为空时才用；CN 失败 / global 成功
  时列表少一半却没有任何提示。响应里补 `errors` 与 `note`。

### 中

* **#6 异步派发后跨过的下一个时点被整天跳过** —— 批次改后台跑后主循环立刻重算
  `nextWake`，若本批跑过了下一个时点（如 10:00 活跃上报），`nextFire` 直接返回次日。
  记入 changelog 作为已知限制（补跑需要引入「已过未执行槽位」队列，属结构性改动）。
* **#7 `model.json` 每条一次全量落盘** —— 冷缓存 + 30 个模型 = 30 次全量写。
  已知限制：与 #11/#12 同属「写放大」，统一改成「置脏 + 定时落盘」需要接 pool 的 flusher
  或新起一个 debounce 定时器，本轮未做（改动面比收益大）。
* **#8 progress 覆盖把 target 写成 0** —— `progress:{current:3,target:0}` 时整对覆盖，
  `Claimable` 判据恒 false → 永远不可领、队列反复重跑并累计跳过名单。改为逐字段覆盖。
* **#9 择新恢复用 `SavedAt` 与 mtime 比较** —— 同秒/时钟回拨会选中旧快照，覆盖本地更新的
  冷却/熔断/成本台账。已知限制：需要引入单调 revision，属 schema 变更。
* **#10 签到日界用本地时区、硬冷却用固定 +08:00** —— 主机时区非 CST 时面板「已签」与
  「何时解冻」互相矛盾。已知限制（统一口径需改配置语义）。
* **#11 跳过名单过期清理在扫描热路径落盘** / **#12 11158 每命中一次落一次盘** ——
  同 #7 的写放大族，本轮只记录。
* **#13 超时文案说「任务仍在后台继续」但实际被中止标记掐掉** —— 文案改成「流水线已中止」。
* **#14 登录轮询任何一次错误就永久停止** —— `api()` 现在把 `status` 挂在 Error 上，
  轮询只在 4xx（state 真失效）时终止，网络抖动/5xx 继续重试。

### 低

* **#15 `/v1/models` 在 global 探测进行中静默少一半名单** —— 面板侧有
  `GlobalProbeInFlight()` 区分，接口侧没有；记录（需要给接口加 `probing` 字段）。
* **#16 注释说 max_body_mb 已移除、实现仍在生效** —— 注释更正（`MaxBytesReader` 默认 64MB）。
* **#17 注释说 `Error.Status` 是真实 HTTP 状态、实现是语义状态**（我 v1.28.9 改的）——
  注释更正，并说明真实状态在 `Error.HTTPStatus`。
* **#18 `CooldownUntilTomorrow4AM` 名不副实** —— 注释写明实际是「当天 09:00 / 次日 04:00 /
  次日 09:00」三选一（不改函数名，避免动调用点）。
* **#19 日志环按单次 Write 切行** —— 多 goroutine 并发时行序会交错（内容不坏）。记录。
* **#20 `loginTimer` 覆盖前未 clearInterval** —— 起新轮询前先 `stopPoll()`。
* **#21 `checkAuthGate` 吞掉所有非密钥错误** —— 其余错误改为 toast 提示。
* **#22 `recentInOrderLocked` 用长度判「未写满」** —— 目前正确但脆弱；记录。

### 测试

新增 `TestSanitizeCcEntrypointFeature`（只带 cc_entrypoint 也必须进净化并剥离）、
`TestParseGrowthTasksProgressKeepsFlatTarget`（progress target=0 不许覆盖平铺 10）。
`upstream 225 / server 96 / panel 53 / pool 188`，**全部 0 失败**（16 秒）。

## v1.28.9

**本版 = 第二十四份审查报告（BUG-REVIEW.md，25 条）。25 条全部为真（其中 3 条是我上一版引入的
#5/#12/#20）。另发现并修掉一处**由本轮 UseNumber 改动引发的连带回归**。**

### 高优先级

* **#1 `hasImagePart` 对混合会话永远 false** —— 结构体要求**每条**消息的 `content` 都是数组，
  而真实请求里 system 与历史轮次是字符串 → `json.Unmarshal` 整体报错 → 直接 return false。
  于是只要会话里有一条纯文本消息（几乎全部带图请求都这样），`reqHasImage` 恒 false，
  11133「模型不支持图片」的 hint 永远不触发。改为 `Content any` 逐条判断。
* **#2 主改写管线没用 UseNumber** —— `prepareBody` 是**每个** chat 请求都走的主路径，
  默认解码把 >2^53 的 `seed`/微秒时间戳静默改写；而同仓 `rewriteModel`/`clampNToOne`/
  `prompt.Rewrite`/`InjectPromptCacheKey` 都刻意用了 UseNumber —— 口径自相矛盾。
  改用 UseNumber 后**连带修掉两处数字分支**：`translateMaxCompletionTokens` 与
  `clampGPTMinMaxTokens` 只认 `float64`，不补 `json.Number` 会让「别名翻译」和
  「GPT 系 max_tokens 抬到下限」静默失效（Claude Code 切模型的极小探针又会被 11133 拒）。
  这两个用例当场把回归抓出来了。
* **#3 `12153` 用整段 body 子串匹配且无状态码门槛** —— token 数/requestId 片段里出现
  12153 就被判 `ErrSessionDead`，连续 3 次**永久禁用账号**。裸数字标记加 401/403 门槛，
  文本标记（Offline user session not found）不设门槛。
* **#4 业务码错误的 `Error.Status` 仍是真实 HTTP 状态（常 200）** —— 下游按 Status 分支的
  逻辑全部失效：`ClaimRewardMP` 的 400 降级不触发、`IsBuddyTaskIncomplete` 判不出门槛未达标
  → 每轮巡检重打一次。`Status` 改为语义状态（`businessCodeStatus` 合成值），
  另加 `HTTPStatus` 保留真实状态供日志/排障。

### 中优先级

* **#5 硬冷却保护分支只置脏不改状态**（我 v1.28.6 改的）—— 每次并发软冷却都 `dirty.Store(true)`，
  `startFlusher` 每 5 秒全量重写 state.json。去掉这三处置脏。
* **#6 `SetCredits` 全仓无调用且不钳负值** —— 补 `<0 钳 0` 并注明仅测试/扩展使用。
* **#7 小时校验无视「任务已禁用」** —— `checkin_enabled=false` + `checkin_hours=[-1]`
  （用户照错误文案去关闭任务后的残留）启动直接失败。改为按各任务的 enabled 开关跳过校验。
* **#9 手动全量刷新余额用无 ctx 版本** —— 46 号最坏 ≈ 7 分钟。改用 Ctx 版并加 10 分钟上界
  （进程级可取消 ctx 需要更大改造，见下）。
* **#10 归档关闭时时间范围筛选被忽略** —— `metrics.recent` 没经过 `reqRange` 过滤，
  用户选「今天/自定义区间」仍看到区间外旧记录。补过滤。
* **#11 `login.sh` 编译时没把 Go 加进 PATH** —— `signin.sh`/`credit.sh` 早补了，login.sh 漏了，
  在 `set -euo pipefail` 下直接 `go: command not found` 退出。
* **#12 `covered` 只在 `snap != nil` 时登记**（我 v1.28.8 改的）—— `loadTaskMap` 失败退化为
  逐次查询时，共用一次上报的两个码会各跑一遍动作链。登记移出 snap 判断。
* **#13 任务码大小写口径不一致** —— `autoActionFor` 用 `==`，`taskMapByCode` 用 ToLower；
  上游一改大小写，队列扫描判「没有该动作」而 runAutoAll 仍命中。改 `EqualFold`。
* **#14 全冷却兜底不校验 `modelCooled`** —— 对该模型处于 6004/11102 冷却的号照样被选中，
  `ModelBlockedForRealm` 的结论被兜底绕过，还顺带推进该号 `lastUsed` 把 LRU 带偏。补判据。
* **#15 `normalizeFrame` 每帧补 `usage: null`** —— 与 OpenAI chunk 形态不一致，
  严格校验存在性的客户端会拿到 null。改为上游没带就**不写该键**（相关 6 条用例已更新）。
* **#16 队列信号量抢占不可取消** —— 裸 `sem <- struct{}{}`，并发槽被长任务占满时派发循环卡住、
  断连也不退出。改 `select + ctx`（`runQueueItems` 增加 ctx 参数）。
* **#18 `loadExpiry` 在途时静默吞掉点击** —— 补一句「上一轮查询还在进行中」的反馈。
* **#20 末端 tier 分支没有 code/status 守卫**（我 v1.28.4 那段之后的邻居）——
  会把 `upstream_unreachable(502)`/`upstream_timeout(503)`/`waf_ip_blocked` 改写成
  `model_not_allowed_for_account_type(400)`，把「上游挂了」说成「换模型」。补守卫。

### 低优先级

* **#21 `expiresIn` 上限两处各写一份字面量** —— 导出 `upstream.RefreshTokenExpiresInMax`，
  login.go 改为引用，避免改一处忘另一处。
* **#23 `if a != nil` 恒真** —— `a` 由 `accountByUID` 保证非 nil，去掉死条件。
* **#25 日志频道前缀表漏项** —— `autotask ` / `panel: 领取任务奖励` / `panel: 全部接受` /
  `panel: 跳过名单更新` 全落到 sys 频道，面板按「任务」筛选时任务日志消失。补齐。
* **#17 / #19 / #22 / #24 死代码与文档口径**：`resolveModel` 永不返回空串（相关 `realm == ""`
  退化分支是死代码）、若干导出 API 仅单测使用、`chatPaths` 恒单元素 —— 记录在案，
  本轮只做最小改动（不删导出 API，避免破坏测试与外部调用）。

### 测试

新增 `TestHasImagePartMixedMessages`（字符串+图片混合、纯文本不误判）、
`TestClassify12153NeedsStatusGate`（401 判死号 / 400 不判 / 文本标记不设门槛）、
`TestAutoActionForCaseInsensitive`。
`server 96 / upstream 223 / panel 53 / pool 188`，**全部 0 失败**（15 秒）。

## v1.28.8

**本版 = 第二十三份审查报告（审查报告-应用缺陷.md，20 条）。20 条全部为真，
其中 4 条（BUG-01/03/15/19）是我最近两版改动引入的。**

### 我引入的四条

* **BUG-01 n 钳制只在「带 realm 前缀」时执行** —— v1.28.7 我把 `clampNToOne` 放进了
  `if bareModel != peek.Model` 分支里，而 OpenAI 兼容客户端默认发**裸名**（两者相等），
  那条分支根本不进 —— 最需要钳制的常规请求反而漏掉。移到分支外。
* **BUG-03 反探测改写按子串替换任意数字串** —— v1.28.7 我用 `strings.ReplaceAll` 改写
  `11-128`，正文里任意位置的同形子串（base64 片段、ID、时间戳、代码常量）都会被改，
  长度 +1、内容被篡改。改为边界正则 `(^|[^0-9A-Za-z_-])11-?128([^0-9A-Za-z_-]|$)`
  只命中独立出现的 token，并从改写表里移除这两条。
* **BUG-15 `Covers` 把 A 的任务对象写进 B 的快照键** —— v1.28.7 我为了防重复上报加的
  `snap[lower(c)] = after`，而 `after` 是 A 的 `*upstream.Task`（TaskCode/Current/Target
  都是 A 的）：A 已达标就误判 B 已完成，A 没推进就让 B 再跑一遍（正是我想防的重复上报）。
  改为只记「已覆盖」标记，B 命中标记直接跳过。
* **BUG-19 同一次 JobScheduler 任务调了两次 `jobFinished`** —— v1.28.7 我在用户已停止分支
  写了 `jobFinished(); return;`，而 `return` 会走 `finally` 再调一次。删掉显式调用。

### P1

* **BUG-02 脱敏删掉正文首尾空白** —— `sanitizeText` 结尾无条件 `strings.TrimSpace`：只要
  命中任一指纹，整段正文的缩进/行首空格/行尾空白全被删，代码块结构被静默破坏，
  而且 `sanitizeContent` 因 `s != c` 判成「已改写」，没有任何日志。去掉 TrimSpace。

### P2

* **BUG-04 显式空 `stream_options: {}` 不补 include_usage** —— 只判「键是否存在」，
  部分 SDK 默认发空对象 → 上游末帧不回 usage → token/credit/速率全缺。改为看内容。
* **BUG-05 `fetchV3ConfigModelMap` 先解引用后判空** —— 1794 行调 `a.AccessTokenValue()`，
  1795 行才判 `a != nil`，真传 nil 就是 panic。判空提前。
* **BUG-06 `mergeGlobalCatalog` 只在 primaryInfos 非 nil 时初始化 outInfos** —— 改任一侧非 nil。
* **BUG-07 非 Cycle 分支只钳 remain 不钳 used** —— 上游 `CapacityUsed > Size` 时出现
  「剩余 50 / 已用 150 / 面额 100」。补 used 钳制并保持 remain+used ≤ size。
* **BUG-08 `ReportMPEvent` 写死 `BillingBaseCN`** —— 同族的 ClaimReward / ReportWebEvent
  都按 realm 取 base，只有它硬编码。改 `c.billingBase(a)`。
* **BUG-09 `time.Time` + `omitempty` 输出 0001 年** —— `encoding/json` 对结构体的 omitempty
  不生效。前端新增 `hasRealTime()`（拒绝 year<1970），`ago()` 与到期排序统一用它，
  不再逐处特判。
* **BUG-10 无到期账号被排到最前** —— 零值时间能解析成巨大负时间戳，`!isNaN` 判成"有值"，
  排序排到所有真实到期时间之前。同上用 `hasRealTime`。
* **BUG-11 `modelExempt` 死代码但注释仍当判据** —— 生产代码零调用（可服务性只看
  `healthy()`），只有单测在用。函数保留（测试依赖），注释补明「不要据此推断探活语义」。
* **BUG-12 登录/导入后到期快照被清空不重建** —— `ReenableIfCredits` 会把
  `creditsExpiring/creditsEarliestExpiry/creditsEarliestRemaining` 全部清零。两条路径改用
  `UserResourceDetailedWithExpiry` + `SetCreditsDetailed` 重建（与面板单号签到路径同口径）。
* **BUG-13 `snapshotAt` 死代码且无边界检查** —— 全仓零调用，直接删除。
* **BUG-14 `Ring` 零值 panic** —— `&Ring{}`（cap==0）时越界 + 除零。`Write`/`Snapshot` 各补设防。
* **BUG-16 断连后队列项永久 pending** —— `runQueueItems` 收尾只复位 `running`，未执行项
  永远停在 pending。收尾时把 pending 标成 skipped（带 seq 守卫防写脏新一轮）。
* **BUG-17 队列结束后 `started` 恒 true** —— 只在「无可执行待办」分支清 `startedAt`。
  收尾统一清。
* **BUG-18 排序选择器存了废弃值时显示空白** —— 赋不存在的 option 值让 `value` 变空串，
  回落分支只改了 JS 变量没回写 select。补一行回写。

### P3

* **BUG-20 watchdog 把 fail 清零导致恢复日志丢失** —— 外部实例接管分支 `fail=0` 不留痕，
  「本实例失败 → 外部接管 → 本实例恢复」这条真实路径在 watchdog.log 里没有恢复记录。
  清零前先打一行说明。

### 测试

新增 `TestSanitizeKeepsLeadingTrailingWhitespace`、`TestSanitizeCodeOnlyStandalone`
（独立出现改写 / 长 token 内子串不动）、`TestPrepareBodyFillsEmptyStreamOptions`、
`TestRingZeroValueDoesNotPanic`。
`upstream 222 / server 95 / panel 52 / pool 188`，**全部 0 失败**（21 秒）。

## v1.28.7

**本版 = 第二十二份审查报告（CODE-REVIEW-BUGS.md，8 条）。8 条全部为真，其中 3 条
（BUG-01 / BUG-06 / BUG-08）直接指向我最近三版的改动。**

### 高危

* **BUG-01 导入的「后台化」被同步信号量抵消** —— 这是 **v1.28.6 我加 `importSem` 时的错**：
  抢槽写在了 `go func` **外面**，从第 4 个账号起 handler 会卡在 channel 上等前 3 个后台
  动作跑完。100 个账号的导入接口要几分钟才回包 → 浏览器/反代先超时 → 用户重试 →
  账号被重复导入。抢槽挪进 goroutine（并发上限仍是 3）。
  教训：同仓其它「先抢槽再起 goroutine」的写法都对，是因为它们后面有 `wg.Wait()`；
  这里没有等待点，同一个模式就变成了阻塞。
* **BUG-02 global 登录的 realm 回填失败漏了终态** —— 同文件另外四个失败分支都在 v1.28.2
  被我改成终态（completed=true + delete），唯独这一处漏了。`auth/token` 已成功即上游
  已消费 state，不置终态前端会空转到 15 分钟 TTL。
* **BUG-03 非流式 `n>1` 把多条候选拼成一条** —— `Aggregate` 把所有 choice 的 delta 追加进
  同一个 builder，只输出 `choices[0]`，客户端拿到两条候选首尾粘连的正文。
  采用报告的兜底方案：非流式把 `n` 钳到 1 并打 WARN（流式逐帧透传不受影响）。

### 中危

* **BUG-04 排程跨日靠 `+24h`** —— `Add(24h)` 是绝对时长，夏令时切换日会偏一小时
  （春季次日 09:00 变 10:00、秋季变 08:00；夜猫子 23:00 窗口会提前到 22:00 白跑一轮）。
  改为重建日期 `time.Date(..., Day()+1, ...)`。本机时区是 Asia/Shanghai 无 DST，
  但容器/别的部署时区会中招。
* **BUG-05 `prompt_cache_miss_tokens: 0` 被当权威值** —— 上游会同时下发真实命中数与 0 值
  兼容别名（本文件头已记录同形态的 `cache_read_input_tokens:0`）。采信 0 → 命中率显示
  100% → 「大前缀整段重算」的告警永不触发。改为 0 值时回落 `prompt-hit` 推导。
* **BUG-06 JobScheduler 兜底不检查「用户已停止」** —— 这是 **v1.28.6 我补 `user_stop` 时
  只改了 BootReceiver**，JobScheduler 这条复活路径没判：`cancel()` 抛异常被吞或系统已重排时，
  网关仍会被拉起。补同一门控。

### 低危

* **BUG-07 三个任务入口缺 global 门控** —— `accountTasks` / `accountTaskAuto` 都返回 501，
  而 accept / accept_all / claim 直接打上游 growth 端点 → 403 → 被包成 `502 accept: ...`，
  用户以为网关坏了。抽出 `rejectNoGrowth` 三个入口复用。
* **BUG-08 `realm_required` 覆盖掉真实原因** —— 这是 **v1.28.4 我加的那段**：`status` 的默认值
  就是 503，而上游超时/5xx/WAF/传输抖动这些分支只改 code/msg 不改 status，于是**任何** 503
  都被改写成「请给模型名加 global: 前缀」，真实原因（上游挂了 / IP 被拦）被抹掉，
  两者排查方向完全相反。补 `code == "no_healthy_account"` 守卫。

### 测试

新增 `TestClampNToOne`（钳制生效 + 无 n/n<=1/非法 body 一律原样）、
`TestUsageCacheMissTokensIgnoresZeroAlias`（0 值别名 → 按 prompt-hit 推导 800；非 0 显式值仍优先）。
改动涉及的 4 个包：`server 95 / upstream 219 / panel 51 / scheduler 65`，**全部 0 失败**。

## v1.28.6

**本版 = 第二十一份审查报告（CODE-REVIEW.md，22 条）。21 条为真、1 条部分属实。
#1「脱敏层整体失效」结论对、成因错 —— 我逐字节验过：那 5 组改写左右值确实不同、
而且都真的 fire，真正的坏点是 header 正则写成全拼、`11-128` 只覆盖一种拼写。**

### P0

* **#1 出站脱敏层** —— 报告说「4 组左右值逐字节相同、是空操作」。**实测不成立**：
  用 Go 打出每组左右值的 hex 与 `sanitizeText(rw[0])` 的结果，5 组都是 `eq=false / fire=true`。
  但脱敏确实基本没在工作，真实原因是四处：
  1. `sanitizeBareHdrRe` 写成 `x-anthropic-billing-hdr`（**全拼 header**），而真实键名与
     特征表都是 `billing-hdr` —— 差一个单词，整个裸键名兜底永远匹配不到；
  2. `sanitizeHdrRe` 的正则丢了键名，只剩 `(?i)\n]*;?\s*` —— 不删 header，反而会吃掉正文里的
     换行与右括号（数据损坏）；
  3. 裸键名替换成 `x-anthropic-billing-hdr` 它自己 —— 等于没改；
  4. `11-128` 只覆盖一种拼写（表里是 5 字节形态），另一种原样出站。
  修法：两种 header 拼写都用 `billing-(?:hdr|header)` 覆盖；剥离层要求「键:值」形态；
  裸键名缩写为 `x-anthropic-hdr`；`11-128` 两种拼写都改写成 U+2011 形态；
  并把**改写右值一律由左值 `strings.Replace` 派生**（结构上杜绝「左右相同 / 左右写反」）。
  另外补了 `cc_version` / `cc_entrypoint` 到特征表 —— 只有 cc_ kv 的文本此前连净化入口都进不去。
* **#2 全冷却兜底选中「无凭证占位号」** —— `pickEarliestExpiryLocked` 只判了 disabled/paused，
  漏了 `needAuth`。占位号带着从 state.json 恢复的冷却截止时 `expiry` 非零，会被兜底选中 →
  handler 拿到 `AccessToken==""` 的号 → `no refreshToken` 熔断+轮转 → 整池 503。
* **#3 队列把「已达标未领奖」的任务整条重跑** —— `growthPending` 只排除 `Claimed`，
  而单任务入口与 `runAutoAll` 用的都是 `taskDone()`（Claimed||Claimable）。
  `expert_5` 是 5 次真实对话 + 5 组召唤事件，白烧一遍。补 `Claimable` 过滤 +
  `runGrowthQueued` 里「已达标直接领奖」分支。
* **#4 `runAutoAll` 用启动快照判完成** —— 快照只拉一次、动作后不回写，任何「前一个动作顺手
  推进了后一个任务」的组合都按旧状态再跑一遍（`Buddy_App`/`Buddy_App_QQ` 共用一次上报最典型）。
  回读结果写回快照，并给 `autoAction` 加 `Covers` 标注「一次上报覆盖多个码」。

### P1

* **#5 `runSequentialEventTask` accept 后不回读 target** —— target 兜底成 1，上报 1 条就误判达标
  → `ClaimRewardMP` 撞 400 → 队列记一次无效，3 次进跳过名单。accept 后回读真实 target。
* **#6 单任务入口的 mp 回读不等待** —— 用 `taskByCodeMP` 只读一次，而上游计分异步（3–8s），
  几乎总是 `claimable=false`；「一键完成全部」用的是 `taskByCodeWaitingMP`。两入口对齐。
* **#7 keepalive 没有账号间节流** —— 签到/旅行/活跃/夜猫子都有 800ms 级延迟，唯独它是裸循环：
  46 个号在 22:00 连发 46 个 refresh（仓库多处注释列为 WAF 频控形态）。补 `keepaliveAccountDelay`
  + `RunKeepaliveNowCtx`（停机可打断），`runBatch` 改调 Ctx 版。
* **#8 `RunNightChatsCtx` 的 ctx 没传进对话** —— 用的是 `ChatStream`（内部 Background），
  ctx 只在两轮间的睡眠生效，一条长回复无法被打断。改 `ChatStreamContext`。
* **#9 两个会话头没做 header 安全清洗** —— `X-Conversation-Request-ID` / `X-Trace-ID` 直接取客户端
  原值，含 `\r\n` 时 net/http 在写请求阶段报 invalid header value → 传输层错误 → 轮转全失败 → 503。
* **#10 非流式缺 `total_tokens` 时整笔 credit 被丢** —— 上游只回 credit+prompt+completion 时
  `ok=false`，账本/reqlog/NoteModelCost 全当没观测到。缺 total 时用 prompt+completion 兜底。
* **#11 硬冷却分支顺手清掉其它模型的 6004 冷却** —— 一个号余额耗尽硬冷却到次日 04:00，期间另一条
  在途请求撞了模型 A 的 6004，就把模型 B/C 未到期的冷却一起抹了。硬冷却守卫改为「只判断不清理」。
* **#12 `Aggregate` 用 `%v` 包装读错误** —— 链断了，`net.Error.Timeout` / `DeadlineExceeded`
  全部判否 → 超时报成 502 stream_error，而不是 503 upstream_timeout，也走不到「超时不罚号」。
* **#13 Dockerfile 没注入版本号** —— 镜像里 `main.appVersion` 恒为 `dev`，面板左上角显示 vdev。
  改为 `APP_VERSION` 构建参数 → `VERSION` → `git describe` 三级回落。

### P2

* **#14 `cmd/credit` 的 global 域只试一条路径** —— 主程序是「无 /v2 → 404 再 /v2」双候选，
  CLI 只取第一条，上游只认 /v2 时全部 global 号报 404。补齐 fallback。
* **#15 `growth` 参数被强制改成 true** —— `if !body.Growth { body.Growth = true }` 让调用方
  永远关不掉。改 `*bool` 区分「未传」与「显式 false」。
* **#16 `writeAuthSnapshot` 不清扁平形遗留键** —— 升级成嵌套形后顶层 `accessToken` 等原样留着，
  按扁平键读的外部工具会拿到过期凭证。写嵌套形时删已知扁平键（未知手写键保留）。
* **#17 `exploreLast` 只增不减** —— 客户端可传任意模型名，map 随历史单调增长，`/status` 还整体
  序列化输出。超过 512 条时清理 24 小时前的条目。
* **#18 cockpit 导入的后置动作没有并发上限** —— 导入几百个号同时发起几百组上游请求。
  补 `sem=3` + panic 兜底。
* **#19 脱敏测试的断言自相矛盾** —— 同一条断言里既要求「包含」又要求「不包含」，两条不可能
  同时成立；实际上两条查的是**两个只差一个连字符的不同字符串**（这也是它一直没抓到 #1 的原因）。
  重写为「改写后不再含被拦原文」，并新增 `TestSanitizeRewritesAllFire`（左右不许相同、写了必须生效）
  与 header 指纹用例。
* **#20 券码批量抢槽不可取消** —— 裸 `sem <- struct{}{}`，关页面后仍把剩余账号查完。改 select+ctx。
* **#21 `model.json` 落盘用固定临时名** —— 多实例/并行测试会互相覆盖。复用 `auth.ReplaceAtomic`。
* **#22 用户点「停止」后重启手机仍会自启** —— `stop()` 只写了 `service_up=false`，没持久化
  「用户主动停止」；`BootReceiver` 无条件拉起。补 `user_stop` 标记：停止时置位、开机时判、
  用户打开 App 时清除。

### 测试

`upstream 218 / pool 188 / panel 51 / server 94 / scheduler 65 / auth 29 / session 21`，
本轮改动涉及的 7 个包 **全部 0 失败**。

## v1.28.5

**本版 = 第二十份审查报告（审查报告-应用bug.md，13 条）。13 条全部为真，其中 #6 是
v1.28.4 我修 #7 时引入的新问题（提前 break 留下零值行）。**

### 高危

* **#1 账号级冷却连带删掉 (账号,模型) 负缓存及退避计数** —— `clearRoutingModelCooldownsLocked`
  无条件删掉全部非 AuditOnly 条目，而这些条目同时承载两种语义：6004 模型级限流 与
  11102/11158 的**上游事实**。一次 60s 的账号级软冷却就把「这个号用不了这个模型」连同
  `Hits` 一起清空 → 下次再去撞同一个必败模型、退避从 6h 重头学（与 v1.28.4 里
  `pruneExpiredModelCooldowns` 刻意保留 Hits 的设计直接冲突）。改为按 reason 前缀保留
  11102/11158 条目。
* **#2 6004 + `Retry-After` 把整号冷却** —— 同一分支里「带重置文案的 6004」走
  `CooldownSoftForModel`（只锁模型），「只有 Retry-After 头」却走 `CooldownSoftRate`
  （整号），同一份上游错误两种行为：换个模型本可继续用，现在全被 429 挡掉。
  改为 `modelRateLimited` 时走模型级冷却。

### 中危

* **#3 `cmd/credit` 非周期包不钳制** —— `CapacityRemain > CapacitySize` 或为负时，
  `-pretty` 打印「剩余 150 / 面额 100」，汇总与依赖这份 JSON 的脚本一起偏大。
  照搬 `internal/upstream` 的钳制口径。
* **#4 三个工具的 auth 扫描口径与网关不一致** —— 网关 `workbuddy*.json`，工具只认
  `workbuddy-*.json`：手工维护的 `workbuddy.json` 会被网关加载却被工具静默跳过
  （signin/trial 还会直接报 "no auth files"）。三处统一。
* **#5 任务码兜底解析遍历 map（随机序）** —— 账号下有两个 valorant/perler 相关码时，
  同一任务点两次可能解析成不同 code，表现为「一次成功一次报没有此任务」，无法稳定复现。
  改为固定关键词优先级 + key 字典序。
* **#6 `packages` 断连时输出空账号行** —— 这是 **v1.28.4 我加 `break dispatch` 时引入的**：
  `out` 是预分配切片，未派发的下标保持零值，最后无条件回出去就是一堆 `{"uid":""}`。
  按 `schoolVouchers` 的同口径过滤。`schoolVouchers` 早就防了这个形态，我漏了。
* **#7 队列 `queueSet` 没有 seq 守卫** —— 上一轮 goroutine 还没退出、下一轮已替换
  `q.items` 时，晚到的 skipped 标记会写脏**新一轮**队列（用户看到「刚点执行就全部跳过」）。
  `queueMarkAt` 早就有守卫，同一文件的 `queueSet` 漏了。
* **#8 扫描槽位不可取消 + 分组顺序不定** —— 裸 `scanSem <- struct{}{}` 让断连后仍要等满
  4 个槽位；各 goroutine 各自 `append` 使完成顺序成为入队顺序（注释承诺的是「保持顺序」）。
  改成 `select` + ctx，并按 `states` 下标原位写入、`wg.Wait` 后顺序拼接。
* **#9 过期绑定压制「优先空闲号」** —— `bound` 统计没过滤过期条目，过期绑定 5 分钟内仍把
  uid 当成「非空闲」，`idle` 集合被低估 → 新会话总撞在同一批号上。读路径跳过过期。

### 低危

* **#10 缓存命中率告警阈值与实现不符** —— 注释承诺 <10%，实现只有 `hit==0` 才判断，
  1%~9%（正是费用异常最该提示的形态）永不出 WARN。改按 `hit/(hit+miss)` 判定，
  阈值抽常量 `cacheHitWarnFloor`，告警文案带上具体命中率。
* **#11 出站 body 经 map 往返丢数字精度** —— `json.Unmarshal` 把整数落成 float64，
  >2^53 的 seed/微秒时间戳回写成 `9007199254740992`，还可能出现科学计数法。
  `rewriteModel` / `prompt.Rewrite` / `prompt.Append` / `InjectPromptCacheKey` 四处
  统一改 `UseNumber`（数字原文进出）。
* **#12 `cmd/credit` 空 auth 目录被当成成功** —— `Glob` 的返回值被 `_` 丢弃、也不校验空数组，
  输出 `{"accounts":[]}` 且 `exit 0`，脚本分不清「没登账号」与「查完了没数据」。
* **#13 `login.sh` 丢掉 cmd/login 返回的 realm** —— 写文件时按 domain 里有没有
  `workbuddy.ai` 反推，global 号 domain 为空时会被写成 `cn`（网关随后把它路由到 CN 上游，
  chat/签到全挂）；脚本也从不给 `--realm` 传参，国际版账号根本登不上。
  改为优先用返回的权威 `realm`，并支持 `bash login.sh [cn|global]` 透传。

### 测试

新增 4 组：`TestClearRoutingKeepsModelNegativeCache`（11102/11158 保留、6004 清除）、
`TestCacheMissWarnsAtLowHitRate`（2% 告警 / 80% 不告警 / 从未命中不告警）、
`TestRewriteModelKeepsBigInt`（`9007199254740993` 原文保留且不出现科学计数法）、
`TestEffectiveTaskCodeDeterministic`（100 次解析同结果）。
全量 **770 通过 / 0 失败**。

## v1.28.4

**本版 = 第十九份审查报告（CODE-REVIEW-2026-10-21.md，14 条）。13 条为真、1 条半真
（#7 里 `syncNicknames` 那半条不成立：它在回完 200 之后的后台 goroutine 里跑，没有活连接可断）。**

### P0

* **#1 `StartBalanceRefresh` 任务族锁无 defer、整条链无 panic 兜底** —— `RunBalanceRefreshNowCtx`
  只要 panic 一次（上游响应形状变化、索引越界），`TaskBalance` 永久不释放：面板「刷新余额」
  此后恒 409、后台周期刷新彻底停摆，且没有任何日志。内层那个每号一个的 goroutine 同样没有
  recover —— 子 goroutine panic 无人接 = 整进程退出（同仓 panel 的各条后台路径都加了兜底）。
  EndTask 改 `defer`，两层各加 recover。
* **#2 自愈退避 2s 短于优雅停机 5s → 新进程 `bind: address already in use`** —— `selfHeal`
  只发 SIGTERM、`backoffMs(1)=2s` 后立刻拉起，而 Go 侧 `srv.Shutdown` 预算是 5s（有在途 SSE
  时必然跑满）→ 旧进程还占着 7863。把 `restartGateway` 里那段「轮询等旧进程退出至多 8s」
  抽成 `awaitExit(old, 8000)`，两处共用；`selfHeal` 先捕获旧句柄再杀（`killProc` 会置 null）。
* **#3 裸模型名恒判 `realm=cn`：纯 global 池被硬过滤成 503** —— OpenAI 兼容客户端 / Claude Code
  / Codex 默认发裸名，而 503 是「可重试」语义，它们会就着同一个裸名无限重试，
  「加前缀 / 换模型」这条正确处置永远不触发。保留 D6 的裸名=cn 语义（改成跨域会把握手
  归一给别的域，风险更大），把「本域无可服务号、但另一域有号」这个确切场景改成
  **400 + `realm_required`**，message 写清要加 `global:` 前缀。
* **#4 降级开关是进程级全局** —— 任意一个客户端被内容策略误拦，全站当天都改走中性提示词
  （`append` 还会退化成 replace），Codex/Claude Code 的项目规范被整段替换。
  改成按客户端隔离：key = 会话键，没有会话头时用「UA + 模型」指纹；键数上限 256（超限先清过期，
  再超就重置并告警）。重置时刻仍是次日 00:00。

### P1

* **#5 非流式聚合断流仍报 `finish_reason:"stop"`** —— 半截答案被客户端当完整结果落库。
  判据**刻意收紧**：只有「既没 `[DONE]` 也没给过任何非空 finish_reason」才改判 `length`。
  只看 `!sawDone` 会把 WorkBuddy「完整结束但不发 [DONE]」的大量正常回复误标成截断
  （工具调用分支早就有同款注释说明这是常态）。
* **#6 `ModelBlockedForRealm` 用写锁做纯只读聚合** —— 它在「每次选号失败」的出口被调用，
  写锁会让模型限流风暴把并发的 pick / `/status` / `/healthz` 一起串行堵住。改 `RLock`。
* **#7 批量接口不响应断连** —— `packages` 抢槽是裸 `sem <- struct{}{}`，关页面后仍会把
  剩余账号全部查完（46 号 ≈ 7 分钟）。改 `select` + ctx。**`syncNicknames` 那半条不成立**：
  它由「手动刷新」在回完 200 之后的后台 goroutine 调用，没有活连接可断，按断连取消
  只会把刷新做一半 —— 已还原并写明缘由。
* **#8 `session.Router.touch` 不校验身份** —— 并发换号后迟到的续期会把会话写回刚失败的号
  （连 Redis 绑定一起写错，重启后恢复的还是错绑定）。写锁内 re-check `cur.uid == uid`。
* **#9 11102 负缓存的指数退避形同虚设** —— `prune` 过期即整条删除，`Hits` 跟着丢，
  下次又从 0 起算 → TTL 永远 6h，注释承诺的 6h→12h→24h 不可达。改为只清冷却、保留 `Hits`。
* **#10 mp 合并任务的 `seen` 只由已入队项构建** —— 被判成 skipped 的任务会被重判一遍，
  「跳过 N 个」双计。`seen` 改由**全量**任务码构建（与 `loadTaskMap` 同口径）。

### P2

* **#11 `NewHandler` 的 `PromptMode` 缺省 `custom` 与 config 层 `passthrough` 相反** ——
  裸用 `NewHandler` 拿到的是 custom，而 custom 在 `PromptText==""` 时什么都不做；
  哪天给 PromptText 赋值就会静默替换所有客户端的 system。缺省对齐，并对「custom 但文本为空」告警。
* **#12 `chatStatsReader.Read` 丢尾巴** —— 这是 **v1.28.3 我自己引入的回归**：为了「数据与错误
  一起返回」而漏了 pend 的存在，`(n, io.EOF)` 一返回调用方就结束，pend 里的字节永远读不出去。
  新增 `pendErr`：EOF 寄存到尾巴吐完之后。
* **#13 日志环每次写入都切片搬家** —— `append` 后 `entries[overflow:]`：容量每写一行少 1，
  容量耗尽整块重分配并拷贝 cap 条（= 每几百条日志一次 500 元素拷贝 + 一次分配）。
  改真环：固定数组 + `head/size`，写 O(1) 零分配，`Snapshot` 按环序展开。
* **#14 `accountRevive` 忽略 `Revive` 的返回值、`Revive` 不清 `needAuth`** ——
  Redis 快照恢复的占位条目只有 UID 没凭证，`healthy()` 因 needAuth 恒 false，
  面板「解冻」后显示已恢复、请求还是 503。`Revive` 一并清 `needAuth`，handler 改用返回值判存在。

### 测试

新增 5 组：`TestRingWrapKeepsOrderAndEvictsOldest`、`TestRingSnapshotBeforeFull`、
`TestTouchDoesNotOverwriteRebind`、`TestAggregateMarksTruncationWithoutDoneOrFinish`、
`TestAggregateKeepsExplicitFinishWithoutDone`（最后这条是反向用例：上游给过 finish_reason
只是没发 `[DONE]` 时**不许**误标截断）。降级门的两条既有用例改成按 key 断言，并补了
「别的客户端不受影响」。全量 **766 通过 / 0 失败**。

## v1.28.3

**本版 = 第十八份审查报告（CODE-REVIEW-BUGS-20261021.md，14 条）。13 条为真、1 条部分属实
（#10 的空 buffer 分支按 `io.Reader` 约定本就合法），全部落实。**

### 高危

* **#1 缺 `uid` 的 auth 文件进池** —— `pool` 以 UID 作 map key，空 key 会让 `auths/` 下所有
  缺 uid 的文件互相顶替（只有第一个能进，其余被 `seenUID` 当重复丢弃），而面板路由
  `/accounts/{uid}/...` 匹配不到空路径段 —— 那个号**能发请求，但复活/禁用/签到/删除全点不动**。
  改为 `LoadDir` 直接跳过并打含路径的 WARN（与 `missing accessToken` 同口径）。
  没有采用「按文件名派生 uid」：那会把假 UID 写进 auth 文件、还会作为 `X-User-Id` 上行。
* **#2 OAuth 登录落盘失败不置终态** —— `MkdirAll` / `SaveAtomic` 两个失败分支只 `return`，
  而 `auth/token` 已成功（上游已消费该 state），defer 又把 `consumed` 复位 → 前端 3 秒一次
  poll 空转到 15 分钟 TTL。与同函数里已有的两个终态分支同构：`completed=true` + `delete`。
* **#3 429 重置时间已成过去 → 冷却 1ms** —— 上游重置时刻落后本机时钟（手机时间偏移、
  容器时区漂移、上游重放旧正文）时，`cappedSoftUntilLocked` 返回 `now+1ms`：被限流的号
  立刻回池，`rotateBackoff` 只等 0.5~1s，同一个号马上又被选中，限流保护形同虚设，
  而且该分支不推进 `softStreak`、连退避都不触发。改为回落到调用方给的退避基数（再按
  `softRateMax` 封顶）。
* **#4 空对象 `{}` 补 `api_key` 写出非法 JSON** —— 注入时无条件拼前导逗号，对 `{}` 得到
  `, "api_key": …`，网关 `Load()` 报 parse config 后 `log.Fatalf` 退出，看门狗按退避无限
  重拉，界面永久「启动中」。分隔符改为按前缀末字符决定（空对象/已含 `{` 时不加逗号）。
* **#5 日志翻代失败后本次运行不再落盘** —— `renameTo` 返回值没看，而 `pump` 起始处
  `writeOn = logFile.length() <= 上限`；翻代失败时原文件仍超限 → 整条流只读不写。
  翻代失败时原地截断（与 `appLog` 的兜底同口径）。

### 中危

* **#6 11115 大小写兜底是死条件** —— 判据 `(m != ToLower(m) && …)` 对全小写的 markers 恒假，
  上游回 `Prompt is too long` / `PROMPT IS TOO LONG` 时判不出来，落成 `ErrNotFound`
  （白冷却账号 60s）或 `ErrClient`（白换一次号）。改为双向比对。
* **#7 归档单文件读失败整页 500** —— `return out, err` 把已读到的记录一起丢掉；并发 prune
  刚删、目录被手工清理、单文件权限丢失都会命中。改为跳过该文件（目录级失败仍在上面上抛）。
* **#8 保留期按 mtime 判断** —— 目录被整体拷贝/从备份恢复/容器换卷后 mtime 全是"现在"，
  `RetentionDays` 永不生效；`MaxBytes` 淘汰同样按 mtime 优先删错文件。改为按文件名日期
  （`requests-<day>[.<idx>].jsonl`，read 侧早已按它排序），日期不可解析时退回 mtime。
* **#9 `maxBuckets` 兜不住内存** —— 折叠只动「早于 90 天」的小时桶，异常流量产生的都是
  「现在」的桶，超限后一个也删不掉。补一条硬上限：超限时按时间淘汰最旧的小时桶（直接丢，
  不折成日桶 —— 它们本就是同一小时里基数爆炸的异常键）。
* **#10 `chatStatsReader.Read`** —— 空 buffer 返回 `(0,nil)` 按 `io.Reader` 约定**本就合法**
  （这条只算部分属实）；但「有数据却把 err 吞到下一轮」确实多余。改为显式短路空 buffer +
  数据与错误一起返回（`n>0` 带 err 是契约允许的）。

### 低危

* **#11 归档读取整文件扫描** —— 注释承诺的「凑够 limit 即停」只在文件之间生效，单文件
  （上限 16MB ≈ 10 万行）仍全量解析。新增 `readFileTail`：64KiB 分块从尾部倒读、凑够即停、
  跨块半行拼接，结果与全量读的最后 N 条逐条一致（有单测钉住）。
* **#12 `load` 对桶数无上限** —— 文件被写坏或合入超大历史后，进程启动即吃满内存，第一次
  flush 还会原样写回。补 `2*maxBuckets` 封顶（按时间保留最新）+ `Scope` 合法性校验。
* **#13 `credits: -1` 哨兵** —— 余额未知时不下发该字段（并给 `credits_ok:false`），
  契约从"靠前端 `>= 0` 判断"改成"缺字段即未知"。
* **#14 环形写仍是 O(n) copy** —— 每请求持锁搬 99 个 `Event`（20 多字段）。改成下标覆盖
  的真环写：写 O(1)，`Snapshot` 时按环序展开一次。

### 测试

新增 9 组：`TestLoadDirSkipsEmptyUID`、`TestCappedSoftUntilExpiredResetFallsBackToBase`、
`TestClassifyPromptTooLongCaseInsensitive`、`TestReadFileTailMatchesFullRead`、
`TestRecentRingOrderAfterWrap`、`TestPruneUsesFilenameDate`、
`TestRollupDropsOldestHourBucketsOverCap`、`TestLoadCapsBucketCountAndSkipsBadScope`
（后两条把 `maxBuckets` 临时压到 5，否则要造 40 万个桶）。
全量 **761 通过 / 0 失败**。

## v1.28.2

**本版 = 第十七份审查报告（BUG-REVIEW-2026-10-21.md，A1-A4 / B1 / C1-C2 / D1-D4 共 11 条）。
逐条复核后**11 条全部为真**，无一条误报。其中 A4 是唯一会造成用户可感知损失的：上游抖一次就吃 1/3 次跳过额度。**

### 高优先级

* **A4 回读失败被当成「进度未变化」→ 好账号+可做任务被砍 7 天** —— `runGrowthQueued`
  与 `accountTaskAutoAll` 两处 `after, _ = p.taskByCode*` 把回读错误丢掉，`after==nil`
  于是 `progressed` 恒 false，调用方 `skipNote` 记一笔「连续无效」；上游每 5xx/超时一次
  吃 1/3 次机会，三次抖动后该 (账号, 任务) 被跳过 7 天 —— 而它什么都没做错。
  新增哨兵 `errTaskReadback` + 判据抽成纯函数 `queueVerdictOf(err, progressed)`，
  三档分流：有进展 → 清零；动作失败/确实没进展 → 计入跳过；**回读失败 → 两样都不做**。
  面板侧另标 `progress_error`，不再把「不知道」显示成「没动」。
* **A1/A2 CI 发布链** —— 版本号回落成 `dev` 导致 `-X main.appVersion=dev` 等于没注入、
  断言 `grep "dev"` 恒真（假绿）、publish 阶段 `TAG != SRC` 任何 tag 都发不出 Release；
  断言/打包/上传三步缺 `working-directory`，产物写进模块目录而校验在仓库根执行。
  改成三级回落（根 VERSION → 子目录 VERSION → `git describe`）+ **取不到就 exit 1**，
  并给三步补齐同一 `working-directory`（`uses` 步骤用显式路径）。
  说明：该 workflow 位于 `workbuddy2api-panel/.github/`，在本仓（根目录不放 .github）
  不会被执行；修的是它自己声明的上游布局。

### 中优先级

* **A3 队列接口吞掉 JSON 解析错误** —— `_ = json.NewDecoder(...).Decode(&body)` 失败后
  仍按默认值真的启动一轮扫描与执行，前端只看到「已启动」。改为解析失败回 400
  （`io.EOF` 仍按默认值跑），把诊断线索还回来。
* **B1 `redisstore.Close()` 的 10s 超时形同虚设** —— 超时只约束第一段等待，
  紧接着无界 `workerWG.Wait()` 又把耗时放开，单个 worker 卡在慢写就能拖穿
  `run.sh` 的 8s 停机预算 → `kill -9` 把最后一次 Flush 夺走。第二段同样套
  `select` + 超时，`Close()` 保证有界返回；两段时长提成变量供测试压到毫秒级。
* **C2 升级后浏览器可能继续跑旧 `app.js`** —— 资源与二进制同体但 URL 没版本串，
  启发式缓存会给出「新后端 + 旧前端」。加 `Cache-Control: no-cache` + 内容 ETag，
  命中 `If-None-Match` 回 304 空体（启动时算一次指纹，不逐请求重算）。
* **C1 到期卡片文案与判据各说一套** —— 过滤条件是「未来有批次」（无 7 天上限），
  空态却写「7 天内无到期积分」。文案改成「无未到期积分」，与判据对齐。

### 低优先级（潜伏雷区）

* **D1** `for _, st := range` 遮蔽外层 `*chatStat st` → 循环变量改名 `as`。
* **D2** `names, infos := ...` 遮蔽命名返回值 → 改赋值，避免日后改裸 return 退化成零值。
* **D3** 构造期 `loadTierLocked()` 无锁调用（注释要求持锁）→ 显式 `p.mu.Lock()`。
* **D4** `model_tier.json.tmp` 固定名 → 复用 `auth.ReplaceAtomic`（唯一 tmp + 原子替换）。

### 测试

* 新增 `TestQueueVerdictOf`（5 条分流，含「回读失败但已确认有进展」）、
  `TestTasksRunQueueRejectsBadJSON`、`TestStaticAssetCacheValidation`（200 + ETag + 304 空体 +
  两个资源指纹不许撞）、`TestCloseBoundedWhenWorkerStuck`（卡死 worker 下 Close 0.06s 返回）。
  全量 **753 通过 / 0 失败**（panel 45→48，redisstore 6→7）。

## v1.28.1

**本版 = 面板「模型与档位」新增 `1017` 版本徽标：WorkBuddy 目录里的 DeepSeek 4.1 Flash
（`cn:` / `global:` / `-sg` 三条线）跟官方直连的 `deepseek-flash` 用同一套判据，一眼看出是哪个版本。**

### 功能

* **1017 版本徽标** —— `mdRowHtml` 的能力徽标行新增一枚 `1017`（新样式 `.tag.info`，中性蓝，
  跟能力徽标的绿/黄分开）。判据 `is1017Model(m)`：先用 `mdModelKey` 归一化
  （去 `cn:` / `global:` 前缀、转小写、空格下划线统一成连字符），再比对
  `deepseek-flash` / `deepseek-v4.1-flash` / `deepseek-v4.1-flash-sg`，id 或 name 命中即标。
  归一化是必须的：上游目录里 id 带 realm 前缀（`cn:deepseek-v4.1-flash`）、
  name 带厂商前缀（`CN:Deepseek-V4.1-Flash`），直接比对一条都命中不了。
  `deepseek-v4-flash`（v4，不是 v4.1）、`glm`、`auto` 一律不标。

### 测试

* 新增 `TestAppJSModel1017Badge`：8 条正反用例（含空对象）+ 归一化脏输入
  （`'  CN: Deepseek V4.1 Flash '` → `deepseek-v4.1-flash`）+ 纯谓词断言。
  全量 **749 通过 / 0 失败**（panel 44 → 45）。

## v1.28.0

**本版 = 第十六份审查报告（BUG-REVIEW-2026-10-20.md，16 条）的落地修复。
逐条复核：14 条为真、2 条误报（#5、#9）。其中 #4 / #10 / #11 / #13 直接指向我最近两版的改动。**

### 高优先级

* **#1 tool 结果重排有缺口：插入物在结果之前时不重排** —— `repackToolResultBlocks` 里
  `if len(results) == 0 { break }` 让「assistant(tool_calls) → developer/system 通知 →
  tool(结果)」这种形态一条都不挪：结果集为空就退出，插入物留在原位，出站载荷仍是
  assistant → system → tool，上游判 **11148（tool_call_sequence_broken）**，客户端每次
  重放同一条历史都失败、换号无效。
  改成「中间消息一律先缓冲」，只在**确实一条结果都没有**时保持原序（零回归）。
  新增两条子用例：`结果未到就先插入的非 tool 消息也要后移`（修复前必挂）、
  `一条结果都没有时保持原序`。【复核】真。

### 中优先级

* **#2「一键完成全部」对每个账号放大 38~57 次任务列表请求** —— 19 个动作各调一次
  `effectiveTaskCode`（内部先 `taskByCode` 1 次、再 `ListTasks` 1 次），主流程又
  `taskByCode` 一次。新增 `loadTaskMap`（默认口径 + mp 合并去重，**每账号一次**）、
  `taskMapByCode`、`effectiveTaskCodeIn`（纯函数），`runAutoAll` 改为一次拉齐后查本地
  map；拉取失败时自动退回逐次查询（行为与旧版一致，只是慢）。【复核】真。
* **#3 `cmd/credit` 的余额口径与网关分叉** —— 它自带一份 `packageRemainUsed` 副本，
  缺 `used > size` 钳制与 `remain = size - used` 反算（v1.27.8 我只修了 upstream 那份）
  → 上游脏数据时 CLI 输出「剩余 50 / 已用 150 / 面额 100」。按 upstream 逐行对齐补齐。
  【复核】真。
* **#4 重复 UID 的告警文案与实际行为相反** —— v1.27.9 我只把日志改成「保留 prev，
  丢弃 f」，但代码仍把两条都 `append`，而 `pool.upsertLocked` 是"后写覆盖" →
  实际保留的是 **f**，日志在说谎。
  改成**真去重**：保留先出现的文件（`files` 已排序，结果确定），日志如实说明丢弃了谁。
  同步更新 `TestLoadDirDuplicateUIDWarning`（它原本断言"两条都返回"）。【复核】真。
* **#6 Android 壳：自愈退避被误判成「系统冻结」** —— `lastTickAt` 在自愈前已更新，
  而 `selfHeal` 里的 `sleep(backoffMs(n))` 退避到 ≥32s（第 5 次起）后，下一轮
  `now-lastTickAt > 3*TICK_MS` 成立 → `freezes++` 并打「系统冻了我 N 秒」，
  把「自己在退避」误报成「被系统冻结」。自愈（进程退出 / 僵死两处）返回后复位心跳基准。
  【复核】真。
* **#7 `ensureConfig`：配置里没有 `api_key` 键时不会补** —— v1.27.8 我只处理了
  「键存在但值是占位/空」，`!m.find()` 时直接 `return`。用户手改删掉该键（`listen` 仍是
  `:7863`）→ 网关 `checkListenKey` 判「非回环 + 空密钥」`log.Fatalf` 退出，看门狗无限重拉，
  界面永远「启动中」。改成在最后一个 `}` 前插入该键（其余内容一个字不动）。【复核】真。
* **#8 积分构成页排序比较器 O(n²·log n)** —— 比较器内部对每个候选重跑 `pkBySource`
  （每次遍历全部账号的全部包），账号 20+ / 包 100+ 时手机打开「积分构成」明显卡顿。
  先建一次「来源 → 最大面额」索引再排序。【复核】真。

### 低优先级

* **#10 全冷却兜底日志的节流标记永不复位** —— v1.27.9 我加的 `lastFallbackLog` 只在
  账号变化时更新，账号恢复后再次进入兜底就不再打日志。新增 `resetFallbackLog`，
  在 `NoteSuccess` 与 `ReenableIfCredits`（解冻 = 恢复）里清标记。【复核】真。
* **#11 `schoolVouchers` 的并发闸门失效** —— v1.27.9 我修了 `packages` 与
  `runQueueItems`，**这是第三处**漏改。同样把 `sem <- struct{}{}` 提到 `go func` 之前。
  【复核】真。
* **#12 `enterpriseResourceCtx` 的 ctx 只作用于重试间隔** —— `retryBillingTransientCtx`
  能被打断，但单次 `billingJSON` 用的是无 ctx 的 `http.NewRequest` + client 120s 超时，
  SIGTERM 时最坏仍等满一次请求，挤压 `run.sh` 的 8s 停机预算。
  新增 `billingJSONCtx` / `billingMeterJSONCtx`（`http.NewRequestWithContext`），
  并把 `enterpriseResourceCtx` / `UserResourceDetailedWithExpiryCtx` / `DailyCheckinCtx`
  三处接上。【复核】真。
* **#13 注释与函数错位** —— v1.27.8 我把 `IsAvailableForModel` 插在
  `AvailableUIDsForRealm` 的文档注释与函数之间，godoc 会读成「IsAvailableForModel 按 realm
  过滤」。注释归位，两个函数各写各的。【复核】真。
* **#14 前端死代码** —— `+ (s.in_flight ? '' : '')` 两个分支都返回空串，删掉。【复核】真。
* **#15 Android 壳：只重写了废弃的 `onReceivedError`** —— 旧签名在部分 ROM/WebView 上对
  **子资源**失败也回调 → `pageHadError=true` → `panelLoaded=false` → tick 反复自动重载
  面板（界面自己闪）。新增 `onReceivedError(WebView, WebResourceRequest, WebResourceError)`
  并只在 `req.isForMainFrame()` 时置标记；旧签名保留给 API < 23（新回调不触发）。
  【复核】真。
* **#16 `watchdog.sh` 自愈后未复位外部实例提示标记** —— 「进程在但不通 → 杀掉重拉」分支
  里 `fail=0` 但 `noted` 不复位，之后切回本进程实例不再提示「已恢复正常」。补 `noted=0`。
  【复核】真。

### 误报（未改，附理由）

* **#5「WAF 分支：请求日志状态码与真实响应码不一致」** —— 报告称日志记 403、客户端收 503。
  实际 `status` 在轮转循环内是**循环局部变量**（`rc, status, respBody, terr := …`），
  而末端 `status := http.StatusServiceUnavailable` 是**另一个变量**：末端
  `writeOpenAIErrorFull(w, status, …)` 之后紧跟 `st.status = status`，写的就是实际发出的
  503。日志与客户端一致，不存在错位。
* **#9「到期卡片『检查』按钮可能永久卡死」** —— `expFetching = false` 位于 `try/catch`
  **之后**、函数末尾：`renderExpiry(d)` 抛的异常会被 `catch` 接住，控制流继续走到那行，
  标志不会卡死。这条与之前某份报告的第 3 条是同一个误判，结论不变。

### 测试

`upstream 213 / server 92 / pool 186 / panel 44 / session 20 / redisstore 6 / reqlog 7 /
auth 28 / usage 14 / scheduler 65 / cmd-server 52`，**727 通过 0 失败**；`gofmt -l` 为 0。

新增 2 条子用例（#1），更新 1 条旧断言（#4：`LoadDir` 现在真去重）。

## v1.27.9

**本版 = 第十五份审查报告（CODE-REVIEW-2026-10-20.md，12 条）的落地修复。
逐条复核：12 条全部为真、0 误报。其中 BUG-01 是我 v1.27.6 的性能优化引入的回归，
BUG-04 / BUG-06 / BUG-09 是「同一类门控/写法只改了一处」的老毛病。**

### 严重

* **BUG-01 凭证文件可能被并发写坏，重启后账号静默消失** —— `ReplaceAtomic` 用**固定**
  临时名 `path+".tmp"`，而 v1.27.6 我把 `SaveAtomic` 的磁盘 I/O 挪出 `a.mu`（性能修复）
  之后，同一账号的并发落盘不再有串行化：A 截断并写完后、B 截断只写了一半，此时 A 的
  `rename` 会把**半截内容**换成正式文件（长度正常、JSON 已坏）→ 下次启动 `LoadDir`
  `Parse` 失败后静默跳过，面板上少一个号且日志无任何提示。
  改成唯一临时名 `path.tmp.<pid>.<seq>`（原子序号）：rename 依然原子，最后一次完整内容胜出，
  不可能交错。【复核】真，且是我上一版引入的回归。
* **BUG-02 构建脚本注入的版本号永远是 `dev`** —— `rebuild.sh` 先 `cd` 到 panel 子目录，
  再用 `dirname "$0"` 找 `VERSION` —— 按文档的 `bash rebuild.sh`（相对路径）执行时
  `dirname` 就是 `.`，指向 panel 目录，`VERSION` 打不开 → 兜底成 `dev` →
  `-X main.appVersion=dev`，面板 `/status` 的 version 恒为 dev。
  改成在 `cd` 之前算 `SELF_DIR="$(cd "$(dirname "$0")" && pwd)"`，并加一条找不到时的告警。
  实测：`VER=1.27.8`（修复前是 `dev`）。【复核】真。
* **BUG-03 `credit.sh` / `signin.sh` 指向不存在的二进制** —— `rebuild.sh` 把产物写到
  `$PREFIX/lib/workbuddy2api/{server,signin,login,credit,trial}`，而 `credit.sh` 执行
  仓库内的 `./credit`（不存在）；`signin.sh` 现场 `go build`，而本机 `go` 不在 `PATH`
  （在 `$PREFIX/lib/go/bin`），`set -e` 下直接退出。
  两个脚本都改成「先找当前目录（Docker 镜像里的形态）→ 再找 `$PREFIX/lib/workbuddy2api/`」，
  都找不到才报错；`signin.sh` 的 `go build` 兜底路径补上 `PATH`。
  实测：`bash credit.sh -json` 正常输出 JSON、`bash signin.sh /tmp/x` 正常打印
  `no auth files`。【复核】真。
* **BUG-04「一键完成全部任务」缺国际版门控** —— v1.27.8 我给 `accountTaskAuto`（单任务）
  加了 `IsGlobal()` 门控，**`accountTaskAutoAll` 漏了**：对 global 号点「一键完成全部」
  会跑满 19 个动作、每个打一次上游全是 403，还长期占着 per-account 锁。
  补同一段门控（与 `accountTasks` 同口径）。【复核】真。

### 中等

* **BUG-05 `taskDone` 漏判「已完成未领」** —— 上游解析侧早已把「进度达标」与
  `accept_status=completed` 统一成 `Claimable`，而 `taskDone` 仍自己判
  `Target>0 && Current>=Target`：一次性任务（`target=0`、完成状态只在 accept_status 上）
  已达标未领时被判"没做完" → 整条动作链重跑（`expert_5` 是 5 次真实对话 + 5 组召唤事件）。
  改成 `t.Claimed || t.Claimable`（已确认全仓只有一处构造 `Task`，必带 Claimable）。
  【复核】真。
* **BUG-06 单任务入口查跳过名单用固定任务码** —— 跳过名单的 key 是 `uid|实际任务码`，
  `runAutoAll` 与队列路径都用 `effectiveTaskCode`，只有 `accountTaskAuto` 在算出
  `effCode` **之前**用 `act.TaskCode` 查了一次 → 按域换码的任务（`WorkBuddy_Valorant`）
  永远命中不了，"做不了"的任务照样白跑一遍。
  把 `effCode` 提前到 `skipBlocked` 之前（顺带避免"拿着账号锁打上游"）。【复核】真。
* **BUG-07 会话分配慢路径持会话写锁扫全池** —— 慢路径在 `r.mu.Lock()` 内部调用
  `usable()`（→ pool RLock）和 `availableSlice(model)`（全池扫描 + 排序），
  于是所有其他会话的命中 / `Bind` / `Unbind`（handler 每请求一次）都要排队；
  而 `pool.Flush`（每 5s 持池写锁做磁盘 IO）一拿住池锁，整张会话表就跟着被堵在磁盘 IO 后面
  —— 表现为面板「偶发整秒卡顿」。
  改成**锁外取可用集合**、锁内只读本地 `avail` map。【复核】真。
* **BUG-08 抢在途名额失败仍记为「本轮已试」** —— `tried[uid]=true` / `st.uid` / `st.nick`
  写在 `Acquire` **之前**：抢失败时该号既没被使用、又被排除出本轮后续轮转（可用号被浪费），
  流水行还会把这次请求记到没服务成功的号头上。三行移到 `Acquire` 成功之后。【复核】真。

### 轻微

* **BUG-09 信号量在 goroutine 内部获取** —— `packages` 与 `runQueueItems` 仍是
  "先起 N 个 goroutine、N-3 个阻塞在 sem 上"（`syncNicknames` 在 v1.27.8 已修过）。
  两处都改成**先抢槽再起 goroutine**。【复核】真。
* **BUG-10 全池冷却兜底每次选号都打一条 WARN** —— 这是**每请求**路径，整池软冷却时
  几秒就能刷满面板 500 行环形缓冲，把任务/错误日志全挤掉。
  改成只在「兜底账号变了」时打印（`p.mu` 已由调用方持有，字段读写安全）。【复核】真。
* **BUG-11 权威余额写入不清扣费余数** —— `SetCredits` / `SetCreditsDetailed` /
  `ReenableIfCredits` 只覆盖 `credits`，`creditFrac`（内插扣费的不足 1 分余数）仍留着，
  后续 `NoteModelCost` 从旧余数继续累加 → `credits` 与真实余额长期差不到 1 分，
  `credit_floor` 触底判定略早/略晚。三处都补 `e.creditFrac = 0`。【复核】真。
* **BUG-12 `LoadDir` 重复 UID 告警直读昵称、且看不出保留谁** —— 改用
  `NicknameValue()`，并把日志改成「保留 X，丢弃 Y」，事后可追溯。【复核】真。

### 测试

`upstream 213 / server 91（+1 skip）/ pool 186 / panel 44 / session 20 / redisstore 6 /
reqlog 7 / auth 28 / usage 14 / scheduler 65 / cmd-server 52`，
**726 通过 + 1 跳过（`TestModelsEndpoint`：上游目录不可达，v1.27.8 起按设计跳过）+ 0 失败**；
`gofmt -l` 为 0。

实测验证：`VER=1.27.8`（BUG-02 修复前为 `dev`）、`bash credit.sh -json` 正常输出、
`bash signin.sh` 正常定位到 `$PREFIX/lib/workbuddy2api/signin`。

## v1.27.8

**本版 = 第十四份审查报告（REVIEW-BUGS.md，13 条）的落地修复。逐条复核：
12 条为真、1 条（BUG-12）机制不成立。其中 BUG-08 是我上一版（v1.27.6）修复的漏洞。**

### P0

* **BUG-01 国际版（global）账号在面板仍显示「签到」「任务」，点了必然报错** ——
  调度器侧一直有 D4 门控（`if a.IsGlobal() { continue }`），但面板的单号入口
  （`accountCheckin` / `accountTasks` / `accountTaskAuto`）只判了 `IsEnterprise`，
  前端按钮也只看 `s.enterprise`。于是 global 号点签到 → 上游 404、点任务 → 403，
  toast 里一串上游错误，看起来像网关坏了。
  后端三处补 `IsGlobal()` 门控（签到退化为「只刷新额度」并带 `checkin_skipped`），
  前端按 `s.realm === 'global'` 一并隐藏签到/任务按钮（余额按钮保留）。
  【复核】真。
* **BUG-02 快过期分桶用 `CycleEndTime`，与 `CreditPackages` 的 `DeductionEndTime` 冲突** ——
  `UserResourceDetailedWithExpiryCtx` 的 Accounts 结构**根本没有 `DeductionEndTime` 字段**，
  `earliestAt`/`expiring` 全由 `CycleEndTime` 决定；而 `CreditPackages` 的注释明确写着
  「`DeductionEndTime` 才是真失效时刻，`CycleEndTime` 只是周期边界」。
  结果：真快失效的包拿不到 ×3 加权（到期作废），周期重置点近的包反而被优先消耗。
  结构体补 `DeductionEndTime`/`ExpiredTime`/`PackageEndTime`，解析按
  `DeductionEndTime > ExpiredTime > PackageEndTime > CycleEndTime` 兜底，两条链路统一。
  【复核】真。
* **BUG-03 APK：已有 config.json 时不检查占位 api_key，网关起不来且看门狗无限重拉** ——
  `ensureConfig` 见文件非空就早退。若首次生成被打断、或用户手改留下 `CHANGE_ME`/`test_key`/空串，
  网关的 `checkListenKey` 直接 `log.Fatalf`（rc=1），看门狗按退避无限重拉也永远起不来，
  界面只有「启动中」。
  改成：文件存在时解析出 `api_key`，**仅当它仍是占位/空**才原地替换（其它配置一个字不动），
  并写一条 app.log。【复核】真。
* **BUG-04 APK：`injected` 先置真再注入，失败后永不重试 → 面板永久 401** ——
  `if (!injected) { injected = true; injectKey(); }`：读不到 key（首次启动 `ensureConfig`
  还没落盘 / 文件被清）时只弹一次 toast，此后 `onPageFinished` 不再注入，
  所有 `/panel/api/*` 一直 401，只能手点「重载面板」再赌一次同样的时序。
  改成**成功才置位**（`onReceiveValue` 里 `ok` 分支），失败分支保持 false 以便下次重试。
  【复核】真。

### P1

* **BUG-05 定时余额刷新丢掉了 ctx** —— 定时分支调 `RunBalanceRefreshNow()`（内部
  `context.Background()`），而我 v1.27.6 刚加的 `RunBalanceRefreshNowCtx(ctx)` 没人用。
  SIGTERM 后若正赶上一轮刷新，`sem` 抢占与账号间请求都不被打断（最多 46×30s），
  把最后一次 `Flush` 挤进强杀窗口。改成调 Ctx 版本。【复核】真（又是我"只接了一半"）。
* **BUG-06 面板「全部接受」：mp 任务不分片、成功后无条件白睡 1.05s** ——
  Web 侧按 20 条分片，mp 侧一次全量提交（任务变多时上游可能整批拒收），
  且成功后无论后面还有没有事都 `sleepAB(1.05s)`。改成与 Web 分支对齐：
  分片 + 只有还有下一批才节流。【复核】真。
* **BUG-07 夜猫子对话：不可取消的 `time.Sleep(4s)`，且最后一轮白等** ——
  每轮结束固定睡 4s（最后一轮也睡 → 3 次对话固定多耗 4s），且不可取消
  （`RunNightChats` 无 ctx）。新增 `RunNightChatsCtx`，最后一轮不睡、等待可被 ctx 打断；
  scheduler 改调 Ctx 版本。【复核】真。
* **BUG-08 `NoteModelCost` 在余额不足时把扣费余数吞掉** —— **我 v1.27.6 的修复有漏洞**：
  `credits=1` 而本次消费 `3.5` 时，先按未钳制的 `d=3` 扣了余数、再把 `d` 钳成 1 扣余额 ——
  另外 2 分**既没扣余额也没留在余数里**，插值余额凭空高估 2 分 → `credit_floor` 延后触发，
  正是保底要防的"打穿"。
  改成 `d > e.credits` 时余数清零、只扣现有余额（欠的部分已无从抵扣）。【复核】真。
* **BUG-09 `rebuild.sh` 编译失败仍以 0 退出** —— 循环里只打印 FAIL 就继续，末尾没有 exit，
  接在 CI/发版流水线里就是"构建失败但流水线绿"。加 `rc` 累计 + 末尾 `exit "$rc"`。【复核】真。

### P2

* **BUG-10 桌面事件 `X-Request-ID` 同微秒可能重复** —— `deriveID(a,"req")` 对同账号恒定，
  后缀只有 `UnixNano()%1e6`（6 位微秒尾数），同账号同微秒两次上报会撞 ID。
  加进程内 `atomic.Uint64` 序号。【复核】真。
* **BUG-11 `syncNicknames` 为每个账号起 goroutine，sem 在 goroutine 内抢** ——
  46 个号 = 46 个 goroutine、43 个阻塞在 `sem` 上，"并发上限"形同虚设。
  改成派发循环里先抢信号量再起 goroutine。【复核】真。

### 误报（未按报告改，附理由）

* **BUG-12 三个脚本 shebang 在本机不存在** —— 机制不成立：`/sdcard` 是 **noexec** 挂载、
  文件也没有 +x，`./signin.sh` 在读到 shebang 之前就 `Permission denied`
  （实测：`bash: ./signin.sh: Permission denied`）。所谓 `bad interpreter` 在这台机器上
  **根本走不到**。
  而且这三个脚本是 **Docker 镜像里要用的**（`COPY login.sh signin.sh credit.sh /app/` +
  `chmod 755`，镜像内 `apk add bash`），把 shebang 改成 Android 绝对路径会**直接破坏容器部署**。
  处理：shebang 统一成可移植的 `#!/usr/bin/env bash`（原 `signin.sh` 是 `#!/bin/bash`），
  并在三个文件头加一行使用说明（容器内 `./x.sh`；本机 `bash x.sh`）。

### 附带：把一条依赖外网的 flaky 测试修成"不通就跳过"

`TestModelsEndpoint` 之前用 `upstream.New()` 真打外网（`/v1/models` 是**纯动态、无静态兜底**
的目录），网络抖动时会卡满 10s 超时再变红 —— 我这轮就撞到一次（10.01s FAIL，重跑 0.65s 通过）。
改成「拿不到目录就 `t.Skip` 并说明是环境问题」，不再伪装成回归。

### 测试

`upstream 213 / server 92 / pool 186 / panel 44 / session 20 / redisstore 6 / reqlog 7 /
auth 28 / usage 14 / scheduler 65 / cmd-server 52`，**727 通过 0 失败**；`gofmt -l` 为 0。

## v1.27.7

**本版 = 第十三份审查报告（17 条）的落地修复。逐条复核：17 条全部为真、0 误报。
#1 是上一版（v1.27.6）我自己引入的；另外顺手修了一个更严重的问题 ——
本机 14 个前端测试**一直在静默跳过**，这类 bug 就是这么漏出去的。**

### P0

* **1 前端「运行日志」空列表时抛 ReferenceError** —— `renderRequestTable()` 里引用 `reqErr`，
  但它只是 `loadLogs` 的局部变量（我上一版为修 #7 引入的形参）。空列表（归档关闭 / 无请求 /
  筛选无命中，**最常见场景**）时直接 `ReferenceError`，`tb.innerHTML` 不清空、`reqCount` 不更新，
  异常冒泡到 catch 后页面显示「日志读取失败：reqErr is not defined」。
  改成模块级 `reqErrText`（`loadLogs` 赋值、`renderRequestTable` 读取），去掉那个形参。
  【复核】真，且是我上一版引入的。
* **2 客户端断连被记成 503 / http_error** —— 断连分支设了 `OutcomeInterrupted` 后 `break`，
  却直接落进末端错误路径：既向已断开的连接写一次 503 JSON，又把 `OutcomeInterrupted`
  覆盖成 `OutcomeHTTPError`、`st.status` 改成 503 —— 流水行与 reqlog 把「用户主动取消」
  记成「服务端 503」，中断率统计与排障全部失真。
  在末端写响应前加 `if r.Context().Err() != nil { 保留 Interrupted; return }`
  （兜住所有 break 出口，含 `rotateBackoff` 判定的那处）。【复核】真。
* **3 非流式「上游中途断流」分支永远不可达** —— `errUpstreamRead` 只在 `StreamHint` 里包装，
  `Aggregate` 从不包装；于是 `IsUpstreamReadError(err)` 恒假，非流式断流一律落到
  `upstream_parse` 502，错误码与流式路径不一致。`Aggregate` 读循环里包装该错误。
  【复核】真（这条也是我上一版加分支时没配套）。
* **4 安卓壳杀进程只发 SIGTERM，无强杀兜底** —— `killProc()` 只 `destroy()`。Go 侧若卡在
  优雅停机（在途 SSE 排空 / 锁竞争）就会留下一个**占着 7863 的旧进程**：新进程 bind 失败
  立刻退出，看门狗反复自愈也救不回来，只有系统杀掉 App 才恢复。
  `killProc` 后起 3s 延时强杀线程（API 26+ 用 `destroyForcibly`，更低版本退回 `destroy`），
  `restartGateway` 的 8s 等待窗口结束时再补一次。【复核】真。

### P1

* **5 任务队列执行时无锁遍历 `q.items`** —— `for i := range q.items` 读 slice header 未持锁，
  与 `queueMarkAt` 的元素写、以及下一轮 `startGrowthQueue` 对 `q.items` 的整体替换构成
  数据竞争。改成遍历**入参快照**，并给按下标写入加 `seq` 守卫（队列已被新一轮替换时丢弃）。
  【复核】真。
* **6 一键完成里 mp 任务不做异步计分等待** —— 非 mp 走 `taskByCodeWaiting`（4×3s 轮询等
  上游异步计分），mp 只读一次；上游计分延迟 3~8s，于是 mp 任务在「一键完成」里几乎总是
  `claimable=false`、奖励不自动领，而单任务入口能领到。抽出
  `taskByCodeWaitingWith`，新增 `taskByCodeWaitingMP` 复用同一套轮询。【复核】真。
* **7 清理孤儿 tool_call 后可能留下 `content:null` 的空 assistant** —— 整批 `tool_calls`
  被裁掉时只 `delete(msg,"tool_calls")`，消息可能变成 `{"role":"assistant","content":null}`，
  这类空 assistant 在部分上游被判参数非法，而清理本身就是为了让请求能过。
  改为把 null/缺失的 `content` 补成空串。
  （**没有删整条消息**：那会改变对话结构、可能变成连续两条 user；既有测试也明确断言
  「assistant 保留」。报告给了两个选项，这里取更保守的那个。）【复核】真。
* **8 `run.sh stop` 无论是否杀成功都删 pid 文件** —— `kill -9` 之后进程仍可能活着
  （僵尸/无权限），`rm -f "$PIDF"` 照样执行，之后 `status/stop` 再也定位不到它。
  改成只有 `kill -0` 失败（确认已退出）才删，否则保留并提示。【复核】真。
* **9 `run.sh restart` 用相对路径递归调用自身** —— `bash "$0" stop`，从别的目录执行时
  找不到脚本，restart 静默只跑一半。开头算 `SELF` 绝对路径，restart 改用 `$SELF`。
  【复核】真。

### P2

* **10 `doJSON` 在 HTTP 200 上无法识别 11102/11115/11135** —— 这三类判定都带 400/404/413
  状态门槛，而 doJSON 拿到的常是 200 + 业务 code → 全部退化成 `ErrClient`（上一版 B8 的修复
  实际只有 14018/11101 受益）。新增 `businessCodeStatus()` 合成**语义状态码**只用于分类，
  `Error.Status` 仍报真实 HTTP 状态（避免影响调用方按 Status 的分支）。【复核】真。
* **11 `ChatStreamContext` 用 `panic` 做不可达兜底** —— HTTP 路径有 net/http recover，但
  `RunNightChats` / `runModelChat` 这些后台 goroutine 没有 → 一次 panic 打死整个网关。
  改成返回错误。【复核】真。
* **12 面板导入的 32MB 上限与 multipart 解析撞边界** —— 外层 `MaxBytesReader` 是 32MB，
  而 multipart 有 boundary 与头开销，一个正好 32MB 的导出文件会在 `ParseMultipartForm`
  处被截断，报 `unexpected EOF`（语法错误而非「文件太大」）。阈值留 1MB 余量。
  【复核】真。
* **13 会话粘性每条请求做两次全池扫描 + 排序** —— 快路径命中也要先 `availableSet(model)`
  （池 RLock + 全池遍历 + 排序），慢路径再来一次。新增 `pool.IsAvailableForModel(uid, model)`
  与 `session.Config.IsAvailableForModel`，快路径改单点校验（O(1)），慢路径才建全集。
  【复核】真。
* **14 `cost_explore_interval` 负值钳 0 后未回写字符串** —— 与 `expiring_soon` 的处理不一致：
  配置里仍留 `"-5m"`，面板保存后写盘还是负值、重启再钳一次。补回写 `"0"`。【复核】真。
* **15 `WriteDefault` 落盘缺 `Sync`** —— 这是 O_EXCL 生成的「唯一一份密钥」，掉电/强杀时
  可能留下 0 字节或半截 config.json，下次启动 `Load` 报 parse config 且密钥无处可寻。
  `Write` 后补 `Sync()`。【复核】真。
* **16 面板 `models` 与 `mdTier` 开关参数无效** —— `loadModels(true)` 而 `loadModels()` 没有
  形参。面板 models 端点每次直连上游、没有客户端缓存，`force` 无意义 —— 删掉实参并注明。
  【复核】真。
* **17 `taskAcceptAll` 失败批次不节流** —— 成功批次间 `sleepAB(1.05s)`，失败分支 `continue`
  直接跳下一批，可对上游连发多次 accept（与「防风控」意图相反）。改成无论成败都在
  「还有下一批」时等。【复核】真。

### 附带修复：本机 14 个前端测试长期静默跳过

`nodeBin()` 探针发现 node 不可直接 exec 就 `t.Skip` —— 而 Android app 私有目录是 noexec，
**所有** JS 测试在这台机器上从来没跑过（`TestAppJSSyntax` / `TestAppJSTopLevelSmoke` /
`TestAppJSRequestLogFormatting` 等 14 个）。v1.27.6 引入的 #1 就是这么漏出去的。

- 新增 `nodeCmd()`：必要时自动改走 `/system/bin/linker64 <node> …` 代跑；17 处调用点统一。
- 钉 `TZ=Asia/Shanghai`（面板按浏览器本地时区渲染，不钉的话 epoch 断言随宿主时区漂 8 小时）。
- 新增 `TestRenderRequestTableWithEmptyEntries`：显式调 `renderRequestTable()`（空列表 +
  有错误文案两种），顶层 smoke 抓不到这类「函数被别处调用才炸」的问题。

结果：panel 从「28 通过 + 14 跳过」变成 **44 通过 + 0 跳过**。

### 测试

`upstream 213 / server 92 / pool 186 / panel 44 / session 20 / redisstore 6 / reqlog 7 /
auth 28 / usage 14 / scheduler 65 / cmd-server 52`，**727 通过 0 失败 0 跳过**；`gofmt -l` 为 0。

## v1.27.6

**本版 = 第十二份审查报告（13 条）的落地修复。逐条对当前代码复核：13 条全部为真、0 误报。
其中 #3/#4/#5/#6/#9 与本会话前几轮改动同源 —— 我把机制加了一半。**

### P1

* **1 非流式请求把上游超时误报成 502 `upstream_parse`** —— 客户端发 `stream:false` 时网关
  内部仍强制 `stream:true` 打上游、再用 `Aggregate` 读完；上游卡住被空闲看门狗 cancel 或
  首字节超时时拿到的是 `context canceled` / `i/o timeout`，而这里直接回 502
  `upstream_parse` —— 既不叫 `upstream_timeout`，也没走「超时不换号、不罚号」的止损
  （那套只写在流式/传输层分支）。补 `isUpstreamTimeout` 判定 → 走末端统一映射
  （503 `upstream_timeout`）；顺带把 `IsUpstreamReadError` 归到 502 `stream_error`，
  与流式路径一致。【复核】真。
* **2 usage.json 落盘没有 bind mount 兜底** —— `flush()` 用裸 `WriteFile`+`Rename`，
  而 pool/auth/config 都处理了「单文件 bind mount 上 rename 恒 EBUSY」，usage 一直漏着。
  用 `-v ./data/usage.json:...` 部署时每 30 秒落盘都失败，用量只活在内存、重启即清零。
  改用 `auth.ReplaceAtomic`。【复核】真。
* **3 登录：UID 非法分支不清理 state** —— v1.27.5 我修了 `acct.UID == ""` 分支（视作终态），
  但**紧挨着的 `!validUID(acct.UID)` 分支漏了**：同样在 `auth/token` 已成功之后，
  同样复位 `consumed`，前端 3 秒一次空转到 15 分钟 TTL。改成同构终态处理。
  【复核】真（我上一轮只修了一半）。
* **4 套餐聚合 `used > size` 时 remain 不回算** —— `if size >= used { remain = size - used }`
  只在 size 足够时回算，上游对账延迟给出 `CycleCapacityUsed > CycleCapacitySize` 时
  remain 保持旧值 → `remain+used > size`，面板出现「剩余 50 / 已用 150 / 面额 100」这种
  自相矛盾的行，`credit_floor` 与选号权重也跟着高估余额。改成先把 used 钳到 size、
  再由 used 反算 remain（恒等式成立）。【复核】真。
* **5 `expiresIn` 先乘后判导致溢出** —— `time.Duration(tok.ExpiresIn)*time.Second < 上限`
  是"乘完再判"，`ExpiresIn > 9.22e9`（约 292 年）时乘法 int64 溢出成负数 → 负数恒小于
  上限 → 校验通过 → `ExpiresAt` 落在过去 → `NeedsRefresh` 恒真 → **每个 chat 请求前都先打
  一次 refresh**，把 refresh 端点打爆，还可能触发 12153 三振禁用。
  改成先按秒比较再乘；`login.go` 侧原本连上限都没有，一并补上（10 年同口径）。
  【复核】真。

### P2

* **6 归档关闭时也在累加 `dropped_writes`** —— `enqueue` 里 `if !w.enabled() { dropped.Add(1) }`，
  而归档配置上没开时 `enabled()` 恒假 → **每条请求都算一次丢弃**；`stats()` 又在
  `if !Enabled { return }` 之前写 `DroppedWrites`。面板显示"JSONL 归档已关闭"而指标一直涨，
  接监控的人会以为归档在丢数据。改成区分 `configured()`（没开 = 不计数）与
  `closed`（停机窗口 = 计数），并把 `DroppedWrites` 移到启用判断之后。【复核】真。
* **7 请求记录接口失败被吞成「空表」** —— `api('request_logs?…').catch(() => ({entries: []}))`
  把 500/超时/密钥失效静默换成空数组，表格显示「暂无请求记录」、计数 0 条。
  改成收集错误并透出：空表文案变「请求记录读取失败：<msg>」。【复核】真。
* **8 模型列表顺序每次刷新随机** —— `fetchV3Models` 直接 `for _, mi := range byID`（map 迭代序
  随机），于是 `/v1/models` 与面板「模型与档位」每 10 分钟缓存过期就重排一次，
  `mergeModelInfos` 承诺的「输出顺序 = primary 原序在前」不成立。改成按 id 排序后输出。
  【复核】真。
* **9 `monitorBody` 在禁用空闲监控时丢 `cancel`** —— `if idle <= 0 { return rc }` 把
  `ChatStreamContext` 派生 ctx 的 cancel 直接丢掉，reqCtx 只能等父 ctx 结束才释放
  （长连接场景每请求留一个 ctx 节点，`go vet` 的 lostcancel 也会报）。
  改成包一层 `cancelOnCloseBody`（Close 时 cancel）。
  【复核】真。同步更新了断言"返回原对象"的旧测试 —— 新契约是"不启动监控 + Close 释放 cancel"。
* **10 跳过名单保存失败被静默吞掉** —— `_ = os.MkdirAll(...)`、`_ = os.Rename(...)` 全吞错，
  目录不可写/磁盘满/EBUSY 时「反复无效 → 跳过 7 天」只留内存，重启后名单回退，
  被判"做不了"的任务又被重新执行（每次都是真实上游调用）。
  改用 `auth.ReplaceAtomic` + 失败打 WARN。【复核】真。
* **11 锁外直读 `a.Nickname` 与 `SetNickname` 数据竞争** —— `auth` 包为此专门加了
  `NicknameValue()`（注释写明"这是真实数据竞争"），但 upstream（日志/请求体）与
  panel（队列行/诊断）共 13 处仍直读字段。全部改为 `NicknameValue()`，
  请求体构造处先取一次快照。顺带新增 `ExpiresAtValue()`（同类锁外读）。【复核】真。
* **12 活跃上报的 conversation id 同毫秒撞车** —— `fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())`
  在逐号循环里，同毫秒处理 2 个以上账号就拿到同一个 conversation id；上游按 conversation
  聚合活跃事件，重复 id 可能被去重 → 「报了 N 个号，连登只涨了一部分」。拼上 `logfmt.UID8`。
  【复核】真。
* **13 `SaveAtomic` 持锁做磁盘 I/O** —— 全程 `a.mu.Lock()` 内做 ReadFile + Marshal + 写文件 +
  rename，而 `pool.pick` 持池写锁调用 `e.a.Realm()`（要 `a.mu.RLock`）→ 一次 token 落盘就把
  全池选号堵住；22:00 全量保活 46 个号依次落盘时 chat 出现周期性卡顿。
  改成**锁内只取快照**（`authFileSnapshot`）、`writeAuthSnapshot` 在锁外做全部 I/O。
  【复核】真。

### 测试（+4 个回归，改 1 个旧断言）

- `TestPackageRemainUsedKeepsIdentityWhenUsedExceedsSize`（#4：三种形态都断言
  `remain+used == size`，含脏数据 used>size）
- `TestRefreshTokenIgnoresOverflowingExpiresIn` + `TestRefreshTokenAppliesReasonableExpiresIn`
  （#5：9.3e9 秒必须被忽略、正常 3600 秒仍生效）
- `TestMonitorBodyDisabledWhenIdleZero` 改写（#9：不启动监控 + Close 必须调 cancel）

`upstream 213 / server 92 / pool 186 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 28 / usage 14 / scheduler 65 / cmd-server 52`，**711 通过 0 失败**；`gofmt -l` 为 0。

## v1.27.5

**本版 = 第十一份审查报告（18 条 + 1 条"待确认"）的落地修复。逐条对当前代码复核：
19 条全部成立，0 误报。其中 #3/#4/#5/#6/#18 与本会话前几轮的改动直接相关
（我加了机制却没在所有消费点同步）。**

### P1

* **1 `max_completion_tokens` 非整数形态被静默丢弃** —— 函数第一行就 `delete` 别名，
  之后才做类型判断：小数（1000.5）与字符串（"1000"）既没翻译也没保留 →
  上游回落自己的默认上限（长任务被截），而注释承诺"非数值别名原样透传由上游报 11101"。
  改成「能翻成正整数就翻（小数四舍五入），翻不了就保留别名交给上游判定」。
  【复核】真。
* **2 `reasoning_content` 为空串时没补齐** —— 客户端普遍会发 `reasoning_content: ""`，
  原实现走 `hasRC` 分支什么都不做，只把 `reasoning` 写成 `" "` → `reasoning_content`
  仍是空串。而本文件顶部注释明确写着「上游 len>0 不 trim：空白串过闸、**空串不过**」，
  这类请求在该账号上照样 400。改成空串也补 `" "`。
  同步更新了两个把旧行为固化的测试（它们断言的是"字段存在即可"，而非"非空"）。
  【复核】真。
* **3 签到/余额的"可取消重试"从未接线** —— v1.27.1 我加了 `retryBillingTransientCtx`，
  但三个调用点全走 `retryBillingTransient`（内部固定 `context.Background()`），
  可取消版本是死代码。新增 `DailyCheckinCtx` / `UserResourceDetailedWithExpiryCtx` /
  `enterpriseResourceCtx`，并把 **scheduler** 的签到与余额刷新（ctx 在手）接到 Ctx 版本 ——
  SIGTERM 时不再把 2s+4s 睡满，与 `srv.Shutdown(5s)` 抢时间的问题解决。
  【复核】真（这正是我上一轮"修了机制没接线"）。
* **4 占位号给"模型可服务"作证** —— v1.27.4 我加了 `needAuth` 标记，但
  `ModelBlockedForRealm` 只跳过 disabled/paused：一个永远选不到的占位号会让
  `!modelCooled` 命中并直接返回空状态 → 全池号都被模型级冷却挡住时被误报成
  "池子没号"（503 `no_healthy_account`，客户端无限重试），正确语义是
  400 `model_unavailable`（换模型才有用）。补 `|| e.needAuth`。【复核】真。
* **5 模型锁池视图把占位号算进分母** —— `routable[realm]++` 无条件统计，
  而 `Servable` 用 `healthyForModel`（排除 needAuth）→ `Locked < Total` 恒成立 →
  状态被判 `starved`（"等一下就好"），实际是 `locked`（换模型才有用）。
  两处都跳过 needAuth。【复核】真。
* **6 占位号在 /status 与面板概览里被算成"冷却中"** —— `countsDetailedForRealm`
  只有 disabled/paused/!healthy/healthy 四档，占位号落到 `cooling`。运维看到
  "有 N 个号在冷却"，真实原因是"凭证还没加载"，排查方向完全不同。占位号改计 disabled 档。
  【复核】真。
* **7「一键完成全部任务」用固定任务码** —— `runAutoAll` 全程 `act.TaskCode`，
  而单任务入口用 `effectiveTaskCode`。国际服 Valorant 在"一键完成全部"里
  `before == nil` → 记"该账号无此任务"，同一任务用单任务按钮却能跑通。
  循环内解析一次 `effCode` 并贯穿查询/回读/领奖/跳过名单门控。
  【复核】真（与 v1.27.2 修的 B6 是同一个病灶的另一半）。
* **8 登录轮询：token 成功但取 uid 失败被当成"可重试"** —— `auth/token` 成功即意味着
  上游已消费该 state；随后 `auth/account` 失败时若按可重试处理（复位 `consumed`），
  下次轮询再打 token 端点只会拿到 pending/报错，前端 `{done:false}` 一直循环到 15 分钟
  TTL，用户拿不到"重新发起"的提示。改成终态：`completed = true` + 删除会话 + 明确文案。
  【复核】真。
* **9 风控闸门头两种拼写出站** —— `h.Set("x-codebuddy-request","1")` 被 Go 规范化成
  `X-Codebuddy-Request`，而 headers.go 是 `X-CodeBuddy-Request`。统一成后者。
  【复核】真。
* **10 连登兑换日志用全量 UID** —— 全仓其它日志一律 `logfmt.Label(...)`（uid8），
  这一行直接打完整 UID。改齐。【复核】真。

### P2

* **11 `SaveAtomic` 只保留顶层未知键** —— 注释承诺"保留用户/插件手写的未知键"，
  实现却是 `doc["auth"] = map{...}` 整组替换，子对象里手写的扩展键一次 refresh 就没了。
  改成「已知键覆盖、未知键保留」的子 map 合并。【复核】真。
* **12「需重启字段」比对口径不一致** —— `prev` 是**原始文件**、`merged` 是**规范化后**
  的结构；配置文件缺省某键时规范化会补默认值，`DeepEqual(nil,"120")` 直接不等 →
  手写的最小配置（只写 listen/api_key/auth_dir）**什么都没改也会被告知需重启**。
  新增 `normalizeForCompare` 让两侧同口径。【复核】真。
* **13 非流式成功路径不看写客户端失败** —— `_, _ = w.Write(raw)` 忽略错误后无条件记
  200 success；客户端写一半断开（Agent 点停止）时流水行/reqlog/usage 全记成功。
  流式路径早有 `errClientWrite` 判定，非流式漏了。补 `OutcomeInterrupted`。
  【复核】真。
* **14 面板多处 goroutine 没有 panic 兜底** —— `syncNicknames`、`packages`、
  `tasksScanAll`、`startGrowthQueue` 扫描、`schoolVouchers` 五处由 handler 起但不是
  net/http 的 goroutine，recover 管不到 → 一次 panic 打死整个网关。逐处补 `defer recover`。
  【复核】真（上一轮修了批量端点，这五处漏了）。
* **15 面板"小时"非法项变 `null` → 后端当 0 点** —— `9, 2x` → `[9, NaN]` →
  `JSON.stringify` 变 `null` → Go `[]int` 反序列化保持零值 → 任务被悄悄排到 00:00，
  前端零提示。改成提交前校验并 `throw`（调用点在 try 内，会走 toast）。
  【复核】真。
* **16 `watchdog.sh` 单实例锁只看 `kill -0`** —— v1.27.3 我加锁时没校验命令行，
  pid 被复用就会认定"已在运行"直接 exit 0，实际没有看门狗在跑。
  补 `/proc/<pid>/cmdline` 校验（与 run.sh 的 B18 修复同口径）。【复核】真。
* **17 脚本只认嵌套形 auth 文件** —— Go 的 `auth.Parse` 同时支持嵌套形与扁平形，
  但 `task_common.py` / `probe_active.py` 写死 `d["auth"]`，手写扁平文件直接 KeyError
  中断整批任务。改成 `d.get("auth") or d`。【复核】真。
* **18 并发刷新模型页时 global 域被误报"上游未返回模型"** —— v1.27.1 我加的 in-flight
  去重对后来者返回 `nil,nil`，而 panel 把"infos 为空"一律写成"上游未返回可用模型"。
  新增 `Client.GlobalProbeInFlight()`，panel 区分"探测进行中"与"真的空"。【复核】真。

### 待确认项（已采纳）

* **19 `max_tokens <= 0` 被抬到 16** —— 报告标为"可能刻意取舍"。我判断应改：
  `0`/负值在客户端语义里常表示"用模型默认上限"，抬到 16 会把响应**静默截断**；
  而让它原样出站、由上游回 11133 更可诊断。改成只钳 `0 < v < 16`
  （Claude Code 的 1..15 探针仍被修好）。【复核】成立。

### 测试（+5 个回归）

- `TestTranslateMaxCompletionTokensKeepsAliasWhenNotTranslatable`（#1：小数四舍五入、
  字符串保留、显式 max_tokens 优先）
- `TestClampGPTMinMaxTokensLeavesZeroAndNegative`（#19）
- `TestPlaceholderDoesNotVouchForModel`（#4/#5：真号冷却 + 占位号时必须是 Blocked，
  且分母不含占位号）
- `TestSaveAtomicPreservesUnknownSubKeys`（#11：auth/account 子对象与顶层未知键都要留）
- `TestNormalizeForCompareAvoidsFalseRestartHint`（#12：最小配置零改动不误报，真改了仍报）

`upstream 210 / server 92 / pool 186 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 28 / usage 14 / scheduler 65 / cmd-server 52`，**708 通过 0 失败**；`gofmt -l` 为 0。

## v1.27.4

**本版 = 第十份审查报告（13 条）的落地修复。逐条对当前代码复核：13 条全部为真、0 误报。
其中 P0-1、P0-2、P1-2 与本会话前几轮的改动直接相关 —— 上一轮我只修了一半。**

### 与前几轮同源的三条（又一次"修了入口没修根"）

* **P0-1 四个批量端点的 goroutine 没有 recover** —— v1.27.2 我给 checkinAll/travelAll/
  activityAll/keepaliveAll 加了 `TryBeginTask` + `EndTask`，**只加了锁、没加 recover**
  （而 balanceAll 有）。Go 不会替非 net/http 起的 goroutine recover：任务链里全是弱类型
  解析上游响应，一次 panic 死的是整个网关进程。四个入口各补 `defer recover`
  （LIFO 下先 recover 再 EndTask）。
* **P0-2 requestId 扫描命中空 id 后重新阻塞 Read** —— v1.26.x 我修了"空 id 让偏移原地不动"
  的死循环，但搜索仍写在 `Read` 之后：推进偏移后 `continue` → 下一轮开头先阻塞 Read，
  **缓冲区里后面的合法 id 永远不看**。流恰好结束就误报"未找到"，流没结束就卡到 120s 超时。
  改成「先把缓冲里能判定的全试一遍，再决定 Read」。
* **P1-2 handler/main 取目录账号固定 `uids[0]`** —— v1.27.3 我只给 **panel** 加了
  `pickExistingAuth`，server 与 main 没同步（`/v1/models` 偶发返回空、预热静默跳过，
  日志零线索）。这版把它收敛成 `pool.PickExistingAuth` 一处实现，panel/server/main 三处共用。

### P0

* **P0-3 global_models 降级丢「窄表」名单** —— 企业端点（或某路 v3）只回 id 名单、
  `infos=nil` 时，`extractEfforts(nil)` 得空名字集 → 调用方按 `len(names)==0` 判失败并写
  5min 负缓存 → 整个 global 域在 /v1/models 与面板模型页**静默消失**，日志只有一句
  "degraded to enterprise endpoint"。抽出 `degradedCatalog()` 给两条降级分支共用
  （两路皆成功的主路径本来就有这个兜底）。【复核】真。
* **P0-4 用量图把 Invalid Date 当跨天** —— `new Date("2026-09-16T13")` 是 Invalid Date，
  `getDate()` 得 `NaN`，而 `NaN !== NaN` 恒真 → 从第 2 个点起每根柱子后都画一条日界线。
  改用已有的 `parsePointTime`（内部补 `:00:00`）并换成 `YYYYMMDD` 整数键
  （原来只比 `getDate()`，跨月 31→1 会漏画）。【复核】真。

### P1

* **P1-1 Claimable 忽略 `accept_status=completed`** —— 一次性任务常 `target=0`/`progress=null`，
  完成状态只在 accept_status 上。`Claimable: !claimed && (tgt>0 && cur>=tgt)` 让它们永远
  不可领取（面板不渲染「领取」按钮），而 autotask 各处却按 `AcceptStatus=="completed"`
  判完成 —— 两侧口径不一致。补 `done` 条件。【复核】真。
* **P1-3 空 auths + Redis 快照 → 幽灵账号** —— 快照恢复先塞「只有 UID」的占位 `auth.Auth`，
  靠 `SyncToDir` 用真实凭证覆盖；但 `SyncToDir` 扫到 0 个账号时直接 return（防清空状态），
  占位对象永久留在池里：healthy 为真 → 被 pick 选中 → `NeedsRefresh` 因 `ExpiresAt=0` 恒真 →
  `RefreshToken` 报 no refreshToken → 熔断+轮转。表现为「面板显示 20 个号、每次请求全 503，
  日志全是 no refreshToken」。
  采用报告方案 A：entry 加 `needAuth` 标记（快照恢复的占位置位、`upsertLocked` 拿到真实
  凭证后清位），`healthy()` 对占位返回 false —— 一处收敛，覆盖 pick / 可用集 / 探活 / 计数。
  【复核】真。**没用「凭证为空就判不健康」那种写法**：空凭证在其它路径（手工构造、
  刷新中）有合法含义，实测会把 28 个 pool 测试打挂，且语义上过宽。
* **P1-4 CN 侧 reasoning_effort 缺静态兜底** —— `prepareBody` 只对 global 用
  `globalEffortMap`（静态表 ∪ 远端桶），CN 只用运行时桶；该桶仅在 `FetchModels` 成功时填充，
  于是启动预热失败期间 CN 档位**完全不降级**：给 `hy3` 发 `max` 这类非法值原样出站 →
  上游 400+11101 → 网关当 `ErrBadParams` 终结请求，用户看到"模型解析参数失败"。
  新增 `cnEffortMap`（与 global 对称）并接到 `prepareBody`。【复核】真。
* **P1-5 `modelExempt()` 恒为假 / `||` 分支是死代码** —— `modelExempt` 要求
  `until/breakerUntil/degradeUntil` 三个截止**都为零**，而 `healthy()` 为假的唯一原因正是
  「其中某个仍在未来」，两者互斥 → `healthy || modelExempt` 恒等于 `healthy`。
  删掉死分支并改写注释（模型级冷却本就不影响账号级健康，注释宣称的语义已由 healthy 满足）。
  【复核】真。

### P2

* **P2-1 `UserTypeFromError` 用小写串的下标切原串** —— `i` 来自 `low`，却用它切 `body`；
  `strings.ToLower` 对非 ASCII 可能改变字节长度（如 `İ`）→ 下标错位 → 取出的"账号等级"是
  乱码 → 被 `NoteModelTierDenied` 写进 `model_tier.json`，等级闸门静默失效。
  改成在同一串（`low`）上切片。【复核】真。
* **P2-2 三处中文按字节截断** —— `cmd/signin` 的 `short()`（60B）、`panel` 的
  `truncateStr`（n B）、`scheduler/streak` 的 `compactJSON`（220B）都直接 `s[:n]`，
  中文被切成半个 UTF-8（终端乱码 / JSON 里 `\ufffd`）。统一改用 `logfmt.Truncate`。
  【复核】真。
* **P2-3 空模型列表吞掉后端说明** —— 后端在「所有模型被等级闸门 11158 隐藏」时返回
  `200 + models:[] + hidden_tier + note`，前端却写死"上游未返回模型"并 return，
  把「你这批号等级不够」误报成「上游没给目录」。改用 `d.note` / `d.hidden_tier`。
  【复核】真。

### 测试（+5 个回归，其中一条抓出了我自己新引入的 bug）

- `TestDesktopChatWithExpertFindsIDAfterEmptyIDInSameChunk`（P0-2：同一 chunk 里空 id 在前、
  合法 id 在后必须能取到）+ 对照用例 `NoValidID`（只有空/不匹配 id 时如实报错）
- `TestDegradedCatalogKeepsNarrowNames`（P0-3：窄表降级不能丢名单）
- `TestParseGrowthTasksClaimableForCompletedSingleTask`（P1-1：completed + target=0 应可领）
- `TestSnapshotPlaceholderNotRoutableUntilCredsArrive`（P1-3：占位不可选、拿到凭证后立即可选）

**P0-2 的回归测试抓到我自己写的新 bug**：`Read` 允许同时返回数据与 `io.EOF`，我第一版
在 `break` 之前没有再扫一遍，于是「id 帧和流结束一起到达」这种最常见的短流形态仍会误报
未找到（该测试在 5 次连跑里挂了 2 次）。已改成退出前补扫一次。

`upstream 208 / pool 185 / server 92 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 51`，**703 通过 0 失败**；`gofmt -l` 为 0。

## v1.27.3

**本版 = 第九份审查报告（20 条）的落地修复。主人问「怎么感觉这些 bug 之前修过」——
我把 20 条逐条拿当前代码核对，并标出与本会话前几轮的渊源：19 条为真、1 条机制误报。
其中 3 条确实与之前修过的病灶同源（我改了调用方没改根、或我重构时把行为带偏了）。**

### 与之前修复同源的三条（主人的直觉来源）

* **B4「`already` 关键词过宽」** —— v1.26.x 我修的是 `cmd/signin` 的宽匹配（去掉
  `code=400`/`checkin`），但**没动共享的 marker 列表**，它仍是 `["已签到","already"]`。
  所以那条只是"堵了一个入口"，根没动。这版收窄成明确幂等文案
  （`已签到/今日已签到/今天已签到/already checked in/...`），并加反向词过滤
  （`already used/exists/claimed/exhausted` 一律不算）。
* **B7「保存配置写未规范化数据」** —— v1.27.2 我重构 `saveConfig` 修 P0-1 时，
  只改了"用什么校验"，落盘仍写 `merged`。这版改成落盘用**规范化后的值**（用不叠 env
  的副本，避免把环境变量里的密钥写进文件），并用 `fillUnknownKeys` 把用户手写的
  未知键补回。
* **B13「回读被中止仍回 200」** —— 这段正是我前几轮反复重构的 `accountTaskAuto`
  回读区。这版补 `errors.Is(aerr, errTaskAborted) → 504`。

### P1

* **B1 流水日志的 rid/out/try/credit 段永不输出** —— 全仓没有任何一处给
  `st.requestID` 赋值，而 logging 的扩展段条件是 `requestID != ""`，恒假。
  handler 里补 `st.requestID = tr.id`。【复核】真（`grep 'requestID ='` 零命中）。
* **B2 面板加载失败被记成成功** —— WebView 顺序是 `onReceivedError(failingUrl)` →
  `onPageFinished(failingUrl)`，只判 `url.startsWith(PANEL_URL)` 会把失败记成成功，
  `tick` 的 `!panelLoaded` 从此不成立 → 网关恢复后也不自动重载。加一次性 `pageHadError`
  与之取与；`tick` 发起加载、「重载面板」、`restart()` 三处都清标记。
  【复核】真（我 v1.26.8 修的 `panelLoading` 只解决了"重复打断"，没覆盖这条竞态）。
* **B3 点「停止」后状态不复位** —— 停止路径没有一处 `setState(ST_STOPPED, ...)`，
  静态 `status` 停在 `ST_RUNNING`，而 `tick` 把 `ST_RUNNING` 读成「启动中」。
  `ACTION_STOP` 分支与静态 `stop(Context)` 都补上（并清 `appRestarts`）。
* **B5 缺 index 的 tool_call 被删工具名** —— `stripToolCallNames` 把缺 `index` 的分片
  一律归 0 号槽，第一个之后所有调用首片的 `function.name` 被删。改成缺 index 时
  **不收敛、原样透传**（宁可不收敛，也不能删名字）。【复核】真。
* **B6 小额扣费被抹零** —— `int64(credit+0.5)` 对 0.02~0.34 量级的扣费全部得 0，
  余额只增不减，而 `credit_floor` 用的就是这个插值 → 保底迟迟不触发。
  改成余数累加（`creditFrac`），攒够 1 分才扣。【复核】真。

### P2

* **B8 业务错误分类丢 code** —— `Classify(status, env.Msg)` 只传 message，
  而 `badParamsMarkerCode("\"code\":11101")` / `hasBusinessCode` / `IsModelBlocked`
  都依赖原始 JSON。改成把 `code` 一起拼进可判定形态（新增 `mustJSONString`）。
  【复核】真：14018/11102/11101 在 doJSON 路径上全部退化成 `ErrClient`。
* **B9 空会话 `prompt_cache_key` 恒定** —— `buildCacheKey` 对空会话仍 `sha256(uid+"|")`，
  同账号所有无会话客户端共用一个缓存槽、互相踩踏，与注释「空会话不复用」相反。
  改成空会话返回 `""` 且**不注入**；同步更新了那个把 bug 固化的测试。
  【复核】真。
* **B10 `req[len(req)-8:]` 无长度守卫（5 处）** —— 新增 `upstream.Tail8`，
  替换 `panel/autotask.go` 4 处 + `upstream/desktop.go` 2 处（报告只列了 1 处 desktop，
  我核对到 `desktop.go:363` 也有同一个写法）。`runValorantExpert` 原本就有守卫。
  【复核】真。
* **B11 导入后置动作对企业号也签到** —— `postImportActivate` 只判了 global，
  没判 `IsEnterprise()`；企业号 `daily-checkin` 上游必回 400 code 10001。
  补 `else if !a.IsEnterprise()`。【复核】真。
* **B12「全部接受」末批后仍等待** —— 末批（`end == len(codes)`）之后照样 sleep 1.05s，
  期间客户端断开就回 504「已中止」，而任务其实全部接受完了。改成只有还有下一批才等。
  【复核】真。
* **B14 重置筛选未清防抖定时器** —— `resetModelFilter` 缺 `clearTimeout(mdQTimer)`，
  120ms 后旧关键字被写回，列表「重置了又自己过滤回去」。【复核】真。
* **B15 常驻图层失败覆盖 reason** —— `reason` 是界面第三行「为什么不可用」，
  被这条次要提示盖掉（例如「启动被系统拒」这类关键原因）。改用独立字段
  `overlayErr` + `overlayError()`，由状态条单独拼接显示。【复核】真。
* **B20 模型目录取号失败静默 503** —— `AvailableUIDsForRealm` 与 `AuthByUID` 是
  两次独立加锁查询，取不到就静默跳过该域。改成 `pickExistingAuth` 依次回退，
  全都不在时把真实原因写进 `fetchErrs`（CN/global 两处）。【复核】真。

### P3

* **B16 清单声明 `QUICKBOOT_POWERON` 但接收器忽略** —— 采取报告的第二方案：
  **从清单删掉该 action**，保持「只认受保护广播」的既有设计（加代码分支会让这个
  exported 接收器接受任意应用可发的非受保护广播）。
  注意：无-aapt2 打包路径复用上一版 APK 的**二进制清单**，所以已装的 APK 里这个
  action 仍在（运行时本来就被忽略，行为无变化）；源码层面从此一致。
* **B17 `watchdog.sh` 无单实例锁** —— 启动前检查 `watchdog.pid` 里的进程是否存活，
  在跑就退出，避免两条看门狗互相对同一个网关 stop/start。【复核】真。
* **B18 `run.sh` 残留 pid 误判** —— `running_pid` 的 pid 文件分支只 `kill -0`，
  pid 被系统复用就会把无关进程当成网关。补 `/proc/<pid>/cmdline` 校验。
  【复核】真。
* **B19 `push_github.sh` 取版本号失败** —— **报告机制不成立**：脚本是 `set -eu`
  且**没有** `pipefail`，管道退出码取 `sed`，grep 找不到**不会**让脚本退出。
  我实测复现：脚本继续跑到 `版本: v`，exit 0。但由此暴露的真问题是
  **空版本号一路跑下去、用空 tag 建 Release** —— 已补显式判空。

### 误报

* **B19 的机制**（"grep 失败在 set -e 下静默退出"）—— 实测不成立（见上）。

### 测试

`upstream 204 / server 92 / pool 184 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 51`，**698 通过 0 失败**；`gofmt -l` 为 0。

B9 的语义变更同步更新了 `TestInjectPromptCacheKey_EmptyConversation`（原测试断言的
正是"空会话也要生成非空恒定键"，等于把 bug 固化了）。

## v1.27.2

**本版 = 第八份审查报告（9 条）的落地修复。按主人要求先逐条复核：9 条全部为真，0 误报。
其中 P0-1 让我上一版的修复失效、P0-3 是我上一版写了个假注释 —— 两条都在这版纠正。**

### P0

* **P0-1 环境变量在校验之后才生效：非回环 listen + `WB2A_API_KEY` 起不来** ——
  `Load` 调 `ParseConfigInto`，而它内部就 `normalize()`（含 `checkListenKey`，用的是**文件里的
  空 key**），`applyEnv` 在这之后才跑。于是「config.json 只写 listen、密钥走环境变量」这种最常见
  的容器/自托管形态直接 `fatal: 必须配置非空 api_key`，起不来。
  修法：新增 `ApplyJSON`（只 unmarshal、不校验），把 `Load` 与面板 `saveConfig` 都改成
  固定顺序 **解析 → 叠 env → normalize（只跑一次）**。
  【复核】真。并且我上一版给 `saveConfig` 补的 `applyEnv` **救不了** —— `ParseConfig` 那一步
  就先报错了，面板连保存都做不了。
* **P0-2 余额后台刷新在「启动时关闭」之后永远开不起来** —— `StartBalanceRefresh` 在
  `interval <= 0` 时直接 return，循环 goroutine 根本没创建；之后面板打开开关保存，
  `SetBalanceInterval` 只写 atomic + `poke(rearmBalance)`，而那个 channel **没有任何监听者**
  → 余额永不后台刷新，只能重启进程。循环体本身已经能处理 `cur<=0`（等 rearm），
  所以删掉提前 return、永远建循环即可。
  【复核】真。
* **P0-3 调度任务族没有互斥，批次会重叠** —— `runBatch` 每族一个 goroutine、**零互斥**
  （全包只有 `mu` 记领养日、`schedMu` 保护配置快照）。签到一轮在慢上游下可达几十分钟，
  跨过下一个时点就两批并发跑同一批账号；面板 5 个「全部xxx」按钮每点一次 `go RunXxxNow()`
  可以叠出任意多轮。
  修法：新增 `taskLocks sync.Map` + `TryBeginTask/EndTask`（按字符串任务族键，面板包也能用），
  `runBatch` 拿不到锁就打日志跳过本轮，面板 5 个手动入口拿不到锁回 **409**，余额定时循环与
  「刷新余额」共用 `TaskBalance` 锁。顺手给 runBatch 的 goroutine 补 `recover`
  （同类问题：非 http 起的 goroutine 一次 panic 打死整个进程）。
  【复核】真 —— 而且 **v1.27.1 我把 `runBatch` 改成异步时还写了句「同族重入由任务族互斥承担」，
  那是假话**：异步反而让重叠更容易发生。这版把互斥真正做出来了。

### P1

* **P1-1 APK 重启只等 4s，短于网关 5s 停机预算 → 高概率撞端口** —— `restartGateway` 里
  `old.waitFor(4s)`，而 Go 侧 `srv.Shutdown` 预算 5s（有在途 SSE 时必然跑满）→ 新进程
  `bind: address already in use` 立刻退出 → 看门狗退避 2s 再拉一次，用户看到「重启了两次」、
  自愈计数 +1。改成轮询 `isAlive(old)`、截止 8s（旧进程提前退出就不干等）。
  【复核】真：`alive()` 只看静态 `proc`，而 `killProc()` 已把它置 null，等不到旧进程退出。
* **P1-2 配置空值回落不对称：删掉一个值就起不来** —— `breaker_cooldown` /
  `degrade_cooldown` / `soft_rate_max` 都有「空串 → 回落默认」，但
  `cooldown.soft_rate`、`session_sticky.ttl`、`session_sticky.gc_interval` 没有 ——
  写 `""`（用户以为「删掉 = 用默认」）→ `time.ParseDuration("")` 失败 → 启动直接失败。
  三处补齐，位置都在 `ParseDuration` 之前。
  【复核】真：全文件 grep 不到这三处的 `== ""` 判断。

### P2

* **P2-1 `normalize` 里的重复死代码** —— `Pool.DegradeCooldown == ""` 出现两次
  （前一处已兜底并 `ParseDuration` 过），第二处永远命中不了，删掉。
  【复核】真（删的是后一处，前一处仍在 `ParseDuration` 之前，行为不变）。
* **P2-2 空池时容器永远 unhealthy** —— `HEALTHCHECK` 打 `/healthz`，而它在池里没有可用账号时
  返回 503 → 新容器（还没加账号）一直 `unhealthy`，用 `depends_on: service_healthy` 的下游
  永久卡住。新增恒 200 的 `GET /livez`（带 `X-Service` 头与 `service` 字段），
  `HEALTHCHECK` 指过去；`/healthz` 保留「可服务」语义不变。
  【复核】真。
* **P2-3 面板保存配置时请求体被静默截断** —— `io.LimitReader(r.Body, 1<<20)` 把 1MB 之外
  静默丢掉，随后报 `unexpected end of JSON input` —— 用户看到语法错误，不知道是体积超了。
  改成多读 1 字节判断超长并回 **413**。
  【复核】真。
* **P2-4 模型页加载失败后不再自动重载** —— 视图切换判据是 `!$('mdBody').children.length`，
  而 `loadModels` 失败时会往 `mdBody` 写一行错误 → children 非 0 → 之后每次切回来都不再请求，
  一直显示旧错误。改用「成功加载」标记 `mdLoaded`（失败保持 false，切回重试；上游确实没返回
  模型时算成功）。
  【复核】真。

### 测试

新增 5 个回归：
- `TestLoadEnvKeySatisfiesNonLoopbackListen`（env 密钥必须满足非回环守卫）
- `TestLoadNonLoopbackWithoutKeyStillFails`（守卫不能被削弱）
- `TestNormalizeEmptyDurationFieldsFallBack`（空时长回落默认）
- `TestLivezAlwaysOKWithEmptyPool` / `TestHealthzStill503WhenEmpty`（两个端点的语义边界）

`upstream 204 / server 92 / pool 184 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 51`，**698 通过 0 失败**；`gofmt -l` 为 0。

## v1.27.1

**本版 = 第七份审查报告（18 条）的落地修复。按主人要求先逐条复核：17 条为真、1 条误报。**

### 会让进程挂掉 / 功能永久失效

* **1 任务队列 goroutine 没有 recover，一次 panic 打死整个网关** —— `runQueueItems`（按账号那层）、
  `accountTaskAutoAll` 的流水线 goroutine、`refreshCreditsAsync` 三处都不是 net/http 起的，
  Go 不会替它们 recover。任务链里大量对上游响应的弱类型解析，上游换个返回形状就可能 panic ——
  死的是**整个网关进程**，而不是"这次任务失败"。三处都补 `defer recover`（队列那层还顺手把该项
  标成 error，不卡状态）。
  【复核】真：三处 goroutine 原文只有复位/解锁 defer，没有 recover。
* **2 refresh 已 rotate 的 refreshToken 不落盘 → 重启后该账号永久无法刷新** ——
  `tok.AccessToken == ""` 分支把新 refreshToken 写进内存后返回 error，而三个调用点
  （handler / scheduler / signin）在 error 分支都直接放弃、不 `SaveAtomic`。进程重启后从磁盘
  读回的是**已被上游作废**的旧 refreshToken，账号从此刷新不了，只能重登。
  改成在该分支内自己落盘（改的是内存里唯一的真值，最安全）。
  【复核】真且后果严重。
* **4 `state.json` / auth 文件用 rename 落盘，单文件 bind mount 时持久化静默失效** ——
  `main.go` 早已为 config.json 处理了「rename 到挂载点返回 EBUSY」，但 state/auth 没同步。
  抽 `auth.ReplaceAtomic()`（tmp 写 → rename → EBUSY 时原地 O_TRUNC 覆写）三处共用。
  【复核】真：Docker `-v ./data/state.json:/app/data/state.json` 这种单文件挂载下 rename 恒失败。

### 状态机 / 调度逻辑

* **5 21:00 之后撞 402 的账号被冷却到次日 04:00，比下次签到早 5 小时** —— 签到时点是
  09:00/21:00，21:00 后的 402 下一次签到是次日 09:00，而 04:00 回池时余额仍是 0 → 必然再撞
  一次 402。`nextHardCooldownDeadline` 补 `h >= 21 → 次日 09:00`。
  【复核】真。
* **6 单任务入口对「已达标未领」不领奖** —— `taskDone(before)` 对该状态返回 true → 直接回
  "无需再跑"，而 `runAutoAll` 会走 claim。改成先领一次再返回（带 `already` 区分文案）。
  【复核】真：口径不一致，用户白丢奖励。
* **7「执行全部待办」的扫描阶段没有并发上限** —— 每账号一个 goroutine、每个打 2 次上游，
  46 号 = 92 并发；同文件 `tasksScanAll` 用 `sem=4`。补 `scanSem`（cap 4）。
  【复核】真。
* **8 global 模型目录探测没有 single-flight** —— 缓存判断在锁内、探测在锁外且无 in-flight
  标记，TTL 一到 N 个并发各探一遍（每次 4 路请求）。补 `probing` 标记。
  【复核】真：CN 侧 `fetchDynamicModels` 有 `modelsFetchMu`，global 侧漏了。
* **9 会话粘性 `Bind` 每个成功请求都写一次 Redis** —— 会把异步队列（1024 条/8 worker）打满，
  丢的不只是绑定，`SaveState` 快照也走同一队列。按 `minTouchInterval` 节流（与 `touch` 对齐）。
  【复核】真。
* **10 余额刷新在 ctx 取消时直接 return，不等在途 goroutine** —— 已起飞的还在跑并占着 `sem`，
  下一轮再建新 sem → 并发闸门失效。改成 `break dispatch` + 末尾统一 `wg.Wait()`。
  【复核】真。
* **11 扫描接口 ctx 取消后空转整个账号列表** —— `continue` 只跳过当前账号，for 会跑完剩余
  （每次立刻再命中 `ctx.Done`）。改 `break scanLoop`。【复核】真。
* **12 `watchAbort` 的「已停止」判断有竞态** —— `atomic.Bool` 的 check-then-act 仍有窗口：
  stop 清完标记后 goroutine 又写回来，该账号下次任务在第一个 `sleepAB` 就提前退出
  （表现为"点了没反应 / 秒失败"）。改用一把 mutex 把两组操作串起来。
  【复核】真：上一版只加了二次确认，没有互斥。
* **13 调度主循环在 `runBatch` 期间不看时点** —— `runBatch` 末尾 `wg.Wait()` 阻塞主循环，
  主循环随后用 `time.Now()` 重算下一次唤醒；若这批任务跨过下一个时点，那次直接被跳过
  （checkin 末尾串跑 streak 管家 + 800ms/号节流，46 号就是数分钟）。改成 `go s.runBatch(...)`。
  【复核】真。

### 次要

* **14 `listen` 只有主机名时被补成非法地址** —— `"localhost"` 被补成 `":localhost"`，
  `ListenAndServe` 报 `unknown port`，错误指不到 `listen`。改成「纯数字才补冒号，
  否则明确报错」。【复核】真。
* **15 运行日志拉取失败被完全吞掉** —— `catch (e) { /* 概览已提示 */ }` 让页面保持旧内容，
  用户以为"日志没更新"。改成写 `logNote`。【复核】真。
* **16 MainActivity 首帧重复 tick** —— `onCreate` 里 `tick()` + `onStart` 又 `tick()+scheduleTick()`
  → 首屏两次端口探测、两次渲染。删掉 `onCreate` 那次。【复核】真。
* **17 `removeOverlay` 与 `installOverlay` 不共用一把锁** —— 前者 `static` 无同步、后者
  `synchronized`（锁 Service 实例），交错时可能泄漏一个悬浮窗（系统仍认为进程有可见窗口）。
  改用显式 `static final Object OVERLAY_LOCK`。【复核】真。
* **18 专家批量执行里的失败计数被丢弃** —— `_ = fail`，用户看到"已对 5 位完成"却不知道其中
  3 个失败。写进返回值。【复核】真。

### 误报（未改）

* **3「面板『检查』点一次异常后永久失灵」** —— 报告称 `expFetching` 永不复位。实际
  `renderExpiry(d)` 在 `try` 内，抛异常会被 `catch` 捕获，随后 `expFetching = false` 照常执行
  （它不在 try 里，但在 catch 之后、函数末尾）。**不会永久失灵**。
  不过报告顺带指出的 `a.uid.slice(0, 8)` 隐患成立（`nickname` 为空且 `uid` 缺失时 TypeError），
  已顺手改成 `String(a.uid || '').slice(0, 8)`，与 `renderExpirySnapshot` 口径一致。

### 测试

新增 2 个回归：`TestRefreshRotatePersistsEvenWithoutAccessToken`（rotate 结果必须落盘）、
`TestRefreshCreditsAsyncRecoversPanic`（观测性 goroutine 自己 recover）。

`upstream 204 / server 90 / pool 184 / panel 28 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 48`，**693 通过 0 失败**；`gofmt -l` 为 0。

`nextHardCooldownDeadline` 的边界用例同步更新（21:00 后 → 次日 09:00），并补了
21:00 / 22:30 / 年末三个用例。

## v1.27.0

**本版 = 第六份审查清单（18 条）的落地修复。按主人要求先逐条复核再改，结论见每条【复核】。**

### P0

* **B1 面板保存配置会丢掉 `WB2A_*` 环境变量覆盖（含 `api_key`）** —— 保存链路用
  `ParseConfig(mergedJSON(merged))` 校验，而它只做 `Default()+JSON+normalize`、**不读环境变量**；
  随后这个 newCfg 被直接灌进 `livecfg` 与 upstream 的热生效字段。用 `WB2A_API_KEY=sk-xxx`
  起服务时，面板随便改一项保存就把鉴权密钥覆盖成文件值（常为空），日志还说「已立即生效」。
  修法：保存路径补 `applyEnv(newCfg)` + `normalize()`，与启动路径（`Load`）同口径。
  【复核】真：`ParseConfig` 的文档注释自己写着「不读环境变量」，而 `Load` 在 330 行调了 `applyEnv`。
* **B2 「需重启字段」清单缺项，面板谎报「已立即生效」** —— `global.enabled/chat_base/billing_base`、
  `upstream.client_version/cli_version/client_name/device_token/device_token_file/passthrough_ip`、
  `prompt.mode/file`、`server.max_body_mb` 都在启动期被装配进 `upstream.Client` /
  `server.Handler` / `auth` 包级开关，保存路径完全不碰、清单也没列。补齐清单（重启生效语义明确）。
  【复核】真：对照 `main.go:169-181` 与 `330-331`，这些字段确实只在装配期写入。

### P1

* **B3 用键存在性判 error 帧，`"error":null` 会把非流式请求打死** —— `json.Unmarshal` 到 map 时
  `"error":null` 会得到「键存在、值为 nil」，原判据 `_, hasErr := chunk["error"]; hasErr` 命中 →
  非流式走 `ErrorFrame` → `FrameKind` 判 `ErrNone` → handler default 只换号 → 每个号失败一次 → 503。
  两处（`Aggregate`、`StreamHint.writeFrame`）都改成「存在且非 nil」。
  【复核】真：OpenAI 兼容实现常在正常 chunk 里带 `"error":null`。
* **B4 `NeedsRefresh` / `RealmStored` 用写锁** —— 前者在 chat 热路径每请求调用
  （`acct.NeedsRefresh(h.cfg.RefreshSkew)`），写锁会让同账号并发请求在此串行化。改 `RLock`。
  【复核】真：`auth.mu` 已是 `RWMutex`（v1.26.2 改的），写方仍是 `Lock`，语义不变。
* **B5 refresh 响应缺 `accessToken` 时丢弃已 rotate 的 `refreshToken`** —— 上游 refresh 会 rotate
  旧 refreshToken 使其作废，原实现解析成功但 accessToken 为空时直接 return，新的就丢了 →
  该账号之后每次刷新都失败，只能重新登录。改成先把 rotate 后的 refreshToken/domain 落回再报错。
  【复核】真且后果严重。
* **B6 `effectiveTaskCode` 算在 404 分支之后，换码任务永远「没有此任务」** —— 前置读取用固定码，
  查不到直接 404 return，按域换码的解析永远走不到。改成先解析实际码再做前置读取/回读/领奖。
  【复核】真：**这是 v1.26.7 加 `WorkBuddy_Valorant` 时我自己引入的**。
* **B7 跳过名单没有 UI 出口，409 文案指向已下线的「任务中心」** —— 任务弹窗新增
  「清空跳过名单」按钮（调 `POST /panel/api/tasks/skip {"clear":true}`），4 处文案同步改成
  「打开该账号的「任务」弹窗，点「清空跳过名单」」。
  【复核】真：`app.js` 里此前零处调用 `tasks/skip`，而文案 4 处都指向不存在的界面。
* **B8 `Stop()` 在未 `Start()` 时永久阻塞** —— `done` 只在 `Start` 的 goroutine 里关闭，
  只 `New` 不 `Start` 的调用方在 `Stop` 时永久 hang（`defer rec.Stop()` 在早期 return 路径同样会挂）。
  加 `startedFlag atomic.Bool`：`Start` 幂等置位，`Stop` 未启动直接返回。
  【复核】真（线上不显形，但确实是定时炸弹）。

### P2

* **B9 `watchAbort` 可能留下陈旧中止标记** —— `select` 在 `done` 与 `finished` 同时就绪时随机选，
  若 `markAborted` 发生在 `stop()` 的 `clearAborted` 之后，标记残留；而队列路径不调 `clearAborted`，
  下一次任务会在 `sleepAB` 处直接 false 提前退出。加 `stopped atomic.Bool` 二次确认，
  并让 `runQueueItems` 在账号开始前 `clearAborted`。【复核】真（潜伏竞态）。
* **B10 扫描阶段 panic 会让 `q.running` 永久为 true** —— 先置位再扫描，扫描段无 recover，
  panic 后「执行全部待办」永远 409、`growth_hours` 也不再推进，只能重启。加 `scanOK` +
  defer recover 复位。【复核】真。
* **B11 脚本领奖走的是上游不存在的路径** —— `task_common.py` 仍用
  `/v2/activity/growth/tasks/reward/claim`（Go 侧注释早已写明该路径一直返回 400），
  真实路径是 `{webBase}/activity/growth/tasks/{code}/claim`（task_code 进路径、无 body、
  带 `x-client-platform: web`）。改路径 + 新增 `web_base()` + 更新说明。
  【复核】真：脚本领奖 100% 失败，而 Go 网关同一任务能领。
* **B12 空流兜底帧注释说「不挂 hint」，实现却走了 hintFn** —— `writeRaw` 内部会调
  `frameGatewayHint`，改用 `writeSSE`。当前 `FrameKind` 判 `ErrClient` 恰好不给 hint，
  属「靠巧合正确」。【复核】真。
* **B13 动作内部领奖成功后不刷新池内积分** —— mp 系任务在动作**内部**就完成 claim 并返回，
  外层判 `claimable` 的 `refreshCreditsAsync` 走不到，面板数字要等 5 分钟。
  6 处内层 claim 分支补 `if !already { p.refreshCreditsAsync(a) }`。【复核】真。
* **B14 维护类重试与全量余额刷新不可取消** —— `retryBillingTransient` 用裸 `time.Sleep(2s)+Sleep(4s)`；
  `RunBalanceRefreshNow()` 无 ctx。新增 `retryBillingTransientCtx` / `RunBalanceRefreshNowCtx`
  （`sem` 抢占也改 `select`），停机不再干等。【复核】真。

### P3

* **B15 `nextDay4AM` 名不副实** —— 改名 `nextHardCooldownDeadline`，文档首句改成实际语义
  （09:00 前 → 当天 09:00；09:00 后 → 次日 04:00），测试同步改名。【复核】真：名/注释与实现相反。
* **B16 `strings.HasPrefix(hint, NoHealthyAccountHint())` 恒真** —— 上一行刚把 hint 赋成该值。
  去掉死条件（保留块结构以最小化 diff）。【复核】真。
* **B17 循环内 `raw := acc.ExpiresAt` 遮蔽外层 `raw []byte`** —— 改名 `expRaw`。
  【复核】真（当前无功能错误，属隐患）。
* **B18 注释错位/重复** —— 删掉 `main.go` 里重复的「成长任务排程接线」注释、把
  `pool.go` 的 `defaultBreaker*` 注释从 `func New` 上挪走、合并 `runGrowthQueued` 的重复文档、
  删掉三处「（原先这里贴的是 XXX 的注释——复制粘贴残留）」考古注释。【复核】真。

### 测试

新增 3 个回归（`cmd/server/saveconfig_test.go`）：需重启清单必须覆盖装配期字段、
未变化字段不进清单、`applyEnv` 确实覆盖文件值。

`upstream 203 / server 90 / pool 184 / panel 27 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 48`，**691 通过 0 失败**；`gofmt -l` 为 0。

## v1.26.9

**本版 = 第五份 bug 清单（24 条）的落地修复。按主人要求，先逐条对着源码复核「是不是真 bug」，
确认后再改；结论见每条末尾的【复核】。**

### 高优先级

* **1 `cmd/signin` 的刷新窗口单位写错** —— `NeedsRefresh(2 * 3600)` 传的是裸常量 7200 =
  **7200 纳秒**（7.2 微秒），等于永不提前刷新，批量签到拿过期 token 打 daily-checkin，
  大量 401 被误判成账号问题。改 `2 * time.Hour`。
  【复核】真：`NeedsRefresh(within time.Duration)` 形参就是 Duration，7200 即 7.2µs。
* **2 `cmd/signin` 的「已签到」判定过宽** —— 原实现只要文案里带 `code=400` / `checkin` /
  `already` 就算已签到，把参数错误、风控拒绝全吞成 ALREADY（账号当天没签到、积分没恢复，
  且无告警）。改用 `upstream.IsAlreadyCheckin(err)`（只认 `*upstream.Error` 且只匹配
  「已签到 / already」）。【复核】真。
* **3 Docker 每次重启都换 API Key** —— entrypoint 无条件生成随机 key 并 `export`，
  而 `applyEnv` 在文件解析之后执行（env 优先），于是挂载的 `config.json` 里的 key 完全不生效，
  每次 `docker restart` 客户端全 401。改成「配置里没有 key 才生成并**写回 config.json**」，
  只有写不进去（只读挂载）时才退回 env 并明确告警。
  【复核】真：这是我上一轮自己写的脚本，注释与实现相反。
* **4 Dockerfile 编了 `trial` 却没 COPY 进镜像** —— 补 `COPY --from=build /out/trial /app/trial`，
  并把 `COPY go.mod ./` 改成 `COPY go.mod go.sum ./`。【复核】真（镜像里确实没有该文件）。
* **5 「系统重启 N 次」把普通启动也算进去** —— `onStartCommand` 不只在进程重建时调用
  （MainActivity 每次 onCreate、BootReceiver、JobScheduler、5 分钟一次的 ACTION_WATCH alarm
  都会进来），而 `service_up` 一直是 true → 计数随每次开 App / 每次 alarm 无限增长。
  加进程级 `startCounted` 标志，每个进程只记一次。
  【复核】真且显眼：alarm 的 `getService` 会走完整 `onStartCommand`，没有 ACTION_WATCH 提前返回分支。
* **6 领奖用 `credit==0 && energy==0` 判「已领取」** —— `parseClaimReward` 解析了
  `already_claimed` 却丢掉，调用方只能拿金额猜；道具类奖励（Buddy 盲盒/补签卡/抽奖次数）
  本来就是 0 分 0 能，那是**成功**领取。现在 `ClaimReward`/`ClaimRewardMP` 把 `already`
  透出，所有调用点改成「无错误即成功」，`already` 只决定文案。
  【复核】真：队列路径还会把它记成 progressed=false 喂给跳过名单，连续 3 次判"做不了"。
* **7 `DesktopChatWithExpert` 遇空 id 卡在同一偏移** —— `"id":""` 时 `end==0`，
  只有 `end>0` 才推进偏移 → 下一轮命中同一位置，后面的合法 requestId 永远找不到。
  空 id 时推进到 `abs+6` 继续找。【复核】真（需要上游先吐一个空 id 帧才触发，属潜伏）。
* **8 配置页把合法的 `"0"` 判成非法时长** —— `DURATION_RE` 要求必须带单位，而 Go 的
  `ParseDuration("0")` 合法且后端用它表示关停（`cost_explore_interval: "0"` 关探索、
  `expiring_soon: "0"` 关快过期加权）。正则放行 `0`。【复核】真（填 0 会被前端 onsubmit 拦死）。

### 中优先级

* **9 首页积分汇总把企业版不限量算进去** —— 企业号在池内的等价表示是
  `credits = 1<<40`、`credits_total = -1`，直接相加会把"剩余"撑成天文数字、把"总额"减掉账号数。
  汇总时排除这类账号。【复核】真（Go 侧 `enterpriseUnlimitedRemain/Total` 常量确认）。
* **10 到期快照「今天到期」显示成「明天」** —— `ceil((ms - 今天00:00)/一天)`，今天 23:00 到期
  得 1 → "明天到期"，日均需耗也偏小；逐包明细版用的是日期差，两处口径不一致。
  改成日历日差。【复核】真。
* **11 第二次进「积分构成」忽略 `package_detail_limit`** —— 缓存分支只在 `cfgLoaded` 非空时
  解析 limit，而 `cfgLoaded` 只有打开配置页才赋值 → 没开过配置页就永远用默认 5。
  缺配置时补拉一次。【复核】真（我上一轮加的修复不完整）。
* **12 `probe_active.py` 硬编码容器路径 + `data:null` 崩溃** —— 路径改「环境变量 → 脚本同级
  ../auths → 旧容器路径」，空列表明确报错并返回非 0；`data` 为 null 时不再 `.get` 崩。
  【复核】真（`task_common.py` 早就修过同类，这份漏了）。
* **13 旅行/夜猫子的「可中断等待」实际不可中断** —— `sleepCtx(context.Background(), …)` 永远
  返回 true（`return` 是死代码），另有两处裸 `time.Sleep`。给 `RunTravelNow`/`RunBlackcatNow`/
  `travelOne`/`travelAdopt` 加 ctx，`runBatch` 传排程 ctx，面板入口走 Background 包装。
  【复核】真。
* **14 连登兑换/抽奖零节流** —— 签到 800ms/号、旅行/活跃/夜猫子都有，唯独
  `RunStreakBonusNow` 没有：46 号一轮数百请求连发（正是仓库多处防范的 WAF 频控形态）。
  加账号间节流 + ctx（`RunStreakBonusNowCtx`）。【复核】真。
* **15 `X-Domain` 写成完整 URL** —— 真实抓包是 `copilot.tencent.com`（host），代码塞的是
  `https://copilot.tencent.com`。新增 `desktopDomain()` 取 host，三处（/v2/report、专家市场、
  DesktopChatWithExpert）统一。
  【复核】真（不一致确定）；影响为**潜伏**——现有桌面任务已验证能点亮，说明上游当前容忍。
* **16 `conversation_id` 未清洗就写进头** —— 带换行/控制字符时 net/http 报
  `invalid header field value` → 传输层错误 → 轮转/503。新增 `HeaderSafe()` 在写头前清洗。
  【复核】真（需要客户端 ID 里带控制字符才触发）。
* **17 密钥弹窗的内联 `onsubmit` 被 CSP 拦** —— CSP 是 `script-src 'self'`，内联处理器不执行，
  回车触发隐式提交 → 整页刷新、异步请求被中断。删掉内联属性，改 JS 绑定
  `submit` 拦默认 + `keydown` 拦回车。【复核】真。

### 低优先级

* **18 到期卡片错误提示二次转义** —— `textContent` 不解析 HTML，再套 `esc()` 会把 `<` 显示成
  `&lt;`。去掉 `esc()`。【复核】真。
* **19 单任务回读用的是动作表里的固定 code** —— `runValorantExpert` 会按域换码，
  但 `accountTaskAuto` 回读/领奖仍用 `act.TaskCode` → 进度回读 nil、自动领奖失效。
  新增 `effectiveTaskCode()`，动作与调用方共用同一口径。
  【复核】真（这是我上一轮加任务时自己引入的）。
* **20 请求归档每秒全目录扫描** —— `prune()` 每次 `os.ReadDir` + 每文件 `Stat`，
  而保留策略按天/按总量，不需要秒级。节流到 10 分钟一次。【复核】真。
* **21 跳过名单展示与运行时口径不一致** —— `skipRows` 把 `Until` 零值显示成"已跳过"，
  而 `skipBlocked` 把零值当已到期并删条目。展示侧对齐运行时。【复核】真。
* **22 「留空 = 不鉴权」与真实行为不符** —— `api_key` 不在 `CLEARABLE_CFG` 里，留空是
  "不下发"（保留旧 key）。占位符改成「留空 = 保持原密钥不变」。【复核】真。
* **23 导入在 HTTP 请求内逐号签到/查余额** —— N 个账号 = 2N 个串行上游调用，几十个号必然
  超时（前端报失败但已导入一半，重试又重复导入）。后置动作改 `go p.postImportActivate(...)`。
  【复核】真。
* **24 requestId 正则只认小写 hex** —— 上游若返回大写 hex 或 `CMB-` 前缀会被判"没找到"。
  改 `(?i)`。【复核】真（潜伏）。

### 测试

`upstream 203 / server 90 / pool 184 / panel 27 / session 20 / redisstore 6 / reqlog 7 /
auth 27 / usage 14 / scheduler 65 / cmd-server 45`，**688 通过 0 失败**；`gofmt -l` 为 0。

`ClaimReward`/`ClaimRewardMP` 的签名变化导致 3 处测试断言同步更新
（其中 `already_claimed` 那条新增了「必须透出 already」的断言）。

## v1.26.8

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

## v1.26.7

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

## v1.26.6

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

## v1.26.5

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

## v1.26.4

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

## v1.26.3

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

## v1.26.2

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

## v1.26.1

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

## v1.26.0

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
