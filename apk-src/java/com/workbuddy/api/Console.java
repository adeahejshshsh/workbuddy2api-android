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

    /** Activity 已销毁：本对象的所有延时回调都必须变成空操作（issue #176⑥）。
     *  主题切换走 recreate()、旋转、进程恢复都会重建 Activity，旧的 Console 不会自己消失 ——
     *  它持有的 Handler 队列会把 Runnable 一直送到主线程执行。 */
    private volatile boolean disposed;

    /** Activity 销毁时必须调。MainActivity.onDestroy 里的 ui.removeCallbacksAndMessages
     *  清的是 MainActivity 自己的 Handler，管不到 Console 这一个（issue #176⑥）。 */
    void dispose() {
        disposed = true;
        try { ui.removeCallbacksAndMessages(null); } catch (Throwable ignored) {}
    }

    /** 还活着吗：所有往 UI 写的地方都要先过这一关。 */
    private boolean dead() {
        return disposed || a.isFinishing() || a.isDestroyed();
    }

    /** 「还没开始」≠「启动失败」—— 这是 issue #176 修复带出来的一处误报，必须分开。
     *
     *  进程刚起时 status 就是默认的 ST_STOPPED；而 Console 构造后 1.2s 就来加载面板，
     *  那一刻网关通常还没开始监听（冷启动 + 慢盘 + 前台服务握手）。若只看
     *  `statusOf() == ST_STOPPED`，正常冷启动会闪一下「网关启动失败」。
     *  `everStarted` 是 onStartCommand 进过的痕迹，用它把两种情况分开。
     *
     *  还要排掉「用户主动停过」：那条路径上 MainActivity 根本不 start()，所以
     *  everStarted 也永远是 false —— 不排除的话，「已停止」会被渲染成
     *  「还没开始启动」，等于把用户的显式操作又说成没发生。 */
    private boolean notYetStarted() {
        return GatewayService.statusOf() == GatewayService.ST_STOPPED
                && !GatewayService.everStarted()
                && !GatewayService.userStopped(a);
    }

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
        navBar.setBackgroundColor(Theme.NAVBAR);
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
        ui.postDelayed(new Runnable() {
            public void run() {
                if (dead()) return;
                loadOverview(true);
            }
        }, 1200);
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
        if (dead()) return;
        host.removeAllViews();
        host.addView(sv, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        currentScroll = sv;
    }

    void show(View v) {
        if (dead()) return;
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
        if (dead()) return;
        svc.overview(new PanelService.Cb<PanelService.Overview>() {
            public void ok(PanelService.Overview o) {
                if (dead()) return;
                startRetries = 0;
                if ("accounts".equals(screen)) accounts.renderBody(o);
                else if ("overview".equals(screen)) overview.renderBody(o);
            }
            public void fail(String msg) {
                if (dead()) return;
                // issue #176③：网关**明确停着**时不许再画「启动中」。
                // 原来只看失败串 —— Connection refused 也命中 isConnectErr，于是
                // 「用户点了停止」和「网关还在起」被渲染成同一个界面。更糟的是重试链
                // 自己调的是 loadOverview(true)（quiet=true），30 次用尽后走到
                // `if (!quiet)` 恒为 false —— 既不重试也不报错，屏幕永久定格。
                if (isConnectErr(msg)) {
                    // 「停着」才算终态；「还没开始」（见 notYetStarted）继续按启动中重试。
                    boolean terminal = GatewayService.statusOf() == GatewayService.ST_STOPPED
                            && !notYetStarted();
                    if (!terminal && startRetries < MAX_START_RETRIES) {
                        startRetries++;
                        showStarting();
                        ui.postDelayed(new Runnable() {
                            public void run() {
                                if (dead()) return;
                                if ("accounts".equals(screen) || "overview".equals(screen)) loadOverview(true);
                            }
                        }, 2000);
                        return;
                    }
                    showTerminal(msg);   // 停着 / 重试用尽：给终态与出口，别再空转
                    return;
                }
                if (!quiet) toast("加载失败：" + msg);
            }
        });
    }

    /**
     * 终态页（issue #176③）：网关停着或起不来时唯一该显示的界面。
     *
     * 原来 Console 全文不读 GatewayService 任何成员（`grep GatewayService Console.java`
     * 零命中），所有连接类失败一律画「网关启动中」—— 包括「用户已经点了停止」。
     * 而 MainActivity 顶部状态条是按 statusOf() + 端口事实判定的，会正确显示「已停止」：
     * 同一个屏幕上两个互相矛盾的状态，用户看的是内容区，于是以为卡死。
     *
     * 这里把「现在是什么状态」「为什么」「下一步点哪」一次说清。
     */
    void showTerminal(String msg) {
        if (dead()) return;
        boolean notYet = notYetStarted();
        boolean stopped = GatewayService.statusOf() == GatewayService.ST_STOPPED && !notYet;
        boolean byUser = GatewayService.userStopped(a);

        LinearLayout column = Theme.column(a);
        LinearLayout card = Theme.card(a);
        column.addView(card, Theme.cardLp(a, Theme.S3));

        String head;
        int headColor;
        if (stopped && byUser) { head = "网关已停止"; headColor = Theme.INK; }
        else if (stopped) { head = "网关启动失败"; headColor = Theme.BAD; }
        else if (notYet) { head = "网关还没开始启动"; headColor = Theme.WARN; }
        else { head = "网关还在启动"; headColor = Theme.WARN; }
        card.addView(Theme.text(a, head, 17f, headColor, 2));

        // 状态（给人看的一句）→ 原因（为什么）→ 最后一行日志（现场）
        String st = GatewayService.stateText();
        if (st != null && !st.isEmpty()) {
            TextView t = Theme.text(a, st, 12.5f, Theme.INK2, 0);
            t.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            card.addView(t);
        }
        String why = GatewayService.reasonText();
        if (why != null && !why.isEmpty()) {
            TextView t = Theme.text(a, why, 12f, Theme.INK3, 0);
            t.setPadding(0, Theme.dp(a, Theme.S1), 0, 0);
            card.addView(t);
        }
        String last = GatewayService.lastLogLine();
        if (last != null && !last.isEmpty() && !last.equals(why)) {
            TextView t = Theme.text(a, last, 11.5f, Theme.INK3, 0);
            t.setPadding(0, Theme.dp(a, Theme.S1), 0, 0);
            card.addView(t);
        }
        // 原始错误：小字灰，保留现场（排障时最不该被藏起来的就是它）
        if (msg != null && !msg.isEmpty()) {
            TextView t = Theme.text(a, msg, 11f, Theme.SLATE, 0);
            t.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            card.addView(t);
        }

        // 出口：停止态给「启动网关」，其它给「重新探测」（把 startRetries 归零）
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(
                0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        if (stopped) {
            TextView go = Theme.primaryButton(a, "启动网关");
            go.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { startGatewayFromUi(); }
            });
            row.addView(go, blp);
        } else {
            TextView retry = Theme.primaryButton(a, "重新探测");
            retry.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    startRetries = 0;
                    showStarting();
                    loadOverview(false);
                }
            });
            row.addView(retry, blp);
        }
        card.addView(row);

        show(scrollOf(column));
    }

    /** 终态页的「启动网关」：显式动作 —— 清标记、拉起服务、重新探测。 */
    void startGatewayFromUi() {
        if (dead()) return;
        GatewayService.clearUserStop(a);
        GatewayService.start(a);
        startRetries = 0;
        showStarting();
        ui.postDelayed(new Runnable() {
            public void run() {
                if (dead()) return;
                loadOverview(false);
            }
        }, 1200);
    }

    /** 从通知栏停止后切回 App：Activity 没销毁，Console 得自己切到终态页。 */
    void showStopped() {
        if (dead()) return;
        startRetries = 0;
        showTerminal(null);
    }

    private static boolean isConnectErr(String msg) {
        if (msg == null) return false;
        String m = msg.toLowerCase();
        return m.contains("connect") || m.contains("refused") || m.contains("timeout")
                || m.contains("timed out") || m.contains("unreachable") || m.contains("http 502")
                || m.contains("http 503");
    }

    private void showStarting() {
        if (dead()) return;
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
