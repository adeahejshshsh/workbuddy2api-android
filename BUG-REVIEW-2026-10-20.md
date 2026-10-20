# workbuddy2api 应用缺陷审查（2026-10-20）

范围：工作区全部源码（`workbuddy2api-panel` Go 后端 + 内嵌面板前端、`apk-src` Android 壳、`*.sh` / `*.py` 工具脚本）。
只记应用 bug（功能/行为/性能缺陷），不记安全议题。每条给「位置 → 现象/原因 → 修法」。

验证方式：`go build ./...` 全量通过；下文标「实测」的条目另写了临时 Go 测试在真机跑过（测试文件已删除）。

---

## 一、高优先级（会造成请求失败或整条会话报废）

### 1. tool 结果重排有缺口：非 tool 消息插在 assistant 与首个 tool 结果之间时不重排

**位置**：`workbuddy2api-panel/internal/upstream/tool_pairing.go:87`

```go
if len(results) == 0 {
    break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
}
```

**现象（实测）**：请求体

```
assistant(tool_calls=[c1]) → developer/system("notice") → tool(c1)
```

经 `PrepareBodyOpt` 出站后顺序仍是 `assistant → system → tool`，一条都没挪。

**原因**：`repackToolResultBlocks` 只在「已经收到过至少一条结果」之后才把中间消息记为 `between`；结果集为空时直接 `break`，把「插入物」留在了原位。`cleanupOrphanToolCalls` 只做删除、不做重排，且 c1 两侧齐全不会被删 → 出站载荷违反「tool 结果必须紧跟 assistant.tool_calls」，上游 deepseek 系判 11148（`tool_call_sequence_broken`），客户端每次重放同一条历史都失败，换号无效。

**修法**：把插入物收集提前，只在真正遇到「新一组 assistant.tool_calls」或「不属于本批的 tool 结果」时 break；仅当最终 `len(results) > 0` 时才应用重排（否则原序返回，保持零回归）：

```go
for i < len(messages) {
    mm, ok := messages[i].(map[string]any)
    if !ok { break }
    role, _ := mm["role"].(string)
    if role == "tool" {
        id, _ := mm["tool_call_id"].(string)
        if !want[id] { break }
        results = append(results, messages[i])
        if sawNonTool { changed = true }
        i++
        continue
    }
    if role == "assistant" {
        if next, _ := mm["tool_calls"].([]any); len(next) > 0 { break }
    }
    between = append(between, messages[i]) // 结果未到齐也先缓冲
    sawNonTool = true
    i++
}
if len(results) == 0 {
    out = append(out, between...) // 没有结果：不重排，原序写回
    continue
}
out = append(out, results...)
out = append(out, between...)
```

---

## 二、中优先级（功能口径错 / 上游请求放大 / 显示与事实不符）

### 2. 「一键完成全部」对每个账号放大 38~57 次任务列表请求

**位置**：`workbuddy2api-panel/internal/panel/autotask.go:205`（`effectiveTaskCode`）、`:796`、`:1580`

**现象**：`runAutoAll` 对 `autoActions` 里 19 个动作各调一次 `effectiveTaskCode` → 内部先 `taskByCode`（1 次 `ListTasks`），查不到再 `ListTasks`（第 2 次）；随后主流程又 `taskByCode(a, effCode)`（第 3 次）。单个账号一轮 ≈ 38~57 次 `GET /v2/activity/growth/tasks`，且 `taskByCodeWaiting` 还会再轮询最多 3 次。

**原因**：每次判定都重新拉上游列表，没有请求内缓存。

**修法**：在 `runAutoAll` / `accountTaskAuto` 入口拉一次（默认 + mp 两份），构造 `map[string]upstream.Task` 传给 `effectiveTaskCode` / `taskByCode` / `taskDone`，把「列表查询」从每动作一次降到每账号一次；`taskByCodeWaiting` 的轮询仍走真实请求（那是等异步计分，必须重新读）。

### 3. `cmd/credit` 的余额口径与网关分叉，会输出自相矛盾的包数据

**位置**：`workbuddy2api-panel/cmd/credit/main.go:85`（自带 `packageRemainUsed`）vs `workbuddy2api-panel/internal/upstream/client.go:2290`（权威实现）

**现象**：上游对账延迟给出 `CycleCapacityUsed > CycleCapacitySize` 时，`cmd/credit` 版本保留 `remain` 旧值 → 输出「剩余 50 / 已用 150 / 面额 100」；网关面板（走 upstream 版）则显示一致的数据。

**原因**：`cmd/credit` 的副本缺两条：`used > size` 钳制、`remain = size - used` 反算。

**修法**：删掉副本，直接调用 `upstream` 的导出口径（把 `packageRemainUsed` 改成导出函数 `PackageRemainUsed` 供 cmd 复用）；若要保持零依赖，按 `client.go:2290-2331` 逐行对齐补齐这两条钳制。

### 4. 重复 UID 的告警文案与实际行为相反

**位置**：`workbuddy2api-panel/internal/auth/auth.go:516`

**现象**：`auths/` 下两个文件同 UID 时日志写「保留 <prev>，丢弃 <f>」，但代码把两条都 `append` 进 `out`，`pool.upsertLocked` 是「后写覆盖」，实际保留的是 **f**。

**修法**：二选一 —— 真去重（`if _, dup := seenUID[a.UID]; dup { log(...); continue }`，保留先出现的文件），或把文案改成「后者覆盖前者（保留 %s）」。推荐前者：与日志承诺一致，也避免 `SyncToDir` 拿两条同 UID 反复 upsert。

### 5. WAF 分支：请求日志状态码与真实响应码不一致

**位置**：`workbuddy2api-panel/internal/server/handler.go:947`（`st.status = status`，此处 status 是上游 403）与 `:1498`（末端 `writeOpenAIErrorFull(w, status, ...)`，ErrWafBlock 保持默认 503）

**现象**：上游 WAF 403 → 客户端实际收到 503 + `code=waf 403 block / waf_ip_blocked`，但面板「运行日志」该行 status 记 403。按状态码做统计/告警会两头对不上。

**修法**：末端写响应前统一回写：`writeOpenAIErrorFull(...)` 之后紧跟 `st.status = status`（当前已有，但轮转分支里先被写成 403），最简做法是在 `:1498` 前加 `st.status = status`（把 947 那处的赋值改为只用于日志的局部变量，不要写进 `st`）。

### 6. Android 壳：自愈退避被误判成「系统冻结」

**位置**：`apk-src/java/com/dsh/wb2api/GatewayService.java:366`（冻结算账）与 `:458`（`selfHeal` 里的 `sleep(backoffMs(n))`）

**现象**：`lastTickAt` 在自愈前已更新，退避 ≥32s（第 5 次自愈起）后下一轮 `now - lastTickAt > 3*TICK_MS` 成立 → `freezes++` 并打「系统冻了我 N 秒」。界面上「冻过 N」与日志会把「自己在退避」误报成「被系统冻结」，排查方向被带偏。

**修法**：`selfHeal` 返回后重置心跳基准（`lastTickAt = SystemClock.elapsedRealtime();`），或在退避期间置一个 `inBackoff` 标志让冻结判定跳过该轮。

### 7. `ensureConfig`：配置里没有 `api_key` 键时不会补，网关会 `log.Fatalf` 起不来

**位置**：`apk-src/java/com/dsh/wb2api/GatewayService.java:637`

```java
if (!m.find()) return;   // 没有该键：交给网关自己的默认值
```

**现象**：用户手改/精简过 `config.json`（删掉 `api_key`，但 `listen` 仍是 `:7863`）时，`config.normalize()` 的 `checkListenKey` 判定「非回环 + 空密钥」→ `log.Fatalf` 退出（rc=1）。看门狗按退避无限重拉，界面永远「启动中」，日志只有一行 fatal。

**修法**：`m.find()` 为 false 时不要 `return`，而是把 `api_key` 键补进去（在最后一个 `}` 前插入 `"api_key": "<随机>"`，或用 `JSONObject` 解析后回写）；同时把 `listen` 保持用户原值不动。

### 8. 面板前端：积分构成页排序比较器 O(n²·log n)

**位置**：`workbuddy2api-panel/internal/panel/app.js:2481`

```js
names.sort((x, y) => {
  const sz = n => Math.max(...list.map(a => {
    const f = pkBySource(a.packages || []).find(s => s.key === n);
    return f ? f.size : 0;
  }));
  return sz(y) - sz(x);
});
```

**现象**：比较器里对每个候选都重跑 `pkBySource`（每次遍历全部账号的全部包）。账号 20+、包 100+ 时打开「积分构成」会明显卡顿（手机端尤其）。

**修法**：先建一次索引再排序：

```js
const maxSizeOf = {};
for (const a of list) for (const s of pkBySource(a.packages || []))
  maxSizeOf[s.key] = Math.max(maxSizeOf[s.key] || 0, s.size);
names.sort((x, y) => (maxSizeOf[y] || 0) - (maxSizeOf[x] || 0));
```

### 9. 面板前端：到期卡片「检查」按钮可能永久卡死

**位置**：`workbuddy2api-panel/internal/panel/app.js:2840-2853`（`loadExpiry`）

**现象**：`expFetching = true` 之后只在函数正常末尾复位；若 `renderExpiry(d)` 抛异常（DOM 被替换、`pkAccountSegments` 遇到畸形包等），标志永远为真，之后点「检查」全部被 `if (expFetching) return;` 静默吞掉。

**修法**：改成 `try { ... } finally { expFetching = false; }`，并让 `renderExpiry` 内部对 `list`/`packages` 做空值兜底。

---

## 三、低优先级（可观测性 / 死代码 / 一致性）

### 10. 全冷却兜底日志的节流标记永不复位

**位置**：`workbuddy2api-panel/internal/pool/pick.go:339-343`

`p.lastFallbackLog` 只在账号变化时更新，账号恢复后再次进入兜底不会再打 `fallback_earliest_expiry`。运维看不到「同一个号第二次被兜底捞起」。

**修法**：`NoteSuccess` / `Revive` / 冷却到期（`healthy()` 由假转真的路径）里清空 `lastFallbackLog`；或改成「每个账号 + 每 10 分钟最多一条」。

### 11. `schoolVouchers` 的并发闸门失效

**位置**：`workbuddy2api-panel/internal/panel/taskcenter.go:813`

同文件 `tasksScanAll`（:179）与 `startGrowthQueue`（:541）都已把 `sem <- struct{}{}` 提到 `go func` 之前，只有这里还在 goroutine 内部抢槽：账号多时瞬间创建 N 个 goroutine 全阻塞在 channel 上（并发上限只限住了上游、没限住资源）。

**修法**：把 `sem <- struct{}{}` 移到 `wg.Add(1)` 之前（与另两处同款）。

### 12. `enterpriseResourceCtx` 的 ctx 只作用于重试间隔

**位置**：`workbuddy2api-panel/internal/upstream/client.go:2109`

`retryBillingTransientCtx` 能被打断，但单次 `billingJSON` 请求不带 ctx（用的是 `http.NewRequest` + client 120s 超时），SIGTERM 时最坏仍等 2×30s 才能收尾，挤压 `run.sh` 的 8s 停机预算。

**修法**：给 `billingJSON` 加 `billingJSONCtx(ctx, ...)`（`http.NewRequestWithContext`），`enterpriseResourceCtx` / `UserResourceDetailedWithExpiryCtx` / `DailyCheckinCtx` 统一走 ctx 版。

### 13. 注释与函数错位

**位置**：`workbuddy2api-panel/internal/pool/realm.go:15-21`

`AvailableUIDsForRealm` 的说明（含「realm=="" 退化为 AvailableUIDs」）挂在了 `IsAvailableForModel` 头上，`AvailableUIDsForRealm` 自己的文档在 `:47` 之前缺失。godoc 里会读成「IsAvailableForModel 按 realm 过滤」。

**修法**：把该段注释移到 `:47` 的 `AvailableUIDsForRealm` 上方，给 `IsAvailableForModel` 单独写两行说明。

### 14. 前端死代码：无意义的三元

**位置**：`workbuddy2api-panel/internal/panel/app.js:401`

```js
} else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
```

两个分支都返回空串。**修法**：删掉 `+ (...)` 部分（若要显示在途数，用 `s.in_flight > 0 ? ' · 在途 '+s.in_flight : ''`）。

### 15. Android 壳：只重写了废弃的 `onReceivedError`

**位置**：`apk-src/java/com/dsh/wb2api/MainActivity.java:201`

`onReceivedError(WebView,int,String,String)` 在部分 ROM/WebView 上对**子资源**失败也会回调 → `pageHadError=true`、`panelLoaded=false` → `tick` 反复自动重载面板（表现为面板自己闪）。

**修法**：改重写 `onReceivedError(WebView, WebResourceRequest, WebResourceError)`，只在 `req.isForMainFrame()` 时置 `pageHadError`；旧签名保留为空实现（兼容 API < 23）。

### 16. `watchdog.sh` 自愈后未复位外部实例提示标记

**位置**：`watchdog.sh` 的 `noted` 变量

「进程在但不通 → 杀掉重拉」分支里 `fail=0` 但 `noted` 不复位；若此前判过「7863 是外部实例」，之后每次切回本进程实例都不会再打「已恢复正常」这类提示（只影响日志可读性）。

**修法**：自愈成功后加 `noted=0`。

---

## 四、复核过但**不是** bug 的点（避免误改）

- `internal/upstream/sanitize.go` 的指纹改写表实测生效：业务码数字串会被改写成带连字符形态，`header` 键名会被缩写成 `hdr`，键值段整段删除；预检词表与两处正则口径一致（写过临时测试实跑确认，非纸面判断）。
- `internal/upstream/payload.go` 的 `translateMaxCompletionTokens` / `clampGPTMinMaxTokens`：0/负数不翻译、不抬升，符合注释承诺。
- `internal/server/logging.go` 的 `responseObserver` 已实现 `Flush`/`Unwrap`，SSE 逐帧刷新不会被包装层吃掉。
- `internal/pool/persist.go` 的「择新恢复」三个分支（无快照 / 本地不可用 / 本地较新）逻辑自洽。
- `internal/panel/taskgate.go` 的跳过名单：`Until` 零值按「已到期」处理，与面板 `blocked` 字段同口径。

---

## 五、修复顺序建议

1. 第 1 条（11148 会话报废）—— 唯一会持续打挂整条会话的缺陷。
2. 第 6、7 条（Android 壳误报冻结 / 配置缺键起不来）—— 影响 7×24 可用性。
3. 第 2、3 条（上游放大 / 口径分叉）—— 影响额度与运维判断。
4. 其余按文件顺带清理即可。
