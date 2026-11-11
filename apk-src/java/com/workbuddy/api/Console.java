package com.workbuddy.api;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.DialogInterface;
import android.content.Intent;
import android.graphics.Typeface;
import android.os.Handler;
import android.os.Looper;
import android.text.TextUtils;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.HorizontalScrollView;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.util.List;

/**
 * 原生控制台的**外壳**（Glassmorphism 深色数据台，ui-ux-pro-max 设计系统）。
 *
 * 结构：AppBar（返回/标题/动作）+ 内容区 + 底部 5 项导航。
 *   底部：账号 · 概览 · 用量 · 日志 · 更多        （导航 ≤5，符合 UX 规则）
 *   二级：数据 / 模型 / 任务 / 设置 / 网关与保活 —— 从「更多」进入，AppBar 带返回
 *
 * 分区已拆成独立文件（每个分区一个类，长驻实例以便切页返回保留状态）：
 *   ScreenAccounts 账号池与账号动作     ScreenOverview 概览（指标/错误汇总）
 *   ScreenUsage    用量（区间/账号/模型）ScreenModels   模型与档位
 *   ScreenData     积分构成/余额趋势/券码 ScreenTasks    队列/作业
 *   ScreenLogs     运行日志/请求日志      ScreenSettings 设置
 *   ScreenMore     更多入口 + 网关与保活
 *
 * 本文件只做：导航、页面切换、共享的滚动容器/图标按钮、概览数据轮询。
 * 只与 PanelService 打交道：不含 HTTP、不含 JSON 解析。
 */
final class Console {

    final Activity a;
    final PanelService svc;
    final Handler ui = new Handler(Looper.getMainLooper());

    private final LinearLayout root, appBar, navBar;
    private final FrameLayout host;
    private final TextView title;
    private final ImageView back;

    private String dest = "accounts";     // 底部导航选中项
    String screen = "accounts";   // 当前实际页面（可能是二级页）
    ScrollView currentScroll;

    // 分区控制器（长驻，切页回来保留各自的区间/筛选/排序状态）
    private ScreenAccounts accounts;
    private ScreenOverview overview;
    private ScreenUsage usage;
    private ScreenModels models;
    private ScreenData data;
    private ScreenTasks tasks;
    private ScreenLogs logs;
    private ScreenSettings settings;
    private ScreenMore more;

    private int startRetries;
    private static final int MAX_START_RETRIES = 30;

    private String taskMode = "queue";


    private int balanceDays = 7;
    private String taskUid = "";

    Console(Activity a) {
        this.a = a;
        this.svc = new PanelService(a);

        root = new LinearLayout(a);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackground(Theme.backdrop(a));

        // AppBar
        appBar = new LinearLayout(a);
        appBar.setOrientation(LinearLayout.HORIZONTAL);
        appBar.setGravity(Gravity.CENTER_VERTICAL);
        appBar.setPadding(Theme.dp(a, Theme.S2), Theme.dp(a, Theme.S2),
                Theme.dp(a, Theme.S2), Theme.dp(a, Theme.S2));
        back = Theme.iconButton(a, R.drawable.ic_back, Theme.INK, "返回");
        back.setVisibility(View.GONE);
        back.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { open("more"); }
        });
        appBar.addView(back);
        title = Theme.text(a, "", 17f, Theme.INK, 2);
        LinearLayout.LayoutParams tlp = new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        tlp.leftMargin = Theme.dp(a, Theme.S2);
        appBar.addView(title, tlp);
        root.addView(appBar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        host = new FrameLayout(a);
        root.addView(host, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        navBar = new LinearLayout(a);
        navBar.setOrientation(LinearLayout.HORIZONTAL);
        navBar.setBackgroundColor(0xF20B1220);
        navBar.setPadding(0, Theme.dp(a, 6), 0, Theme.dp(a, 8));
        root.addView(navBar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        accounts = new ScreenAccounts(this);
        overview = new ScreenOverview(this);
        usage = new ScreenUsage(this);
        models = new ScreenModels(this);
        data = new ScreenData(this);
        tasks = new ScreenTasks(this);
        logs = new ScreenLogs(this);
        settings = new ScreenSettings(this);
        more = new ScreenMore(this);

        buildNav();
        open("accounts");
        ui.postDelayed(new Runnable() { public void run() { loadOverview(true); } }, 1200);
    }

    View view() { return root; }

    // ── 导航 ───────────────────────────────────────────────────────────────
    private void buildNav() {
        navBar.removeAllViews();
        nav("accounts", "账号", R.drawable.ic_accounts);
        nav("overview", "概览", R.drawable.ic_overview);
        nav("usage", "用量", R.drawable.ic_data);
        nav("logs", "日志", R.drawable.ic_logs);
        nav("more", "更多", R.drawable.ic_more);
    }

    private void nav(final String id, String label, int icon) {
        boolean on = id.equals(dest);
        LinearLayout item = new LinearLayout(a);
        item.setOrientation(LinearLayout.VERTICAL);
        item.setGravity(Gravity.CENTER);
        item.setPadding(0, Theme.dp(a, 6), 0, Theme.dp(a, 4));
        ImageView iv = new ImageView(a);
        iv.setImageResource(icon);
        iv.setColorFilter(on ? Theme.ACCENT : Theme.INK3);
        LinearLayout.LayoutParams ilp = new LinearLayout.LayoutParams(
                Theme.dp(a, 22), Theme.dp(a, 22));
        item.addView(iv, ilp);
        TextView t = Theme.text(a, label, 10.5f, on ? Theme.ACCENT : Theme.INK3, on ? 2 : 0);
        LinearLayout.LayoutParams tlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        tlp.topMargin = Theme.dp(a, 4);
        item.addView(t, tlp);
        item.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { open(id); }
        });
        navBar.addView(item, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
    }

    void setBar(String t, boolean withBack, View... actions) {
        title.setText(t);
        back.setVisibility(withBack ? View.VISIBLE : View.GONE);
        // AppBar 尾部动作（除返回与标题外全部重建）
        while (appBar.getChildCount() > 2) appBar.removeViewAt(2);
        for (View v : actions) {
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.leftMargin = Theme.dp(a, Theme.S1);
            appBar.addView(v, lp);
        }
    }

    /** 打开页面：底部导航项直接切；二级页保持底部选中「更多」。 */
    void open(String id) {
        screen = id;
        if ("accounts".equals(id) || "overview".equals(id) || "usage".equals(id)
                || "logs".equals(id) || "more".equals(id)) dest = id;
        else dest = "more";   // data / models / tasks / settings / gateway 都是二级页
        buildNav();
        if ("accounts".equals(id)) { accounts.renderShell(); loadOverview(false); }
        else if ("overview".equals(id)) { overview.renderShell(); loadOverview(false); }
        else if ("data".equals(id)) data.render();
        else if ("logs".equals(id)) logs.render();
        else if ("more".equals(id)) more.render();
        else if ("usage".equals(id)) usage.render();
        else if ("models".equals(id)) models.render();
        else if ("tasks".equals(id)) tasks.render();
        else if ("settings".equals(id)) settings.render();
        else if ("gateway".equals(id)) more.renderGatewayPage();
    }

    void show(ScrollView sv) {
        host.removeAllViews();
        host.addView(sv, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        currentScroll = sv;
    }

    void show(View v) {
        host.removeAllViews();
        host.addView(v, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    ScrollView scrollOf(LinearLayout column) {
        return scrollOf(column, false);
    }

    /** scrollbar=true 时显示右侧竖向滚动条（日志页需要：几千行必须能看出当前在哪）。 */
    ScrollView scrollOf(LinearLayout column, boolean scrollbar) {
        ScrollView sv = new ScrollView(a);
        sv.setVerticalScrollBarEnabled(scrollbar);
        if (scrollbar) {
            sv.setScrollbarFadingEnabled(false);
            sv.setScrollBarStyle(View.SCROLLBARS_INSIDE_INSET);
        }
        sv.addView(column);
        return sv;
    }

    /** 同一页面内刷新时保留滚动位置。 */
    /** 日志页的 TextView（自动刷新据此回写文本）。 */
    View findLogView() { return host.findViewById(android.R.id.text1); }

    void refresh(LinearLayout column) {
        int y = currentScroll == null ? 0 : currentScroll.getScrollY();
        ScrollView sv = scrollOf(column);
        show(sv);
        if (y > 0) sv.post(new Runnable() { public void run() { sv.scrollTo(0, y); } });
    }

    // ── 数据加载 ───────────────────────────────────────────────────────────
    void loadOverview(final boolean quiet) {
        svc.overview(new PanelService.Cb<PanelService.Overview>() {
            public void ok(PanelService.Overview o) {
                startRetries = 0;
                if ("accounts".equals(screen)) accounts.renderBody(o);
                else if ("overview".equals(screen)) overview.renderBody(o);
            }
            public void fail(String msg) {
                if (isConnectErr(msg) && startRetries < MAX_START_RETRIES) {
                    startRetries++;
                    showStarting();
                    ui.postDelayed(new Runnable() {
                        public void run() {
                            if ("accounts".equals(screen) || "overview".equals(screen)) loadOverview(true);
                        }
                    }, 2000);
                    return;
                }
                if (!quiet) toast("加载失败：" + msg);
            }
        });
    }

    private static boolean isConnectErr(String msg) {
        if (msg == null) return false;
        String m = msg.toLowerCase();
        return m.contains("connect") || m.contains("refused") || m.contains("timeout")
                || m.contains("timed out") || m.contains("unreachable") || m.contains("http 502")
                || m.contains("http 503");
    }

    private void showStarting() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setGravity(Gravity.CENTER);
        ProgressBar pb = new ProgressBar(a);
        pb.setIndeterminate(true);
        box.addView(pb);
        TextView t = Theme.text(a, "网关启动中", 16f, Theme.INK, 2);
        t.setGravity(Gravity.CENTER);
        t.setPadding(0, Theme.dp(a, Theme.S4), 0, Theme.dp(a, 4));
        box.addView(t);
        TextView sub = Theme.text(a, "等待后端监听 · 已重试 " + startRetries + " 次（每 2 秒）",
                12f, Theme.INK3, 0);
        sub.setGravity(Gravity.CENTER);
        box.addView(sub);
        show(box);
    }






    // 这些入口仍被 Console/宿主调用，转给账号分区
    void addSheet() { accounts.addSheet(); }
    void importUri(android.net.Uri uri) { accounts.importUri(uri); }

    /** 账号卡「任务」按钮：委托给任务分区（弹层逻辑都在那边）。 */
    void accountTasks(String uid) { tasks.accountTasks(uid); }

    View iconAction(int icon, String desc, final Runnable r) {
        ImageView iv = Theme.iconButton(a, icon, Theme.INK2, desc);
        iv.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        return iv;
    }



























































    /** 版本串取自 PackageManager（与清单 versionName 一致）。 */
    String appVersion() {
        try {
            return a.getPackageManager().getPackageInfo(a.getPackageName(), 0).versionName;
        } catch (Throwable t) { return "—"; }
    }

    void toast(String s) {
        if (a instanceof MainActivity) ((MainActivity) a).toast(s);
        else Toast.makeText(a, s, Toast.LENGTH_SHORT).show();
    }
}
