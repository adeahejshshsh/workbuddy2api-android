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
        usageChart(list, u);
        usageSection(list, "按账号", u.byAccount);
        usageSection(list, "按模型", u.byModel);
    }

    // ── 时间分布柱状图（issue #167 第 2 条） ────────────────────────────────
    //
    // 数据源是 /panel/api/usage 的 series（后端早就在返回，日桶 + 小时桶拼成一条）。
    // 两个必须遵守的细节：
    //   ① 24H 只取 scope=hour、其余只取 scope=day —— 混着画会把同一段画两遍；
    //   ② 小时桶只保留 7 天（审查 U3），所以 30D 区间里本来就没有小时点，正好是
    //      「24H→小时、其余→天」这个期望，不需要额外兜底。
    private int chartMetric = 0;                                   // 0 请求 / 1 TOKENS / 2 积分
    private static final String[] CHART_METRICS = {"请求", "TOKENS", "积分"};

    private void usageChart(final LinearLayout list, final PanelService.Usage u) {
        LinearLayout card = Theme.card(a);
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(Theme.label(a, "时间分布"),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        final LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        head.addView(seg);
        card.addView(head);

        final BarChart chart = new BarChart(a);
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, Theme.dp(a, 96));
        clp.topMargin = Theme.dp(a, Theme.S2);
        card.addView(chart, clp);

        final TextView note = Theme.text(a, "", 11.5f, Theme.INK3, 0);
        LinearLayout.LayoutParams nlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        nlp.topMargin = Theme.dp(a, Theme.S2);
        card.addView(note, nlp);

        fillChartPills(seg, chart, note, u);
        applyChart(chart, note, u);
        list.addView(card, Theme.cardLp(a, Theme.S3));
    }

    /** 指标切换（请求 / TOKENS / 积分）。重建这一行是为了让选中态跟着走，不整页重绘。 */
    private void fillChartPills(final LinearLayout seg, final BarChart chart,
                                final TextView note, final PanelService.Usage u) {
        seg.removeAllViews();
        for (int i = 0; i < CHART_METRICS.length; i++) {
            final int idx = i;
            TextView p = Theme.pill(a, CHART_METRICS[i], i == chartMetric);
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    if (idx == chartMetric) return;
                    chartMetric = idx;
                    fillChartPills(seg, chart, note, u);
                    applyChart(chart, note, u);
                }
            });
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            if (i < CHART_METRICS.length - 1) lp.rightMargin = Theme.dp(a, Theme.S1);
            seg.addView(p, lp);
        }
    }

    private void applyChart(BarChart chart, TextView note, PanelService.Usage u) {
        boolean wantHour = usageHours <= 24;
        java.util.List<PanelService.Usage.Point> pts = new java.util.ArrayList<PanelService.Usage.Point>();
        for (PanelService.Usage.Point p : u.series) {
            if (wantHour ? "hour".equals(p.scope) : "day".equals(p.scope)) pts.add(p);
        }
        float[] v = new float[pts.size()];
        String[] lb = new String[pts.size()];
        for (int i = 0; i < pts.size(); i++) {
            PanelService.Usage.Point p = pts.get(i);
            v[i] = chartMetric == 0 ? p.requests
                    : chartMetric == 1 ? p.tokens
                    : (float) p.credits;
            lb[i] = shortLabel(p.t, wantHour);
        }
        chart.setData(v, lb, chartMetric == 0 ? "" : chartMetric == 1 ? "tok" : "");
        if (pts.isEmpty()) {
            note.setText("该区间没有时间点（小时桶只保留 7 天，更早的区间按天统计）");
        } else {
            note.setText("共 " + pts.size() + " 个" + (wantHour ? "小时" : "天") + " · "
                    + (wantHour ? "粒度 1 小时" : "粒度 1 天"));
        }
    }

    /** "2026-11-16T13" → "13时"；"2026-11-16" → "11-16"。 */
    private static String shortLabel(String t, boolean hour) {
        if (t == null) return "";
        if (hour) {
            int i = t.indexOf('T');
            return i >= 0 && i + 1 < t.length() ? t.substring(i + 1) + "时" : t;
        }
        return t.length() >= 10 ? t.substring(5) : t;
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
