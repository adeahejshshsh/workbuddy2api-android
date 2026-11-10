package com.workbuddy.api;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.DialogInterface;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.os.Handler;
import android.os.Looper;
import android.text.TextUtils;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.CheckBox;
import android.widget.CompoundButton;
import android.widget.EditText;
import android.widget.HorizontalScrollView;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import java.util.List;

/**
 * 原生控制台 = **纯 UI**：只负责把 PanelService 给到的模型画成控件、把点击转成服务调用。
 *
 * 分层（这次重排的目的）：
 *   PanelApi      传输层：鉴权、超时、错误文案（不知道业务）
 *   PanelService  数据层：路径、JSON 解析、模型（不含任何 view/widget）
 *   Console       界面层：布局、交互（不含任何 JSONObject / HTTP）
 *
 * 这样接口加字段只改 PanelService，界面改版只改本文件，互不影响。
 * 进阶页面（积分构成、余额趋势、券码、队列、作业、日志、全部配置……）同样在本文件里
 * 分页组织；配置编辑因为控件生成逻辑独立，单独放在 ConfigActivity。
 */
final class Console {

    private static final int BG = 0xFF0F1115, SURFACE = 0xFF171A20, SURFACE2 = 0xFF1F232B;
    private static final int LINE = 0xFF2A2F39, INK = 0xFFE7EAF0, INK2 = 0xFFA7AEBB, INK3 = 0xFF6E7683;
    private static final int ACCENT = 0xFF6E8BFF, OK = 0xFF4CC38A, WARN = 0xFFE5B567, BAD = 0xFFE5677D;

    private final Activity a;
    private final PanelService svc;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private final LinearLayout root, pageBox, tabs;

    private String page = "accounts";
    private ScrollView pageScroll;
    private int startRetries;
    private static final int MAX_START_RETRIES = 30;

    // 各页局部状态
    private String logMode = "run", logCh = "all";
    private boolean logAuto = true, logLoop;
    private String dataMode = "packages";
    private int balanceDays = 7, usageHours = 72;
    private String taskMode = "queue";

    Console(Activity a) {
        this.a = a;
        this.svc = new PanelService(a);
        root = new LinearLayout(a);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);
        pageBox = new LinearLayout(a);
        pageBox.setOrientation(LinearLayout.VERTICAL);
        root.addView(pageBox, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        tabs = new LinearLayout(a);
        tabs.setOrientation(LinearLayout.HORIZONTAL);
        tabs.setBackgroundColor(SURFACE);
        tabs.setPadding(dp(4), dp(6), dp(4), dp(6));
        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        hs.setBackgroundColor(SURFACE);
        hs.addView(tabs);
        root.addView(hs, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        show("accounts");
        ui.postDelayed(new Runnable() { public void run() { loadOverview(true); } }, 1200);
    }

    View view() { return root; }

    // ── 导航 ───────────────────────────────────────────────────────────────
    private void buildTabs() {
        tabs.removeAllViews();
        tab("accounts", "账号"); tab("overview", "概览"); tab("usage", "用量");
        tab("models", "模型"); tab("data", "数据"); tab("tasks", "任务");
        tab("logs", "日志"); tab("settings", "设置");
    }

    private void tab(final String id, String label) {
        TextView t = new TextView(a);
        t.setText(label);
        t.setGravity(Gravity.CENTER);
        t.setTextSize(12.5f);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setTextColor(id.equals(page) ? ACCENT : INK3);
        t.setMinWidth(dp(64));
        t.setPadding(dp(8), dp(9), dp(8), dp(9));
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { show(id); }
        });
        tabs.addView(t);
    }

    void show(String id) {
        page = id;
        buildTabs();
        pageBox.removeAllViews();
        if ("overview".equals(id)) { buildOverviewShell(); loadOverview(false); }
        else if ("usage".equals(id)) buildUsage();
        else if ("models".equals(id)) buildModels();
        else if ("data".equals(id)) buildData();
        else if ("tasks".equals(id)) buildTasks();
        else if ("logs".equals(id)) buildLogs();
        else if ("settings".equals(id)) buildSettings();
        else { buildAccountsShell(); loadOverview(false); }
    }

    /** 换页内容但保留滚动位置（动作后整页重建时不弹回顶部）。 */
    private void restoreInto(final ScrollView sv) {
        final int y = pageScroll == null ? 0 : pageScroll.getScrollY();
        pageBox.removeAllViews();
        pageBox.addView(sv);
        pageScroll = sv;
        if (y > 0) sv.post(new Runnable() { public void run() { sv.scrollTo(0, y); } });
    }

    // ── 数据加载 ───────────────────────────────────────────────────────────
    private void loadOverview(final boolean quiet) {
        svc.overview(new PanelService.Cb<PanelService.Overview>() {
            public void ok(PanelService.Overview o) {
                startRetries = 0;
                if ("overview".equals(page)) renderOverview(o);
                else if ("accounts".equals(page)) renderAccounts(o);
            }
            public void fail(String msg) {
                if (isConnectErr(msg) && startRetries < MAX_START_RETRIES) {
                    startRetries++;
                    showStarting();
                    ui.postDelayed(new Runnable() {
                        public void run() { if ("accounts".equals(page) || "overview".equals(page)) loadOverview(true); }
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
                || m.contains("timed out") || m.contains("unreachable") || m.contains("sockettimeout")
                || m.contains("http 502") || m.contains("http 503");
    }

    private void showStarting() {
        pageBox.removeAllViews();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setGravity(Gravity.CENTER);
        box.setPadding(dp(24), dp(40), dp(24), dp(24));
        ProgressBar pb = new ProgressBar(a);
        pb.setIndeterminate(true);
        box.addView(pb);
        TextView t = new TextView(a);
        t.setText("网关启动中…");
        t.setTextSize(14f);
        t.setTextColor(INK2);
        t.setGravity(Gravity.CENTER);
        t.setPadding(0, dp(12), 0, dp(4));
        box.addView(t);
        TextView sub = new TextView(a);
        sub.setText("Go 后端还没开始监听（已重试 " + startRetries + " 次，每 2 秒一次）");
        sub.setTextSize(11.5f);
        sub.setTextColor(INK3);
        sub.setGravity(Gravity.CENTER);
        box.addView(sub);
        pageBox.addView(box, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
    }

    // ── 账号页 ─────────────────────────────────────────────────────────────
    private void buildAccountsShell() {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        sv.addView(box);
        pageBox.addView(sv);
        pageScroll = sv;
    }

    private void renderAccounts(PanelService.Overview o) {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        sv.addView(box);

        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.setPadding(dp(4), 0, dp(4), dp(8));
        TextView t = new TextView(a);
        t.setText("账号池 · " + o.total + " 个（可用 " + o.healthy + "）");
        t.setTextSize(13f);
        t.setTextColor(INK2);
        head.addView(t, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        Button reload = btn("重载目录", false);
        reload.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        });
        head.addView(reload);
        Button add = btn("＋ 添加", true);
        add.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { addSheet(); }
        });
        head.addView(add);
        box.addView(head);

        if (o.accounts.isEmpty()) {
            box.addView(empty("还没有账号。点右上「＋ 添加」导入文件或登录。"));
        } else {
            for (PanelService.Account acc : o.accounts) box.addView(accountCard(acc));
        }
        restoreInto(sv);
    }

    private View accountCard(final PanelService.Account s) {
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackground(cardBg());
        card.setPadding(dp(13), dp(11), dp(13), dp(10));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(9);
        card.setLayoutParams(lp);

        LinearLayout r1 = new LinearLayout(a);
        r1.setOrientation(LinearLayout.HORIZONTAL);
        r1.setGravity(Gravity.CENTER_VERTICAL);
        View dot = new View(a);
        dot.setBackground(oval(s.cooling || s.paused ? WARN : (s.disabled ? BAD : OK)));
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(dp(8), dp(8));
        dlp.rightMargin = dp(8);
        r1.addView(dot, dlp);
        TextView name = new TextView(a);
        name.setText(TextUtils.isEmpty(s.nickname) ? "未命名" : s.nickname);
        name.setTextSize(14.5f);
        name.setTextColor(INK);
        name.setTypeface(Typeface.DEFAULT_BOLD);
        name.setSingleLine(true);
        name.setEllipsize(TextUtils.TruncateAt.END);
        r1.addView(name, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if ("global".equals(s.realm)) r1.addView(badge("国际", ACCENT));
        if (s.enterprise) r1.addView(badge("企业", WARN));
        if (s.apiKey) r1.addView(badge("Key", INK3));
        if ("personal".equals(s.override)) r1.addView(badge("个人*", OK));
        if ("enterprise".equals(s.override)) r1.addView(badge("企业*", WARN));
        card.addView(r1);

        TextView sub = new TextView(a);
        sub.setText(s.uid + " · " + s.statusText());
        sub.setTextSize(11.5f);
        sub.setTextColor(INK3);
        sub.setPadding(0, dp(3), 0, 0);
        card.addView(sub);

        TextView meta = new TextView(a);
        meta.setText("积分 " + s.credits + (s.creditsTotal > 0 ? "/" + s.creditsTotal : "")
                + " · 成功 " + s.success + " / 失败 " + s.errTotal + " · 在途 " + s.inFlight);
        meta.setTextSize(12f);
        meta.setTextColor(INK2);
        meta.setPadding(0, dp(4), 0, 0);
        card.addView(meta);

        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        LinearLayout acts = new LinearLayout(a);
        acts.setOrientation(LinearLayout.HORIZONTAL);
        acts.setPadding(0, dp(8), 0, 0);
        hs.addView(acts);
        card.addView(hs);

        boolean personal = !s.enterprise && !s.apiKey;
        if (personal) acts.addView(checkinBtn(s));
        acts.addView(balanceBtn(s));
        if (personal) acts.addView(tasksBtn(s));
        acts.addView(pauseBtn(s));
        acts.addView(typeBtn(s));
        acts.addView(moreBtn(s));
        return card;
    }

    private View checkinBtn(final PanelService.Account s) {
        Button b = btn("签到", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                v.setEnabled(false);
                svc.checkin(s.uid, new PanelService.Cb<PanelService.TaskResult>() {
                    public void ok(PanelService.TaskResult r) {
                        v.setEnabled(true);
                        toast("签到完成" + (r.message.isEmpty() ? "" : "：" + r.message));
                        loadOverview(false);
                    }
                    public void fail(String m) { v.setEnabled(true); toast("签到失败：" + m); }
                });
            }
        });
        return b;
    }

    private View balanceBtn(final PanelService.Account s) {
        Button b = btn("余额", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                v.setEnabled(false);
                svc.balance(s.uid, new PanelService.Cb<long[]>() {
                    public void ok(long[] v2) {
                        v.setEnabled(true);
                        toast("余额 " + v2[0] + (v2[1] > 0 ? " / " + v2[1] : ""));
                        loadOverview(false);
                    }
                    public void fail(String m) { v.setEnabled(true); toast("余额刷新失败：" + m); }
                });
            }
        });
        return b;
    }

    private View pauseBtn(final PanelService.Account s) {
        Button b = btn(s.paused ? "恢复选号" : "暂停选号", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                v.setEnabled(false);
                svc.pause(s.uid, s.paused, new PanelService.Cb<String>() {
                    public void ok(String m) { toast(s.paused ? "已恢复选号" : "已暂停选号"); loadOverview(false); }
                    public void fail(String m) { v.setEnabled(true); toast("失败：" + m); }
                });
            }
        });
        return b;
    }

    private View tasksBtn(final PanelService.Account s) {
        Button b = btn("任务", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                currentTaskUid = s.uid;   // autoAll / accept / claim 都以它为目标账号
                openTasks(s.uid);
            }
        });
        return b;
    }

    private View typeBtn(final PanelService.Account s) {
        Button b = btn("类型", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { typeSheet(s); }
        });
        return b;
    }

    private void typeSheet(final PanelService.Account s) {
        final String[] values = new String[]{"", "enterprise", "personal"};
        final String cur = s.override == null ? "" : s.override;
        final String[] labels = new String[]{
                "自动判定" + (cur.isEmpty() ? "（当前）" : ""),
                "强制企业版" + ("enterprise".equals(cur) ? "（当前）" : ""),
                "强制个人版" + ("personal".equals(cur) ? "（当前）" : "")
        };
        int checked = "enterprise".equals(cur) ? 1 : ("personal".equals(cur) ? 2 : 0);
        new AlertDialog.Builder(a)
                .setTitle("账号类型")
                .setSingleChoiceItems(labels, checked, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        d.dismiss();
                        svc.enterprise(s.uid, values[which], new PanelService.Cb<Boolean>() {
                            public void ok(Boolean changed) {
                                toast("账号类型已更新" + (changed ? "" : "（判定结果未变）"));
                                loadOverview(false);
                            }
                            public void fail(String m) { toast("切换失败：" + m); }
                        });
                    }
                }).show();
    }

    private View moreBtn(final PanelService.Account s) {
        Button b = btn("更多", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { moreSheet(s); }
        });
        return b;
    }

    private void moreSheet(final PanelService.Account s) {
        String[] items = new String[]{
                s.paused ? "恢复选号" : "暂停选号",
                s.disabled ? "启用账号" : "禁用账号",
                "账号类型：" + typeLabel(s),
                "探活（只读确认是否已解封）",
                "备注",
                "分组",
                "清空该账号的任务跳过名单",
                "模型开关（该账号禁用哪些模型）",
                "移除账号（不可撤销）"
        };
        new AlertDialog.Builder(a)
                .setTitle(TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname)
                .setItems(items, new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int which) {
                        if (which == 0) svc.pause(s.uid, s.paused, afterAction("已更新"));
                        else if (which == 1) svc.disable(s.uid, !s.disabled, afterAction("已更新"));
                        else if (which == 2) typeSheet(s);
                        else if (which == 3) probe(s);
                        else if (which == 4) editText("备注", "只存本机，不影响上游", s.remark,
                                new TextCommit() { public void onText(String t) { remark(s.uid, t); } });
                        else if (which == 5) editText("分组", "用于面板筛选，仅本机保存", s.group,
                                new TextCommit() { public void onText(String t) { group(s.uid, t); } });
                        else if (which == 6) clearSkip(s.uid);
                        else if (which == 7) modelSheet(s);
                        else confirmRemove(s);
                    }
                }).show();
    }

    private String typeLabel(PanelService.Account s) {
        if ("enterprise".equals(s.override)) return "强制企业版";
        if ("personal".equals(s.override)) return "强制个人版";
        return s.enterprise ? "自动（企业）" : "自动（个人）";
    }

    private PanelService.Cb<String> afterAction(final String okMsg) {
        return new PanelService.Cb<String>() {
            public void ok(String m) { toast(okMsg); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        };
    }

    private void probe(PanelService.Account s) {
        svc.probeBan(s.uid, new PanelService.Cb<PanelService.ProbeResult>() {
            public void ok(PanelService.ProbeResult r) {
                String msg;
                if ("lifted".equals(r.verdict)) msg = "已解封，可以点「启用账号」恢复使用";
                else if ("still_banned".equals(r.verdict)) msg = "仍被封禁，继续等待（勿反复启用）";
                else if ("blocked".equals(r.verdict)) msg = "会话有效但被风控拦截 —— 继续观察";
                else if ("session_dead".equals(r.verdict)) msg = "不是封禁问题：登录态失效，需要重新登录";
                else if ("inconclusive".equals(r.verdict)) msg = "探测无明确结论（网络/限流），稍后再试";
                else msg = "无法探测：" + r.verdict;
                toast(msg + (r.detail.isEmpty() ? "" : "（" + r.detail + "）"));
            }
            public void fail(String m) { toast("探活失败：" + m); }
        });
    }

    private interface TextCommit { void onText(String text); }

    private void editText(String title, String hint, String initial, final TextCommit cb) {
        final EditText et = new EditText(a);
        et.setText(initial == null ? "" : initial);
        et.setHint(hint);
        et.setTextSize(14f);
        new AlertDialog.Builder(a).setTitle(title).setView(et)
                .setNegativeButton("取消", null)
                .setPositiveButton("保存", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        cb.onText(et.getText() == null ? "" : et.getText().toString().trim());
                    }
                }).show();
    }

    private void remark(final String uid, String text) {
        svc.setRemark(uid, text, afterAction("备注已保存"));
    }

    private void group(final String uid, String text) {
        svc.setGroup(uid, text, afterAction("分组已保存"));
    }

    private void clearSkip(final String uid) {
        svc.clearSkip(uid, afterAction("已清空跳过名单"));
    }

    private void confirmRemove(final PanelService.Account s) {
        new AlertDialog.Builder(a)
                .setTitle("移除账号")
                .setMessage("将删除该账号的本地凭据文件，且不可撤销。\n\n"
                        + (TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname) + "（" + s.uid + "）")
                .setNegativeButton("取消", null)
                .setPositiveButton("移除", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.remove(s.uid, afterAction("已移除"));
                    }
                }).show();
    }

    // ── 模型开关 ───────────────────────────────────────────────────────────
    private void modelSheet(final PanelService.Account s) {
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(final List<PanelService.ModelInfo> models) {
                svc.modelsOff(s.uid, new PanelService.Cb<List<String>>() {
                    public void ok(List<String> off) { showModelSheet(s.uid, models, off); }
                    public void fail(String m) { toast("读取模型开关失败：" + m); }
                });
            }
            public void fail(String m) { toast("读取模型目录失败：" + m); }
        });
    }

    private void showModelSheet(final String uid, List<PanelService.ModelInfo> models,
                                List<String> offList) {
        if (models.isEmpty()) { dialogText("模型开关", "模型目录为空"); return; }
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(8), dp(12), dp(8));
        TextView hint = new TextView(a);
        hint.setText("勾选 = 该账号可用；取消勾选 = 手动关闭（永久，落 state.json）。");
        hint.setTextSize(11.5f);
        hint.setTextColor(INK3);
        box.addView(hint);
        for (final PanelService.ModelInfo m : models) {
            final String bare = m.id.contains(":") ? m.id.substring(m.id.indexOf(':') + 1) : m.id;
            final CheckBox cb = new CheckBox(a);
            cb.setText(m.name + "  ·  " + m.id);
            cb.setTextSize(12.5f);
            cb.setTextColor(INK);
            cb.setChecked(!offList.contains(bare));
            cb.setOnCheckedChangeListener(new CompoundButton.OnCheckedChangeListener() {
                public void onCheckedChanged(CompoundButton b, boolean enabled) {
                    svc.setModelOff(uid, bare, !enabled, new PanelService.Cb<String>() {
                        public void ok(String msg) { toast((enabled ? "已启用 " : "已关闭 ") + bare); }
                        public void fail(String msg) {
                            toast("切换失败：" + msg);
                            b.setChecked(!enabled);
                        }
                    });
                }
            });
            box.addView(cb);
        }
        ScrollView sv = new ScrollView(a);
        sv.addView(box);
        new AlertDialog.Builder(a).setTitle("模型开关").setView(sv)
                .setPositiveButton("关闭", null).show();
    }

    // ── 任务弹层 ───────────────────────────────────────────────────────────
    private void openTasks(String uid) {
        svc.tasks(uid, new PanelService.Cb<PanelService.TasksView>() {
            public void ok(PanelService.TasksView v) { showTasks(v); }
            public void fail(String m) { dialogText("任务查询失败", m); }
        });
    }

    private void showTasks(final PanelService.TasksView v) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(14), dp(12), dp(14), dp(8));
        ScrollView sv = new ScrollView(a);
        sv.addView(box);
        if (!v.message.isEmpty()) {
            TextView m = new TextView(a);
            m.setText(v.message);
            m.setTextSize(12f);
            m.setTextColor(WARN);
            m.setPadding(0, 0, 0, dp(8));
            box.addView(m);
        }
        if (v.tasks.isEmpty()) {
            box.addView(empty(v.message.isEmpty() ? "该账号暂无任务" : v.message));
        } else {
            for (PanelService.TaskItem t : v.tasks) box.addView(taskRow(t, v));
        }
        AlertDialog.Builder b = new AlertDialog.Builder(a).setTitle("积分任务").setView(sv)
                .setNegativeButton("关闭", null);
        if (v.canAuto) {
            b.setPositiveButton("一键完成全部", new DialogInterface.OnClickListener() {
                public void onClick(DialogInterface d, int w) { autoAll(v); }
            });
        }
        b.show();
    }

    private View taskRow(final PanelService.TaskItem t, final PanelService.TasksView v) {
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.VERTICAL);
        row.setPadding(0, dp(6), 0, dp(8));
        TextView title = new TextView(a);
        title.setText(t.title);
        title.setTextSize(13.5f);
        title.setTextColor(INK);
        row.addView(title);
        String st = t.claimed ? "已领取" : t.claimable ? "可领取" : t.locked ? "未解锁"
                : "accepted".equals(t.acceptStatus) ? "进行中" : "未接受";
        TextView meta = new TextView(a);
        meta.setText((t.target > 0 ? t.current + " / " + t.target : "—") + " · " + st
                + (t.credit > 0 ? " · +" + t.credit + " 分" : ""));
        meta.setTextSize(11.5f);
        meta.setTextColor(INK3);
        row.addView(meta);
        if (!t.claimed && !t.locked) {
            if (t.claimable && v.canClaim) row.addView(taskAction("领取", t.code, false));
            else if (!t.claimable && v.canAccept && !"accepted".equals(t.acceptStatus)
                    && !t.claimable) row.addView(taskAction("接受", t.code, true));
        }
        return row;
    }

    private View taskAction(final String label, final String code, final boolean accept) {
        Button b = btn(label, true);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(final View v) {
                v.setEnabled(false);
                PanelService.Cb<String> cb = new PanelService.Cb<String>() {
                    public void ok(String m) { toast(label + "完成"); v.setEnabled(true); }
                    public void fail(String m) { toast(label + "失败：" + m); v.setEnabled(true); }
                };
                if (accept) svc.acceptTask(currentTaskUid, code, cb);
                else svc.claimTask(currentTaskUid, code, cb);
            }
        });
        return b;
    }

    private String currentTaskUid = "";

    private void autoAll(PanelService.TasksView v) {
        if (currentTaskUid.isEmpty()) return;
        toast("开始执行可自动任务（含真实对话，可能 1–2 分钟）…");
        svc.autoAll(currentTaskUid, new PanelService.Cb<PanelService.TaskResult>() {
            public void ok(PanelService.TaskResult r) {
                toast("执行完成：成功 " + r.done + "，跳过 " + r.skipped + "，失败 " + r.failed);
            }
            public void fail(String m) { toast("一键完成失败：" + m); }
        });
    }

    // ── 概览 ───────────────────────────────────────────────────────────────
    private void buildOverviewShell() {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(12));
        sv.addView(box);
        pageBox.addView(sv);
        pageScroll = sv;
    }

    private void renderOverview(PanelService.Overview o) {
        if (currentTaskUid == null) currentTaskUid = "";
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(12));
        ScrollView sv = new ScrollView(a);
        sv.addView(box);

        LinearLayout grid = new LinearLayout(a);
        grid.setOrientation(LinearLayout.VERTICAL);
        grid.setBackground(cardBg());
        box.addView(grid);
        statRow(grid, "账号总数", String.valueOf(o.total), INK, "可用", String.valueOf(o.healthy), OK);
        statRow(grid, "冷却中", String.valueOf(o.cooling), WARN, "已禁用", String.valueOf(o.disabled), BAD);
        statRow(grid, "已暂停选号", String.valueOf(o.paused), WARN, "粘性会话", String.valueOf(o.sticky), INK);

        LinearLayout info = new LinearLayout(a);
        info.setOrientation(LinearLayout.VERTICAL);
        info.setBackground(cardBg());
        info.setPadding(dp(14), dp(12), dp(14), dp(12));
        LinearLayout.LayoutParams ilp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        ilp.topMargin = dp(10);
        box.addView(info, ilp);
        info.addView(kv("版本", o.version));
        info.addView(kv("运行", (o.uptimeSec / 3600) + " 时 " + (o.uptimeSec % 3600) / 60 + " 分"));
        info.addView(kv("鉴权", o.authRequired ? "已启用（Bearer）" : "未启用"));
        info.addView(kv("Redis", o.redisMode));

        LinearLayout metrics = new LinearLayout(a);
        metrics.setOrientation(LinearLayout.VERTICAL);
        metrics.setBackground(cardBg());
        metrics.setPadding(dp(14), dp(11), dp(14), dp(11));
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        mlp.topMargin = dp(10);
        box.addView(metrics, mlp);
        renderMetrics(metrics);

        LinearLayout errBox = new LinearLayout(a);
        errBox.setOrientation(LinearLayout.VERTICAL);
        errBox.setBackground(cardBg());
        errBox.setPadding(dp(14), dp(11), dp(14), dp(11));
        LinearLayout.LayoutParams elp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        elp.topMargin = dp(10);
        box.addView(errBox, elp);
        renderErrors(errBox);

        restoreInto(sv);
    }

    private void renderMetrics(final LinearLayout box) {
        box.removeAllViews();
        box.addView(h3("请求指标（本次进程）"));
        svc.metrics(new PanelService.Cb<PanelService.Metrics>() {
            public void ok(PanelService.Metrics m) {
                box.addView(kv("完成 / 在飞", m.completed + " / " + m.inFlight));
                box.addView(kv("成功 / 失败", m.succeeded + " / " + m.failed));
                box.addView(kv("成功率", String.format(java.util.Locale.US, "%.1f%%（HTTP %.1f%%）",
                        m.successRate, m.httpSuccessRate)));
                box.addView(kv("平均耗时", String.format(java.util.Locale.US, "%.0f ms", m.avgDurationMs)));
                box.addView(kv("归档", (m.archiveEnabled ? "开启" : "关闭") + " · 文件 "
                        + m.archiveFiles + " · " + String.format(java.util.Locale.US, "%.1f MB",
                        m.archiveBytes / 1048576.0)));
            }
            public void fail(String msg) { box.addView(kv("请求指标", "不可用：" + msg)); }
        });
    }

    private void renderErrors(final LinearLayout box) {
        box.removeAllViews();
        box.addView(h3("错误汇总"));
        svc.errors(new PanelService.Cb<PanelService.ErrSummary>() {
            public void ok(PanelService.ErrSummary s) {
                box.addView(kv("扫描", s.scanned + " 行 · " + s.updatedAt));
                if (s.rows.isEmpty()) { box.addView(kv("最近", "没有 WARN/ERR")); return; }
                int n = Math.min(s.rows.size(), 20);
                for (int i = 0; i < n; i++) {
                    PanelService.ErrStat r = s.rows.get(i);
                    TextView line = new TextView(a);
                    line.setText(r.count + " 次 · " + r.kind + (r.code.isEmpty() ? "" : " [" + r.code + "]"));
                    line.setTextSize(12.5f);
                    line.setTextColor("bad".equals(r.sev) ? BAD : WARN);
                    line.setPadding(0, dp(4), 0, 0);
                    box.addView(line);
                    TextView meta = new TextView(a);
                    meta.setText((r.account.isEmpty() ? "非账号级" : r.account)
                            + (r.model.isEmpty() ? "" : " · " + r.model)
                            + (r.last.isEmpty() ? "" : " · 最近 " + r.last));
                    meta.setTextSize(11f);
                    meta.setTextColor(INK3);
                    box.addView(meta);
                }
            }
            public void fail(String msg) { box.addView(kv("错误汇总", "不可用：" + msg)); }
        });
    }

    // ── 用量 ───────────────────────────────────────────────────────────────
    private void buildUsage() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), dp(8), dp(10), dp(4));
        final int[] hours = {24, 72, 168, 720};
        String[] labels = {"近 24 小时", "近 3 天", "近 7 天", "近 30 天"};
        for (int i = 0; i < hours.length; i++) {
            final int h = hours[i];
            seg.addView(chipSel(labels[i], h == usageHours, new Runnable() {
                public void run() { usageHours = h; buildUsage(); }
            }));
        }
        box.addView(seg);
        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        list.setPadding(dp(10), dp(4), dp(10), dp(12));
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;
        list.addView(empty("加载中…"));
        svc.usage(usageHours, new PanelService.Cb<PanelService.Usage>() {
            public void ok(PanelService.Usage u) { renderUsage(list, u); }
            public void fail(String m) { list.removeAllViews(); list.addView(empty("用量加载失败：" + m)); }
        });
    }

    private void renderUsage(LinearLayout list, PanelService.Usage u) {
        list.removeAllViews();
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackground(cardBg());
        card.setPadding(dp(13), dp(11), dp(13), dp(11));
        list.addView(card);
        card.addView(kv("统计区间", (u.from + " ~ " + u.to).trim()));
        card.addView(kv("请求", u.requests + "（失败 " + u.errors + "）"));
        card.addView(kv("Token", "总 " + u.totalTokens + " · 入 " + u.promptTokens + " · 出 " + u.completionTokens));
        if (u.cacheHitRate > 0) card.addView(kv("前缀缓存命中率",
                String.format(java.util.Locale.US, "%.1f%%", u.cacheHitRate * 100)));
        card.addView(kv("平均延迟", String.format(java.util.Locale.US, "%.0f ms", u.avgLatencyMs)));
        card.addView(kv("平均吐字", String.format(java.util.Locale.US, "%.1f tok/s", u.avgTps)));
        if (u.credits > 0) card.addView(kv("积分消耗",
                String.format(java.util.Locale.US, "%.2f c", u.credits)));
        agg(list, "按账号", u.byAccount);
        agg(list, "按模型", u.byModel);
    }

    private void agg(LinearLayout list, String title, List<PanelService.Usage.Row> rows) {
        if (rows.isEmpty()) return;
        list.addView(h3(title));
        for (int i = 0; i < Math.min(rows.size(), 20); i++) {
            PanelService.Usage.Row r = rows.get(i);
            LinearLayout row = new LinearLayout(a);
            row.setOrientation(LinearLayout.HORIZONTAL);
            row.setPadding(dp(2), dp(5), dp(2), dp(5));
            TextView name = new TextView(a);
            name.setText((r.extra.isEmpty() ? r.key : r.extra)
                    + (r.realm.isEmpty() ? "" : " · " + r.realm));
            name.setTextSize(12.5f);
            name.setTextColor(INK2);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            row.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView val = new TextView(a);
            val.setText(r.requests + " 次 · " + r.tokens + " tok"
                    + (r.credits > 0 ? String.format(java.util.Locale.US, " · %.2fc", r.credits) : ""));
            val.setTextSize(11.5f);
            val.setTextColor(INK3);
            row.addView(val);
            list.addView(row);
        }
    }

    // ── 模型 ───────────────────────────────────────────────────────────────
    private void buildModels() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;
        list.addView(empty("加载中…"));
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(List<PanelService.ModelInfo> models) { renderModels(list, models); }
            public void fail(String m) { list.removeAllViews(); list.addView(empty("模型加载失败：" + m)); }
        });
    }

    private void renderModels(LinearLayout list, List<PanelService.ModelInfo> models) {
        list.removeAllViews();
        if (models.isEmpty()) { list.addView(empty("没有可用模型")); return; }
        for (PanelService.ModelInfo m : models) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(7);
            card.setLayoutParams(lp);
            LinearLayout r1 = new LinearLayout(a);
            r1.setOrientation(LinearLayout.HORIZONTAL);
            TextView name = new TextView(a);
            name.setText(m.name);
            name.setTextSize(13.5f);
            name.setTextColor(INK);
            name.setTypeface(Typeface.DEFAULT_BOLD);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            r1.addView(name, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView price = new TextView(a);
            price.setText(m.hasPromo() ? String.format(java.util.Locale.US, "%.2f c", m.promoCredits)
                    : (m.credits > 0 ? String.format(java.util.Locale.US, "%.2f c", m.credits) : "免费"));
            price.setTextSize(12.5f);
            price.setTextColor(m.hasPromo() ? OK : INK2);
            r1.addView(price);
            card.addView(r1);

            StringBuilder sb = new StringBuilder(m.id);
            if (m.hasPromo()) sb.append(" · 原价 ").append(String.format(java.util.Locale.US, "%.2f c", m.credits));
            if (!m.tags.isEmpty()) sb.append(" · ").append(m.tags);
            if (m.reasoning) sb.append(" · 思考");
            if (m.images) sb.append(" · 图片");
            if (m.tools) sb.append(" · 工具");
            TextView meta = new TextView(a);
            meta.setText(sb);
            meta.setTextSize(11f);
            meta.setTextColor(INK3);
            card.addView(meta);
            list.addView(card);
        }
    }

    // ── 数据（积分构成 / 余额趋势 / 券码）─────────────────────────────────
    private void buildData() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), dp(8), dp(10), dp(4));
        seg.addView(chipSel("积分构成", "packages".equals(dataMode), new Runnable() {
            public void run() { dataMode = "packages"; buildData(); }
        }));
        seg.addView(chipSel("余额趋势", "balance".equals(dataMode), new Runnable() {
            public void run() { dataMode = "balance"; buildData(); }
        }));
        seg.addView(chipSel("开学季券码", "vouchers".equals(dataMode), new Runnable() {
            public void run() { dataMode = "vouchers"; buildData(); }
        }));
        box.addView(seg);

        if ("balance".equals(dataMode)) {
            LinearLayout seg2 = new LinearLayout(a);
            seg2.setOrientation(LinearLayout.HORIZONTAL);
            seg2.setPadding(dp(10), 0, dp(10), dp(6));
            final int[] days = {7, 30};
            String[] labels = {"近 7 天", "近 30 天"};
            for (int i = 0; i < days.length; i++) {
                final int d = days[i];
                seg2.addView(chipSel(labels[i], d == balanceDays, new Runnable() {
                    public void run() { balanceDays = d; buildData(); }
                }));
            }
            box.addView(seg2);
        }

        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        list.setPadding(dp(10), dp(4), dp(10), dp(12));
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;
        list.addView(empty("加载中…"));

        if ("vouchers".equals(dataMode)) {
            svc.vouchers(new PanelService.Cb<List<PanelService.VoucherAccount>>() {
                public void ok(List<PanelService.VoucherAccount> v) { renderVouchers(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("券码加载失败：" + m)); }
            });
        } else if ("packages".equals(dataMode)) {
            svc.packages(new PanelService.Cb<List<PanelService.PkgAccount>>() {
                public void ok(List<PanelService.PkgAccount> v) { renderPackages(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("积分构成加载失败：" + m)); }
            });
        } else {
            svc.balanceHistory(balanceDays, new PanelService.Cb<List<PanelService.BalanceSeries>>() {
                public void ok(List<PanelService.BalanceSeries> v) { renderBalance(list, v); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("余额趋势加载失败：" + m)); }
            });
        }
    }

    private void renderPackages(LinearLayout list, List<PanelService.PkgAccount> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) { list.addView(empty("没有账号")); return; }
        for (PanelService.PkgAccount r : rows) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(8);
            card.setLayoutParams(lp);
            TextView head = new TextView(a);
            head.setText((r.nickname.isEmpty() ? r.uid : r.nickname) + "  " + r.remain + " / " + r.size
                    + ("global".equals(r.realm) ? "  · 国际" : ""));
            head.setTextSize(13.5f);
            head.setTextColor(INK);
            head.setTypeface(Typeface.DEFAULT_BOLD);
            card.addView(head);
            if (!r.plan.isEmpty() && !"unknown".equals(r.plan)) {
                card.addView(kv("套餐", r.plan + (r.planEvidence.isEmpty() ? "" : "（" + r.planEvidence + "）")));
            }
            if (!r.error.isEmpty()) {
                TextView er = new TextView(a);
                er.setText("查询失败：" + r.error);
                er.setTextSize(11.5f);
                er.setTextColor(BAD);
                card.addView(er);
                list.addView(card);
                continue;
            }
            for (PanelService.Pkg p : r.packages) {
                String end = p.endTime.length() > 10 ? p.endTime.substring(0, 10) : p.endTime;
                TextView row = new TextView(a);
                row.setText("· " + p.name + "  " + p.remain + "/" + p.size
                        + (end.isEmpty() ? "" : " 到期 " + end));
                row.setTextSize(11.5f);
                row.setTextColor(INK2);
                card.addView(row);
            }
            list.addView(card);
        }
    }

    private void renderBalance(LinearLayout list, List<PanelService.BalanceSeries> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) { list.addView(empty("还没有余额采样（网关每 30 分钟采样一次）")); return; }
        for (PanelService.BalanceSeries s : rows) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(8);
            card.setLayoutParams(lp);
            TextView head = new TextView(a);
            head.setText(s.name + "   当前 " + s.current + "（窗口内 " + (s.change >= 0 ? "+" : "") + s.change + "）");
            head.setTextSize(13f);
            head.setTextColor(INK);
            card.addView(head);
            card.addView(kv("区间", s.min + " ~ " + s.max + " · " + s.values.size() + " 个采样"
                    + (s.realm.isEmpty() ? "" : " · " + s.realm)));
            float[] vals = new float[s.values.size()];
            for (int i = 0; i < vals.length; i++) vals[i] = s.values.get(i);
            Sparkline sp = new Sparkline(a, ACCENT);
            sp.setValues(vals);
            LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, dp(56));
            slp.topMargin = dp(6);
            card.addView(sp, slp);
            list.addView(card);
        }
    }

    private void renderVouchers(LinearLayout list, List<PanelService.VoucherAccount> rows) {
        list.removeAllViews();
        int total = 0;
        for (PanelService.VoucherAccount a1 : rows) {
            if (a1.vouchers.isEmpty() && a1.error.isEmpty()) continue;
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(8);
            card.setLayoutParams(lp);
            TextView head = new TextView(a);
            head.setText(a1.nickname.isEmpty() ? a1.uid : a1.nickname);
            head.setTextSize(13f);
            head.setTextColor(INK);
            head.setTypeface(Typeface.DEFAULT_BOLD);
            card.addView(head);
            if (!a1.error.isEmpty()) {
                TextView er = new TextView(a);
                er.setText("查询失败：" + a1.error);
                er.setTextSize(11.5f);
                er.setTextColor(BAD);
                card.addView(er);
            }
            for (PanelService.Voucher v : a1.vouchers) {
                total++;
                TextView row = new TextView(a);
                row.setText("· " + v.prize + (v.validTo.isEmpty() ? "" : "（有效期至 " + v.validTo + "）"));
                row.setTextSize(11.5f);
                row.setTextColor(INK2);
                card.addView(row);
                TextView code = new TextView(a);
                code.setText(v.code);
                code.setTextSize(12.5f);
                code.setTextColor(ACCENT);
                code.setTypeface(Typeface.MONOSPACE);
                code.setTextIsSelectable(true);
                card.addView(code);
            }
            list.addView(card);
        }
        if (total == 0) list.addView(empty("没有可用的开学季券码"));
    }

    // ── 任务页（队列 / 作业）──────────────────────────────────────────────
    private void buildTasks() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), dp(8), dp(10), dp(4));
        seg.addView(chipSel("队列", "queue".equals(taskMode), new Runnable() {
            public void run() { taskMode = "queue"; buildTasks(); }
        }));
        seg.addView(chipSel("作业历史", "jobs".equals(taskMode), new Runnable() {
            public void run() { taskMode = "jobs"; buildTasks(); }
        }));
        seg.addView(spacerH());
        seg.addView(chipSel("刷新", false, new Runnable() {
            public void run() { buildTasks(); }
        }));
        box.addView(seg);
        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        list.setPadding(dp(10), dp(4), dp(10), dp(12));
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;
        list.addView(empty("加载中…"));

        if ("queue".equals(taskMode)) {
            svc.queue(new PanelService.Cb<PanelService.Queue>() {
                public void ok(PanelService.Queue q) { renderQueue(list, q); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("加载失败：" + m)); }
            });
        } else {
            svc.jobs(new PanelService.Cb<List<PanelService.Job>>() {
                public void ok(List<PanelService.Job> js) { renderJobs(list, js); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("加载失败：" + m)); }
            });
        }
    }

    private void renderQueue(LinearLayout list, PanelService.Queue q) {
        list.removeAllViews();
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.VERTICAL);
        head.setBackground(cardBg());
        head.setPadding(dp(12), dp(9), dp(12), dp(9));
        list.addView(head);
        head.addView(kv("状态", q.running ? "运行中" : (q.started ? "已结束" : "未启动")));
        head.addView(kv("条目", q.total + " 条 · 并发 " + q.conc));
        if (q.items.isEmpty()) {
            list.addView(empty("队列为空（面板里「一键完成」或定时成长队列启动后这里会有条目）"));
            return;
        }
        for (PanelService.Queue.Item it : q.items) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(8), dp(12), dp(8));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(6);
            card.setLayoutParams(lp);
            LinearLayout r1 = new LinearLayout(a);
            r1.setOrientation(LinearLayout.HORIZONTAL);
            TextView st = new TextView(a);
            st.setText(statusText(it.status));
            st.setTextSize(11f);
            st.setTextColor(statusColor(it.status));
            st.setTypeface(Typeface.DEFAULT_BOLD);
            r1.addView(st);
            TextView who = new TextView(a);
            who.setText("  " + (it.nickname.isEmpty() ? it.uid : it.nickname) + " · " + it.code);
            who.setTextSize(12.5f);
            who.setTextColor(INK);
            who.setSingleLine(true);
            who.setEllipsize(TextUtils.TruncateAt.END);
            r1.addView(who, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            card.addView(r1);
            if (!it.message.isEmpty()) card.addView(kv("", it.message));
            list.addView(card);
        }
    }

    private void renderJobs(LinearLayout list, List<PanelService.Job> jobs) {
        list.removeAllViews();
        if (jobs.isEmpty()) { list.addView(empty("还没有作业记录")); return; }
        for (PanelService.Job j : jobs) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(12), dp(8), dp(12), dp(8));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(6);
            card.setLayoutParams(lp);
            TextView head = new TextView(a);
            head.setText(j.uid + "  ·  " + statusText(j.status));
            head.setTextSize(13f);
            head.setTextColor("error".equals(j.status) ? BAD : ("interrupted".equals(j.status) ? WARN : INK));
            head.setTypeface(Typeface.DEFAULT_BOLD);
            card.addView(head);
            String upd = j.updatedAt.length() > 19 ? j.updatedAt.substring(5, 19).replace('T', ' ') : j.updatedAt;
            card.addView(kv("进度", "完成 " + j.completed + " / 失败 " + j.failed + " / 跳过 "
                    + j.skipped + " / 共 " + j.total + (upd.isEmpty() ? "" : " · " + upd)));
            if (!j.error.isEmpty() || !j.message.isEmpty()) {
                card.addView(kv("", j.error.isEmpty() ? j.message : j.error));
            }
            list.addView(card);
        }
    }

    // ── 日志 ───────────────────────────────────────────────────────────────
    private void buildLogs() {
        pageBox.removeAllViews();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), dp(8), dp(10), dp(2));
        seg.addView(chipSel("运行日志", "run".equals(logMode), new Runnable() {
            public void run() { logMode = "run"; buildLogs(); }
        }));
        seg.addView(chipSel("请求日志", "req".equals(logMode), new Runnable() {
            public void run() { logMode = "req"; buildLogs(); }
        }));
        box.addView(seg);

        LinearLayout bar = new LinearLayout(a);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setPadding(dp(10), dp(4), dp(10), dp(6));
        if ("run".equals(logMode)) {
            bar.addView(chip("全部", "all"));
            bar.addView(chip("任务", "task"));
            bar.addView(chip("对话", "chat"));
            bar.addView(chip("系统", "sys"));
            bar.addView(spacerH());
            Button auto = btn(logAuto ? "自动刷新：开" : "自动刷新：关", false);
            auto.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { logAuto = !logAuto; buildLogs(); }
            });
            bar.addView(auto);
        } else {
            bar.addView(spacerH());
            Button clear = btn("清空归档", false);
            clear.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { confirmClearArchive(); }
            });
            bar.addView(clear);
            Button reload = btn("刷新", true);
            reload.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { buildLogs(); }
            });
            bar.addView(reload);
        }
        box.addView(bar);

        if ("req".equals(logMode)) {
            LinearLayout list = new LinearLayout(a);
            list.setOrientation(LinearLayout.VERTICAL);
            list.setPadding(dp(10), dp(4), dp(10), dp(12));
            box.addView(list);
            pageBox.addView(box);
            pageScroll = null;
            list.addView(empty("加载中…"));
            svc.requestLogs(new PanelService.Cb<List<PanelService.ReqEvent>>() {
                public void ok(List<PanelService.ReqEvent> rows) { renderRequestLogs(list, rows); }
                public void fail(String m) { list.removeAllViews(); list.addView(empty("请求日志加载失败：" + m)); }
            });
            return;
        }

        final TextView tv = new TextView(a);
        tv.setId(android.R.id.text1);
        tv.setTextSize(10.5f);
        tv.setTextColor(INK2);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setTextIsSelectable(true);
        tv.setPadding(dp(10), dp(4), dp(10), dp(12));
        ScrollView sv = new ScrollView(a);
        sv.addView(tv);
        box.addView(sv, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        pageBox.addView(box);
        loadLogs();
        startLogLoop();
    }

    private void loadLogs() {
        svc.logs(new PanelService.Cb<List<PanelService.LogRow>>() {
            public void ok(List<PanelService.LogRow> rows) {
                TextView tv = (TextView) pageBox.findViewById(android.R.id.text1);
                if (tv == null) return;
                StringBuilder sb = new StringBuilder();
                int start = Math.max(0, rows.size() - 400);
                for (int i = start; i < rows.size(); i++) {
                    PanelService.LogRow r = rows.get(i);
                    if (!"all".equals(logCh) && !logCh.equals(r.channel)) continue;
                    String ts = r.time;
                    int t = ts.indexOf('T');
                    if (t > 0 && ts.length() > t + 9) ts = ts.substring(t + 1, t + 9);
                    sb.append(ts).append(' ').append(r.text).append('\n');
                }
                tv.setText(sb.length() == 0 ? "（暂无日志）" : sb.toString());
            }
            public void fail(String msg) {
                TextView tv = (TextView) pageBox.findViewById(android.R.id.text1);
                if (tv != null) tv.setText("日志加载失败：" + msg);
            }
        });
    }

    private void startLogLoop() {
        if (logLoop) return;
        logLoop = true;
        ui.postDelayed(new Runnable() {
            public void run() {
                if (!"logs".equals(page) || !"run".equals(logMode)) { logLoop = false; return; }
                if (logAuto) loadLogs();
                ui.postDelayed(this, 3000);
            }
        }, 3000);
    }

    private void renderRequestLogs(LinearLayout list, List<PanelService.ReqEvent> rows) {
        list.removeAllViews();
        if (rows.isEmpty()) {
            list.addView(empty("暂无请求记录（检查 logging.request_archive_enabled 是否开启）"));
            return;
        }
        for (final PanelService.ReqEvent e : rows) {
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(cardBg());
            card.setPadding(dp(11), dp(8), dp(11), dp(8));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(7);
            card.setLayoutParams(lp);

            LinearLayout line1 = new LinearLayout(a);
            line1.setOrientation(LinearLayout.HORIZONTAL);
            line1.setGravity(Gravity.CENTER_VERTICAL);
            TextView st = new TextView(a);
            st.setText(String.valueOf(e.status));
            st.setTextSize(12.5f);
            st.setTypeface(Typeface.DEFAULT_BOLD);
            st.setTextColor(e.status >= 200 && e.status < 300 ? OK : (e.status >= 500 ? BAD : WARN));
            line1.addView(st);
            TextView model = new TextView(a);
            String m = e.model.isEmpty() ? "-" : e.model;
            if (!e.modelEff.isEmpty() && !e.modelEff.equals(e.model)) m = m + " ⤳ " + e.modelEff;
            model.setText(m);
            model.setTextSize(12.5f);
            model.setTextColor(INK);
            model.setSingleLine(true);
            model.setEllipsize(TextUtils.TruncateAt.MIDDLE);
            model.setPadding(dp(8), 0, 0, 0);
            line1.addView(model, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView time = new TextView(a);
            String ts = e.time;
            int t = ts.indexOf('T');
            time.setText(t > 0 && ts.length() > t + 9 ? ts.substring(t + 1, t + 9) : ts);
            time.setTextSize(11f);
            time.setTextColor(INK3);
            line1.addView(time);
            card.addView(line1);

            StringBuilder sb = new StringBuilder();
            if (!e.account.isEmpty()) sb.append(e.account).append(" · ");
            sb.append("总 ").append(e.totalTokens).append(" tok");
            if (e.ttfbMs > 0) sb.append(" · 首字 ").append(e.ttfbMs).append("ms");
            if (e.durationMs > 0) sb.append(" · 用时 ").append(e.durationMs).append("ms");
            if (e.creditKnown) sb.append(" · ").append(e.credit).append("c");
            if (!e.outcome.isEmpty()) sb.append(" · ").append(e.outcome);
            TextView meta = new TextView(a);
            meta.setText(sb.toString());
            meta.setTextSize(11f);
            meta.setTextColor(INK3);
            meta.setSingleLine(true);
            meta.setEllipsize(TextUtils.TruncateAt.END);
            card.addView(meta);

            card.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { showRequestDetail(e); }
            });
            list.addView(card);
        }
    }

    private void showRequestDetail(PanelService.ReqEvent e) {
        StringBuilder sb = new StringBuilder();
        sb.append("时间     ").append(e.time).append('\n');
        sb.append("请求 ID  ").append(e.requestId).append('\n');
        sb.append("路径     ").append(e.path).append('\n');
        sb.append("账号     ").append(e.account).append('\n');
        sb.append("模型     ").append(e.model);
        if (!e.modelEff.isEmpty() && !e.modelEff.equals(e.model)) sb.append(" ⤳ ").append(e.modelEff);
        sb.append('\n');
        sb.append("状态     ").append(e.status).append(" · ").append(e.outcome).append('\n');
        sb.append("耗时     ").append(e.durationMs).append("ms · 首字 ").append(e.ttfbMs).append("ms\n");
        sb.append("Token    ").append(e.promptTokens).append(" 入 / ")
                .append(e.completionTokens).append(" 出 / ").append(e.totalTokens).append(" 总\n");
        if (e.creditKnown) sb.append("积分     ").append(e.credit).append('\n');
        if (e.cacheHit > 0 || e.cacheMiss > 0) {
            sb.append("缓存     ").append(e.cacheHit).append(" 命中 / ").append(e.cacheMiss).append(" 未命中\n");
        }
        if (e.toolsDeclared > 0 || e.toolCalls > 0) {
            sb.append("工具     ").append(e.toolsDeclared).append(" 声明 / ").append(e.toolCalls).append(" 调用\n");
        }
        if (!e.clientIp.isEmpty()) sb.append("来源     ").append(e.clientIp).append("  ").append(e.userAgent).append('\n');
        if (!e.context.isEmpty()) sb.append("\n── 会话上下文 ──\n").append(e.context);

        TextView tv = new TextView(a);
        tv.setText(sb.toString());
        tv.setTextSize(12f);
        tv.setTextColor(INK2);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setTextIsSelectable(true);
        tv.setPadding(dp(14), dp(10), dp(14), dp(10));
        ScrollView sv = new ScrollView(a);
        sv.addView(tv);
        new AlertDialog.Builder(a).setTitle("请求详情").setView(sv)
                .setPositiveButton("关闭", null).show();
    }

    private void confirmClearArchive() {
        new AlertDialog.Builder(a)
                .setTitle("清空请求归档")
                .setMessage("将删除磁盘上的 requests-*.jsonl 请求事件文件，不可恢复。\n运行日志（内存环）不受影响。")
                .setNegativeButton("取消", null)
                .setPositiveButton("清空", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.clearArchive(new PanelService.Cb<String>() {
                            public void ok(String m) { toast("归档已清空"); buildLogs(); }
                            public void fail(String msg) { toast("清空失败：" + msg); }
                        });
                    }
                }).show();
    }

    // ── 设置 ───────────────────────────────────────────────────────────────
    private void buildSettings() {
        svc.loadSettings(new PanelService.Cb<PanelService.Settings>() {
            public void ok(PanelService.Settings st) { renderSettings(st); }
            public void fail(String msg) { dialogText("设置", "读取配置失败：" + msg); }
        });
    }

    private void renderSettings(final PanelService.Settings st) {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(16));

        LinearLayout sched = section("定时任务");
        sched.addView(sw("每日签到", st.checkin, new BoolSet() {
            public void set(boolean v) { st.checkin = v; }
        }));
        sched.addView(sw("猫猫旅行", st.travel, new BoolSet() {
            public void set(boolean v) { st.travel = v; }
        }));
        sched.addView(sw("活跃上报", st.activity, new BoolSet() {
            public void set(boolean v) { st.activity = v; }
        }));
        sched.addView(sw("国际版网页活跃", st.webActivity, new BoolSet() {
            public void set(boolean v) { st.webActivity = v; }
        }));
        sched.addView(sw("保活", st.keepalive, new BoolSet() {
            public void set(boolean v) { st.keepalive = v; }
        }));
        box.addView(sched);

        LinearLayout other = section("日志与进阶");
        other.addView(sw("运行日志落盘", st.panelLog, new BoolSet() {
            public void set(boolean v) { st.panelLog = v; }
        }));
        Button all = btn("全部配置（高级）", true);
        all.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                a.startActivity(new android.content.Intent(a, ConfigActivity.class));
            }
        });
        LinearLayout.LayoutParams alp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        alp.topMargin = dp(8);
        other.addView(all, alp);
        box.addView(other);

        Button save = btn("保存上面的开关", true);
        save.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                svc.saveSettings(st, new PanelService.Cb<String>() {
                    public void ok(String extra) { toast("已保存" + extra); }
                    public void fail(String msg) { toast("保存失败：" + msg); }
                });
            }
        });
        LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        slp.topMargin = dp(14);
        box.addView(save, slp);
        box.addView(hint("只覆盖这里显示的开关，其余配置原样保留；选号策略等全部字段请在「全部配置」里改。"));

        sv.addView(box);
        pageBox.removeAllViews();
        pageBox.addView(sv);
        pageScroll = sv;
    }

    private LinearLayout section(String title) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackground(cardBg());
        box.setPadding(dp(14), dp(12), dp(14), dp(12));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(10);
        box.setLayoutParams(lp);
        box.addView(h3(title));
        return box;
    }

    private interface BoolSet { void set(boolean v); }

    private Switch sw(String label, boolean on, final BoolSet setter) {
        Switch s = new Switch(a);
        s.setText(label);
        s.setTextSize(13.5f);
        s.setTextColor(INK);
        s.setChecked(on);
        s.setOnCheckedChangeListener(new CompoundButton.OnCheckedChangeListener() {
            public void onCheckedChanged(CompoundButton b, boolean checked) { setter.set(checked); }
        });
        return s;
    }

    // ── 添加账号 ───────────────────────────────────────────────────────────
    private void addSheet() {
        String[] items = {"导入文件（cockpit 导出 / auths JSON）", "导出账号（JSON 文件）",
                "添加 API Key 账号", "登录添加（浏览器授权，支持国际区）"};
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
            if (in == null) { toast("读不到该文件"); return; }
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
            toast("导入中…（" + data.length + " 字节）");
            svc.importFile(name, data, new PanelService.Cb<PanelService.ImportResult>() {
                public void ok(PanelService.ImportResult r) {
                    toast("导入完成：新增 " + r.imported + "，跳过 " + r.skipped);
                    if (!r.errors.isEmpty()) {
                        StringBuilder sb = new StringBuilder();
                        for (int i = 0; i < Math.min(r.errors.size(), 20); i++) {
                            sb.append("· ").append(r.errors.get(i)).append('\n');
                        }
                        dialogText("导入错误（前 20 条）", sb.toString());
                    }
                    loadOverview(false);
                }
                public void fail(String msg) { toast("导入失败：" + msg); }
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
                    toast("已导出到 " + path + "（" + data.length + " 字节）");
                } catch (Throwable t) { toast("保存失败：" + t); }
            }
            public void fail(String msg) { toast("导出失败：" + msg); }
        });
    }

    private void apiKeyDialog() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(16), dp(8), dp(16), dp(8));
        final EditText uid = new EditText(a);
        uid.setHint("uid（手机号或 UUID）");
        final EditText token = new EditText(a);
        token.setHint("access token（ck_ / pt_ 开头）");
        final EditText nick = new EditText(a);
        nick.setHint("昵称（可选）");
        box.addView(uid);
        box.addView(token);
        box.addView(nick);
        new AlertDialog.Builder(a).setTitle("添加 API Key 账号").setView(box)
                .setNegativeButton("取消", null)
                .setPositiveButton("添加", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        svc.addApiKey(uid.getText().toString().trim(), token.getText().toString().trim(),
                                nick.getText().toString().trim(), new PanelService.Cb<String>() {
                                    public void ok(String id) { toast("已添加 " + id); loadOverview(false); }
                                    public void fail(String msg) { toast("添加失败：" + msg); }
                                });
                    }
                }).show();
    }

    private void loginSheet() {
        new AlertDialog.Builder(a).setTitle("登录添加账号")
                .setItems(new String[]{"国内版（workbuddy.cn）", "国际版（workbuddy.ai）"},
                        new DialogInterface.OnClickListener() {
                            public void onClick(DialogInterface d, int which) {
                                startLogin(which == 1 ? "global" : "cn");
                            }
                        }).show();
    }

    private void startLogin(String realm) {
        svc.loginStart(realm, new PanelService.Cb<PanelService.LoginStart>() {
            public void ok(final PanelService.LoginStart s) {
                toast("已打开浏览器，请完成授权（最长等 5 分钟）");
                try {
                    a.startActivity(new android.content.Intent(android.content.Intent.ACTION_VIEW,
                            android.net.Uri.parse(s.url)));
                } catch (Throwable t) { dialogText("请手动打开链接", s.url); }
                pollLogin(s.state, 0);
            }
            public void fail(String msg) { toast("发起登录失败：" + msg); }
        });
    }

    private void pollLogin(final String state, final int attempt) {
        if (attempt > 100) { toast("登录超时（5 分钟未完成）"); return; }
        svc.loginPoll(state, new PanelService.Cb<PanelService.LoginPoll>() {
            public void ok(PanelService.LoginPoll p) {
                if (p.done) {
                    toast("登录成功" + (p.uid.isEmpty() ? "" : "：" + p.uid));
                    loadOverview(false);
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

    // ── 小组件 ─────────────────────────────────────────────────────────────
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
        if ("done".equals(st)) return OK;
        if ("error".equals(st)) return BAD;
        if ("running".equals(st)) return ACCENT;
        if ("partial".equals(st) || "interrupted".equals(st)) return WARN;
        return INK3;
    }

    private TextView h3(String text) {
        TextView t = new TextView(a);
        t.setText(text);
        t.setTextSize(13f);
        t.setTextColor(INK2);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setPadding(0, dp(6), 0, dp(6));
        return t;
    }

    private TextView kv(String k, String v) {
        TextView t = new TextView(a);
        t.setText(k.isEmpty() ? v : (k + "：" + v));
        t.setTextSize(12.5f);
        t.setTextColor(INK2);
        t.setPadding(0, dp(2), 0, dp(2));
        return t;
    }

    private TextView hint(String text) {
        TextView t = new TextView(a);
        t.setText(text);
        t.setTextSize(11.5f);
        t.setTextColor(INK3);
        t.setPadding(dp(4), dp(8), dp(4), dp(8));
        return t;
    }

    private TextView empty(String msg) {
        TextView t = new TextView(a);
        t.setText(msg);
        t.setTextSize(12.5f);
        t.setTextColor(INK3);
        t.setGravity(Gravity.CENTER);
        t.setPadding(dp(6), dp(24), dp(6), dp(24));
        return t;
    }

    private TextView badge(String text, int color) {
        TextView t = new TextView(a);
        t.setText(text);
        t.setTextSize(10f);
        t.setTextColor(color);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor((color & 0x00FFFFFF) | 0x22000000);
        bg.setCornerRadius(dp(6));
        t.setBackground(bg);
        t.setPadding(dp(5), dp(1), dp(5), dp(1));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.leftMargin = dp(5);
        t.setLayoutParams(lp);
        return t;
    }

    private Button btn(String label, boolean primary) {
        Button b = new Button(a);
        b.setText(label);
        b.setAllCaps(false);
        b.setTextSize(12.5f);
        b.setTextColor(primary ? Color.WHITE : INK);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(primary ? ACCENT : SURFACE2);
        bg.setCornerRadius(dp(9));
        b.setBackground(bg);
        b.setMinHeight(dp(38));
        b.setMinimumHeight(dp(38));
        b.setPadding(dp(12), dp(6), dp(12), dp(6));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = dp(7);
        b.setLayoutParams(lp);
        return b;
    }

    private View chipSel(String label, boolean on, final Runnable onClick) {
        TextView t = new TextView(a);
        t.setText(label);
        t.setTextSize(12.5f);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setTextColor(on ? Color.WHITE : INK3);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(on ? ACCENT : SURFACE2);
        bg.setCornerRadius(dp(8));
        t.setBackground(bg);
        t.setPadding(dp(12), dp(6), dp(12), dp(6));
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { onClick.run(); }
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = dp(8);
        t.setLayoutParams(lp);
        return t;
    }

    private View chip(String label, final String ch) {
        TextView t = new TextView(a);
        t.setText(label);
        t.setTextSize(12f);
        t.setPadding(dp(10), dp(6), dp(10), dp(6));
        t.setTextColor(ch.equals(logCh) ? ACCENT : INK3);
        t.setBackground(cardBg());
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { logCh = ch; buildLogs(); }
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = dp(6);
        t.setLayoutParams(lp);
        return t;
    }

    private View spacerH() {
        View v = new View(a);
        v.setLayoutParams(new LinearLayout.LayoutParams(0, 1, 1f));
        return v;
    }

    private GradientDrawable cardBg() {
        GradientDrawable g = new GradientDrawable();
        g.setColor(SURFACE);
        g.setCornerRadius(dp(12));
        g.setStroke(dp(1), LINE);
        return g;
    }

    private GradientDrawable oval(int color) {
        GradientDrawable g = new GradientDrawable();
        g.setShape(GradientDrawable.OVAL);
        g.setColor(color);
        return g;
    }

    private void statRow(LinearLayout parent, String k1, String v1, int c1, String k2, String v2, int c2) {
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(dp(14), dp(11), dp(14), dp(11));
        row.addView(stat(k1, v1, c1), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(stat(k2, v2, c2), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        parent.addView(row);
        View div = new View(a);
        div.setBackgroundColor(LINE);
        parent.addView(div, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(1)));
    }

    private View stat(String label, String value, int color) {
        LinearLayout v = new LinearLayout(a);
        v.setOrientation(LinearLayout.VERTICAL);
        TextView val = new TextView(a);
        val.setText(value);
        val.setTextSize(22f);
        val.setTextColor(color);
        val.setTypeface(Typeface.create("monospace", Typeface.BOLD));
        TextView lab = new TextView(a);
        lab.setText(label);
        lab.setTextSize(11.5f);
        lab.setTextColor(INK3);
        v.addView(val);
        v.addView(lab);
        return v;
    }

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                a.getResources().getDisplayMetrics());
    }

    private void toast(String s) {
        if (a instanceof MainActivity) ((MainActivity) a).toast(s);
        else Toast.makeText(a, s, Toast.LENGTH_SHORT).show();
    }

    private void dialogText(String title, String msg) {
        new AlertDialog.Builder(a).setTitle(title).setMessage(msg)
                .setPositiveButton("知道了", null).show();
    }
}
