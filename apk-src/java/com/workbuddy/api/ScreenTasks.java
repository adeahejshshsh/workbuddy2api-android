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
        if ("done".equals(st)) return Theme.ACCENT;
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

    private void showAccountTasks(final String uid, final PanelService.TasksView v) {
        LinearLayout col = Theme.column(a);
        if (!v.message.isEmpty()) {
            col.addView(Theme.text(a, v.message, 12f, Theme.WARN, 0));
        }
        if (v.tasks.isEmpty()) {
            col.addView(Theme.empty(a, v.message.isEmpty() ? "该账号暂无任务" : v.message));
        }
        for (final PanelService.TaskItem t : v.tasks) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            card.addView(Theme.text(a, t.title, 14f, Theme.INK, 1));
            String st = t.claimed ? "已领取" : t.claimable ? "可领取" : t.locked ? "未解锁"
                    : "accepted".equals(t.acceptStatus) ? "进行中" : "未接受";
            card.addView(Theme.text(a, (t.target > 0 ? t.current + " / " + t.target : "—")
                    + "  ·  " + st + (t.credit > 0 ? "  ·  +" + t.credit + " 分" : ""),
                    11.5f, Theme.INK3, 0));
            if (!t.claimed && !t.locked) {
                final boolean accept = !t.claimable && v.canAccept
                        && !"accepted".equals(t.acceptStatus);
                final boolean claim = t.claimable && v.canClaim;
                if (accept || claim) {
                    TextView b = Theme.pill(a, accept ? "接受" : "领取", true);
                    b.setOnClickListener(new View.OnClickListener() {
                        public void onClick(View x) {
                            PanelService.Cb<String> cb = new PanelService.Cb<String>() {
                                public void ok(String m) { toast("已完成"); accountTasks(uid); }
                                public void fail(String m) { toast("失败：" + m); }
                            };
                            if (accept) svc.acceptTask(uid, t.code, cb);
                            else svc.claimTask(uid, t.code, cb);
                        }
                    });
                    LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                            ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
                    lp.topMargin = Theme.dp(a, Theme.S2);
                    card.addView(b, lp);
                }
            }
            col.addView(card);
        }
        Sheet.custom(a, "积分任务", col, v.canAuto ? "一键完成" : null,
                v.canAuto ? new Runnable() {
                    public void run() { autoAll(uid); }
                } : null);
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
