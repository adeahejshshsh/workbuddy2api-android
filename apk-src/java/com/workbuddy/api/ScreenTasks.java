package com.workbuddy.api;

import android.app.Activity;
import android.app.Dialog;
import android.content.Intent;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
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

import java.util.ArrayList;
import java.util.List;

/**
 * 任务（分区：积分任务弹层 / 队列 / 作业历史）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenTasks {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;
    private String taskMode = "queue";
    private String taskUid = "";

    ScreenTasks(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderTasks(); }

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

    // ── 任务 ───────────────────────────────────────────────────────────────
    private void renderTasks() {
        setBar("任务", true, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { renderTasks(); }
        }));
        LinearLayout col = Theme.column(a);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        String[] ids = {"queue", "jobs"};
        String[] labels = {"队列", "作业历史"};
        for (int i = 0; i < ids.length; i++) {
            final String id = ids[i];
            TextView p = Theme.pill(a, labels[i], id.equals(taskMode));
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { taskMode = id; renderTasks(); }
            });
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.rightMargin = Theme.dp(a, Theme.S2);
            seg.addView(p, lp);
        }
        col.addView(seg);

        final LinearLayout list = Theme.list(a);
        list.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        col.addView(list);
        list.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));

        if ("queue".equals(taskMode)) {
            svc.queue(new PanelService.Cb<PanelService.Queue>() {
                public void ok(PanelService.Queue q) { bodyQueue(list, q); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        } else {
            svc.jobs(new PanelService.Cb<List<PanelService.Job>>() {
                public void ok(List<PanelService.Job> js) { bodyJobs(list, js); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        }
    }

    private void bodyQueue(LinearLayout list, PanelService.Queue q) {
        list.removeAllViews();
        LinearLayout head = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.addView(Theme.kpi(a, q.running ? "运行中" : (q.started ? "已结束" : "未启动"), "状态",
                q.running ? Theme.ACCENT : Theme.INK2),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1.4f));
        row.addView(Theme.kpi(a, String.valueOf(q.total), "条目", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(q.conc), "并发", Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        head.addView(row);
        list.addView(head, Theme.cardLp(a, Theme.S3));

        if (q.items.isEmpty()) {
            list.addView(Theme.empty(a, "队列为空"));
            return;
        }
        for (PanelService.Queue.Item it : q.items) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            LinearLayout top = new LinearLayout(a);
            top.setOrientation(LinearLayout.HORIZONTAL);
            top.setGravity(Gravity.CENTER_VERTICAL);
            top.addView(Theme.statusChip(a, statusText(it.status), statusColor(it.status)));
            TextView who = Theme.text(a, (it.nickname.isEmpty() ? it.uid : it.nickname) + " · " + it.code,
                    12.5f, Theme.INK, 0);
            who.setPadding(Theme.dp(a, Theme.S2), 0, 0, 0);
            who.setSingleLine(true);
            who.setEllipsize(TextUtils.TruncateAt.END);
            top.addView(who, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            card.addView(top);
            if (!it.message.isEmpty()) {
                card.addView(Theme.text(a, it.message, 11.5f, Theme.INK3, 0));
            }
            list.addView(card);
        }
    }

    private void bodyJobs(LinearLayout list, List<PanelService.Job> jobs) {
        list.removeAllViews();
        if (jobs.isEmpty()) { list.addView(Theme.empty(a, "还没有作业记录")); return; }
        for (PanelService.Job j : jobs) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            LinearLayout top = new LinearLayout(a);
            top.setOrientation(LinearLayout.HORIZONTAL);
            top.setGravity(Gravity.CENTER_VERTICAL);
            TextView uid = Theme.num(a, j.uid, 12.5f, Theme.INK);
            top.addView(uid, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            top.addView(Theme.statusChip(a, statusText(j.status), statusColor(j.status)));
            card.addView(top);
            card.addView(Theme.kvRow(a, "完成 / 失败 / 跳过",
                    j.completed + " / " + j.failed + " / " + j.skipped));
            String upd = j.updatedAt.length() > 19 ? j.updatedAt.substring(5, 19).replace('T', ' ') : j.updatedAt;
            if (!upd.isEmpty()) card.addView(Theme.kvRow(a, "更新", upd));
            if (!j.error.isEmpty()) card.addView(Theme.text(a, j.error, 11.5f, Theme.BAD, 0));
            else if (!j.message.isEmpty()) card.addView(Theme.text(a, j.message, 11.5f, Theme.INK3, 0));
            list.addView(card);
        }
    }

    private static String statusText(String st) {
        if ("done".equals(st)) return "完成";
        if ("running".equals(st)) return "执行中";
        if ("error".equals(st)) return "失败";
        if ("skipped".equals(st)) return "跳过";
        if ("partial".equals(st)) return "部分完成";
        if ("interrupted".equals(st)) return "中断";
        return "待执行";
    }

    private static int statusColor(String st) {
        if ("done".equals(st)) return Theme.MINT;
        if ("error".equals(st)) return Theme.BAD;
        if ("running".equals(st)) return Theme.INFO;
        if ("partial".equals(st) || "interrupted".equals(st)) return Theme.WARN;
        return Theme.INK3;
    }

    // ── 账号积分任务（弹层）────────────────────────────────────────────────
    void accountTasks(final String uid) {
        toast("读取任务…");
        svc.tasks(uid, new PanelService.Cb<PanelService.TasksView>() {
            public void ok(final PanelService.TasksView v) { showAccountTasks(uid, v); }
            public void fail(String m) { toast("任务查询失败：" + m); }
        });
    }

    /** 任务状态归一：claim 可领取 / doing 进行中 / todo 未开始 / locked 未解锁 / done 已完成。 */
    private static String stateOf(PanelService.TaskItem t) {
        if (t.claimed) return "done";
        if (t.claimable) return "claim";
        if (t.locked) return "locked";
        if ("accepted".equals(t.acceptStatus)) return "doing";
        return "todo";
    }

    private void showAccountTasks(final String uid, final PanelService.TasksView v) {
        LinearLayout col = Theme.column(a);
        if (!v.message.isEmpty()) {
            col.addView(Theme.text(a, v.message, 12f, Theme.WARN, 0));
        }
        if (v.tasks.isEmpty()) {
            col.addView(Theme.empty(a, v.message.isEmpty() ? "该账号暂无任务" : v.message));
            Sheet.custom(a, "积分任务", col, v.canAuto ? "一键完成" : null,
                    v.canAuto ? new Runnable() {
                        public void run() { autoAll(uid); }
                    } : null);
            return;
        }

        int done = 0, claimable = 0, credit = 0;
        for (PanelService.TaskItem t : v.tasks) {
            if ("done".equals(stateOf(t))) done++;
            else if (t.claimable) { claimable++; credit += t.credit; }
        }
        col.addView(headerCard(v.tasks.size(), done, claimable, credit),
                Theme.cardLp(a, Theme.S3));

        // 按状态分组：能动手的排前面，已完成沉底。原来一条流水列表里「已领取」和
        // 「可领取」只差尾部几个字，得逐条读才能分辨，想要的动作会被灰条目淹没。
        List<PanelService.TaskItem> claim = new ArrayList<PanelService.TaskItem>();
        List<PanelService.TaskItem> doing = new ArrayList<PanelService.TaskItem>();
        List<PanelService.TaskItem> todo = new ArrayList<PanelService.TaskItem>();
        List<PanelService.TaskItem> locked = new ArrayList<PanelService.TaskItem>();
        List<PanelService.TaskItem> finished = new ArrayList<PanelService.TaskItem>();
        for (PanelService.TaskItem t : v.tasks) {
            String st = stateOf(t);
            if ("claim".equals(st)) claim.add(t);
            else if ("doing".equals(st)) doing.add(t);
            else if ("todo".equals(st)) todo.add(t);
            else if ("locked".equals(st)) locked.add(t);
            else finished.add(t);
        }
        section(col, "可领取", claim, Theme.ACCENT, uid, v);
        section(col, "进行中", doing, Theme.INFO, uid, v);
        section(col, "未开始", todo, Theme.INK2, uid, v);
        section(col, "未解锁", locked, Theme.SLATE, uid, v);
        section(col, "已完成", finished, Theme.MINT, uid, v);

        Sheet.custom(a, "积分任务", col, v.canAuto ? "一键完成" : null,
                v.canAuto ? new Runnable() {
                    public void run() { autoAll(uid); }
                } : null);
    }

    /** 顶部总览：已完成 N / 总数 + 进度条 + 还可领多少分。 */
    private LinearLayout headerCard(int total, int done, int claimable, int credit) {
        LinearLayout card = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        LinearLayout left = new LinearLayout(a);
        left.setOrientation(LinearLayout.VERTICAL);
        left.addView(Theme.num(a, done + " / " + total, 20f,
                done >= total ? Theme.MINT : Theme.INK));
        left.addView(Theme.label(a, "已完成任务"));
        row.addView(left, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if (claimable > 0) {
            row.addView(Theme.statusChip(a, "可领 " + claimable + " 项 +" + credit + " 分", Theme.ACCENT));
        } else if (done >= total) {
            row.addView(Theme.statusChip(a, "全部完成", Theme.MINT));
        } else {
            row.addView(Theme.statusChip(a, "待推进 " + (total - done) + " 项", Theme.INK2));
        }
        card.addView(row);
        LinearLayout.LayoutParams plp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, Theme.dp(a, 5));
        plp.topMargin = Theme.dp(a, Theme.S3);
        card.addView(Theme.progress(a, done, total, done >= total ? Theme.MINT : Theme.ACCENT), plp);
        return card;
    }

    /** 分组标题（色点 + 名称 + 条数），空组不渲染。 */
    private void section(LinearLayout col, String label, List<PanelService.TaskItem> items,
                         int color, String uid, PanelService.TasksView v) {
        if (items.isEmpty()) return;
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(Theme.dot(a, color, 6));
        TextView name = Theme.text(a, label, 12f, color, 2);
        name.setPadding(Theme.dp(a, 6), 0, 0, 0);
        head.addView(name);
        TextView count = Theme.num(a, String.valueOf(items.size()), 11f, Theme.INK3);
        count.setPadding(Theme.dp(a, 6), 0, 0, 0);
        head.addView(count);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = Theme.dp(a, Theme.S3);
        lp.bottomMargin = Theme.dp(a, Theme.S2);
        col.addView(head, lp);
        for (final PanelService.TaskItem t : items) {
            col.addView(taskCard(uid, v, t), Theme.cardLp(a, Theme.S2));
        }
    }

    /**
     * 单条任务卡：状态用四层信号同时表达——描边色、标题色/前置勾、状态胶囊、进度条，
     * 缩略浏览也能分辨「能领 / 在做 / 做完 / 锁着」，不必逐条读正文。
     */
    private LinearLayout taskCard(final String uid, final PanelService.TasksView v,
                                  final PanelService.TaskItem t) {
        final String st = stateOf(t);
        boolean done = "done".equals(st);
        final boolean claimNow = "claim".equals(st);
        final boolean acceptNow = "todo".equals(st) && v.canAccept;

        int border, bg, titleColor;
        if (claimNow) { border = 0x9922C55E; bg = 0xF71B2336; titleColor = Theme.INK; }
        else if ("doing".equals(st)) { border = 0x8038BDF8; bg = 0xF21B2336; titleColor = Theme.INK; }
        else if ("todo".equals(st)) { border = Theme.LINE; bg = 0xE61B2336; titleColor = Theme.INK2; }
        else if ("locked".equals(st)) { border = 0x33475569; bg = 0xCC1B2336; titleColor = Theme.INK3; }
        else { border = 0x5522C55E; bg = 0xB31B2336; titleColor = Theme.INK3; }

        LinearLayout card = Theme.card(a);
        GradientDrawable g = Theme.outlined(a, bg, Theme.R_CARD, border, claimNow ? 1.5f : 1f);
        card.setBackground(claimNow ? Theme.ripple(a, g, 0x3322C55E) : g);

        LinearLayout top = new LinearLayout(a);
        top.setOrientation(LinearLayout.HORIZONTAL);
        top.setGravity(Gravity.CENTER_VERTICAL);
        if (done) {
            TextView tick = Theme.text(a, "✓", 13f, Theme.MINT, 2);
            tick.setPadding(0, 0, Theme.dp(a, 6), 0);
            top.addView(tick);
        }
        TextView title = Theme.text(a, t.title, 13.5f, titleColor, claimNow ? 2 : 1);
        top.addView(title, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        top.addView(Theme.statusChip(a, chipText(st), chipColor(st)));
        card.addView(top);

        if (t.target > 0) {
            LinearLayout pr = new LinearLayout(a);
            pr.setOrientation(LinearLayout.HORIZONTAL);
            pr.setGravity(Gravity.CENTER_VERTICAL);
            int barColor = done ? Theme.MINT : claimNow ? Theme.ACCENT
                    : "doing".equals(st) ? Theme.INFO : Theme.SLATE;
            LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(0, Theme.dp(a, 5), 1f);
            pr.addView(Theme.progress(a, t.current, t.target, barColor), blp);
            TextView pv = Theme.num(a, Math.min(t.current, t.target) + " / " + t.target,
                    11f, done ? Theme.INK3 : Theme.INK2);
            pv.setPadding(Theme.dp(a, Theme.S3), 0, 0, 0);
            pr.addView(pv);
            LinearLayout.LayoutParams prlp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            prlp.topMargin = Theme.dp(a, Theme.S2);
            card.addView(pr, prlp);
        }

        if (t.credit > 0) {
            TextView tag = Theme.tag(a, done ? "+" + t.credit + " 分 已入账" : "+" + t.credit + " 分",
                    done ? Theme.MINT : claimNow || "doing".equals(st) ? Theme.ACCENT : Theme.INK2);
            LinearLayout.LayoutParams tlp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            tlp.topMargin = Theme.dp(a, Theme.S2);
            card.addView(tag, tlp);
        }

        if (claimNow && v.canClaim) {
            TextView b = Theme.primaryButton(a, "领取 +" + t.credit + " 分");
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.topMargin = Theme.dp(a, Theme.S3);
            b.setOnClickListener(new View.OnClickListener() {
                public void onClick(View x) {
                    svc.claimTask(uid, t.code, new PanelService.Cb<String>() {
                        public void ok(String m) { toast("已领取 " + t.title); accountTasks(uid); }
                        public void fail(String m) { toast("领取失败：" + m); }
                    });
                }
            });
            card.addView(b, lp);
        } else if (acceptNow) {
            TextView b = Theme.ghostButton(a, "接受任务");
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.topMargin = Theme.dp(a, Theme.S3);
            b.setOnClickListener(new View.OnClickListener() {
                public void onClick(View x) {
                    svc.acceptTask(uid, t.code, new PanelService.Cb<String>() {
                        public void ok(String m) { toast("已接受 " + t.title); accountTasks(uid); }
                        public void fail(String m) { toast("接受失败：" + m); }
                    });
                }
            });
            card.addView(b, lp);
        }
        return card;
    }

    private static String chipText(String st) {
        if ("claim".equals(st)) return "可领取";
        if ("doing".equals(st)) return "进行中";
        if ("todo".equals(st)) return "未开始";
        if ("locked".equals(st)) return "未解锁";
        return "已完成";
    }

    private static int chipColor(String st) {
        if ("claim".equals(st)) return Theme.ACCENT;
        if ("doing".equals(st)) return Theme.INFO;
        if ("locked".equals(st)) return Theme.SLATE;
        if ("todo".equals(st)) return Theme.INK2;
        return Theme.MINT;
    }

    private void autoAll(String uid) {
        toast("开始执行可自动任务（含真实对话，1–2 分钟）…");
        svc.autoAll(uid, new PanelService.Cb<PanelService.TaskResult>() {
            public void ok(PanelService.TaskResult r) {
                toast("执行完成：成功 " + r.done + " · 跳过 " + r.skipped + " · 失败 " + r.failed);
            }
            public void fail(String m) { toast("一键完成失败：" + m); }
        });
    }
}
