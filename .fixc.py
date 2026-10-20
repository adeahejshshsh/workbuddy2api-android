import re

WS = "/storage/emulated/0/新建文件夹/workbuddy2api"

def rep(path, old, new, tag):
    s = open(path, encoding="utf-8").read()
    assert old in s, tag + " 锚点未命中"
    open(path, "w", encoding="utf-8").write(s.replace(old, new, 1))
    print(tag, "ok")

# ---------- #10：expiresIn 缺失不写「现在」 ----------
rep(WS + "/workbuddy2api-panel/internal/panel/login.go",
'''	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),''',
'''	// 上游没下发 expiresIn（或为 0）时不要写「现在」（bug #10）：那会让
	// NeedsRefresh(10m) 恒真 → **每个** chat 请求都先打一次 refresh（多一次 RTT、
	// 放大 refresh 竞态窗口，失败还会换号）。upstream.RefreshToken 对同一情况有
	// 「缺 expiresIn 保留旧值」的防御，登录路径没对齐：这里留 0（= 未知，交给首次
	// 刷新补齐）。
	var expiresAt int64
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a := &auth.Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,''')

# ---------- #13：已签到不再把错误文案回给前端 ----------
rep(WS + "/workbuddy2api-panel/internal/panel/panel.go",
'''	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		}
	} else {''',
'''	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		// 幂等拒绝（今天已签到）是**正常结果**，不是错误（bug #13）：此前把
		// err.Error() 原样回给前端，toast 会拼出「（code=10001 msg=今天已签到）」
		// 这种错误文本，而按钮已经显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
			checkinMsg = "今天已签到"
		} else {
			checkinMsg = err.Error() // 其它业务错误照常回显，同时继续查余额
		}
	} else {''')

# ---------- #14：日志文案与实际行为对齐 ----------
rep(WS + "/workbuddy2api-panel/internal/panel/panel.go",
    'log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")',
    'log.Printf("panel: 手动全量签到已触发（猫猫旅行是独立排程，需另点「旅行巡检」）")')

# ---------- #12：Until 零值视为已到期 ----------
rep(WS + "/workbuddy2api-panel/internal/panel/taskgate.go",
'''	if e.Until.IsZero() || time.Now().Before(e.Until) {
		return true, e.Why
	}''',
'''	// Until 零值 = 已到期（bug #12）：正常路径 skipNote 一定会写 Until，但
	// task_skip.json 被手改 / 被旧版本写入 / 写入中途断电时，会留下
	// Fails>=3 && Until==0 的条目 —— 该 (账号,任务) 会**永远**被判"做不了"，
	// 7 天自动放回的承诺失效，只能手动清空名单。
	if !e.Until.IsZero() && time.Now().Before(e.Until) {
		return true, e.Why
	}''')

# ---------- #19：连登日志统一 logfmt.Label ----------
p = WS + "/workbuddy2api-panel/internal/scheduler/streak.go"
s = open(p, encoding="utf-8").read()
old = 'log.Printf("streak-bonus %s: ★ 兑换 %s 档 …", a.UID,'
if old in s:
    s = s.replace('log.Printf("streak-bonus %s: ★ 兑换 %s 档 …", a.UID,',
                  'log.Printf("streak-bonus %s: ★ 兑换 %s 档 …", logfmt.Label(a.UID, a.NicknameValue()),', 1)
    open(p, "w", encoding="utf-8").write(s)
    print("#19 ok")
else:
    # 行可能被 gofmt 换行，退化为按行扫描
    lines = s.split("\n")
    done = False
    for i, l in enumerate(lines):
        if "streak-bonus" in l and "a.UID" in l:
            lines[i] = l.replace("a.UID", "logfmt.Label(a.UID, a.NicknameValue())", 1)
            done = True
            break
    if done:
        open(p, "w", encoding="utf-8").write("\n".join(lines))
        print("#19 ok (按行替换)")
    else:
        print("#19 MISS")
