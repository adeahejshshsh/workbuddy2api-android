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
                        : Theme.outlined(a, 0x0DFFFFFF, Theme.R_PILL, Theme.LINE, 1));
                sw.setTextColor(next ? Theme.ON_ACCENT : Theme.INK2);
                setter.set(next);
            }
        });
        row.addView(sw);
        return row;
    }
}
