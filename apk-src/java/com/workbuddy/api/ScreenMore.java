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
 * 更多（分区：分区入口（用量/模型/任务/设置/数据/网关）、关于）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenMore {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenMore(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderMore(); }
    void renderGatewayPage() { renderGateway(); }

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

    // ── 更多 ───────────────────────────────────────────────────────────────
    private void renderMore() {
        setBar("更多", false);
        LinearLayout col = Theme.column(a);

        LinearLayout nav = Theme.card(a);
        nav.addView(Theme.label(a, "数据与运行"));
        nav.addView(entry("数据", "积分构成、余额趋势、开学季券码", new Runnable() {
            public void run() { open("data"); }
        }));
        nav.addView(Theme.divider(a));
        nav.addView(entry("模型与档位", "价格、限时优惠、能力标记", new Runnable() {
            public void run() { open("models"); }
        }));
        nav.addView(Theme.divider(a));
        nav.addView(entry("任务队列", "队列进度与作业历史", new Runnable() {
            public void run() { open("tasks"); }
        }));
        col.addView(nav, Theme.cardLp(a, Theme.S3));

        LinearLayout tools = Theme.card(a);
        tools.addView(Theme.label(a, "连接与账号"));
        tools.addView(entry("网关与保活", "运行状态、重启、密钥、后台保活", new Runnable() {
            public void run() { open("gateway"); }
        }));
        tools.addView(Theme.divider(a));
        tools.addView(entry("添加 / 迁移账号", "导入文件、导出、API Key、登录", new Runnable() {
            public void run() { c.addSheet(); }
        }));
        tools.addView(Theme.divider(a));
        tools.addView(entry("设置", "定时任务、日志、全部配置", new Runnable() {
            public void run() { open("settings"); }
        }));
        tools.addView(Theme.divider(a));
        tools.addView(entry("网页面板", "完整 Web 界面（兜底）", new Runnable() {
            public void run() {
                try {
                    a.startActivity(new Intent(a, WebPanelActivity.class));
                } catch (Throwable t) { toast("打开失败：" + t); }
            }
        }));
        col.addView(tools, Theme.cardLp(a, Theme.S3));

        LinearLayout about = Theme.card(a);
        about.addView(Theme.label(a, "关于"));
        about.addView(Theme.kvRow(a, "界面", "Glassmorphism · Fira Sans / Mono"));
        about.addView(Theme.kvRow(a, "版本", c.appVersion()));
        col.addView(about);

        show(scrollOf(col));
    }

    private View entry(String title, String sub, final Runnable r) {
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, Theme.dp(a, Theme.S3), 0, Theme.dp(a, Theme.S3));
        LinearLayout texts = new LinearLayout(a);
        texts.setOrientation(LinearLayout.VERTICAL);
        texts.addView(Theme.text(a, title, 14.5f, Theme.INK, 1));
        texts.addView(Theme.text(a, sub, 11.5f, Theme.INK3, 0));
        row.addView(texts, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        TextView arrow = Theme.text(a, "›", 20f, Theme.INK3, 0);
        row.addView(arrow);
        row.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        return row;
    }

    // ── 网关与保活 ─────────────────────────────────────────────────────────
    private void renderGateway() {
        setBar("网关与保活", true);
        LinearLayout col = Theme.column(a);
        if (a instanceof MainActivity) {
            col.addView(((MainActivity) a).gatewayPanel(), Theme.cardLp(a, Theme.S3));
        } else {
            col.addView(Theme.empty(a, "当前宿主没有网关面板"));
        }
        LinearLayout tips = Theme.card(a);
        tips.addView(Theme.label(a, "说明"));
        tips.addView(Theme.text(a, "状态每 2 秒刷新；「保活设置」会按缺失项依次引导（省电白名单 / 悬浮窗 / "
                + "电池优化）。网关跑在应用内进程，锁屏被杀后由前台服务自愈。", 12.5f, Theme.INK3, 0));
        col.addView(tips);
        show(scrollOf(col));
    }
}
