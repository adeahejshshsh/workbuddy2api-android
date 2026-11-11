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
 * 运行概览（分区：统计卡、实例信息、请求指标、错误汇总）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenOverview {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenOverview(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderOverviewShell(); }
    void renderShell() { renderOverviewShell(); }
    void renderBody(PanelService.Overview o) { renderOverview(o); }

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

    // ── 概览 ───────────────────────────────────────────────────────────────
    private void renderOverviewShell() {
        setBar("运行概览", false);
        LinearLayout col = Theme.column(a);
        col.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
    }

    private void renderOverview(PanelService.Overview o) {
        setBar("运行概览", false, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { loadOverview(false); }
        }));
        LinearLayout col = Theme.column(a);

        LinearLayout hero = Theme.card(a);
        LinearLayout row1 = new LinearLayout(a);
        row1.setOrientation(LinearLayout.HORIZONTAL);
        row1.addView(Theme.kpi(a, String.valueOf(o.total), "账号", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row1.addView(Theme.kpi(a, String.valueOf(o.healthy), "可用", Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row1.addView(Theme.kpi(a, String.valueOf(o.sticky), "粘性", Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        hero.addView(row1);
        col.addView(hero, Theme.cardLp(a, Theme.S3));

        LinearLayout info = Theme.card(a);
        info.addView(Theme.label(a, "实例"));
        info.addView(Theme.kvRow(a, "版本", o.version));
        info.addView(Theme.kvRow(a, "运行时长", (o.uptimeSec / 3600) + " 小时 " + (o.uptimeSec % 3600) / 60 + " 分"));
        info.addView(Theme.kvRow(a, "鉴权", o.authRequired ? "Bearer" : "关闭"));
        info.addView(Theme.kvRow(a, "Redis", o.redisMode.isEmpty() ? "—" : o.redisMode));
        col.addView(info, Theme.cardLp(a, Theme.S3));

        LinearLayout metrics = Theme.card(a);
        col.addView(metrics, Theme.cardLp(a, Theme.S3));
        loadMetrics(metrics);

        LinearLayout errs = Theme.card(a);
        col.addView(errs);
        loadErrors(errs);

        refresh(col);
    }

    private void loadMetrics(final LinearLayout box) {
        box.removeAllViews();
        box.addView(Theme.label(a, "请求指标 · 本次进程"));
        svc.metrics(new PanelService.Cb<PanelService.Metrics>() {
            public void ok(PanelService.Metrics m) {
                LinearLayout row = new LinearLayout(a);
                row.setOrientation(LinearLayout.HORIZONTAL);
                row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
                row.addView(Theme.kpi(a, String.valueOf(m.completed), "完成", Theme.INK),
                        new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                row.addView(Theme.kpi(a, String.format(java.util.Locale.US, "%.0f%%", m.successRate * 100),
                        "成功率", m.successRate >= 0.95 ? Theme.ACCENT : Theme.WARN),
                        new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                row.addView(Theme.kpi(a, String.format(java.util.Locale.US, "%.0f", m.avgDurationMs), "ms 均值",
                        Theme.INFO), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                box.addView(row);
                box.addView(Theme.kvRow(a, "成功 / 失败", m.succeeded + " / " + m.failed));
                box.addView(Theme.kvRow(a, "在途", String.valueOf(m.inFlight)));
                box.addView(Theme.kvRow(a, "HTTP 成功率",
                        String.format(java.util.Locale.US, "%.1f%%", m.httpSuccessRate * 100)));
                box.addView(Theme.kvRow(a, "归档", (m.archiveEnabled ? "开启" : "关闭") + " · "
                        + m.archiveFiles + " 文件 · "
                        + String.format(java.util.Locale.US, "%.1f MB", m.archiveBytes / 1048576.0)));
            }
            public void fail(String msg) { box.addView(Theme.text(a, "不可用：" + msg, 12f, Theme.INK3, 0)); }
        });
    }

    private void loadErrors(final LinearLayout box) {
        box.removeAllViews();
        box.addView(Theme.label(a, "错误汇总"));
        svc.errors(new PanelService.Cb<PanelService.ErrSummary>() {
            public void ok(PanelService.ErrSummary s) {
                box.addView(Theme.text(a, "扫描 " + s.scanned + " 行 · " + s.updatedAt,
                        11.5f, Theme.INK3, 0));
                if (s.rows.isEmpty()) {
                    box.addView(Theme.text(a, "最近没有 WARN / ERR", 13f, Theme.ACCENT, 1));
                    return;
                }
                int n = Math.min(s.rows.size(), 12);
                for (int i = 0; i < n; i++) {
                    PanelService.ErrStat r = s.rows.get(i);
                    LinearLayout row = new LinearLayout(a);
                    row.setOrientation(LinearLayout.VERTICAL);
                    row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
                    LinearLayout top = new LinearLayout(a);
                    top.setOrientation(LinearLayout.HORIZONTAL);
                    top.setGravity(Gravity.CENTER_VERTICAL);
                    top.addView(Theme.statusChip(a, String.valueOf(r.count), r.count > 3 ? Theme.BAD : Theme.WARN));
                    TextView kind = Theme.text(a, r.kind + (r.code.isEmpty() ? "" : " · " + r.code),
                            13f, Theme.INK, 1);
                    kind.setPadding(Theme.dp(a, Theme.S2), 0, 0, 0);
                    kind.setSingleLine(true);
                    kind.setEllipsize(TextUtils.TruncateAt.END);
                    top.addView(kind, new LinearLayout.LayoutParams(0,
                            ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                    row.addView(top);
                    String meta = (r.account.isEmpty() ? "非账号级" : r.account)
                            + (r.model.isEmpty() ? "" : " · " + r.model)
                            + (r.last.isEmpty() ? "" : " · " + r.last);
                    row.addView(Theme.text(a, meta, 11f, Theme.INK3, 0));
                    box.addView(row);
                    if (i < n - 1) box.addView(Theme.divider(a));
                }
            }
            public void fail(String msg) {
                box.addView(Theme.text(a, "不可用：" + msg, 12f, Theme.INK3, 0));
            }
        });
    }
}
