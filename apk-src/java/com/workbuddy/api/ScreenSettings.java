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
 * 设置（分区：定时任务开关、全部配置入口）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenSettings {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenSettings(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderSettings(); }

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

    // ── 设置 ───────────────────────────────────────────────────────────────
    private void renderSettings() {
        setBar("设置", true);
        LinearLayout col = Theme.column(a);
        col.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
        svc.loadSettings(new PanelService.Cb<PanelService.Settings>() {
            public void ok(PanelService.Settings st) { bodySettings(st); }
            public void fail(String m) {
                LinearLayout col = Theme.column(a);
                col.addView(Theme.empty(a, "读取配置失败：" + m));
                show(scrollOf(col));
            }
        });
    }

    private void bodySettings(final PanelService.Settings st) {
        LinearLayout col = Theme.column(a);

        // 外观（issue #167 第 1 条）：跟随系统 / 深色 / 浅色。默认跟随系统。
        //
        // 改完必须 recreate()：调色板是进程级 static，已经建好的视图不会自己变色；
        // 重建后 onCreate 会重新 apply + 重挂窗口主题（手动覆盖走 setTheme）。
        LinearLayout look = Theme.card(a);
        look.addView(Theme.label(a, "外观"));
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
        String[] modes = {Theme.MODE_AUTO, Theme.MODE_DARK, Theme.MODE_LIGHT};
        for (final String m : modes) {
            TextView p = Theme.pill(a, Theme.modeLabel(m), m.equals(Theme.mode));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
            if (!m.equals(modes[modes.length - 1])) lp.rightMargin = Theme.dp(a, Theme.S2);
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    if (m.equals(Theme.mode)) return;
                    Theme.saveMode(a, m);
                    a.recreate();
                }
            });
            seg.addView(p, lp);
        }
        look.addView(seg);
        look.addView(Theme.text(a, "浅色是为系统浅色环境调的；深色保持原观感。",
                11.5f, Theme.INK3, 0));
        col.addView(look, Theme.cardLp(a, Theme.S3));

        LinearLayout sched = Theme.card(a);
        sched.addView(Theme.label(a, "定时任务"));
        sched.addView(toggleRow("每日签到", st.checkin, new BoolSet() {
            public void set(boolean v) { st.checkin = v; }
        }));
        sched.addView(toggleRow("猫猫旅行", st.travel, new BoolSet() {
            public void set(boolean v) { st.travel = v; }
        }));
        sched.addView(toggleRow("活跃上报", st.activity, new BoolSet() {
            public void set(boolean v) { st.activity = v; }
        }));
        sched.addView(toggleRow("国际版网页活跃", st.webActivity, new BoolSet() {
            public void set(boolean v) { st.webActivity = v; }
        }));
        sched.addView(toggleRow("保活", st.keepalive, new BoolSet() {
            public void set(boolean v) { st.keepalive = v; }
        }));
        col.addView(sched, Theme.cardLp(a, Theme.S3));

        // 余额刷新间隔（issue #194）：此前只在「全部配置（高级）」里能改，而那一页是裸
        // key-value 列表，改坏了没有校验也没有回退。这个值直接决定「设备不睡的前提下
        // 还额外打多少上游请求」，是 #177 里除 wakelock 外唯一可调、又真正影响耗电的项，
        // 不该让用户自己去翻 schedule.balance_refresh_minutes。
        //
        // 保存后**不需要重启网关**：cmd/server 的 ApplyConfig 会调
        // scheduler.SetBalanceInterval（main.go:1049），热生效、下一轮就用新间隔。
        // 「关闭」走 balance_refresh_enabled=false —— 不能把分钟数设成 0，
        // config 校验会把 <=0 回落成 5（config.go:1501）。
        col.addView(balanceCard(st), Theme.cardLp(a, Theme.S3));

        LinearLayout other = Theme.card(a);
        other.addView(Theme.label(a, "日志"));
        other.addView(toggleRow("运行日志落盘", st.panelLog, new BoolSet() {
            public void set(boolean v) { st.panelLog = v; }
        }));
        col.addView(other, Theme.cardLp(a, Theme.S3));

        TextView save = Theme.primaryButton(a, "保存开关");
        save.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                svc.saveSettings(st, new PanelService.Cb<String>() {
                    public void ok(String extra) { toast("已保存" + extra); }
                    public void fail(String m) { toast("保存失败：" + m); }
                });
            }
        });
        col.addView(save, Theme.cardLp(a, Theme.S3));

        TextView all = Theme.ghostButton(a, "全部配置（高级）");
        all.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                a.startActivity(new Intent(a, ConfigActivity.class));
            }
        });
        col.addView(all, Theme.cardLp(a, Theme.S3));

        col.addView(Theme.text(a, "只覆盖这里的开关，其余配置原样保留。", 11.5f, Theme.INK3, 0));
        show(scrollOf(col));
    }

    /**
     * 余额刷新间隔卡片（issue #194）。
     *
     * 为什么不复用 toggleRow：这一项是「多个档位 + 关闭」，不是布尔。做成横向 pill
     * 组而不是下拉，是因为设置页其余控件都是 pill/开关，插一个 Spinner 会破坏手感；
     * 档位只有 5 个，横排刚好一屏放得下（窄屏可横滑）。
     */
    private View balanceCard(final PanelService.Settings st) {
        LinearLayout card = Theme.card(a);
        card.addView(Theme.label(a, "余额刷新"));

        final int[] options = {5, 10, 30, 60, 0};      // 0 = 关闭
        final String[] labels = {"5 分钟", "10 分钟", "30 分钟", "60 分钟", "关闭"};
        final TextView[] pills = new TextView[options.length];
        final LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);

        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        for (int i = 0; i < options.length; i++) {
            final int idx = i;
            pills[i] = Theme.pill(a, labels[i], false);
            pills[i].setTextSize(11.5f);
            pills[i].setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    st.balanceEnabled = options[idx] > 0;
                    if (options[idx] > 0) st.balanceMinutes = options[idx];
                    for (int k = 0; k < pills.length; k++) applyPill(pills[k], k == idx);
                }
            });
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            if (i < options.length - 1) lp.rightMargin = Theme.dp(a, Theme.S1);
            row.addView(pills[i], lp);
        }
        hs.addView(row);
        card.addView(hs);

        // 初始选中：关闭时选最后一档；否则找最接近的档位（老配置 5/10/30/60 之外的
        // 值不会凭空改用户配置，只把「最接近的那档」标成选中，保存前用户不动它就不写）。
        int sel = options.length - 1;
        if (st.balanceEnabled) {
            sel = 0;
            int best = Integer.MAX_VALUE;
            for (int i = 0; i < options.length - 1; i++) {
                int d = Math.abs(options[i] - st.balanceMinutes);
                if (d < best) { best = d; sel = i; }
            }
        }
        for (int k = 0; k < pills.length; k++) applyPill(pills[k], k == sel);

        card.addView(Theme.text(a,
                "每轮会对池内每个账号发一次上游额度查询，调小会增加耗电。"
                        + "保存后立即生效，不需要重启网关。",
                11.5f, Theme.INK3, 0));
        return card;
    }

    /** pill 选中态样式（与 toggleRow 同一套令牌，避免两处视觉漂移）。 */
    private void applyPill(TextView p, boolean on) {
        p.setBackground(on ? Theme.round(a, Theme.ACCENT, Theme.R_PILL)
                : Theme.outlined(a, Theme.OVERLAY_SOFT, Theme.R_PILL, Theme.LINE, 1));
        p.setTextColor(on ? Theme.ON_ACCENT : Theme.INK2);
    }

    private interface BoolSet { void set(boolean v); }

    private View toggleRow(String label, boolean on, final BoolSet setter) {
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
        row.addView(Theme.text(a, label, 14f, Theme.INK, 0),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        final TextView sw = Theme.pill(a, on ? "开" : "关", on);
        sw.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                boolean next = !"开".equals(String.valueOf(sw.getText()));
                sw.setText(next ? "开" : "关");
                sw.setBackground(next ? Theme.round(a, Theme.ACCENT, Theme.R_PILL)
                        : Theme.outlined(a, Theme.OVERLAY_SOFT, Theme.R_PILL, Theme.LINE, 1));
                sw.setTextColor(next ? Theme.ON_ACCENT : Theme.INK2);
                setter.set(next);
            }
        });
        row.addView(sw);
        return row;
    }
}
