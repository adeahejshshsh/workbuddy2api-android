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
 * 数据（分区：积分构成 / 余额趋势（折线） / 开学季券码）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenData {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;
    private int balanceDays = 7;
    private String dataMode = "packages";

    ScreenData(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderData(); }

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

    // ── 数据 ───────────────────────────────────────────────────────────────
    private void renderData() {
        setBar("数据", false, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { renderData(); }
        }));
        LinearLayout col = Theme.column(a);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        String[] ids = {"packages", "balance", "vouchers"};
        String[] labels = {"积分构成", "余额趋势", "券码"};
        for (int i = 0; i < ids.length; i++) {
            final String id = ids[i];
            TextView p = Theme.pill(a, labels[i], id.equals(dataMode));
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { dataMode = id; renderData(); }
            });
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.rightMargin = Theme.dp(a, Theme.S2);
            seg.addView(p, lp);
        }
        col.addView(seg);

        if ("balance".equals(dataMode)) {
            LinearLayout seg2 = new LinearLayout(a);
            seg2.setOrientation(LinearLayout.HORIZONTAL);
            seg2.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            final int[] days = {7, 30};
            String[] dl = {"近 7 天", "近 30 天"};
            for (int i = 0; i < days.length; i++) {
                final int d = days[i];
                TextView p = Theme.pill(a, dl[i], d == balanceDays);
                p.setOnClickListener(new View.OnClickListener() {
                    public void onClick(View v) { balanceDays = d; renderData(); }
                });
                LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                        ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
                lp.rightMargin = Theme.dp(a, Theme.S2);
                seg2.addView(p, lp);
            }
            col.addView(seg2);
        }

        final LinearLayout list = Theme.list(a);
        list.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        col.addView(list);
        list.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));

        if ("vouchers".equals(dataMode)) {
            svc.vouchers(new PanelService.Cb<List<PanelService.VoucherAccount>>() {
                public void ok(List<PanelService.VoucherAccount> v) { bodyVouchers(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        } else if ("packages".equals(dataMode)) {
            svc.packages(new PanelService.Cb<List<PanelService.PkgAccount>>() {
                public void ok(List<PanelService.PkgAccount> v) { bodyPackages(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        } else {
            svc.balanceHistory(balanceDays, new PanelService.Cb<List<PanelService.BalanceSeries>>() {
                public void ok(List<PanelService.BalanceSeries> v) { bodyBalance(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        }
    }

    private void bodyPackages(LinearLayout list, List<PanelService.PkgAccount> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) { list.addView(Theme.empty(a, "没有账号")); return; }
        for (PanelService.PkgAccount r : rows) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            LinearLayout top = new LinearLayout(a);
            top.setOrientation(LinearLayout.HORIZONTAL);
            top.setGravity(Gravity.CENTER_VERTICAL);
            TextView name = Theme.text(a, r.nickname.isEmpty() ? r.uid : r.nickname, 14.5f, Theme.INK, 2);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            top.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            top.addView(Theme.num(a, r.remain + " / " + r.size, 13f, Theme.ACCENT));
            card.addView(top);
            if (!r.plan.isEmpty() && !"unknown".equals(r.plan)) {
                card.addView(Theme.text(a, "套餐 " + r.plan
                        + (r.planEvidence.isEmpty() ? "" : " · " + r.planEvidence), 11.5f, Theme.INK3, 0));
            }
            if (!r.error.isEmpty()) {
                card.addView(Theme.text(a, "查询失败：" + r.error, 12f, Theme.BAD, 0));
                list.addView(card);
                continue;
            }
            for (PanelService.Pkg p : r.packages) {
                String end = p.endTime.length() > 10 ? p.endTime.substring(0, 10) : p.endTime;
                LinearLayout line = new LinearLayout(a);
                line.setOrientation(LinearLayout.HORIZONTAL);
                line.setPadding(0, Theme.dp(a, 5), 0, 0);
                TextView n = Theme.text(a, p.name, 12.5f, Theme.INK2, 0);
                n.setSingleLine(true);
                n.setEllipsize(TextUtils.TruncateAt.END);
                line.addView(n, new LinearLayout.LayoutParams(0,
                        ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                line.addView(Theme.num(a, p.remain + "/" + p.size
                        + (end.isEmpty() ? "" : " · " + end), 11.5f, Theme.INK3));
                card.addView(line);
            }
            list.addView(card);
        }
    }

    private void bodyBalance(LinearLayout list, List<PanelService.BalanceSeries> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) {
            list.addView(Theme.empty(a, "还没有余额采样\n网关每 30 分钟采样一次"));
            return;
        }
        for (PanelService.BalanceSeries s : rows) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            LinearLayout top = new LinearLayout(a);
            top.setOrientation(LinearLayout.HORIZONTAL);
            top.setGravity(Gravity.CENTER_VERTICAL);
            TextView name = Theme.text(a, s.name, 14f, Theme.INK, 2);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            top.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView cur = Theme.num(a, String.valueOf(s.current), 16f, Theme.INK);
            top.addView(cur);
            card.addView(top);
            card.addView(Theme.kvRow(a, "窗口内变化", (s.change >= 0 ? "+" : "") + s.change));
            card.addView(Theme.kvRow(a, "区间", s.min + " ~ " + s.max + " · " + s.values.size() + " 点"));

            float[] vals = new float[s.values.size()];
            for (int i = 0; i < vals.length; i++) vals[i] = s.values.get(i);
            Sparkline sp = new Sparkline(a, Theme.ACCENT);
            sp.setValues(vals);
            LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, Theme.dp(a, 60));
            slp.topMargin = Theme.dp(a, Theme.S2);
            card.addView(sp, slp);
            list.addView(card);
        }
    }

    private void bodyVouchers(LinearLayout list, List<PanelService.VoucherAccount> rows) {
        list.removeAllViews();
        int total = 0;
        for (PanelService.VoucherAccount v : rows) {
            if (v.vouchers.isEmpty() && v.error.isEmpty()) continue;
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            card.addView(Theme.text(a, v.nickname.isEmpty() ? v.uid : v.nickname, 14f, Theme.INK, 2));
            if (!v.error.isEmpty()) {
                card.addView(Theme.text(a, "查询失败：" + v.error, 12f, Theme.BAD, 0));
            }
            for (PanelService.Voucher vc : v.vouchers) {
                total++;
                card.addView(Theme.text(a, vc.prize
                        + (vc.validTo.isEmpty() ? "" : " · 至 " + vc.validTo), 12f, Theme.INK2, 0));
                TextView code = Theme.num(a, vc.code, 14f, Theme.ACCENT);
                code.setTextIsSelectable(true);
                card.addView(code);
            }
            list.addView(card);
        }
        if (total == 0) list.addView(Theme.empty(a, "没有可用的券码"));
    }
}
