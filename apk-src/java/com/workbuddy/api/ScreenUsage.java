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
 * 用量（分区：按区间看请求/token/积分，含按账号与按模型排行）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenUsage {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;
    private int usageHours = 72;

    ScreenUsage(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderUsage(); }

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

    // ── 用量 ───────────────────────────────────────────────────────────────
    private void renderUsage() {
        setBar("用量", true, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { renderUsage(); }
        }));
        LinearLayout col = Theme.column(a);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        final int[] hours = {24, 72, 168, 720};
        String[] labels = {"24H", "3D", "7D", "30D"};
        for (int i = 0; i < hours.length; i++) {
            final int h = hours[i];
            TextView p = Theme.pill(a, labels[i], h == usageHours);
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { usageHours = h; renderUsage(); }
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

        svc.usage(usageHours, new PanelService.Cb<PanelService.Usage>() {
            public void ok(PanelService.Usage u) { renderUsageBody(list, u); }
            public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
        });
    }

    private void renderUsageBody(LinearLayout list, PanelService.Usage u) {
        list.removeAllViews();
        LinearLayout totals = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.addView(Theme.kpi(a, String.valueOf(u.requests), "请求", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, fmtTokens(u.totalTokens), "TOKENS", Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.format(java.util.Locale.US, "%.0f", u.credits), "积分", Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        totals.addView(row);
        totals.addView(Theme.kvRow(a, "失败", String.valueOf(u.errors)));
        totals.addView(Theme.kvRow(a, "入 / 出", u.promptTokens + " / " + u.completionTokens));
        if (u.cacheHitRate > 0) {
            totals.addView(Theme.kvRow(a, "缓存命中", String.format(java.util.Locale.US, "%.1f%%", u.cacheHitRate * 100)));
        }
        totals.addView(Theme.kvRow(a, "平均延迟", String.format(java.util.Locale.US, "%.0f ms", u.avgLatencyMs)));
        totals.addView(Theme.kvRow(a, "平均吐字", String.format(java.util.Locale.US, "%.1f tok/s", u.avgTps)));
        list.addView(totals, Theme.cardLp(a, Theme.S3));
        usageSection(list, "按账号", u.byAccount);
        usageSection(list, "按模型", u.byModel);
    }

    private String fmtTokens(long n) {
        if (n >= 1000000) return String.format(java.util.Locale.US, "%.1fM", n / 1000000.0);
        if (n >= 1000) return String.format(java.util.Locale.US, "%.1fK", n / 1000.0);
        return String.valueOf(n);
    }

    private void usageSection(LinearLayout list, String title, List<PanelService.Usage.Row> rows) {
        if (rows.isEmpty()) return;
        LinearLayout card = Theme.card(a);
        card.addView(Theme.label(a, title));
        int n = Math.min(rows.size(), 12);
        for (int i = 0; i < n; i++) {
            PanelService.Usage.Row r = rows.get(i);
            LinearLayout line = new LinearLayout(a);
            line.setOrientation(LinearLayout.HORIZONTAL);
            line.setGravity(Gravity.CENTER_VERTICAL);
            line.setPadding(0, Theme.dp(a, 6), 0, Theme.dp(a, 6));
            TextView name = Theme.text(a, r.extra.isEmpty() ? r.key : r.extra, 13f, Theme.INK, 0);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            line.addView(name, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView v = Theme.num(a, r.requests + " · " + fmtTokens(r.tokens), 12f, Theme.INK3);
            line.addView(v);
            card.addView(line);
            if (i < n - 1) card.addView(Theme.divider(a));
        }
        list.addView(card, Theme.cardLp(a, Theme.S3));
    }
}
