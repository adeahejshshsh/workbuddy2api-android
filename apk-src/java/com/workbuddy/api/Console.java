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
 * 原生控制台界面（Glassmorphism 深色数据台，ui-ux-pro-max 设计系统）。
 *
 * 结构：AppBar（返回/标题/动作）+ 内容区 + 底部 5 项导航。
 *   底部：账号 · 概览 · 数据 · 日志 · 更多      （导航 ≤5，符合 UX 规则）
 *   二级：用量 / 模型 / 任务 / 设置 —— 从「更多」进入，AppBar 带返回
 *
 * 只与 PanelService 打交道：本文件不含 HTTP、不含 JSON 解析。
 */
final class Console {

    private final Activity a;
    private final PanelService svc;
    private final Handler ui = new Handler(Looper.getMainLooper());

    private final LinearLayout root, appBar, navBar;
    private final FrameLayout host;
    private final TextView title;
    private final ImageView back;

    private String dest = "accounts";     // 底部导航选中项
    private String screen = "accounts";   // 当前实际页面（可能是二级页）
    private ScrollView currentScroll;

    private int startRetries;
    private static final int MAX_START_RETRIES = 30;

    private String logMode = "run", logCh = "all", dataMode = "packages", taskMode = "queue";
    private boolean logAuto = true, logLoop;
    private int usageHours = 72, balanceDays = 7;
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
        nav("data", "数据", R.drawable.ic_data);
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

    private void setBar(String t, boolean withBack, View... actions) {
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
    private void open(String id) {
        screen = id;
        if ("accounts".equals(id) || "overview".equals(id) || "data".equals(id)
                || "logs".equals(id) || "more".equals(id)) dest = id;
        else dest = "more";
        buildNav();
        if ("accounts".equals(id)) { renderAccountsShell(); loadOverview(false); }
        else if ("overview".equals(id)) { renderOverviewShell(); loadOverview(false); }
        else if ("data".equals(id)) renderData();
        else if ("logs".equals(id)) renderLogs();
        else if ("more".equals(id)) renderMore();
        else if ("usage".equals(id)) renderUsage();
        else if ("models".equals(id)) renderModels();
        else if ("tasks".equals(id)) renderTasks();
        else if ("settings".equals(id)) renderSettings();
    }

    private void show(ScrollView sv) {
        host.removeAllViews();
        host.addView(sv, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        currentScroll = sv;
    }

    private void show(View v) {
        host.removeAllViews();
        host.addView(v, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    private ScrollView scrollOf(LinearLayout column) {
        ScrollView sv = new ScrollView(a);
        sv.setVerticalScrollBarEnabled(false);
        sv.addView(column);
        return sv;
    }

    /** 同一页面内刷新时保留滚动位置。 */
    private void refresh(LinearLayout column) {
        int y = currentScroll == null ? 0 : currentScroll.getScrollY();
        ScrollView sv = scrollOf(column);
        show(sv);
        if (y > 0) sv.post(new Runnable() { public void run() { sv.scrollTo(0, y); } });
    }

    // ── 数据加载 ───────────────────────────────────────────────────────────
    private void loadOverview(final boolean quiet) {
        svc.overview(new PanelService.Cb<PanelService.Overview>() {
            public void ok(PanelService.Overview o) {
                startRetries = 0;
                if ("accounts".equals(screen)) renderAccounts(o);
                else if ("overview".equals(screen)) renderOverview(o);
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

    // ── 账号 ───────────────────────────────────────────────────────────────
    private void renderAccountsShell() {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));
        LinearLayout col = Theme.column(a);
        col.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
    }

    private void renderAccounts(PanelService.Overview o) {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));

        LinearLayout col = Theme.column(a);

        // 概览条：总数 / 可用 / 冷却 / 禁用
        LinearLayout hero = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.addView(Theme.kpi(a, String.valueOf(o.total), "账号", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.healthy), "可用", Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.cooling), "冷却", Theme.WARN),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.disabled), "禁用", Theme.BAD),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        hero.addView(row);
        col.addView(hero, Theme.cardLp(a, Theme.S4));

        if (o.accounts.isEmpty()) {
            col.addView(Theme.empty(a, "还没有账号\n点右上「＋」导入文件或登录添加"));
        } else {
            for (PanelService.Account acc : o.accounts) col.addView(accountCard(acc));
        }
        refresh(col);
    }

    private View accountCard(final PanelService.Account s) {
        LinearLayout card = Theme.card(a);
        card.setLayoutParams(Theme.cardLp(a, Theme.S3));

        // 第一行：状态点 + 昵称 + 标签
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        int dotColor = s.disabled ? Theme.BAD : (s.cooling || s.paused ? Theme.WARN : Theme.ACCENT);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                Theme.dp(a, 8), Theme.dp(a, 8));
        dlp.rightMargin = Theme.dp(a, Theme.S2);
        head.addView(Theme.dot(a, dotColor, 8), dlp);
        TextView name = Theme.text(a, TextUtils.isEmpty(s.nickname) ? "未命名账号" : s.nickname,
                16f, Theme.INK, 2);
        name.setSingleLine(true);
        name.setEllipsize(TextUtils.TruncateAt.END);
        head.addView(name, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if ("global".equals(s.realm)) head.addView(Theme.tag(a, "国际", Theme.INFO));
        if (s.enterprise) head.addView(Theme.tag(a, "企业", Theme.WARN));
        if (s.apiKey) head.addView(Theme.tag(a, "KEY", Theme.INK3));
        if ("personal".equals(s.override)) head.addView(Theme.tag(a, "强制个人", Theme.ACCENT));
        if ("enterprise".equals(s.override)) head.addView(Theme.tag(a, "强制企业", Theme.WARN));
        card.addView(head);

        // 第二行：uid + 状态
        TextView sub = Theme.text(a, s.uid + "  ·  " + s.statusText(), 11.5f, Theme.INK3, 0);
        sub.setPadding(0, Theme.dp(a, 3), 0, 0);
        card.addView(sub);

        // 第三行：积分 / 成功失败 / 在途（数字用等宽）
        LinearLayout stats = new LinearLayout(a);
        stats.setOrientation(LinearLayout.HORIZONTAL);
        stats.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        stats.addView(statBlock("积分", s.credits + (s.creditsTotal > 0 ? "/" + s.creditsTotal : ""),
                Theme.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1.2f));
        stats.addView(statBlock("成功", String.valueOf(s.success), Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("失败", String.valueOf(s.errTotal), s.errTotal > 0 ? Theme.BAD : Theme.INK3),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("在途", String.valueOf(s.inFlight), Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        card.addView(stats);

        // 动作行：主操作（签到/余额/任务）+ 更多
        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        LinearLayout acts = new LinearLayout(a);
        acts.setOrientation(LinearLayout.HORIZONTAL);
        acts.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        hs.addView(acts);
        card.addView(hs);

        boolean personal = !s.enterprise && !s.apiKey;
        if (personal) acts.addView(actionPill("签到", new Runnable() {
            public void run() { doCheckin(s); }
        }));
        acts.addView(actionPill("余额", new Runnable() {
            public void run() { doBalance(s); }
        }));
        if (personal) acts.addView(actionPill("任务", new Runnable() {
            public void run() { accountTasks(s.uid); }
        }));
        acts.addView(actionPill(s.paused ? "恢复" : "暂停", new Runnable() {
            public void run() { doPause(s); }
        }));
        acts.addView(actionPill("类型", new Runnable() {
            public void run() { typeSheet(s); }
        }));
        acts.addView(actionPill("更多", new Runnable() {
            public void run() { moreSheet(s); }
        }));
        return card;
    }

    private View statBlock(String label, String value, int color) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.addView(Theme.label(a, label));
        TextView v = Theme.num(a, value, 13f, color);
        v.setPadding(0, Theme.dp(a, 2), 0, 0);
        box.addView(v);
        return box;
    }

    private View actionPill(String label, final Runnable r) {
        TextView p = Theme.pill(a, label, false);
        p.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        LinearLayout.LayoutParams lp = (LinearLayout.LayoutParams) p.getLayoutParams();
        if (lp == null) lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = Theme.dp(a, Theme.S2);
        p.setLayoutParams(lp);
        return p;
    }

    private View iconAction(int icon, String desc, final Runnable r) {
        ImageView iv = Theme.iconButton(a, icon, Theme.INK2, desc);
        iv.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        return iv;
    }

    private void doCheckin(PanelService.Account s) {
        toast("签到中…");
        svc.checkin(s.uid, new PanelService.Cb<PanelService.TaskResult>() {
            public void ok(PanelService.TaskResult r) {
                if ("skipped".equals(r.error)) toast("今日已签到");
                else if ("not_done".equals(r.error)) toast("签到未完成，稍后再试");
                else if (!r.error.isEmpty()) toast("签到异常：" + r.error);
                else toast("签到完成" + (r.message.isEmpty() ? "" : "：" + r.message));
                loadOverview(false);
            }
            public void fail(String m) { toast("签到失败：" + m); }
        });
    }

    private void doBalance(PanelService.Account s) {
        svc.balance(s.uid, new PanelService.Cb<long[]>() {
            public void ok(long[] v) {
                toast("余额 " + v[0] + (v[1] > 0 ? " / " + v[1] : ""));
                loadOverview(false);
            }
            public void fail(String m) { toast("余额查询失败：" + m); }
        });
    }

    private void doPause(PanelService.Account s) {
        svc.pause(s.uid, s.paused, new PanelService.Cb<String>() {
            public void ok(String m) { toast("已更新选号状态"); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        });
    }

    private void typeSheet(final PanelService.Account s) {
        final String[] values = {"", "enterprise", "personal"};
        String cur = s.override == null ? "" : s.override;
        final String[] labels = {
                "自动判定" + (cur.isEmpty() ? " · 当前" : ""),
                "强制企业版" + ("enterprise".equals(cur) ? " · 当前" : ""),
                "强制个人版" + ("personal".equals(cur) ? " · 当前" : "")
        };
        int checked = "enterprise".equals(cur) ? 1 : ("personal".equals(cur) ? 2 : 0);
        new AlertDialog.Builder(a, android.R.style.Theme_Material_Dialog_Alert)
                .setTitle("账号类型")
                .setSingleChoiceItems(labels, checked, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        d.dismiss();
                        svc.enterprise(s.uid, values[which], new PanelService.Cb<Boolean>() {
                            public void ok(Boolean changed) {
                                toast("类型已更新" + (changed ? "" : "（判定结果未变）"));
                                loadOverview(false);
                            }
                            public void fail(String m) { toast("切换失败：" + m); }
                        });
                    }
                }).show();
    }

    private void moreSheet(final PanelService.Account s) {
        String[] items = {
                s.paused ? "恢复选号" : "暂停选号",
                s.disabled ? "启用账号" : "禁用账号",
                "账号类型：" + typeLabel(s),
                "探活（只读确认是否已解封）",
                "备注",
                "分组",
                "清空任务跳过名单",
                "模型开关",
                "移除账号"
        };
        new AlertDialog.Builder(a)
                .setTitle(TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname)
                .setItems(items, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        if (which == 0) doPause(s);
                        else if (which == 1) svc.disable(s.uid, !s.disabled, after("已更新"));
                        else if (which == 2) typeSheet(s);
                        else if (which == 3) probe(s);
                        else if (which == 4) editText("备注", "仅本机保存", s.remark, new TextCommit() {
                            public void onText(String t) { svc.setRemark(s.uid, t, after("备注已保存")); }
                        });
                        else if (which == 5) editText("分组", "用于面板筛选", s.group, new TextCommit() {
                            public void onText(String t) { svc.setGroup(s.uid, t, after("分组已保存")); }
                        });
                        else if (which == 6) svc.clearSkip(s.uid, after("已清空跳过名单"));
                        else if (which == 7) modelSheet(s);
                        else confirmRemove(s);
                    }
                }).show();
    }

    private String typeLabel(PanelService.Account s) {
        if ("enterprise".equals(s.override)) return "强制企业版";
        if ("personal".equals(s.override)) return "强制个人版";
        return s.enterprise ? "自动 · 企业" : "自动 · 个人";
    }

    private PanelService.Cb<String> after(final String ok) {
        return new PanelService.Cb<String>() {
            public void ok(String m) { toast(ok); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        };
    }

    private void probe(PanelService.Account s) {
        toast("探活中…");
        svc.probeBan(s.uid, new PanelService.Cb<PanelService.ProbeResult>() {
            public void ok(PanelService.ProbeResult r) {
                String msg;
                if ("lifted".equals(r.verdict)) msg = "已解封，可启用";
                else if ("still_banned".equals(r.verdict)) msg = "仍被封禁，继续等待";
                else if ("blocked".equals(r.verdict)) msg = "被风控拦截，继续观察";
                else if ("session_dead".equals(r.verdict)) msg = "登录态失效，需重新登录";
                else if ("inconclusive".equals(r.verdict)) msg = "无明确结论，稍后再试";
                else msg = "无法探测：" + r.verdict;
                toast(msg + (r.detail.isEmpty() ? "" : "（" + r.detail + "）"));
            }
            public void fail(String m) { toast("探活失败：" + m); }
        });
    }

    private interface TextCommit { void onText(String t); }

    private void editText(String title, String hint, String initial, final TextCommit cb) {
        EditText et = new EditText(a);
        et.setText(initial == null ? "" : initial);
        et.setHint(hint);
        et.setTextSize(15f);
        et.setTypeface(Theme.sans(a));
        new AlertDialog.Builder(a).setTitle(title).setView(et)
                .setNegativeButton("取消", null)
                .setPositiveButton("保存", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        cb.onText(et.getText() == null ? "" : et.getText().toString().trim());
                    }
                }).show();
    }

    private void confirmRemove(final PanelService.Account s) {
        new AlertDialog.Builder(a)
                .setTitle("移除账号")
                .setMessage("删除本地凭据文件，不可撤销。\n\n"
                        + (TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname) + "\n" + s.uid)
                .setNegativeButton("取消", null)
                .setPositiveButton("移除", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.remove(s.uid, after("已移除"));
                    }
                }).show();
    }

    // ── 模型开关 ───────────────────────────────────────────────────────────
    private void modelSheet(final PanelService.Account s) {
        toast("读取模型目录…");
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(final List<PanelService.ModelInfo> models) {
                svc.modelsOff(s.uid, new PanelService.Cb<List<String>>() {
                    public void ok(List<String> off) { showModels(s.uid, models, off); }
                    public void fail(String m) { toast("读取开关失败：" + m); }
                });
            }
            public void fail(String m) { toast("读取模型目录失败：" + m); }
        });
    }

    private void showModels(final String uid, List<PanelService.ModelInfo> models, List<String> off) {
        if (models.isEmpty()) { toast("模型目录为空"); return; }
        LinearLayout box = Theme.column(a);
        TextView hint = Theme.text(a, "勾选 = 该账号可用；取消 = 手动关闭（永久写入 state.json）",
                12f, Theme.INK3, 0);
        box.addView(hint);
        for (final PanelService.ModelInfo m : models) {
            final String bare = m.id.contains(":") ? m.id.substring(m.id.indexOf(':') + 1) : m.id;
            final boolean enabled = !off.contains(bare);
            LinearLayout row = new LinearLayout(a);
            row.setOrientation(LinearLayout.HORIZONTAL);
            row.setGravity(Gravity.CENTER_VERTICAL);
            row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
            LinearLayout info = new LinearLayout(a);
            info.setOrientation(LinearLayout.VERTICAL);
            info.addView(Theme.text(a, m.name, 14f, Theme.INK, 1));
            info.addView(Theme.text(a, m.id, 11f, Theme.INK3, 0));
            row.addView(info, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            final TextView toggle = Theme.pill(a, enabled ? "可用" : "已关闭",
                    enabled ? true : false);
            toggle.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    final boolean next = !"可用".equals(String.valueOf(toggle.getText()));
                    svc.setModelOff(uid, bare, !next, new PanelService.Cb<String>() {
                        public void ok(String msg) {
                            toggle.setText(next ? "可用" : "已关闭");
                            toggle.setBackground(next ? Theme.round(a, Theme.ACCENT, Theme.R_PILL)
                                    : Theme.outlined(a, 0x0DFFFFFF, Theme.R_PILL, Theme.LINE, 1));
                            toggle.setTextColor(next ? Theme.ON_ACCENT : Theme.INK2);
                        }
                        public void fail(String m) { toast("切换失败：" + m); }
                    });
                }
            });
            row.addView(toggle);
            box.addView(row);
            box.addView(Theme.divider(a));
        }
        ScrollView sv = scrollOf(box);
        new AlertDialog.Builder(a).setTitle("模型开关").setView(sv)
                .setPositiveButton("完成", null).show();
    }

    // ── 账号积分任务（弹层）────────────────────────────────────────────────
    private void accountTasks(final String uid) {
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
        AlertDialog.Builder builder = new AlertDialog.Builder(a)
                .setTitle("积分任务").setView(scrollOf(col)).setNegativeButton("关闭", null);
        if (v.canAuto) {
            builder.setPositiveButton("一键完成", new DialogInterface.OnClickListener() {
                public void onClick(DialogInterface d, int w) { autoAll(uid); }
            });
        }
        builder.show();
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

        final LinearLayout list = new LinearLayout(a);
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

    // ── 模型 ───────────────────────────────────────────────────────────────
    private void renderModels() {
        setBar("模型与档位", true, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { renderModels(); }
        }));
        LinearLayout col = Theme.column(a);
        final LinearLayout list = new LinearLayout(a);
        col.addView(list);
        list.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(List<PanelService.ModelInfo> models) {
                list.removeAllViews();
                if (models.isEmpty()) { list.addView(Theme.empty(a, "没有可用模型")); return; }
                for (PanelService.ModelInfo m : models) {
                    LinearLayout card = Theme.card(a);
                    card.setLayoutParams(Theme.cardLp(a, Theme.S2));
                    LinearLayout top = new LinearLayout(a);
                    top.setOrientation(LinearLayout.HORIZONTAL);
                    top.setGravity(Gravity.CENTER_VERTICAL);
                    TextView name = Theme.text(a, m.name, 14.5f, Theme.INK, 2);
                    name.setSingleLine(true);
                    name.setEllipsize(TextUtils.TruncateAt.END);
                    top.addView(name, new LinearLayout.LayoutParams(0,
                            ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                    TextView price = Theme.num(a, m.hasPromo()
                            ? String.format(java.util.Locale.US, "%.2f", m.promoCredits)
                            : (m.credits > 0 ? String.format(java.util.Locale.US, "%.2f", m.credits) : "0"),
                            14f, m.hasPromo() ? Theme.ACCENT : Theme.INK2);
                    top.addView(price);
                    card.addView(top);

                    LinearLayout tags = new LinearLayout(a);
                    tags.setOrientation(LinearLayout.HORIZONTAL);
                    tags.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
                    tags.addView(Theme.tag(a, m.id, Theme.INK3));
                    if (m.hasPromo()) tags.addView(Theme.tag(a, "限时价", Theme.ACCENT));
                    if (m.reasoning) tags.addView(Theme.tag(a, "思考", Theme.INFO));
                    if (m.images) tags.addView(Theme.tag(a, "图片", Theme.INFO));
                    if (m.tools) tags.addView(Theme.tag(a, "工具", Theme.INFO));
                    card.addView(tags);
                    list.addView(card);
                }
            }
            public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
        });
    }

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

        final LinearLayout list = new LinearLayout(a);
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

        final LinearLayout list = new LinearLayout(a);
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

    // ── 日志 ───────────────────────────────────────────────────────────────
    private void renderLogs() {
        setBar("日志", false,
                iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
                    public void run() { renderLogs(); }
                }));
        LinearLayout col = Theme.column(a);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        String[] ids = {"run", "req"};
        String[] labels = {"运行日志", "请求日志"};
        for (int i = 0; i < ids.length; i++) {
            final String id = ids[i];
            TextView p = Theme.pill(a, labels[i], id.equals(logMode));
            p.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { logMode = id; renderLogs(); }
            });
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.rightMargin = Theme.dp(a, Theme.S2);
            seg.addView(p, lp);
        }
        col.addView(seg);

        if ("run".equals(logMode)) {
            LinearLayout chips = new LinearLayout(a);
            chips.setOrientation(LinearLayout.HORIZONTAL);
            chips.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            String[] ch = {"all", "task", "chat", "sys"};
            String[] cl = {"全部", "任务", "对话", "系统"};
            for (int i = 0; i < ch.length; i++) {
                final String id = ch[i];
                TextView p = Theme.pill(a, cl[i], id.equals(logCh));
                p.setTextSize(11.5f);
                p.setOnClickListener(new View.OnClickListener() {
                    public void onClick(View v) { logCh = id; renderLogs(); }
                });
                LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                        ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
                lp.rightMargin = Theme.dp(a, Theme.S1);
                chips.addView(p, lp);
            }
            chips.addView(space());
            final TextView auto = Theme.pill(a, logAuto ? "自动" : "暂停", logAuto);
            auto.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { logAuto = !logAuto; renderLogs(); }
            });
            chips.addView(auto);
            col.addView(chips);

            final TextView tv = Theme.num(a, "加载中…", 11f, Theme.INK2);
            tv.setId(android.R.id.text1);   // 自动刷新据此找回这个 TextView
            tv.setTextIsSelectable(true);
            LinearLayout wrap = Theme.card(a);
            wrap.setPadding(Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S3),
                    Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S3));
            wrap.addView(tv);
            LinearLayout.LayoutParams wlp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            wlp.topMargin = Theme.dp(a, Theme.S3);
            col.addView(wrap, wlp);
            show(scrollOf(col));
            loadLogs(tv);
            startLogLoop();
        } else {
            LinearLayout chips = new LinearLayout(a);
            chips.setOrientation(LinearLayout.HORIZONTAL);
            chips.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            TextView clear = Theme.pill(a, "清空归档", false);
            clear.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { confirmClear(); }
            });
            chips.addView(clear);
            col.addView(chips);

            final LinearLayout list = new LinearLayout(a);
            list.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
            col.addView(list);
            list.addView(Theme.empty(a, "加载中…"));
            show(scrollOf(col));
            svc.requestLogs(new PanelService.Cb<List<PanelService.ReqEvent>>() {
                public void ok(List<PanelService.ReqEvent> rows) { bodyRequests(list, rows); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        }
    }

    private View space() {
        View v = new View(a);
        v.setLayoutParams(new LinearLayout.LayoutParams(0, 1, 1f));
        return v;
    }

    private void loadLogs(final TextView tv) {
        svc.logs(new PanelService.Cb<List<PanelService.LogRow>>() {
            public void ok(List<PanelService.LogRow> rows) {
                StringBuilder sb = new StringBuilder();
                int start = Math.max(0, rows.size() - 400);
                int shown = 0;
                for (int i = start; i < rows.size(); i++) {
                    PanelService.LogRow r = rows.get(i);
                    if (!"all".equals(logCh) && !logCh.equals(r.channel)) continue;
                    String ts = r.time;
                    int t = ts.indexOf('T');
                    if (t > 0 && ts.length() > t + 9) ts = ts.substring(t + 1, t + 9);
                    sb.append(ts).append("  ").append(r.text).append('\n');
                    shown++;
                }
                tv.setText(shown == 0 ? "（暂无日志）" : sb.toString());
            }
            public void fail(String msg) { tv.setText("日志加载失败：" + msg); }
        });
    }

    private void startLogLoop() {
        if (logLoop) return;
        logLoop = true;
        ui.postDelayed(new Runnable() {
            public void run() {
                if (!"logs".equals(screen) || !"run".equals(logMode)) { logLoop = false; return; }
                if (logAuto) {
                    View v = host.findViewById(android.R.id.text1);
                    if (v instanceof TextView) loadLogs((TextView) v);
                }
                ui.postDelayed(this, 3000);
            }
        }, 3000);
    }

    private void bodyRequests(LinearLayout list, List<PanelService.ReqEvent> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) {
            list.addView(Theme.empty(a, "暂无请求记录\n检查 logging.request_archive_enabled"));
            return;
        }
        for (final PanelService.ReqEvent e : rows) {
            LinearLayout card = Theme.card(a);
            card.setLayoutParams(Theme.cardLp(a, Theme.S2));
            card.setClickable(true);
            LinearLayout top = new LinearLayout(a);
            top.setOrientation(LinearLayout.HORIZONTAL);
            top.setGravity(Gravity.CENTER_VERTICAL);
            int color = e.status >= 200 && e.status < 300 ? Theme.ACCENT
                    : (e.status >= 500 ? Theme.BAD : Theme.WARN);
            top.addView(Theme.statusChip(a, String.valueOf(e.status), color));
            String model = e.model.isEmpty() ? "—" : e.model;
            if (!e.modelEff.isEmpty() && !e.modelEff.equals(e.model)) model += " ⤳ " + e.modelEff;
            TextView m = Theme.text(a, model, 12.5f, Theme.INK, 1);
            m.setSingleLine(true);
            m.setEllipsize(TextUtils.TruncateAt.MIDDLE);
            m.setPadding(Theme.dp(a, Theme.S2), 0, 0, 0);
            top.addView(m, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            String ts = e.time;
            int t = ts.indexOf('T');
            top.addView(Theme.num(a, t > 0 && ts.length() > t + 9 ? ts.substring(t + 1, t + 9) : ts,
                    11f, Theme.INK3));
            card.addView(top);

            StringBuilder sb = new StringBuilder();
            if (!e.account.isEmpty()) sb.append(e.account).append(" · ");
            sb.append("总 ").append(e.totalTokens).append(" tok");
            if (e.ttfbMs > 0) sb.append(" · 首字 ").append(e.ttfbMs).append("ms");
            if (e.durationMs > 0) sb.append(" · ").append(e.durationMs).append("ms");
            if (e.creditKnown) sb.append(" · ").append(e.credit).append("c");
            TextView meta = Theme.text(a, sb.toString(), 11f, Theme.INK3, 0);
            meta.setSingleLine(true);
            meta.setEllipsize(TextUtils.TruncateAt.END);
            card.addView(meta);

            card.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { requestDetail(e); }
            });
            list.addView(card);
        }
    }

    private void requestDetail(PanelService.ReqEvent e) {
        LinearLayout col = Theme.column(a);
        LinearLayout card = Theme.card(a);
        card.addView(Theme.kvRow(a, "时间", e.time));
        card.addView(Theme.kvRow(a, "ID", e.requestId));
        card.addView(Theme.kvRow(a, "路径", e.path));
        card.addView(Theme.kvRow(a, "账号", e.account));
        card.addView(Theme.kvRow(a, "模型", e.model + (e.modelEff.isEmpty() || e.modelEff.equals(e.model)
                ? "" : " → " + e.modelEff)));
        card.addView(Theme.kvRow(a, "状态", e.status + " · " + e.outcome));
        card.addView(Theme.kvRow(a, "耗时", e.durationMs + "ms · 首字 " + e.ttfbMs + "ms"));
        card.addView(Theme.kvRow(a, "Token", e.promptTokens + " / " + e.completionTokens + " / " + e.totalTokens));
        if (e.creditKnown) card.addView(Theme.kvRow(a, "积分", String.valueOf(e.credit)));
        if (e.cacheHit > 0 || e.cacheMiss > 0) {
            card.addView(Theme.kvRow(a, "缓存", e.cacheHit + " 命中 / " + e.cacheMiss + " 未命中"));
        }
        card.addView(Theme.kvRow(a, "工具", e.toolsDeclared + " 声明 / " + e.toolCalls + " 调用"));
        if (!e.clientIp.isEmpty()) card.addView(Theme.kvRow(a, "来源", e.clientIp));
        col.addView(card, Theme.cardLp(a, Theme.S3));
        if (!e.context.isEmpty()) {
            LinearLayout ctx = Theme.card(a);
            ctx.addView(Theme.label(a, "会话上下文"));
            TextView tv = Theme.num(a, e.context, 11f, Theme.INK2);
            tv.setTextIsSelectable(true);
            ctx.addView(tv);
            col.addView(ctx);
        }
        new AlertDialog.Builder(a).setTitle("请求详情").setView(scrollOf(col))
                .setPositiveButton("关闭", null).show();
    }

    private void confirmClear() {
        new AlertDialog.Builder(a)
                .setTitle("清空请求归档")
                .setMessage("删除磁盘上的 requests-*.jsonl，不可恢复。\n运行日志（内存）不受影响。")
                .setNegativeButton("取消", null)
                .setPositiveButton("清空", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.clearArchive(new PanelService.Cb<String>() {
                            public void ok(String m) { toast("已清空"); renderLogs(); }
                            public void fail(String msg) { toast("失败：" + msg); }
                        });
                    }
                }).show();
    }

    // ── 更多 ───────────────────────────────────────────────────────────────
    private void renderMore() {
        setBar("更多", false);
        LinearLayout col = Theme.column(a);

        LinearLayout nav = Theme.card(a);
        nav.addView(Theme.label(a, "数据与运行"));
        nav.addView(entry("用量与成本", "按区间、账号、模型看 token 与积分", new Runnable() {
            public void run() { open("usage"); }
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
        tools.addView(entry("添加 / 迁移账号", "导入文件、导出、API Key、登录", new Runnable() {
            public void run() { addSheet(); }
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
        about.addView(Theme.kvRow(a, "版本", appVersion()));
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

    // ── 添加账号 ───────────────────────────────────────────────────────────
    private void addSheet() {
        String[] items = {"导入文件（cockpit / auths JSON）", "导出账号到下载目录",
                "添加 API Key 账号", "登录添加（浏览器授权）"};
        new AlertDialog.Builder(a).setTitle("添加 / 迁移账号")
                .setItems(items, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        if (which == 0) {
                            if (a instanceof MainActivity) ((MainActivity) a).requestImportFile();
                            else toast("当前宿主不支持文件选择");
                        } else if (which == 1) exportAccounts();
                        else if (which == 2) apiKeyDialog();
                        else loginSheet();
                    }
                }).show();
    }

    void importUri(android.net.Uri uri) {
        try {
            java.io.InputStream in = a.getContentResolver().openInputStream(uri);
            if (in == null) { toast("读不到文件"); return; }
            java.io.ByteArrayOutputStream bos = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            in.close();
            byte[] data = bos.toByteArray();
            if (data.length == 0) { toast("文件为空"); return; }
            String name = String.valueOf(uri.getLastPathSegment());
            int slash = name.lastIndexOf('/');
            if (slash >= 0) name = name.substring(slash + 1);
            toast("导入中…");
            svc.importFile(name, data, new PanelService.Cb<PanelService.ImportResult>() {
                public void ok(PanelService.ImportResult r) {
                    toast("导入完成：新增 " + r.imported + "，跳过 " + r.skipped);
                    if (!r.errors.isEmpty()) {
                        StringBuilder sb = new StringBuilder();
                        for (int i = 0; i < Math.min(r.errors.size(), 20); i++) {
                            sb.append("· ").append(r.errors.get(i)).append('\n');
                        }
                        new AlertDialog.Builder(a).setTitle("导入错误")
                                .setMessage(sb.toString()).setPositiveButton("知道了", null).show();
                    }
                    open("accounts");
                }
                public void fail(String m) { toast("导入失败：" + m); }
            });
        } catch (Throwable t) { toast("导入失败：" + t); }
    }

    private void exportAccounts() {
        svc.exportAccounts(new PanelService.FileCb() {
            public void ok(byte[] data, String filename) {
                try {
                    String path;
                    if (android.os.Build.VERSION.SDK_INT >= 29) {
                        android.content.ContentValues cv = new android.content.ContentValues();
                        cv.put(android.provider.MediaStore.MediaColumns.DISPLAY_NAME, filename);
                        cv.put(android.provider.MediaStore.MediaColumns.MIME_TYPE, "application/json");
                        cv.put(android.provider.MediaStore.MediaColumns.RELATIVE_PATH,
                                android.os.Environment.DIRECTORY_DOWNLOADS);
                        android.net.Uri out = a.getContentResolver()
                                .insert(android.provider.MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv);
                        if (out == null) { toast("写入下载目录失败"); return; }
                        java.io.OutputStream os = a.getContentResolver().openOutputStream(out);
                        os.write(data);
                        os.close();
                        path = "下载/" + filename;
                    } else {
                        java.io.File f = new java.io.File(a.getExternalFilesDir(null), filename);
                        java.io.FileOutputStream fos = new java.io.FileOutputStream(f);
                        fos.write(data);
                        fos.close();
                        path = f.getAbsolutePath();
                    }
                    toast("已导出 " + path);
                } catch (Throwable t) { toast("保存失败：" + t); }
            }
            public void fail(String m) { toast("导出失败：" + m); }
        });
    }

    private void apiKeyDialog() {
        LinearLayout box = Theme.column(a);
        final EditText uid = field("uid（手机号或 UUID）");
        final EditText token = field("access token（ck_ / pt_ 开头）");
        final EditText nick = field("昵称（可选）");
        box.addView(uid);
        box.addView(token);
        box.addView(nick);
        new AlertDialog.Builder(a).setTitle("添加 API Key 账号").setView(box)
                .setNegativeButton("取消", null)
                .setPositiveButton("添加", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.addApiKey(uid.getText().toString().trim(),
                                token.getText().toString().trim(),
                                nick.getText().toString().trim(),
                                new PanelService.Cb<String>() {
                                    public void ok(String id) { toast("已添加 " + id); open("accounts"); }
                                    public void fail(String m) { toast("添加失败：" + m); }
                                });
                    }
                }).show();
    }

    private EditText field(String hint) {
        EditText et = new EditText(a);
        et.setHint(hint);
        et.setTextSize(15f);
        et.setTypeface(Theme.sans(a));
        return et;
    }

    private void loginSheet() {
        new AlertDialog.Builder(a).setTitle("登录添加账号")
                .setItems(new String[]{"国内版 · workbuddy.cn", "国际版 · workbuddy.ai"},
                        new DialogInterface.OnClickListener() {
                            public void onClick(DialogInterface d, int which) {
                                startLogin(which == 1 ? "global" : "cn");
                            }
                        }).show();
    }

    private void startLogin(String realm) {
        svc.loginStart(realm, new PanelService.Cb<PanelService.LoginStart>() {
            public void ok(final PanelService.LoginStart s) {
                toast("浏览器已打开，完成授权后自动返回（最长 5 分钟）");
                try {
                    a.startActivity(new Intent(Intent.ACTION_VIEW, android.net.Uri.parse(s.url)));
                } catch (Throwable t) { toast("请手动打开：" + s.url); }
                pollLogin(s.state, 0);
            }
            public void fail(String m) { toast("发起登录失败：" + m); }
        });
    }

    private void pollLogin(final String state, final int attempt) {
        if (attempt > 100) { toast("登录超时"); return; }
        svc.loginPoll(state, new PanelService.Cb<PanelService.LoginPoll>() {
            public void ok(PanelService.LoginPoll p) {
                if (p.done) {
                    toast("登录成功" + (p.uid.isEmpty() ? "" : "：" + p.uid));
                    open("accounts");
                    return;
                }
                if (!p.error.isEmpty()) { toast("登录失败：" + p.error); return; }
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
            public void fail(String msg) {
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
        });
    }

    /** 版本串取自 PackageManager（与清单 versionName 一致）。 */
    private String appVersion() {
        try {
            return a.getPackageManager().getPackageInfo(a.getPackageName(), 0).versionName;
        } catch (Throwable t) { return "—"; }
    }

    private void toast(String s) {
        if (a instanceof MainActivity) ((MainActivity) a).toast(s);
        else Toast.makeText(a, s, Toast.LENGTH_SHORT).show();
    }
}
