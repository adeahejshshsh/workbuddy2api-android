# 代码缺陷审查报告

审查范围：`workbuddy2api-panel/`（Go 网关，29 243 行非测试代码 + 测试 21 489 行）、`apk-src/`（Android 壳，1 828 行 Java/XML）、仓库根与 `workbuddy2api-panel/` 下的构建与运维脚本。文档类文件不在审查范围内。

下面每一条都是"缺陷在哪 → 为什么错 → 怎么改"。按严重度排序。

---

## 严重（会损坏数据或让功能直接不可用）

### BUG-01 凭证文件可能被并发写坏，重启后账号静默消失

**位置**：`internal/auth/auth.go:345`（`SaveAtomic`）、`internal/auth/auth.go:449-450`（`ReplaceAtomic`）

**现象**：账号 auth 文件偶尔变成半截/混合的 JSON，下次启动 `LoadDir` 里 `Parse` 失败后**静默跳过**（`internal/auth/auth.go:499-502`），面板上少一个号，日志里没有任何提示。

**根因**：`ReplaceAtomic` 用固定临时文件名：

```go
tmp := path + ".tmp"
os.WriteFile(tmp, raw, mode)   // O_TRUNC 后写
os.Rename(tmp, path)
```

而 `SaveAtomic` 全文没有任何串行化。同一个账号的 `SaveAtomic` 真会并发：

- `internal/scheduler/scheduler.go:RunKeepaliveNow`（22:00 全量保活）刷完 token 后落盘；
- `internal/server/handler.go` chat 前 `NeedsRefresh` 触发 `RefreshToken` 后落盘；
- `internal/pool/cooldown.go:SetNickname`（面板同步昵称）落盘。

两个 goroutine 同时 `WriteFile` 同一个 `path+".tmp"`：A 截断→写 600 字节，B 截断→写 600 字节，A 的剩余字节落在 B 的内容之后，`Rename` 把这份交错内容换成正式文件。文件仍是"完整长度"，但 JSON 已损坏。

**修法**（二选一，推荐第一种，改动最小）：

```go
// 唯一临时名：rename 原子，最后一次完整内容胜出，不可能交错
var atomicSeq atomic.Uint64
tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomicSeq.Add(1))
```

或给同一 path 加进程内互斥：

```go
var replaceLocks sync.Map // path -> *sync.Mutex
func lockFor(path string) *sync.Mutex {
    v, _ := replaceLocks.LoadOrStore(path, &sync.Mutex{})
    return v.(*sync.Mutex)
}
// ReplaceAtomic 开头：
mu := lockFor(path); mu.Lock(); defer mu.Unlock()
```

---

### BUG-02 构建脚本注入的版本号永远是 `dev`

**位置**：`rebuild.sh:41`

```sh
VER=$(tr -d ' \r\n' < "$(dirname "$0")/VERSION" 2>/dev/null || echo dev)
```

**根因**：脚本在第 33 行已经 `cd "$REPO"`（`.../workbuddy2api/workbuddy2api-panel`），而 `VERSION` 在**上一级**目录。按文件头写的用法 `bash rebuild.sh`（相对路径）执行时，`dirname "$0"` 就是 `.`，指向 panel 目录，`VERSION` 打不开 → 重定向失败 → `|| echo dev` 兜底 → `-X main.appVersion=dev`。

结果是面板 `/panel/api/overview`、`/status` 的 `version` 恒为 `dev`，`cmd/server/main.go:47` 注释里"单一版本源"的意图落空。

**修法**：

```sh
# 在 cd 之前用脚本自身的绝对目录定位仓库根
ROOT="$(cd "$(dirname "$0")" && pwd)"
VER=$(tr -d ' \r\n' < "$ROOT/VERSION" 2>/dev/null || echo dev)
[ "$VER" = "dev" ] && echo "  ⚠ 未找到 $ROOT/VERSION，版本按 dev 注入"
```

---

### BUG-03 `credit.sh` / `signin.sh` 指向不存在的二进制，直接跑必失败

**位置**：`workbuddy2api-panel/credit.sh:16,18`；`workbuddy2api-panel/signin.sh:11-14`

**现象**：

- `bash credit.sh` → `./credit: No such file or directory`；
- `bash signin.sh` → `go: command not found`（`set -e` 直接退出）。

**根因**：

1. `rebuild.sh` 把产物写到 `$PREFIX/lib/workbuddy2api/{server,signin,login,credit,trial}`（实测目录里确实只有这五个），而 `credit.sh` 执行的是仓库内的 `./credit` —— 仓库里没有这个文件。
2. `signin.sh` 现场 `go build -o ./signin_bin ./cmd/signin`，但本机 `go` 不在 `PATH`（在 `$PREFIX/lib/go/bin`，只有 `rebuild.sh` 里 export 过）。而 `$PREFIX/lib/workbuddy2api/signin` 其实已经构建好了。

**修法**（两个脚本统一走产物目录）：

```sh
# credit.sh
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
BIN="$PREFIX/lib/workbuddy2api/credit"
[ -x "$BIN" ] || { echo "缺少 $BIN，先跑 bash rebuild.sh"; exit 1; }
exec "$BIN" -pretty
```

```sh
# signin.sh
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
BIN="$PREFIX/lib/workbuddy2api/signin"
[ -x "$BIN" ] || { echo "缺少 $BIN，先跑 bash rebuild.sh"; exit 1; }
exec "$BIN" "${1:-auths}"
```

---

### BUG-04 "一键完成全部任务"缺国际版门控，对 global 号白跑 19 次上游请求

**位置**：`internal/panel/autotask.go:1667`（`accountTaskAutoAll`）对比 `internal/panel/autotask.go:759-770`（`accountTaskAuto`）

**现象**：对国际版（realm=global）账号点"一键完成全部"，接口跑满整条流水线，返回 19 条 `查询失败`；日志里 19 条 `403 growth system is only available for personal users`。

**根因**：单任务入口有门控：

```go
if a.IsGlobal() {
    writeErr(w, http.StatusNotImplemented, "国际版账号无成长任务体系…")
    return
}
```

`accountTaskAutoAll` 里没有这一段，直接进 `runAutoAll`。`runAutoAll` 对 `autoActions` 里 19 个动作逐个调 `effectiveTaskCode` → `ListTasks`（global 号一律 403），每个都记一条 error。顺带还长期占着 per-account 锁。

**修法**：在 `accountTaskAutoAll` 取到 `a` 之后、`tryLockAccount` 之前加同一段门控（与 `internal/panel/tasks.go:38` 的 `accountTasks` 保持同口径）。

---

## 中等（功能打折、白跑上游、请求被无谓阻塞）

### BUG-05 `taskDone` 漏判"已完成未领"，一次性任务会被整条重跑

**位置**：`internal/panel/autotask.go:271-279`

```go
func taskDone(t *upstream.Task) bool {
    if t == nil { return true }
    if t.Claimed { return true }
    return t.Target > 0 && t.Current >= t.Target
}
```

**现象**：一次性任务（`target=0`，完成状态只写在 `accept_status=completed` 上）已达标但还没领奖时，单任务入口与"一键完成全部"都判 `taskDone=false`，于是把整条动作链重跑一遍（`expert_5` 是 5 次真实对话 + 5 组召唤事件），最后才领奖。

**根因**：`internal/upstream/tasks.go:219` 已经把"completed 即 claimable"这层语义做进了解析结果：

```go
Claimable: !claimed && ((tgt > 0 && cur >= tgt) || done),
```

`taskDone` 没有跟着用这个结论，两个口径分叉（`autotask.go:271` 的注释本身写的就是"已达目标但还没领取"，只是漏了 completed 这一形态）。

**修法**：

```go
func taskDone(t *upstream.Task) bool {
    if t == nil { return true }
    return t.Claimed || t.Claimable   // Claimable 已含 target 判定与 completed 判定
}
```

---

### BUG-06 单任务入口查跳过名单用固定任务码，换码任务永远命中不了

**位置**：`internal/panel/autotask.go:791`（`p.skipBlocked(a.UID, act.TaskCode)`），对比 `internal/panel/autotask.go:827`（`effCode := p.effectiveTaskCode(a, act.TaskCode)`）与 `internal/panel/autotask.go:1603`（`runAutoAll` 用的是 `effCode`）

**现象**：被判"做不了"的任务（`task_skip.json` 里已有记录）在单任务入口仍会执行，每次白跑一遍完整动作链。

**根因**：跳过名单的 key 是 `uid|真实任务码`。`runAutoAll` 和队列路径（`internal/panel/taskcenter.go:605`）都用**实际**任务码写/读；`accountTaskAuto` 在算出 `effCode` **之前**就用 `act.TaskCode` 查了一次。对按域换码的任务（目前是 `WorkBuddy_Valorant`），两个 key 永远对不上。

**修法**：把 `effCode := p.effectiveTaskCode(a, act.TaskCode)` 提到 `skipBlocked` 之前，并用 `effCode` 查询：

```go
effCode := p.effectiveTaskCode(a, act.TaskCode)
if blocked, why := p.skipBlocked(a.UID, effCode); blocked { … }
```

（`effectiveTaskCode` 内部会 `ListTasks`，放在 `tryLockAccount` 之前也顺带避免了"拿锁期间打上游"。）

---

### BUG-07 会话分配慢路径持会话写锁扫全池，把整张会话表拖到池锁与磁盘 IO 后面

**位置**：`internal/session/session.go:183-200`

```go
r.mu.Lock()
defer r.mu.Unlock()
…
if usable(e2.uid) { … }          // → pool.IsAvailableForModel（pool.mu.RLock）
…
uids := r.availableSlice(model)  // → pool.WeightedAvailableUIDsForModelRealm（全池扫描 + 排序）
```

**现象**：池子越大、并发越高，未绑定会话的请求（每个新对话的第一跳）越容易互相排队；面板上表现为"偶发整秒卡顿"，卡顿周期与 `state.json` 落盘周期（5s）吻合。

**根因**：会话表写锁内部调用了要拿 `pool.mu` 读锁、并且要做全池遍历 + 排序的函数。于是：

- 所有其他会话的命中/`Bind`/`Unbind`（handler 每请求一次 `Session.Bind`）都要等这次全池扫描结束；
- `pool.Flush`（`internal/pool/persist.go`，每 5s 持 `pool.mu` 写锁做磁盘 IO）一旦拿住池锁，会话表也跟着被堵在磁盘 IO 上。

**修法**：锁外取数，锁内只做判定与赋值：

```go
// 1) 锁外先拿可用集合（这是唯一需要 pool 锁的部分）
uids := r.availableSlice(model)
if len(uids) == 0 { return "", false }
avail := make(map[string]bool, len(uids))
for _, u := range uids { avail[u] = true }

// 2) 再进写锁：re-check + 双段分配，全部只读本地 map
r.mu.Lock()
defer r.mu.Unlock()
```

`usable()` 同理，改成在锁外先查一次单点可用性（`IsAvailableForModel`），锁内只判 `avail[...]`。

---

### BUG-08 抢在途名额失败仍把账号记为"本轮已试"，日志也会记错号

**位置**：`internal/server/handler.go:838-845`

```go
st.uid = acct.UID
st.nick = acct.NicknameValue()
tried[acct.UID] = true            // ← 先标记

if !h.cfg.Pool.Acquire(acct.UID) {  // ← 后抢名额
    …continue
}
```

**现象**：并发高峰下偶发 `503 no_healthy_account`，而 `/status` 显示还有健康号；请求流水行里的账号与实际服务账号不符。

**根因**：`PickExcludingForRealm` 已过滤在途满载账号，`Acquire` 失败只发生在"另一个 goroutine 刚抢走最后一个名额"的竞态窗口。此时该账号既没被真正使用，也被写进 `tried` 排除出本轮后续轮转——可用号被无谓浪费一次；同时 `st.uid/st.nick` 已经被写成这个没服务成功的号，日志归属错误。

**修法**：把三行移到 `Acquire` 成功之后：

```go
if !h.cfg.Pool.Acquire(acct.UID) {
    …unbindSticky / rotateBackoff / continue
}
heldUID = acct.UID
tried[acct.UID] = true
st.uid, st.nick = acct.UID, acct.NicknameValue()
```

---

## 轻微（资源、日志与一致性）

### BUG-09 信号量在 goroutine 内部获取，goroutine 数等于账号数

**位置**：`internal/panel/panel.go:1031,1043`（`packages`）；`internal/panel/taskcenter.go:537,555`（`runQueueItems`）

```go
sem := make(chan struct{}, 3)
for i, s := range accts {
    wg.Add(1)
    go func(i int, s pool.Status) {
        …
        sem <- struct{}{}          // ← 在 goroutine 里抢
        defer func() { <-sem }()
```

**现象**：账号多时一次请求/一轮队列瞬时创建 N 个 goroutine，其中 N-3 个阻塞在 channel 上；对上游的并发确实被限住了，但 goroutine、栈和闭包捕获的账号对象全部提前分配。

**根因**：`internal/panel/panel.go:syncNicknames`（`panel.go:901`）里已经修过这个模式（注释写着"bug-11：46 个号就是 46 个 goroutine 同时创建、43 个阻塞在 sem 上"），`packages` 与 `runQueueItems` 没跟着改。

**修法**：抢槽放在 `go` 之前：

```go
for i, s := range accts {
    sem <- struct{}{}              // 满 3 路就等，不再堆 goroutine
    wg.Add(1)
    go func(i int, s pool.Status) {
        defer wg.Done()
        defer func() { <-sem }()
        …
    }(i, s)
}
```

（`runQueueItems` 同理；注意 `sem` 的容量就是 `concurrency` 入参。）

---

### BUG-10 全池冷却兜底每次选号都打一条 WARN，高 QPS 下刷屏

**位置**：`internal/pool/pick.go:336`（`pickEarliestExpiryLocked` 末尾）

```go
log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", …)
```

**现象**：整池都在软冷却时，每个请求都会选到兜底号并打一行 WARN。这条日志同时经 `MultiWriter` 进面板 500 行环形缓冲（`internal/panel/ring.go`），几秒就把任务日志、错误日志全挤掉。

**根因**：兜底是**每请求路径**，日志却按"异常事件"的粒度写，没有节流。

**修法**：按 `(uid, kind)` 做时间窗节流（与 `internal/server/logging.go` 的 `cacheMissSignal` 同款实现），或把该行降级为仅在"兜底账号与上一次不同"时打印。

---

### BUG-11 `accountCheckin` 与 `accountBalance` 不复位扣费余数，余额会与真实值偏差 1 分内

**位置**：`internal/pool/cooldown.go:12`（`SetCredits`）、`internal/pool/cooldown.go:71`（`SetCreditsDetailed`）、`internal/pool/state.go:139`（`ReenableIfCredits`）；余数累加在 `internal/pool/state.go:235`

**现象**：手动"签到/刷新余额"拿到权威余额后，`entry.creditFrac` 仍保留刷新前的不足 1 分的余数；后续 `NoteModelCost` 从旧余数继续累加，`credits` 与真实余额长期差不到 1 分，`credit_floor` 的触底判定会略早/略晚。

**根因**：权威余额写入路径（`SetCreditsDetailed`、`ReenableIfCredits`、`SetCredits`）只覆盖 `credits/creditsTotal/…`，没有清 `creditFrac`；而 `creditFrac` 是"内插扣费"专用的中间量，权威值到达时应当归零。

**修法**：三个入口写 `credits` 时一并 `e.creditFrac = 0`。

---

### BUG-12 `LoadDir` 的重复 UID 告警读昵称未加锁，且覆盖语义无日志区分

**位置**：`internal/auth/auth.go:506`、`:512`、`:514`（`LoadDir` 里的 `logfmt.Label(a.UID, a.Nickname)`）

**现象**：启动日志里同 UID 的两个文件"后者胜出"，两条日志文本一致，事后无法判断到底哪个文件留下了；另外这处直读 `Nickname` 绕过了同包为消除竞争而专门加的加锁访问器，一旦 `LoadDir` 将来被并发调用（或测试并发调用）立刻变成数据竞争。

**根因**：`Nickname` 有加锁读访问器 `NicknameValue()`（同文件 78 行专门为竞争加的），这一处仍直读字段。启动期虽然还没并发，但 `LoadDir` 会在 `BackfillRealm` 之后调用 `SaveAtomic`，一旦将来把加载改成并发（或 `LoadDir` 被测试并发调用），就是真竞争。

**修法**：改用 `a.NicknameValue()`；日志里补上"保留 %s，丢弃 %s"的文件名，让覆盖结果可追溯。

---

## 修复优先级建议

1. **BUG-01**（数据损坏，且只在重启时暴露，最难排查）
2. **BUG-04 / BUG-05 / BUG-06**（面板任务链路白跑上游真实对话，直接烧积分）
3. **BUG-02 / BUG-03**（构建与运维脚本，影响版本追溯与日常运维）
4. **BUG-07 / BUG-08**（并发路径，影响高负载下的可用性与日志可信度）
5. **BUG-09 ~ BUG-12**（资源与一致性，可随手一起改）
