# 工作区代码审查报告 · 应用 Bug 清单

审查对象：`/storage/emulated/0/新建文件夹/workbuddy2api`
审查方式：逐文件静态阅读（未使用子代理），**未读取任何 markdown 文档**，只看代码本身的行为是否正确。
本报告只列**应用功能性缺陷**（行为错、算错、点了没反应、构建/部署误判），不含安全类问题。

---

## 0. 审查范围

| 模块 | 文件 | 结论 |
|---|---|---|
| Go 后端 `cmd/` | main.go、config.go、wiring.go、login/credit/trial/signin | 已全量阅读 |
| Go `internal/server` | handler.go、logging.go、degrade.go、wafip.go、backoff.go、resolve_model.go | 已全量阅读 |
| Go `internal/upstream` | client.go、sse.go、payload.go、headers.go、dsml.go、tool_pairing.go、sanitize.go、thinking.go、tasks/travel/school/desktop/blackcat/streak/usage/report/errbrief/hint/trial/profile/device_token/transport/idle/cache_key/model_catalog/context_catalog/effort_catalog/modelsdev/global_* | 已全量阅读 |
| Go `internal/pool` | pool、pick、entry、state、cooldown、persist、transition、degrade、realm、modelview、modeltier | 已全量阅读 |
| Go `internal/scheduler` | scheduler、travel、streak、blackcat | 已全量阅读 |
| Go `internal/panel` | panel、config、login、index、ring、cancel、import、tasks、taskgate、taskcenter、autotask | 已全量阅读 |
| Go 其它 | auth、httpauth、livecfg、logfmt、prompt、session（含 ids） | 已全量阅读 |
| Go 未覆盖 | redisstore、reqlog、usage、global_models、global_register（仅结构扫描） | 结构已看，细节未逐行 |
| 前端 | app.js（2852 行）、index.html（1217 行） | 交互/接口/渲染/配置表单全量核对，样式区跳过 |
| Java（APK 壳） | GatewayService、MainActivity、ReviveJobService、BootReceiver、WatchdogSelfCheck | 已全量阅读 |
| 脚本 | run.sh、watchdog.sh、rebuild.sh、push_github.sh、credit.sh、signin.sh、login.sh、patches/*.sh、docker/entrypoint.sh | 已阅读 |
| Python | patches/patch_axml_version.py、changelog_prepend.py 等 | 已阅读（未发现缺陷） |
| 配置 | AndroidManifest.xml、assets/config.json、config.example.json | 已核对 |

代码量：非测试源码约 38,000 行，测试约 21,500 行。

---

## 1. P0 —— 功能错误，用户必然撞到

### BUG-01 国际版（global）账号在面板上仍显示「签到」「任务」按钮，点了必然报错

**位置**

- `workbuddy2api-panel/internal/panel/panel.go:637`（`accountCheckin`，只判了 `IsEnterprise`）
- `workbuddy2api-panel/internal/panel/tasks.go:36`（`accountTasks`，直接 `ListTasks`）
- `workbuddy2api-panel/internal/panel/autotask.go:759`（`accountTaskAuto`，同样无门控）
- `workbuddy2api-panel/internal/panel/app.js:453-457`（按钮渲染只判 `s.enterprise`）

**现象**

池里放了国际版账号后，账号行照样出现「签到」「任务」按钮。点签到 → `POST /v2/billing/meter/daily-checkin` 对 global 域不存在；点任务 → `GET /v2/activity/growth/tasks` 返回 403（上游只对个人 CN 用户开放 growth）。面板 toast 里是一串上游错误，看起来像网关坏了。

**原因**

调度器侧一直有 D4 门控（`scheduler.RunCheckinNowCtx`、`runActivity`、`travelOne`、`blackcat` 全部 `if a.IsGlobal() { continue }`），但**面板的单号入口没有对应门控**，前端也没按 `realm` 隐藏按钮。同一个事实（global 无签到/无成长体系）在调度器和面板两处口径不一致。

**修复**

1. 后端三个入口加门控（返回明确文案，而不是打上游）：

```go
// panel.go accountCheckin
if a.IsGlobal() {
    writeJSON(w, http.StatusOK, map[string]any{
        "ok": true, "checkin_done": false,
        "checkin_message": "国际版账号无签到体系（已跳过签到，仅刷新额度）",
    })
    // 继续往下走余额查询，或直接 return
    remain, total, expiring, at, rem, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
    ...
}
```

2. `accountTasks` / `accountTaskAuto` 开头加：

```go
if a.IsGlobal() {
    writeErr(w, http.StatusNotImplemented, "国际版账号无成长任务体系（上游 growth 仅对个人 CN 用户开放）")
    return
}
```

3. 前端 `renderAccounts` 把按钮条件从 `s.enterprise ? '' : ...` 改成
   `(s.enterprise || s.realm === 'global') ? '' : ...`（签到与任务两处），并给「余额/额度」按钮保留。

**验证**：池里放一个 `realm=global` 的号 → 面板行内应只剩「余额」「暂停/解冻」「禁用」「移除」；直接 curl 打 `/panel/api/accounts/<uid>/checkin` 应返回 200 且 `checkin_message` 说明跳过，日志里不出现上游 403。

---

### BUG-02 快过期积分分桶用 `CycleEndTime`，与 `CreditPackages` 的 `DeductionEndTime` 口径冲突

**位置**

- `workbuddy2api-panel/internal/upstream/client.go:2201-2216`：`UserResourceDetailedWithExpiryCtx` 的 Accounts 结构体**根本没有 `DeductionEndTime` 字段**
- `workbuddy2api-panel/internal/upstream/client.go:2240-2253`：`earliestAt` / `expiring` 全部由 `parsePackageEndTime(acct.CycleEndTime)` 决定
- 对照 `client.go:1954-1996`：`CreditPackages` 明确写着「`DeductionEndTime` 可抵扣窗口结束 = 真失效时刻，`CycleEndTime` 是周期边界（额度重置点），判『用不完就没了』以 `DeductionEndTime` 为准」

**现象**

1. 某包真正快失效（`DeductionEndTime` 就在 3 天后），但 `CycleEndTime` 在 30 天后 → `creditsExpiring = 0`，`pool.expiringNow` 为假 → `routingWeightOf` 不给 ×3 加权，快过期的积分反而排在后面，到期作废。
2. 反过来：`CycleEndTime` 很近（周期重置）但包可以继续抵扣 → 被误判"快过期"，×3 加权优先消耗，实际没必要。
3. `/status.credits_expiring`、`credits_earliest_expiry` 与面板「到期」卡片同样偏差。

**原因**

同一个上游字段在两条链路上取了两套口径：`CreditPackages`（面板展示）用 `DeductionEndTime`，`UserResourceDetailedWithExpiryCtx`（路由 + 分桶）用 `CycleEndTime`。注释里已经写明前者才对，实现没跟上。

**修复**

```go
// 结构体补字段
DeductionEndTime int64  `json:"DeductionEndTime"` // epoch ms，真失效时刻
ExpiredTime      string `json:"ExpiredTime"`
PackageEndTime   string `json:"PackageEndTime"`

// 解析处：优先 DeductionEndTime（ms → time），缺失再回落字符串口径
var end time.Time
var ok bool
switch {
case acct.DeductionEndTime > 0:
    end, ok = time.UnixMilli(acct.DeductionEndTime), true
case acct.ExpiredTime != "":
    end, ok = parsePackageEndTime(acct.ExpiredTime)
case acct.PackageEndTime != "":
    end, ok = parsePackageEndTime(acct.PackageEndTime)
default:
    end, ok = parsePackageEndTime(acct.CycleEndTime)
}
```

**验证**：构造一份含 `DeductionEndTime = now+2d`、`CycleEndTime = now+30d` 的响应，跑 `UserResourceDetailedWithExpiry`，断言 `expiring > 0`、`earliestAt ≈ now+2d`；与 `/panel/api/packages` 里同一包的 `end_time` 一致。

---

### BUG-03 APK：已有 `config.json` 时不再检查/修复占位 `api_key`，网关起不来且看门狗无限重拉

**位置**

`apk-src/java/com/dsh/wb2api/GatewayService.java:611-613`

```java
void ensureConfig(File gwDir) {
    File cfg = new File(gwDir, "config.json");
    if (cfg.exists() && cfg.length() > 0) return;   // ← 早退
```

**现象**

首次生成被打断（进程被杀、存储满）、或用户手改过 `config.json` 留下 `"api_key": "CHANGE_ME"` / `"test_key"` 时：

1. `config.normalize()` 的 `checkListenKey` 看到 `listen:":7863"`（非回环）+ 占位 key → 直接 `log.Fatalf` 退出（rc=1）；
2. 看门狗 `selfHeal` 每 10s 一拍、按 2/4/8…60s 退避重拉，永远起不来；
3. 界面上状态停在「启动中」，第三行只有 `rc=1`，看不出真实原因；`app.log` 里也没有一条说明是 key 占位。

**原因**

`ensureConfig` 只在"文件不存在或为空"时工作，把"文件存在但内容不可用"当成正常情况。而上游生成逻辑（`config.WriteDefault`）里对占位 key 是明确拒绝的。

**修复**

```java
void ensureConfig(File gwDir) {
    File cfg = new File(gwDir, "config.json");
    String key = "wb2api-" + UUID.randomUUID().toString().replace("-", "").substring(0, 20);
    if (cfg.exists() && cfg.length() > 0) {
        // 已存在：只在 api_key 仍是占位/空时原地修好，绝不覆盖用户其它配置
        String cur = readAll(cfg);           // MainActivity.readAll 可复用（挪成 static 工具）
        boolean bad = cur.contains("\"api_key\": \"CHANGE_ME\"")
                   || cur.contains("\"api_key\":\"CHANGE_ME\"")
                   || cur.contains("\"api_key\": \"test_key\"")
                   || cur.contains("\"api_key\":\"test_key\"")
                   || cur.matches("(?s).*\"api_key\"\\s*:\\s*\"\\s*\".*");
        if (!bad) return;
        String fixed = cur.replaceAll("(\"api_key\"\\s*:\\s*\")[^\"]*(\")", "$1" + key + "$2");
        try (FileOutputStream os = new FileOutputStream(cfg)) { os.write(fixed.getBytes("UTF-8")); }
        catch (Throwable ignored) {}
        appLog(this, "config.json 里是占位/空 api_key，已替换为随机 key（网关否则会拒绝启动）");
        return;
    }
    ... 原有生成逻辑 ...
}
```

**验证**：把 `gateway/config.json` 的 `api_key` 改成 `CHANGE_ME` → 重启 App → 应能起来，`app.log` 有一条替换记录，`/healthz` 可达。

---

### BUG-04 APK：`injected` 先置真再注入，注入失败后永不重试，面板永久 401

**位置**

- `apk-src/java/com/dsh/wb2api/MainActivity.java:192-195`
- `apk-src/java/com/dsh/wb2api/MainActivity.java:435-437`

```java
if (!injected) {
    injected = true;      // ← 先置位
    injectKey();
}
...
void injectKey(final int attempt) {
    apiKey = readApiKey();
    if (apiKey.isEmpty()) { toast("读不到 config.json 里的 api_key"); return; }  // ← 只弹一次
```

**现象**

`readApiKey()` 读不到 key（首次启动时 `ensureConfig` 还没落盘、文件被清、解析失败）时只弹一次 toast，`injected` 恒为 true，此后 `onPageFinished` 不再注入；面板所有 `/panel/api/*` 一直 401，用户只能手点「重载面板」（而重载也只是把 `injected` 复位，再赌一次同样的时序）。

**原因**

"是否已注入"这个状态在**尝试前**置位，而不是在**成功后**置位；失败分支没有回滚。

**修复**

```java
// onPageFinished
if (!injected) {
    injectKey();          // 成功路径里再置 injected = true
}
```

```java
// injectKey 成功回调
public void onReceiveValue(String got) {
    boolean ok = got != null && got.contains(want);
    if (ok) { injected = true; web.reload(); return; }
    if (attempt >= 2) { injected = false; /* 下次 onPageFinished 还能再试 */
        logLine.setText("自动登录失败：面板拿不到 api_key，请点「重载面板」重试");
        logLine.setTextColor(WARN); return; }
    ui.postDelayed(() -> injectKey(attempt + 1), 300);
}
```

**验证**：临时把 `gateway/config.json` 改名 → 启动 App，面板应 401 并提示；把文件放回 → 点「重载面板」或等下一次页面加载，应自动注入成功、API 恢复 200。

---

## 2. P1 —— 行为不对，影响可观测性与运维

### BUG-05 定时余额刷新丢掉了 ctx，停机时无法中断

**位置**：`workbuddy2api-panel/internal/scheduler/scheduler.go:808-813`

```go
case <-timer.C:
    if s.TryBeginTask(TaskBalance) {
        s.RunBalanceRefreshNow()        // ← Background ctx
        s.EndTask(TaskBalance)
    }
```

**现象**

`SIGTERM` 之后，若正赶上一轮余额刷新，`RunBalanceRefreshNow()` 用 `context.Background()`，`sem` 抢占与账号间请求都不会被打断，最多拖 46 号 × 30s；`main` 里 `srv.Shutdown(5s)` 之后 `p.Flush()` 与 `store.Close()` 被挤到强杀窗口里，最后一次落盘可能丢（正是 `run.sh stop` 里 8s 预算要保护的东西）。

**原因**

同文件已经有 `RunBalanceRefreshNowCtx(ctx)`（专为停机打断写的），定时分支却调了无 ctx 版本。

**修复**

```go
case <-timer.C:
    if s.TryBeginTask(TaskBalance) {
        s.RunBalanceRefreshNowCtx(ctx)
        s.EndTask(TaskBalance)
    }
```

**验证**：造 20 个号 + `balance_refresh_minutes=1`，在刷新进行中发 SIGTERM，进程应在 1~2s 内退出（而不是等满 30s）。

---

### BUG-06 面板「全部接受」：小程序任务不分片、成功后无条件白睡 1.05s

**位置**：`workbuddy2api-panel/internal/panel/tasks.go:143-154`

**现象**

Web 侧 `codes` 按 20 条分片（`tasks.go:110-133`），但 mp 任务用一次 `AcceptTasksMP(a, mpCodes)` 全量提交；mp 任务随活动增长到十几条时数组变长，上游可能整批拒收（`failed` 一次全记）。另外这一批**成功后无论后面还有没有事**都要 `sleepAB(1.05s)`，用户点「全部接受」会莫名多等一秒多。

**原因**

mp 分支是后补的，沿用了"一次性提交 + 固定节流"的写法，没对齐 Web 分支的分片逻辑。

**修复**

```go
const batch = 20
for i := 0; i < len(mpCodes); i += batch {
    end := i + batch
    if end > len(mpCodes) { end = len(mpCodes) }
    if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes[i:end]); err != nil {
        log.Printf("panel: mp 批量接受失败 uid=%s err=%v", uid, err)
        failed = append(failed, mpCodes[i:end]...)
    } else {
        accepted += end - i
    }
    if end < len(mpCodes) {              // 只有还有下一批才节流
        if !sleepAB(uid, acceptBatchGap) { ...; return }
    }
}
```

**验证**：mock 一个 30 条 mp 任务的响应，断言发出 2 次 `AcceptTasksMP`（20 + 10），且总耗时 ≈ 1 次节流。

---

### BUG-07 夜猫子对话：不可取消的 `time.Sleep(4s)`，且最后一轮白等

**位置**：`workbuddy2api-panel/internal/upstream/blackcat.go:66`

**现象**

`RunNightChats` 每轮结束固定 `time.Sleep(4 * time.Second)`：

1. 最后一轮也睡 → 3 次对话固定多耗 4s（`blackcat_hours` 触发的整批任务里 ×N 个账号）；
2. 不可取消 → 停机/客户端断开后仍要睡完；而 `scheduler.RunBlackcatNowCtx` 的 `ctx` 在函数里根本传不下来（`RunNightChats(a, need)` 无 ctx 参数）。

**修复**

```go
func (c *Client) RunNightChatsCtx(ctx context.Context, a *auth.Auth, need int) (int64, error) {
    for i := 0; i < need; i++ {
        ...
        ok++
        if i < need-1 {                        // 最后一轮不睡
            select {
            case <-ctx.Done():
                return ok, ctx.Err()
            case <-time.After(4 * time.Second):
            }
        }
    }
    return ok, nil
}
// 保留 RunNightChats 作为 Background 包装，scheduler 改调 Ctx 版本
```

**验证**：`need=3` 的调用耗时从 ≈12s 降到 ≈8s；ctx 取消后 1s 内返回。

---

### BUG-08 `NoteModelCost` 在余额不足时把扣费余数吞掉，插值余额偏高

**位置**：`workbuddy2api-panel/internal/pool/state.go:235-246`

```go
e.creditFrac += credit
d := int64(e.creditFrac)
if d <= 0 {
    d = 0
} else {
    e.creditFrac -= float64(d)   // ← 先按未钳制的 d 扣余数
}
if d > e.credits {
    d = e.credits                // ← 再钳余额
}
e.credits -= d
```

**现象**

账号 `credits = 1`，本次 `credit = 3.5`：`creditFrac` 变成 3.5 → `d = 3` → 余数被减到 0.5 → 余额被钳成 `d = 1` → 实际只扣了 1 分，另外 2 分**既没扣余额也没留在余数里**。余额被高估 2 分，`credit_floor` 判据（`credits < floor` 才拦收费模型）因此延后触发，正是保底想防的"打穿"场景。

**原因**

"扣余数"和"扣余额"用了两个不同的 d（未钳制 / 钳制后）。

**修复**

```go
e.creditFrac += credit
d := int64(e.creditFrac)
if d <= 0 {
    d = 0
} else if d > e.credits {
    // 余额不够：余数清零（欠的部分已无从抵扣），只扣现有余额
    d = e.credits
    e.creditFrac = 0
} else {
    e.creditFrac -= float64(d)
}
e.credits -= d
```

**验证**：`credits=1`、`credit=3.5` 调一次 `NoteModelCost` → 断言 `credits==0` 且 `creditFrac==0`；`credits=100`、`credit=3.5` 连调两次 → `credits==94`、`creditFrac==0`。

---

### BUG-09 `rebuild.sh` 编译失败仍以 0 退出，CI/自动化会误判成功

**位置**：`rebuild.sh:44-56`

```bash
for c in server signin login credit trial; do
  if go build ... ; then echo "OK ..."; else echo "FAIL"; tail -6 "$PREFIX/tmp/e_$c.log"; fi
done
echo "== 产物 =="
ls -la "$OUT"
```

**现象**

任何一个 `cmd/*` 编译失败，脚本只打印 `FAIL` 并继续，最后 `exit 0`。接在 CI / 发版流水线里就是"构建失败但流水线绿"。

**原因**

循环内没有累计失败状态，脚本末尾也没有 `exit`。

**修复**

```bash
rc=0
for c in server signin login credit trial; do
  [ -d "./cmd/$c" ] || continue
  printf "  %-8s " "$c"
  if go build ... ; then echo "OK ..."; else echo "FAIL"; rc=1; tail -6 "$PREFIX/tmp/e_$c.log"; fi
done
...
exit "$rc"
```

**验证**：故意在 `cmd/trial/main.go` 里制造一个语法错误 → `bash rebuild.sh; echo $?` 应输出 1。

---

## 3. P2 —— 小缺陷，建议顺手修

### BUG-10 桌面事件 `X-Request-ID` 同一毫秒内可能重复

**位置**：`workbuddy2api-panel/internal/upstream/desktop.go:134`

```go
req.Header.Set("X-Request-ID", deriveID(a, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
```

`deriveID` 对同一账号恒定，后缀只有微秒尾数的 6 位；同一账号在同一微秒内发两个桌面上报（`ReportDesktopEvent` 与 `ReportWebEvent` 并发，或批量任务里连续调用）会撞同一个 ID。

**修复**：加进程内原子序号：

```go
var desktopReqSeq atomic.Uint64
suffix := fmt.Sprintf("%d%04d", time.Now().UnixNano()%1e6, desktopReqSeq.Add(1)%10000)
```

**验证**：并发调用 100 次 `ReportDesktopEvent`，收集 `X-Request-ID` 应无重复。

---

### BUG-11 `syncNicknames` 为每个账号起 goroutine，且 sem 在 goroutine 内获取

**位置**：`workbuddy2api-panel/internal/panel/panel.go:884-910`

```go
for _, j := range jobs {
    wg.Add(1)
    go func(j job) {
        ...
        sem <- struct{}{}        // ← 在 goroutine 里抢，46 号就 46 个 goroutine 挂着
        defer func() { <-sem }()
```

**现象**

46 个账号 = 46 个 goroutine 同时创建、43 个阻塞在 `sem <-`；且这条路径没有 ctx，点「刷新」时用户关页面也不会中止。功能上没错，但和同文件 `packages`（也是同款写法）一起把"并发上限"做成了"goroutine 上限没管"。

**修复**：把获取信号量挪到派发循环（与 `scheduler.RunBalanceRefreshNowCtx` 一致），或改用 `errgroup.SetLimit(3)`。

**验证**：46 号时 `runtime.NumGoroutine()` 在刷新期间增量 ≈3 而不是 ≈46。

---

### BUG-12 三个脚本的 shebang 在本机不存在，直接执行会 `bad interpreter`

**位置**

- `workbuddy2api-panel/signin.sh:1` → `#!/bin/bash`
- `workbuddy2api-panel/credit.sh:1` → `#!/usr/bin/env bash`
- `workbuddy2api-panel/login.sh:1` → `#!/usr/bin/env bash`

本机实测 `/bin/bash`、`/usr/bin/env` 都不存在（只有 `/data/data/com.dsharnessmobile.shell/files/usr/bin/bash`），而同仓库 `run.sh` / `watchdog.sh` / `rebuild.sh` / `patches/*.sh` 用的都是 PREFIX 绝对路径。

**现象**：`./signin.sh` 报 `bad interpreter: No such file or directory`；`run.sh` 注释里也写了"脚本本身没有 x 位，必须 `bash xxx.sh`"——但这三个文件在文档/习惯上仍会被直接执行。

**修复**：统一 shebang：

```bash
#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
```

或在文件头注明「必须 `bash signin.sh` 执行」，并在 `run.sh` 里调用时保持 `bash "$DIR/xxx.sh"`。

**验证**：`bash -n signin.sh` 通过；按 shebang 直接 `./signin.sh` 不再报 bad interpreter（若工作区 noexec 则改用 `bash signin.sh`）。

---

### BUG-13 `panel.accountCheckin` 对 global 账号会把上游错误塞进 `checkin_message`（BUG-01 的伴生表现）

**位置**：`workbuddy2api-panel/internal/panel/panel.go:647-649`

```go
} else {
    checkinMsg = err.Error() // 其它业务错误照常回显，同时继续查余额
}
```

配合前端 `toast('签到完成…（' + r.checkin_message + '）')`（`app.js:552`），国际版账号点签到后会出现「签到完成（http_error: upstream 404）」这类自相矛盾的提示。修 BUG-01 后自然消失；若暂不修 BUG-01，至少把 `err` 分支的文案改成「签到失败：…」而不是「签到完成（…）」。

**验证**：见 BUG-01。

---

## 4. 复核清单（改完怎么确认）

| Bug | 一句话验证 |
|---|---|
| BUG-01 | global 号面板只剩余额/暂停/禁用/移除；curl checkin 返回 200 且说明跳过 |
| BUG-02 | 构造 `DeductionEndTime=now+2d`、`CycleEndTime=now+30d` → `expiring>0` 且与 `/panel/api/packages` 的 end_time 一致 |
| BUG-03 | 把 config.json 的 key 改成 CHANGE_ME → 重启 App 能起，app.log 有替换记录 |
| BUG-04 | 临时改名 config.json 再放回 → 面板能自动恢复 200，无需手点重载 |
| BUG-05 | 余额刷新进行中 SIGTERM → 1~2s 内退出 |
| BUG-06 | 30 条 mp 任务 → 2 次 AcceptTasksMP，总耗时约 1 次节流 |
| BUG-07 | need=3 耗时 ≈8s；ctx 取消后 1s 内返回 |
| BUG-08 | credits=1 / credit=3.5 → credits=0 且 creditFrac=0 |
| BUG-09 | 制造编译错误 → `bash rebuild.sh; echo $?` 得 1 |
| BUG-10 | 并发 100 次上报 → X-Request-ID 无重复 |
| BUG-11 | 46 号刷新期间 goroutine 增量 ≈3 |
| BUG-12 | 直接执行 signin.sh 不再 bad interpreter |

---

## 5. 审查中确认**没有**问题的关键点（避免重复排查）

- `internal/upstream/sanitize.go` 的指纹改写对（错误码那串数字中间插入连字符、`CLI tool for Claude` 去掉 `tool`、`give feedback` 换成 `provide feedback` 等）逐字节核对过，**不是空操作**（同一行里左右两侧看起来一样只是终端渲染的错觉，实际字节长度不同）。
- `patches/patch_axml_version.py` 的 AXML 偏移（`off+hsize+8` 取 attributeStart、`a+15` 取 dataType）与 ResXMLTree 布局一致，定位正确。
- 前端 `index.html` 无重复 id；`app.js` 里 `$('...')` 引用的 id 全部存在。
- 面板配置表单的 40 个字段与 `CFG_MAP` 一一对应（无"表单有、保存不进去"的字段）。
- 前后端接口路径逐条比对：前端调用的每个 `api()` 路径都有对应路由（`accounts/{uid}/…` 为模板拼接，非缺失）。
- `cmd/server/config.go` 的 `ApplyJSON → applyEnv → normalize` 顺序、`saveConfig` 的合并/规范化/EBUSY 兜底逻辑自洽。
- `internal/pool/pick.go` 的 tier 分层、探索窗口、LRU 兜底、usedSeq 单调序号逻辑自洽（`cands[:0]` 复用底层数组后 `candsAll` 仍指向完整列表，正确）。
- `internal/session/session.go` 的 StartGC 局部 stop channel、慢路径 re-check + `DelBind`、`Bind` 节流均正确。
- `internal/server/handler.go` 的轮转循环：租约 `heldUID` 由函数出口 defer 统一释放，所有 `break`/`return` 分支都不泄漏；内容拦截降级重试的 `i--` 有 `degradedApplied` 兜底，不会死循环。

---

## 6. 未逐行覆盖（如需可补）

- `internal/usage/usage.go`（853 行，桶折叠/保留期）、`internal/reqlog/reqlog.go`（620 行，JSONL 归档轮转）、`internal/redisstore/redisstore.go`、`internal/upstream/global_models.go`（709 行）与 `global_register.go`：仅做结构与接口一致性检查，未逐行验证时序/边界。
- `internal/panel/autotask.go` 的动作实现体（1713 行中约 1200 行为各任务的上游调用链）：入口/门控/跳过名单/领奖回读已核对，单个任务的具体事件载荷未逐字段对照上游抓包。
- 21,500 行 `*_test.go` 未作为审查对象（按需求只看应用代码）。
