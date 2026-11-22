package com.workbuddy.api;

import android.app.Activity;
import android.app.Dialog;
import android.content.Intent;
import android.graphics.Typeface;
import android.os.Handler;
import android.text.TextUtils;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.EditText;
import android.widget.HorizontalScrollView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import java.util.List;

/**
 * 账号池（分区：账号卡、行内动作、更多菜单、模型开关、添加/导入/导出/登录）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenAccounts {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenAccounts(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderAccountsShell(); }
    void renderShell() { renderAccountsShell(); }
    void renderBody(PanelService.Overview o) { renderAccounts(o); }

    // ── 与 Console 的共享能力（保持方法体与本类原有写法一致）────────────────
    private void toast(String s) { c.toast(s); }
    private void show(View v) { c.show(v); }
    private void setBar(String t, boolean back, View... acts) { c.setBar(t, back, acts); }
    private ScrollView scrollOf(LinearLayout col) { return c.scrollOf(col); }
    private ScrollView scrollOf(LinearLayout col, boolean sb) { return c.scrollOf(col, sb); }
    private void refresh(LinearLayout col) { c.refresh(col); }
    private void open(String id) { c.open(id); }
    private View iconAction(int icon, String desc, Runnable r) { return c.iconAction(icon, desc, r); }
    private void loadOverview(boolean quiet) { c.loadOverview(quiet); }
    private ScrollView currentScroll() { return c.currentScroll; }
    private void setCurrentScroll(ScrollView sv) { c.currentScroll = sv; }

    // ── 账号 ───────────────────────────────────────────────────────────────
    private void renderAccountsShell() {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));
        LinearLayout col = Theme.column(a);
        col.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
    }

    private void renderAccounts(PanelService.Overview o) {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));

        LinearLayout col = Theme.column(a);

        // 概览条：总数 / 可用 / 冷却 / 禁用
        LinearLayout hero = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.addView(Theme.kpi(a, String.valueOf(o.total), "账号", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.healthy), "可用", Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.cooling), "冷却", Theme.WARN),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.disabled), "禁用", Theme.BAD),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        hero.addView(row);
        // 积分合计（issue #197）：另起一行，不塞进上面四个 weight=1f 的格子 ——
        // 五列在竖屏会被挤成两位数折行。
        String[] creditSum = creditsSummary(o.accounts);
        if (creditSum != null) {
            LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, Math.max(1, Theme.dp(a, 0.6f)));
            dlp.topMargin = Theme.dp(a, Theme.S3);
            dlp.bottomMargin = Theme.dp(a, Theme.S2);
            View line = new View(a);
            line.setBackgroundColor(Theme.LINE);
            hero.addView(line, dlp);
            hero.addView(Theme.kpi(a, creditSum[0], creditSum[1], Theme.INK));
        }
        col.addView(hero, Theme.cardLp(a, Theme.S4));
        lockCard(col, o);
        expiryCard(col, o.accounts);

        if (o.accounts.isEmpty()) {
            col.addView(Theme.empty(a, "还没有账号\n点右上「＋」导入文件或登录添加"));
        } else {
            for (PanelService.Account acc : o.accounts) col.addView(accountCard(acc));
        }
        refresh(col);
    }

    /**
     * 积分合计（issue #197）：口径逐条对照网页端 app.js 的 sCredits —— 同一个池子
     * 两个界面各算各的，迟早对不上。
     *
     * 网页端原文：
     * <pre>
     *   const isUnlimited = s =&gt; Number(s.credits_total) === -1 || Number(s.credits) &gt;= 1e12;
     *   remSum = Σ credits         （仅 !isUnlimited）
     *   totSum = Σ credits_total   （仅 !isUnlimited 且 credits_total &gt; 0）
     * </pre>
     *
     * 三条口径必须一起抄：
     *   1. **不限量哨兵要排除**：credits_total == -1 或 credits >= 1e12 视为不限量，
     *      既不进分子也不进分母 —— 漏掉这条，一个不限量号就能把总额撑成天文数字；
     *   2. 分母只累加 credits_total &gt; 0 —— 0 是「未知」，不是「零额」；
     *   3. 禁用号的 credits / credits_total 本来就是 0（封号前的快照在 CreditsRaw），
     *      所以自然进不了分子分母，与 state.go 里「面板积分合计」同口径。
     *
     * 与网页端唯一的措辞差异：分子上钳了 Math.max(0, credits)。网页端是
     * `Number(s.credits) || 0`，负值会原样累加；上游不会给负数，这里只是防脏数据
     * 让合计变小 —— 出现负值时宁可显示 0，也不要让「合计」比单号还小。
     *
     * 返回 {数值, 标签}；没有可展示的内容返回 null（渲染方据此整行不画）。
     * 全部账号都是不限量时给「不限量」而不是「0」—— 0 会被读成「一分不剩」。
     */
    static String[] creditsSummary(java.util.List<PanelService.Account> accs) {
        if (accs == null || accs.isEmpty()) return null;
        long rem = 0, tot = 0;
        int counted = 0, unlimited = 0;
        for (PanelService.Account s : accs) {
            if (isUnlimited(s)) {
                unlimited++;
                continue;
            }
            rem += Math.max(0, s.credits);
            if (s.creditsTotal > 0) tot += s.creditsTotal;
            counted++;
        }
        if (counted == 0) {
            return unlimited > 0 ? new String[]{"不限量", "积分合计"} : null;
        }
        String value = tot > 0 ? rem + " / " + tot : String.valueOf(rem);
        String label = tot > 0 ? "积分剩余 / 总额" : "积分剩余";
        // 有号被排除时必须说出来：否则「合计」和用户自己加起来的数对不上，会被当成算错。
        if (unlimited > 0) label += "（不含 " + unlimited + " 个不限量）";
        return new String[]{value, label};
    }

    /** 不限量哨兵（网页端 isUnlimited 的同口径实现；credits=1e12 是上游约定的「无限」）。 */
    static boolean isUnlimited(PanelService.Account s) {
        return s == null || s.creditsTotal == -1 || s.credits >= 1000000000000L;
    }

    // ── 模型锁池（issue #206 第 1 条：原生端此前完全看不到） ──────────────────
    //
    // 数据来自 /panel/api/overview 的 model_locks[]（Go 侧 pool.ModelLockRow），口径与
    // 选号/ModelBlocked 严格一致：只看参与选号的号、只看真正拦路由的冷却、已过期不算。
    //
    // 为什么必须显示：上游按 (账号, 模型) 记限流。某个模型被全池锁住时，聊天只回
    // no_healthy_account —— 用户分不清「这个模型不能用」还是「号都挂了」，于是去重登、
    // 去换号，全都没用。网页面板早就有这张表，原生端一直缺。
    private void lockCard(LinearLayout col, PanelService.Overview o) {
        LinearLayout card = Theme.card(a);
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(Theme.label(a, "模型锁池"),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        int n = o.modelLocks == null ? 0 : o.modelLocks.size();
        head.addView(Theme.text(a, n == 0 ? "全部可选" : n + " 个模型受限",
                11.5f, n == 0 ? Theme.INK3 : Theme.WARN, 0));
        card.addView(head);

        if (n == 0) {
            // 空态也要画出来：用户抱怨的就是「看不到」，直接不画等于还是看不到。
            TextView ok = Theme.text(a, "当前没有模型级限流，所有模型都能选到号", 12f, Theme.INK3, 0);
            ok.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            card.addView(ok);
            col.addView(card, Theme.cardLp(a, Theme.S3));
            return;
        }

        long now = System.currentTimeMillis();
        int shown = Math.min(n, 8);
        for (int i = 0; i < shown; i++) {
            PanelService.ModelLock m = o.modelLocks.get(i);
            int color = "locked".equals(m.state) ? Theme.BAD
                    : "starved".equals(m.state) ? Theme.WARN : Theme.INFO;

            LinearLayout line = new LinearLayout(a);
            line.setOrientation(LinearLayout.HORIZONTAL);
            line.setGravity(Gravity.CENTER_VERTICAL);
            line.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                    Theme.dp(a, 7), Theme.dp(a, 7));
            dlp.rightMargin = Theme.dp(a, Theme.S2);
            line.addView(Theme.dot(a, color, 7), dlp);
            TextView name = Theme.text(a, m.model, 13f, Theme.INK, 2);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            line.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            line.addView(Theme.tag(a, m.stateText(), color));
            TextView avail = Theme.num(a, "可用 " + m.servable + "/" + m.total, 11.5f, Theme.INK3);
            avail.setPadding(Theme.dp(a, Theme.S2), 0, 0, 0);
            line.addView(avail);
            card.addView(line);

            // 第二行：何时解锁 + 上游原文（原因常常就是「该模型限流」这类，看一眼就知道
            // 该等还是该换模型）。
            String when = unlockText(m.unlockAt, now);
            StringBuilder sb = new StringBuilder();
            if (!when.isEmpty()) sb.append("最早 ").append(when);
            if ("locked".equals(m.state)) {
                String full = unlockText(m.fullyUnlockAt, now);
                if (!full.isEmpty()) sb.append(sb.length() > 0 ? " · " : "").append("全部 ").append(full);
            }
            if (!TextUtils.isEmpty(m.reason)) {
                sb.append(sb.length() > 0 ? " · " : "").append(m.reason);
            }
            if (sb.length() > 0) {
                TextView why = Theme.text(a, sb.toString(), 11f, Theme.INK3, 0);
                why.setSingleLine(true);
                why.setEllipsize(TextUtils.TruncateAt.END);
                why.setPadding(Theme.dp(a, 15), Theme.dp(a, 2), 0, 0);
                card.addView(why);
            }
            if (i < shown - 1) card.addView(Theme.divider(a));
        }
        if (n > shown) {
            card.addView(Theme.text(a, "另有 " + (n - shown) + " 个模型受限", 11f, Theme.INK3, 0));
        }
        card.addView(Theme.text(a, "锁定 = 上游按（账号, 模型）限流；换模型立刻可用，换号通常没用",
                11f, Theme.INK3, 0));
        col.addView(card, Theme.cardLp(a, Theme.S3));
    }

    /** "MM-dd HH:mm 解锁（约 N 分钟后）"；无有效时刻返回空串。 */
    private static String unlockText(String rfc3339, long nowMs) {
        long ms = rfc3339Ms(rfc3339);
        if (ms <= 0) return "";
        long leftMin = Math.max(0, (ms - nowMs + 59999) / 60000);
        java.util.Calendar cal = java.util.Calendar.getInstance();
        cal.setTimeInMillis(ms);
        String hhmm = String.format(java.util.Locale.US, "%02d:%02d",
                cal.get(java.util.Calendar.HOUR_OF_DAY), cal.get(java.util.Calendar.MINUTE));
        boolean sameDay = cal.get(java.util.Calendar.YEAR) == nowCal(nowMs, java.util.Calendar.YEAR)
                && cal.get(java.util.Calendar.DAY_OF_YEAR) == nowCal(nowMs, java.util.Calendar.DAY_OF_YEAR);
        String stamp = sameDay ? hhmm
                : String.format(java.util.Locale.US, "%02d-%02d %s",
                        cal.get(java.util.Calendar.MONTH) + 1, cal.get(java.util.Calendar.DAY_OF_MONTH), hhmm);
        return leftMin <= 0 ? stamp + " 解锁（即将）" : stamp + " 解锁（约 " + leftMin + " 分钟后）";
    }

    private static int nowCal(long ms, int field) {
        java.util.Calendar c = java.util.Calendar.getInstance();
        c.setTimeInMillis(ms);
        return c.get(field);
    }

    /**
     * RFC3339 → 毫秒；无效返回 -1。
     *
     * 手写解析而不是 SimpleDateFormat：Go 的 time.Time 序列化带**数字时区偏移**
     * （+08:00）和可选小数秒，SimpleDateFormat 的 Z 模式对这两种形态都挑剔；而这里
     * 只需要「年-月-日 时:分:秒" + 偏移」。年份 < 2000 一律当无效（Go 零值 0001-01-01）。
     */
    private static long rfc3339Ms(String s) {
        if (s == null || s.length() < 19) return -1;
        try {
            int y = Integer.parseInt(s.substring(0, 4));
            if (y < 2000) return -1;
            int mo = Integer.parseInt(s.substring(5, 7));
            int d = Integer.parseInt(s.substring(8, 10));
            int h = Integer.parseInt(s.substring(11, 13));
            int mi = Integer.parseInt(s.substring(14, 16));
            int sec = Integer.parseInt(s.substring(17, 19));
            int i = 19;
            if (i < s.length() && s.charAt(i) == '.') {
                i++;
                while (i < s.length() && Character.isDigit(s.charAt(i))) i++;
            }
            int offMin = 0;
            if (i < s.length()) {
                char c = s.charAt(i);
                if ((c == '+' || c == '-') && s.length() >= i + 6) {
                    int oh = Integer.parseInt(s.substring(i + 1, i + 3));
                    int om = Integer.parseInt(s.substring(i + 4, i + 6));
                    offMin = (oh * 60 + om) * (c == '-' ? -1 : 1);
                }
            }
            java.util.Calendar cal = java.util.Calendar.getInstance(
                    java.util.TimeZone.getTimeZone("UTC"));
            cal.set(y, mo - 1, d, h, mi, sec);
            cal.set(java.util.Calendar.MILLISECOND, 0);
            return cal.getTimeInMillis() - offMin * 60000L;
        } catch (Throwable t) {
            return -1;
        }
    }

    // ── 积分到期提醒（issue #167 第 3 条） ──────────────────────────────────
    //
    // 口径照网页端 renderExpirySnapshot()：
    //   · 按 credits_earliest_expiry 升序；没有真实到期的排最后（这里直接不列，改一行汇总）；
    //   · 日均需耗 = ceil(该批剩余 / max(1, 距到期天数))，天数是**日历日差**；
    //   · 色点 ≤3 天红、≤7 天黄、其余绿；
    //   · 「检查」拉逐包明细，不走系统通知 —— 卡片本身就是提醒（与网页端一致）。
    //
    // 为什么天差必须按日历日算（网页端 bug #10）：今天 23:00 到期，用
    // ceil((ms - now) / 86400000) 会得 1 → 文案变「明天到期」，日均需耗也跟着偏小。
    private void expiryCard(LinearLayout col, java.util.List<PanelService.Account> accs) {
        java.util.List<PanelService.Account> soon = new java.util.ArrayList<PanelService.Account>();
        int rest = 0;
        for (PanelService.Account x : accs) {
            if (expiryDayMs(x.earliestExpiry) > 0 && x.earliestRemaining > 0) soon.add(x);
            else rest++;
        }
        if (soon.isEmpty()) return;   // 全都没有到期批次：不占一屏去说「没有」

        java.util.Collections.sort(soon, new java.util.Comparator<PanelService.Account>() {
            public int compare(PanelService.Account l, PanelService.Account r) {
                long a1 = expiryDayMs(l.earliestExpiry), b1 = expiryDayMs(r.earliestExpiry);
                return a1 < b1 ? -1 : (a1 > b1 ? 1 : 0);
            }
        });

        LinearLayout card = Theme.card(a);
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(Theme.label(a, "积分到期"),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        TextView chk = Theme.pill(a, "检查", false);
        chk.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { openExpiryPackages(); }
        });
        head.addView(chk);
        card.addView(head);

        long todayMs = todayMs();
        int n = Math.min(soon.size(), 8);
        for (int i = 0; i < n; i++) {
            PanelService.Account x = soon.get(i);
            long ms = expiryDayMs(x.earliestExpiry);
            int days = (int) Math.max(0, Math.round((ms - todayMs) / 86400000.0));
            long remain = x.earliestRemaining;
            long daily = (long) Math.ceil(remain / (double) Math.max(1, days));
            int color = days <= 3 ? Theme.BAD : days <= 7 ? Theme.WARN : Theme.ACCENT;

            LinearLayout line = new LinearLayout(a);
            line.setOrientation(LinearLayout.HORIZONTAL);
            line.setGravity(Gravity.CENTER_VERTICAL);
            line.setPadding(0, Theme.dp(a, 6), 0, Theme.dp(a, 6));
            LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                    Theme.dp(a, 7), Theme.dp(a, 7));
            dlp.rightMargin = Theme.dp(a, Theme.S2);
            line.addView(Theme.dot(a, color, 7), dlp);
            TextView nm = Theme.text(a, TextUtils.isEmpty(x.nickname) ? x.uid : x.nickname,
                    12.5f, Theme.INK, 1);
            nm.setSingleLine(true);
            nm.setEllipsize(TextUtils.TruncateAt.END);
            line.addView(nm, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            String word = days == 0 ? "今天到期" : days == 1 ? "明天到期" : days + " 天后到期";
            line.addView(Theme.text(a, ymd(ms) + "（" + word + "）· " + remain + " 积分 · 日均需耗 ≥" + daily,
                    11.5f, color, 0));
            card.addView(line);
            if (i < n - 1) card.addView(Theme.divider(a));
        }
        if (soon.size() > n || rest > 0) {
            String more = "";
            if (soon.size() > n) more += "另有 " + (soon.size() - n) + " 个账号也有到期批次";
            if (rest > 0) more += (more.isEmpty() ? "" : "；") + rest + " 个账号无近期到期批次";
            card.addView(Theme.text(a, more, 11f, Theme.INK3, 0));
        }
        card.addView(Theme.text(a, "上游按失效时刻先后自动优先扣减（先到期的先用）",
                11f, Theme.INK3, 0));
        col.addView(card, Theme.cardLp(a, Theme.S3));
    }

    /** 逐包明细（「检查」按钮）：7 天内合计 + 每个包的到期日与剩余。 */
    private void openExpiryPackages() {
        svc.packages(new PanelService.Cb<java.util.List<PanelService.PkgAccount>>() {
            public void ok(java.util.List<PanelService.PkgAccount> list) { showExpirySheet(list); }
            public void fail(String m) { toast("读取积分包失败：" + m); }
        });
    }

    private void showExpirySheet(java.util.List<PanelService.PkgAccount> list) {
        long today = todayMs();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2),
                Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2));

        long within7 = 0;
        int shown = 0;
        for (PanelService.PkgAccount pa : list) {
            if (pa.packages.isEmpty()) continue;
            LinearLayout sec = Theme.card(a);
            sec.addView(Theme.label(a, TextUtils.isEmpty(pa.nickname) ? pa.uid : pa.nickname));
            java.util.List<PanelService.Pkg> pkgs = new java.util.ArrayList<PanelService.Pkg>(pa.packages);
            java.util.Collections.sort(pkgs, new java.util.Comparator<PanelService.Pkg>() {
                public int compare(PanelService.Pkg l, PanelService.Pkg r) {
                    long a1 = expiryDayMs(l.endTime), b1 = expiryDayMs(r.endTime);
                    if (a1 <= 0 && b1 <= 0) return 0;
                    if (a1 <= 0) return 1;
                    if (b1 <= 0) return -1;
                    return a1 < b1 ? -1 : (a1 > b1 ? 1 : 0);
                }
            });
            for (PanelService.Pkg p : pkgs) {
                long ms = expiryDayMs(p.endTime);
                String when;
                if (ms <= 0) {
                    when = "长期有效";
                } else {
                    int days = (int) Math.max(0, Math.round((ms - today) / 86400000.0));
                    when = ymd(ms) + "（" + (days == 0 ? "今天" : days + " 天后") + "）";
                    if (days <= 7) within7 += p.remain;
                }
                sec.addView(Theme.kvRow(a, p.name + " · " + when, p.remain + " 积分"));
            }
            box.addView(sec, Theme.cardLp(a, Theme.S2));
            shown++;
            if (shown >= 6) break;   // 弹窗不无限长
        }
        if (shown == 0) box.addView(Theme.empty(a, "没有可用的积分包明细"));
        box.addView(Theme.text(a, "7 天内到期合计：" + within7 + " 积分", 12.5f, Theme.WARN, 1));
        Sheet.custom(a, "积分到期明细", box, "关闭", null).show();
    }

    /** 今天 00:00 的毫秒（日历日差的基准）。 */
    private static long todayMs() {
        java.util.Calendar cal = java.util.Calendar.getInstance();
        cal.set(java.util.Calendar.HOUR_OF_DAY, 0);
        cal.set(java.util.Calendar.MINUTE, 0);
        cal.set(java.util.Calendar.SECOND, 0);
        cal.set(java.util.Calendar.MILLISECOND, 0);
        return cal.getTimeInMillis();
    }

    /**
     * 把 "YYYY-MM-DD..." 解析成当天 00:00 的毫秒；无效或年份 < 2000 返回 -1。
     *
     * 年份判断不是可选的：Go 的 time.Time 是 struct，`omitempty` 对它无效，无到期时后端
     * 会给出 0001-01-01T00:00:00Z —— 只看字符串非空会把它当成一个「两千年前到期」的批次。
     * 只取前 10 个字符，不解析纳秒与时区（RFC3339Nano 的小数位会让 SimpleDateFormat 失败）。
     */
    private static long expiryDayMs(String s) {
        if (s == null || s.length() < 10) return -1;
        try {
            int y = Integer.parseInt(s.substring(0, 4));
            if (y < 2000) return -1;
            int mo = Integer.parseInt(s.substring(5, 7));
            int d = Integer.parseInt(s.substring(8, 10));
            java.util.Calendar cal = java.util.Calendar.getInstance();
            cal.set(y, mo - 1, d, 0, 0, 0);
            cal.set(java.util.Calendar.MILLISECOND, 0);
            return cal.getTimeInMillis();
        } catch (Throwable t) {
            return -1;
        }
    }

    /** 毫秒 → "YYYY-MM-DD"（本地时区；不能用 toISOString，那是 UTC，西半球会差一天）。 */
    private static String ymd(long ms) {
        java.util.Calendar cal = java.util.Calendar.getInstance();
        cal.setTimeInMillis(ms);
        return String.format(java.util.Locale.US, "%04d-%02d-%02d",
                cal.get(java.util.Calendar.YEAR), cal.get(java.util.Calendar.MONTH) + 1,
                cal.get(java.util.Calendar.DAY_OF_MONTH));
    }

    private View accountCard(final PanelService.Account s) {
        LinearLayout card = Theme.card(a);
        card.setLayoutParams(Theme.cardLp(a, Theme.S3));

        // 第一行：状态点 + 昵称 + 标签
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        int dotColor = s.disabled ? Theme.BAD : (s.cooling || s.paused ? Theme.WARN : Theme.ACCENT);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                Theme.dp(a, 8), Theme.dp(a, 8));
        dlp.rightMargin = Theme.dp(a, Theme.S2);
        head.addView(Theme.dot(a, dotColor, 8), dlp);
        TextView name = Theme.text(a, TextUtils.isEmpty(s.nickname) ? "未命名账号" : s.nickname,
                16f, Theme.INK, 2);
        name.setSingleLine(true);
        name.setEllipsize(TextUtils.TruncateAt.END);
        head.addView(name, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        // 可用性用**标签**而不是只有一个小圆点：色弱/强光下圆点几乎看不出区别，
        // 而「这个号现在能不能用」是打开账号页最想确认的一件事。
        head.addView(Theme.tag(a, s.statusText(), dotColor));
        if (s.enterprise) head.addView(Theme.tag(a, "企业", Theme.WARN));
        if (s.apiKey) head.addView(Theme.tag(a, "KEY", Theme.INK3));
        if ("personal".equals(s.override)) head.addView(Theme.tag(a, "强制个人", Theme.ACCENT));
        if ("enterprise".equals(s.override)) head.addView(Theme.tag(a, "强制企业", Theme.WARN));
        card.addView(head);

        // 第二行：uid（状态已上移成标签；禁用原因比状态更值得占位置）
        String subText = s.uid;
        if (s.disabled && !TextUtils.isEmpty(s.disabledReason)) subText += "  ·  " + s.disabledReason;
        TextView sub = Theme.text(a, subText, 11.5f, Theme.INK3, 0);
        sub.setPadding(0, Theme.dp(a, 3), 0, 0);
        card.addView(sub);

        // 第三行：积分 / 成功失败 / 在途（数字用等宽）
        LinearLayout stats = new LinearLayout(a);
        stats.setOrientation(LinearLayout.HORIZONTAL);
        stats.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        stats.addView(statBlock("积分", s.credits + (s.creditsTotal > 0 ? "/" + s.creditsTotal : ""),
                Theme.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1.2f));
        stats.addView(statBlock("成功", String.valueOf(s.success), Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("失败", String.valueOf(s.errTotal), s.errTotal > 0 ? Theme.BAD : Theme.INK3),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("在途", String.valueOf(s.inFlight), Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        card.addView(stats);

        // 动作行：主操作（签到/余额/任务）+ 更多
        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        LinearLayout acts = new LinearLayout(a);
        acts.setOrientation(LinearLayout.HORIZONTAL);
        acts.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        hs.addView(acts);
        card.addView(hs);

        boolean personal = !s.enterprise && !s.apiKey;
        if (personal) acts.addView(actionPill("签到", new Runnable() {
            public void run() { doCheckin(s); }
        }));
        acts.addView(actionPill("余额", new Runnable() {
            public void run() { doBalance(s); }
        }));
        if (personal) acts.addView(actionPill("任务", new Runnable() {
            public void run() { c.accountTasks(s.uid); }
        }));
        acts.addView(actionPill(s.paused ? "恢复" : "暂停", new Runnable() {
            public void run() { doPause(s); }
        }));
        // 类型 / 探活 / 备注 / 分组 / 模型开关都在「更多」里：动作行保持 5 个，
        // 360px 屏不用横滑就能看全（旧版第 6 个被右边缘切掉）。
        TextView morePill = Theme.pill(a, "更多", false);
        morePill.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { moreSheet(v, s); }
        });
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        mlp.rightMargin = Theme.dp(a, Theme.S2);
        acts.addView(morePill, mlp);
        return card;
    }

    private View statBlock(String label, String value, int color) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.addView(Theme.label(a, label));
        TextView v = Theme.num(a, value, 13f, color);
        v.setPadding(0, Theme.dp(a, 2), 0, 0);
        box.addView(v);
        return box;
    }

    private View actionPill(String label, final Runnable r) {
        TextView p = Theme.pill(a, label, false);
        p.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        LinearLayout.LayoutParams lp = (LinearLayout.LayoutParams) p.getLayoutParams();
        if (lp == null) lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = Theme.dp(a, Theme.S2);
        p.setLayoutParams(lp);
        return p;
    }

    private void doCheckin(PanelService.Account s) {
        toast("签到中…");
        svc.checkin(s.uid, new PanelService.Cb<PanelService.TaskResult>() {
            public void ok(PanelService.TaskResult r) {
                // 四态分开说：already 与 checked 的事实不同（一个积分都没发 vs 本次发了）。
                String gain = r.credit >= 0 ? " +" + r.credit + " 积分" : "";
                if ("skipped".equals(r.state) || "skipped".equals(r.error)) {
                    toast(r.message.isEmpty() ? "该账号无签到体系（已跳过）" : r.message);
                } else if ("already".equals(r.state)) {
                    toast("今天已签到（重复签发不了积分）");
                } else if ("pending".equals(r.state)) {
                    toast("上游还在处理签到，稍后重试");
                } else if ("checked".equals(r.state)) {
                    toast("签到成功" + gain);
                } else if ("failed".equals(r.state) || "not_done".equals(r.error)) {
                    toast("签到未完成" + (r.message.isEmpty() ? "，稍后再试" : "：" + r.message));
                } else if (!r.error.isEmpty()) {
                    toast("签到异常：" + r.error);
                } else {
                    toast("签到完成" + gain);
                }
                loadOverview(false);
            }
            public void fail(String m) { toast("签到失败：" + m); }
        });
    }

    private void doBalance(PanelService.Account s) {
        svc.balance(s.uid, new PanelService.Cb<long[]>() {
            public void ok(long[] v) {
                toast("余额 " + v[0] + (v[1] > 0 ? " / " + v[1] : ""));
                loadOverview(false);
            }
            public void fail(String m) { toast("余额查询失败：" + m); }
        });
    }

    private void doPause(PanelService.Account s) {
        svc.pause(s.uid, s.paused, new PanelService.Cb<String>() {
            public void ok(String m) { toast("已更新选号状态"); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        });
    }

    private void typeSheet(final PanelService.Account s) {
        final String[] values = {"", "enterprise", "personal"};
        String cur = s.override == null ? "" : s.override;
        String[] labels = {
                "自动判定" + (cur.isEmpty() ? " · 当前" : ""),
                "强制企业版" + ("enterprise".equals(cur) ? " · 当前" : ""),
                "强制个人版" + ("personal".equals(cur) ? " · 当前" : "")
        };
        int checked = "enterprise".equals(cur) ? 1 : ("personal".equals(cur) ? 2 : 0);
        Sheet.single(a, "账号类型", labels, checked, new Sheet.Pick() {
            public void onPick(int which) {
                svc.enterprise(s.uid, values[which], new PanelService.Cb<Boolean>() {
                    public void ok(Boolean changed) {
                        toast("类型已更新" + (changed ? "" : "（判定结果未变）"));
                        loadOverview(false);
                    }
                    public void fail(String m) { toast("切换失败：" + m); }
                });
            }
        });
    }

    private void moreSheet(View anchor, final PanelService.Account s) {
        String[] items = {
                s.paused ? "恢复选号" : "暂停选号",
                s.disabled ? "启用账号" : "禁用账号",
                "账号类型：" + typeLabel(s),
                "探活（只读确认是否已解封）",
                "备注",
                "分组",
                "清空任务跳过名单",
                "模型开关",
                "移除账号"
        };
        Sheet.anchor(anchor, TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname, items, 8,
                new Sheet.Pick() {
                    public void onPick(int which) {
                        if (which == 0) doPause(s);
                        else if (which == 1) svc.disable(s.uid, !s.disabled, after("已更新"));
                        else if (which == 2) typeSheet(s);
                        else if (which == 3) probe(s);
                        else if (which == 4) editText("备注", "仅本机保存", s.remark, new TextCommit() {
                            public void onText(String t) { svc.setRemark(s.uid, t, after("备注已保存")); }
                        });
                        else if (which == 5) editText("分组", "用于面板筛选", s.group, new TextCommit() {
                            public void onText(String t) { svc.setGroup(s.uid, t, after("分组已保存")); }
                        });
                        else if (which == 6) svc.clearSkip(s.uid, after("已清空跳过名单"));
                        else if (which == 7) modelSheet(s);
                        else confirmRemove(s);
                    }
                });
    }

    private String typeLabel(PanelService.Account s) {
        if ("enterprise".equals(s.override)) return "强制企业版";
        if ("personal".equals(s.override)) return "强制个人版";
        return s.enterprise ? "自动 · 企业" : "自动 · 个人";
    }

    private PanelService.Cb<String> after(final String ok) {
        return new PanelService.Cb<String>() {
            public void ok(String m) { toast(ok); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        };
    }

    private void probe(PanelService.Account s) {
        toast("探活中…");
        svc.probeBan(s.uid, new PanelService.Cb<PanelService.ProbeResult>() {
            public void ok(PanelService.ProbeResult r) {
                String msg;
                if ("lifted".equals(r.verdict)) msg = "已解封，可启用";
                else if ("still_banned".equals(r.verdict)) msg = "仍被封禁，继续等待";
                else if ("blocked".equals(r.verdict)) msg = "被风控拦截，继续观察";
                else if ("session_dead".equals(r.verdict)) msg = "登录态失效，需重新登录";
                else if ("inconclusive".equals(r.verdict)) msg = "无明确结论，稍后再试";
                else msg = "无法探测：" + r.verdict;
                toast(msg + (r.detail.isEmpty() ? "" : "（" + r.detail + "）"));
            }
            public void fail(String m) { toast("探活失败：" + m); }
        });
    }

    private interface TextCommit { void onText(String t); }

    private void editText(String title, String hint, String initial, final TextCommit cb) {
        final EditText et = field(hint);
        et.setText(initial == null ? "" : initial);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(Theme.dp(a, Theme.S4), 0, Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2));
        box.addView(et);
        Sheet.custom(a, title, box, "保存", new Runnable() {
            public void run() {
                cb.onText(et.getText() == null ? "" : et.getText().toString().trim());
            }
        });
    }

    private void confirmRemove(final PanelService.Account s) {
        Sheet.confirm(a, "移除账号",
                "删除本地凭据文件，不可撤销。\n\n"
                        + (TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname) + "\n" + s.uid,
                "移除", true, new Runnable() {
                    public void run() { svc.remove(s.uid, after("已移除")); }
                });
    }

    // ── 模型开关 ───────────────────────────────────────────────────────────
    private void modelSheet(final PanelService.Account s) {
        toast("读取模型目录…");
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(final List<PanelService.ModelInfo> models) {
                svc.modelsOff(s.uid, new PanelService.Cb<List<String>>() {
                    public void ok(List<String> off) { showModels(s.uid, models, off); }
                    public void fail(String m) { toast("读取开关失败：" + m); }
                });
            }
            public void fail(String m) { toast("读取模型目录失败：" + m); }
        });
    }

    private void showModels(final String uid, List<PanelService.ModelInfo> models, List<String> off) {
        if (models.isEmpty()) { toast("模型目录为空"); return; }
        LinearLayout box = Theme.column(a);
        TextView hint = Theme.text(a, "勾选 = 该账号可用；取消 = 手动关闭（永久写入 state.json）",
                12f, Theme.INK3, 0);
        box.addView(hint);
        for (final PanelService.ModelInfo m : models) {
            final String bare = m.id.contains(":") ? m.id.substring(m.id.indexOf(':') + 1) : m.id;
            final boolean enabled = !off.contains(bare);
            LinearLayout row = new LinearLayout(a);
            row.setOrientation(LinearLayout.HORIZONTAL);
            row.setGravity(Gravity.CENTER_VERTICAL);
            row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
            LinearLayout info = new LinearLayout(a);
            info.setOrientation(LinearLayout.VERTICAL);
            info.addView(Theme.text(a, m.name, 14f, Theme.INK, 1));
            info.addView(Theme.text(a, m.id, 11f, Theme.INK3, 0));
            row.addView(info, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            final TextView toggle = Theme.pill(a, enabled ? "可用" : "已关闭",
                    enabled ? true : false);
            toggle.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    final boolean next = !"可用".equals(String.valueOf(toggle.getText()));
                    svc.setModelOff(uid, bare, !next, new PanelService.Cb<String>() {
                        public void ok(String msg) {
                            toggle.setText(next ? "可用" : "已关闭");
                            toggle.setBackground(next ? Theme.round(a, Theme.ACCENT, Theme.R_PILL)
                                    : Theme.outlined(a, Theme.OVERLAY_SOFT, Theme.R_PILL, Theme.LINE, 1));
                            toggle.setTextColor(next ? Theme.ON_ACCENT : Theme.INK2);
                        }
                        public void fail(String m) { toast("切换失败：" + m); }
                    });
                }
            });
            row.addView(toggle);
            box.addView(row);
            box.addView(Theme.divider(a));
        }
        Sheet.custom(a, "模型开关", box, "完成", null);
    }

    // ── 添加账号 ───────────────────────────────────────────────────────────
    void addSheet() {
        String[] items = {"导入文件（cockpit / auths JSON）", "导出账号到下载目录",
                "添加 API Key 账号", "登录添加（浏览器授权）"};
        Sheet.menu(a, "添加 / 迁移账号", items, -1, new Sheet.Pick() {
            public void onPick(int which) {
                if (which == 0) {
                    if (a instanceof MainActivity) ((MainActivity) a).requestImportFile();
                    else toast("当前宿主不支持文件选择");
                } else if (which == 1) exportAccounts();
                else if (which == 2) apiKeyDialog();
                else loginSheet();
            }
        });
    }

    /**
     * 导入文件大小上限（W6-H-04）。
     *
     * 32MB 的依据：一份 cockpit 导出的 auths JSON 正常在几百 KB 量级，32MB 已经比
     * 真实用量的上限高两个数量级；而 `ByteArrayOutputStream` 扩容时峰值约 2.5×
     * （旧缓冲 + 新缓冲 + 目标数组），32MB → 峰值 80MB 左右，仍在低端机可承受范围。
     */
    private static final long IMPORT_MAX_BYTES = 32L * 1024 * 1024;

    void importUri(android.net.Uri uri) {
        if (uri == null) { toast("读不到文件"); return; }
        // 先看 provider 报的大小：能在读之前就拒掉就不读（300MB 视频连 8KB 都不用读）。
        try (android.database.Cursor c = a.getContentResolver().query(uri,
                new String[]{android.provider.OpenableColumns.SIZE}, null, null, null)) {
            if (c != null && c.moveToFirst() && !c.isNull(0)) {
                long size = c.getLong(0);
                if (size > IMPORT_MAX_BYTES) {
                    toast("文件 " + (size / 1024 / 1024) + "MB，超过 32MB 上限，已中止导入");
                    return;
                }
            }
        } catch (Throwable ignored) {
            // 有的 provider 不支持查 SIZE：落到下面的流式累计上限。
        }
        byte[] data;
        // try-with-resources：旧代码在 read 抛异常时 in 永不 close，连续失败几次就是
        // 句柄泄漏；而这里读的正是用户随手选的大文件（异常概率不低）。
        try (java.io.InputStream in = a.getContentResolver().openInputStream(uri)) {
            if (in == null) { toast("读不到文件"); return; }
            java.io.ByteArrayOutputStream bos = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            long total = 0;
            while ((n = in.read(buf)) > 0) {
                // 累计到上限立刻中止：provider 报的 SIZE 可能是 null/撒谎，这里才是硬闸。
                // 不设上限时 OOM 被外层 catch(Throwable) 吞掉，但堆已经碎了，
                // 同一进程里的 GatewayService（7×24 网关 + 在途 SSE）会跟着一起崩。
                total += n;
                if (total > IMPORT_MAX_BYTES) {
                    toast("文件超过 32MB 上限，已中止导入");
                    return;
                }
                bos.write(buf, 0, n);
            }
            data = bos.toByteArray();
        } catch (Throwable t) {
            toast("导入失败：" + t);
            return;
        }
        if (data.length == 0) { toast("文件为空"); return; }
        String name = String.valueOf(uri.getLastPathSegment());
        int slash = name.lastIndexOf('/');
        if (slash >= 0) name = name.substring(slash + 1);
        toast("导入中…");
        try {
            svc.importFile(name, data, new PanelService.Cb<PanelService.ImportResult>() {
                public void ok(PanelService.ImportResult r) {
                    toast("导入完成：新增 " + r.imported + "，跳过 " + r.skipped);
                    if (!r.errors.isEmpty()) {
                        StringBuilder sb = new StringBuilder();
                        for (int i = 0; i < Math.min(r.errors.size(), 20); i++) {
                            sb.append("· ").append(r.errors.get(i)).append('\n');
                        }
                        Sheet.confirm(a, "导入错误", sb.toString(), "知道了", false, null);
                    }
                    open("accounts");
                }
                public void fail(String m) { toast("导入失败：" + m); }
            });
        } catch (Throwable t) { toast("导入失败：" + t); }
    }

    private void exportAccounts() {
        svc.exportAccounts(new PanelService.FileCb() {
            public void ok(byte[] data, String filename) {
                try {
                    String path;
                    if (android.os.Build.VERSION.SDK_INT >= 29) {
                        android.content.ContentValues cv = new android.content.ContentValues();
                        cv.put(android.provider.MediaStore.MediaColumns.DISPLAY_NAME, filename);
                        cv.put(android.provider.MediaStore.MediaColumns.MIME_TYPE, "application/json");
                        cv.put(android.provider.MediaStore.MediaColumns.RELATIVE_PATH,
                                android.os.Environment.DIRECTORY_DOWNLOADS);
                        android.net.Uri out = a.getContentResolver()
                                .insert(android.provider.MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv);
                        if (out == null) { toast("写入下载目录失败"); return; }
                        java.io.OutputStream os = a.getContentResolver().openOutputStream(out);
                        os.write(data);
                        os.close();
                        path = "下载/" + filename;
                    } else {
                        java.io.File f = new java.io.File(a.getExternalFilesDir(null), filename);
                        java.io.FileOutputStream fos = new java.io.FileOutputStream(f);
                        fos.write(data);
                        fos.close();
                        path = f.getAbsolutePath();
                    }
                    toast("已导出 " + path);
                } catch (Throwable t) { toast("保存失败：" + t); }
            }
            public void fail(String m) { toast("导出失败：" + m); }
        });
    }

    private void apiKeyDialog() {
        LinearLayout box = Theme.column(a);
        final EditText uid = field("uid（手机号或 UUID）");
        // W6-M-11：access token 是账号凭据，输入框必须掩码（同屏还有人围观/录屏）。
        // 用 VARIATION_PASSWORD 而不是全屏数字键盘：token 是 ASCII 混合串。
        final EditText token = secretField("access token（ck_ / pt_ 开头）");
        final EditText nick = field("昵称（可选）");
        box.addView(uid);
        box.addView(token);
        box.addView(nick);
        Sheet.custom(a, "添加 API Key 账号", box, "添加", new Runnable() {
            public void run() {
                svc.addApiKey(uid.getText().toString().trim(),
                        token.getText().toString().trim(),
                        nick.getText().toString().trim(),
                        new PanelService.Cb<String>() {
                            public void ok(String id) { toast("已添加 " + id); open("accounts"); }
                            public void fail(String m) { toast("添加失败：" + m); }
                        });
            }
        });
    }

    private EditText field(String hint) {
        EditText et = new EditText(a);
        et.setHint(hint);
        et.setTextSize(15f);
        et.setTypeface(Theme.sans(a));
        return et;
    }

    /**
     * 掩码输入框（W6-M-11）。
     *
     * 单行 + 密码变体：显示成圆点，且不进入输入法的学习/联想词库
     * （VARIATION_PASSWORD 会关掉自动纠正与建议）。长按仍可粘贴，符合"输入一次"的用法。
     */
    private EditText secretField(String hint) {
        EditText et = field(hint);
        et.setSingleLine(true);
        et.setInputType(android.text.InputType.TYPE_CLASS_TEXT
                | android.text.InputType.TYPE_TEXT_VARIATION_PASSWORD);
        return et;
    }

    private void loginSheet() {
        Sheet.menu(a, "登录添加账号",
                new String[]{"国内版 · workbuddy.cn", "国际版 · workbuddy.ai"}, -1,
                new Sheet.Pick() {
                    public void onPick(int which) { startLogin(which == 1 ? "global" : "cn"); }
                });
    }

    private void startLogin(String realm) {
        svc.loginStart(realm, new PanelService.Cb<PanelService.LoginStart>() {
            public void ok(final PanelService.LoginStart s) {
                toast("浏览器已打开，完成授权后自动返回（最长 5 分钟）");
                try {
                    a.startActivity(new Intent(Intent.ACTION_VIEW, android.net.Uri.parse(s.url)));
                } catch (Throwable t) { toast("请手动打开：" + s.url); }
                pollLogin(s.state, 0);
            }
            public void fail(String m) { toast("发起登录失败：" + m); }
        });
    }

    private void pollLogin(final String state, final int attempt) {
        if (attempt > 100) { toast("登录超时"); return; }
        svc.loginPoll(state, new PanelService.Cb<PanelService.LoginPoll>() {
            public void ok(PanelService.LoginPoll p) {
                if (p.done) {
                    toast("登录成功" + (p.uid.isEmpty() ? "" : "：" + p.uid));
                    open("accounts");
                    return;
                }
                if (!p.error.isEmpty()) { toast("登录失败：" + p.error); return; }
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
            public void fail(String msg) {
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
        });
    }
}
