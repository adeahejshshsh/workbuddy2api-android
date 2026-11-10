package com.workbuddy.api;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.Context;
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
import android.widget.CompoundButton;
import android.widget.EditText;
import android.widget.HorizontalScrollView;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.ScrollView;
import android.widget.Spinner;
import android.widget.ArrayAdapter;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

/**
 * 原生控制台（替代 WebView 面板）。
 *
 * 为什么改原生：面板是给桌面浏览器写的，塞进手机 WebView 后 6–9 列的表格怎么都别扭，
 * 弹层、滚动、点按都要靠 CSS 硬凑。这里用平台控件重排成 App 的形态：
 *   底部四个 Tab（概览 / 账号 / 日志 / 设置）+ 卡片列表 + 底部抽屉式的任务弹层。
 *
 * 后端一行没改：所有数据仍来自 Go 网关的 /panel/api/*，与网页版同一套接口、同一套语义
 * （企业版/国际版门控、任务 capabilities、账号类型覆盖……）。
 *
 * 未覆盖的进阶页面（积分构成、余额趋势、模型锁池、请求记录、导入导出）保留「网页面板」
 * 入口，避免为了重写而丢功能。
 */
final class Console {

    // 与 MainActivity / 面板同一套暗色令牌
    private static final int BG = 0xFF0F1115, SURFACE = 0xFF171A20, SURFACE2 = 0xFF1F232B;
    private static final int LINE = 0xFF2A2F39, INK = 0xFFE7EAF0, INK2 = 0xFFA7AEBB, INK3 = 0xFF6E7683;
    private static final int ACCENT = 0xFF6E8BFF, OK = 0xFF4CC38A, WARN = 0xFFE5B567, BAD = 0xFFE5677D;

    private final Activity a;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private final LinearLayout root;      // 整块（页面 + 底部 tab）
    private final LinearLayout pageBox;   // 页面容器
    private final LinearLayout tabs;
    private String page = "accounts";
    private boolean logAuto = true;
    private String logCh = "all";
    private JSONObject overviewCache;

    Console(Activity a) {
        this.a = a;
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
        root.addView(tabs, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        buildTabs();
        show("accounts");
        ui.postDelayed(new Runnable() { public void run() { autoRefresh(); } }, 1200);
    }

    View view() { return root; }

    // ── Tab ────────────────────────────────────────────────────────────────
    private void buildTabs() {
        tabs.removeAllViews();
        addTab("accounts", "账号");
        addTab("overview", "概览");
        addTab("logs", "日志");
        addTab("settings", "设置");
    }

    private void addTab(final String id, String label) {
        TextView t = new TextView(a);
        t.setText(label);
        t.setGravity(Gravity.CENTER);
        t.setTextSize(12.5f);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setTextColor(id.equals(page) ? ACCENT : INK3);
        t.setPadding(0, dp(9), 0, dp(9));
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { show(id); }
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        tabs.addView(t, lp);
    }

    void show(String id) {
        page = id;
        buildTabs();
        pageBox.removeAllViews();
        if ("overview".equals(id)) buildOverview();
        else if ("logs".equals(id)) buildLogs();
        else if ("settings".equals(id)) buildSettings();
        else buildAccounts();
    }

    private void autoRefresh() {
        if ("logs".equals(page)) { loadLogs(); return; }
        if ("accounts".equals(page) || "overview".equals(page)) loadOverview(true);
    }

    // ── 概览 ───────────────────────────────────────────────────────────────
    private void buildOverview() {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(12));
        sv.addView(box);

        LinearLayout grid = new LinearLayout(a);
        grid.setOrientation(LinearLayout.VERTICAL);
        grid.setBackground(card());
        box.addView(grid);

        pageBox.addView(sv);
        loadOverview(false);
    }

    private void renderOverview(JSONObject o) {
        pageBox.removeAllViews();
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(12));
        sv.addView(box);

        LinearLayout grid = new LinearLayout(a);
        grid.setOrientation(LinearLayout.VERTICAL);
        grid.setBackground(card());
        box.addView(grid);

        int total = o.optInt("total"), healthy = o.optInt("healthy");
        int cooling = o.optInt("cooling"), disabled = o.optInt("disabled");
        int paused = o.optInt("paused"), sticky = o.optInt("sticky_sessions");
        addStatRow(grid, "账号总数", String.valueOf(total), INK, "可用", String.valueOf(healthy), OK);
        addStatRow(grid, "冷却中", String.valueOf(cooling), WARN, "已禁用", String.valueOf(disabled), BAD);
        addStatRow(grid, "已暂停选号", String.valueOf(paused), WARN, "粘性会话", String.valueOf(sticky), INK);

        LinearLayout info = new LinearLayout(a);
        info.setOrientation(LinearLayout.VERTICAL);
        info.setBackground(card());
        info.setPadding(dp(14), dp(12), dp(14), dp(12));
        LinearLayout.LayoutParams ilp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        ilp.topMargin = dp(10);
        box.addView(info, ilp);
        info.addView(kv("版本", o.optString("version", "-")));
        int up = o.optInt("uptime_sec");
        info.addView(kv("运行", (up / 3600) + " 时 " + (up % 3600) / 60 + " 分"));
        info.addView(kv("鉴权", o.optBoolean("auth_required") ? "已启用（Bearer）" : "未启用"));
        info.addView(kv("Redis", o.optString("redis_mode", "-")));

        Button refresh = btn("刷新", false);
        refresh.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { loadOverview(false); }
        });
        LinearLayout.LayoutParams rlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        rlp.topMargin = dp(10);
        box.addView(refresh, rlp);

        pageBox.addView(sv);
    }

    private void addStatRow(LinearLayout parent, String k1, String v1, int c1,
                            String k2, String v2, int c2) {
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(dp(14), dp(11), dp(14), dp(11));
        row.addView(stat(k1, v1, c1), new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(stat(k2, v2, c2), new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        parent.addView(row);
        View div = new View(a);
        div.setBackgroundColor(LINE);
        parent.addView(div, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(1)));
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

    // ── 账号列表 ───────────────────────────────────────────────────────────
    private void buildAccounts() {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        sv.addView(box);
        pageBox.addView(sv);
        loadOverview(false);
    }

    private void renderAccounts(JSONObject o) {
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
        t.setText("账号池 · " + o.optInt("total") + " 个（可用 " + o.optInt("healthy") + "）");
        t.setTextSize(13f);
        t.setTextColor(INK2);
        head.addView(t, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        Button reload = btn("重载目录", true);
        reload.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { reloadAccounts(); }
        });
        head.addView(reload);
        box.addView(head);

        JSONArray arr = o.optJSONArray("accounts");
        if (arr == null || arr.length() == 0) {
            box.addView(empty("还没有账号。点右上「网页面板」导入，或用登录/API Key 入口添加。"));
        } else {
            for (int i = 0; i < arr.length(); i++) {
                JSONObject s = arr.optJSONObject(i);
                if (s != null) box.addView(accountCard(s));
            }
        }
        pageBox.removeAllViews();
        pageBox.addView(sv);
    }

    private View accountCard(final JSONObject s) {
        final String uid = s.optString("uid");
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackground(card());
        card.setPadding(dp(13), dp(11), dp(13), dp(10));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(9);
        card.setLayoutParams(lp);

        // 第一行：昵称 + 状态点 + 徽章
        LinearLayout r1 = new LinearLayout(a);
        r1.setOrientation(LinearLayout.HORIZONTAL);
        r1.setGravity(Gravity.CENTER_VERTICAL);
        View dot = new View(a);
        dot.setBackground(oval(statusColor(s)));
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(dp(8), dp(8));
        dlp.rightMargin = dp(8);
        r1.addView(dot, dlp);
        TextView name = new TextView(a);
        String nick = s.optString("nickname", "");
        name.setText(TextUtils.isEmpty(nick) ? "未命名" : nick);
        name.setTextSize(14.5f);
        name.setTextColor(INK);
        name.setTypeface(Typeface.DEFAULT_BOLD);
        name.setSingleLine(true);
        name.setEllipsize(TextUtils.TruncateAt.END);
        r1.addView(name, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if ("global".equals(s.optString("realm"))) r1.addView(badge("国际", ACCENT));
        if (s.optBoolean("enterprise")) r1.addView(badge("企业", WARN));
        if (s.optBoolean("api_key")) r1.addView(badge("Key", INK3));
        String ov = s.optString("enterprise_override", "");
        if ("personal".equals(ov)) r1.addView(badge("个人*", OK));
        if ("enterprise".equals(ov)) r1.addView(badge("企业*", WARN));
        card.addView(r1);

        TextView sub = new TextView(a);
        sub.setText(uid + " · " + statusText(s));
        sub.setTextSize(11.5f);
        sub.setTextColor(INK3);
        sub.setPadding(0, dp(3), 0, 0);
        card.addView(sub);

        TextView meta = new TextView(a);
        String cred = s.has("credits") ? ("积分 " + s.optInt("credits")
                + (s.optInt("credits_total") > 0 ? "/" + s.optInt("credits_total") : "")) : "积分 未查询";
        meta.setText(cred + " · 成功 " + s.optInt("success_count") + " / 失败 "
                + s.optInt("err_total") + " · 在途 " + s.optInt("in_flight"));
        meta.setTextSize(12f);
        meta.setTextColor(INK2);
        meta.setPadding(0, dp(4), 0, 0);
        card.addView(meta);

        // 操作行：可横滑，按钮保持自然宽度（手机上一行放不下 7 个）
        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        LinearLayout acts = new LinearLayout(a);
        acts.setOrientation(LinearLayout.HORIZONTAL);
        acts.setPadding(0, dp(8), 0, 0);
        hs.addView(acts);
        card.addView(hs);

        boolean personal = !s.optBoolean("enterprise") && !s.optBoolean("api_key");
        if (personal) {
            acts.addView(actionBtn("签到", uid, "/checkin", null));
        }
        acts.addView(actionBtn("余额", uid, "/balance", null));
        if (personal) acts.addView(taskBtn(uid));
        if (s.optBoolean("paused")) acts.addView(actionBtn("恢复选号", uid, "/resume", null));
        else acts.addView(actionBtn("暂停选号", uid, "/pause", null));
        acts.addView(moreBtn(s));
        return card;
    }

    private View actionBtn(String label, final String uid, final String path, final JSONObject body) {
        Button b = btn(label, false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                final Button me = (Button) v;
                me.setEnabled(false);
                PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + path, body, new PanelApi.Cb() {
                    public void ok(JSONObject o) {
                        me.setEnabled(true);
                        toast(label + "：完成" + (o.has("message") ? "（" + o.optString("message") + "）" : ""));
                        loadOverview(false);
                    }
                    public void fail(String msg) {
                        me.setEnabled(true);
                        toast(label + "失败：" + msg);
                    }
                });
            }
        });
        return b;
    }

    private View moreBtn(final JSONObject s) {
        Button b = btn("更多", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { moreSheet(s); }
        });
        return b;
    }

    private void moreSheet(final JSONObject s) {
        final String uid = s.optString("uid");
        String[] items = new String[]{
                s.optBoolean("paused") ? "恢复选号" : "暂停选号",
                s.optBoolean("disabled") ? "启用账号" : "禁用账号",
                "账号类型：" + typeLabel(s) + "（点击切换）",
                "移除账号（不可撤销）"
        };
        new AlertDialog.Builder(a)
                .setTitle(s.optString("nickname", uid))
                .setItems(items, new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int which) {
                        if (which == 0) {
                            hit(uid, s.optBoolean("paused") ? "/resume" : "/pause", null);
                        } else if (which == 1) {
                            // 被禁用的号用 revive 恢复（/resume 只解暂停，不解除禁用）
                            if (s.optBoolean("disabled")) hit(uid, "/revive", null);
                            else hit(uid, "/disable", null);
                        } else if (which == 2) {
                            cycleType(s);
                        } else {
                            confirmRemove(uid, s.optString("nickname", uid));
                        }
                    }
                }).show();
    }

    private String typeLabel(JSONObject s) {
        String o = s.optString("enterprise_override", "");
        if ("enterprise".equals(o)) return "强制企业版";
        if ("personal".equals(o)) return "强制个人版";
        return s.optBoolean("enterprise") ? "自动（企业）" : "自动（个人）";
    }

    private void cycleType(final JSONObject s) {
        String cur = s.optString("enterprise_override", "");
        String next = cur.isEmpty() ? "enterprise" : ("enterprise".equals(cur) ? "personal" : "");
        try {
            JSONObject body = new JSONObject();
            body.put("override", next);
            final String label = next.isEmpty() ? "自动判定" : ("enterprise".equals(next) ? "强制企业版" : "强制个人版");
            PanelApi.post(a, "/panel/api/accounts/" + enc(s.optString("uid")) + "/enterprise", body,
                    new PanelApi.Cb() {
                        public void ok(JSONObject o) { toast("账号类型：" + label); loadOverview(false); }
                        public void fail(String msg) { toast("切换失败：" + msg); }
                    });
        } catch (Throwable t) {
            toast("切换失败：" + t);
        }
    }

    private void confirmRemove(final String uid, String name) {
        new AlertDialog.Builder(a)
                .setTitle("移除账号")
                .setMessage("将删除该账号的本地凭据文件，且不可撤销。\n\n" + name + "（" + uid + "）")
                .setNegativeButton("取消", null)
                .setPositiveButton("移除", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) { hit(uid, "/remove", null); }
                }).show();
    }

    private void hit(final String uid, String path, JSONObject body) {
        PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + path, body, new PanelApi.Cb() {
            public void ok(JSONObject o) { toast("完成"); loadOverview(false); }
            public void fail(String msg) { toast("失败：" + msg); }
        });
    }

    private void reloadAccounts() {
        PanelApi.post(a, "/panel/api/accounts/reload", new JSONObject(), new PanelApi.Cb() {
            public void ok(JSONObject o) { toast("目录已重载"); loadOverview(false); }
            public void fail(String msg) { toast("重载失败：" + msg); }
        });
    }

    // ── 任务弹层 ───────────────────────────────────────────────────────────
    private View taskBtn(final String uid) {
        Button b = btn("任务", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { openTasks(uid); }
        });
        return b;
    }

    private void openTasks(final String uid) {
        PanelApi.get(a, "/panel/api/accounts/" + enc(uid) + "/tasks", new PanelApi.Cb() {
            public void ok(JSONObject o) { showTasks(uid, o); }
            public void fail(String msg) { dialogText("任务查询失败", msg); }
        });
    }

    private void showTasks(final String uid, JSONObject d) {
        final JSONObject caps = d.optJSONObject("capabilities");
        boolean canAccept = caps == null || caps.optBoolean("accept", true);
        boolean canClaim = caps == null || caps.optBoolean("claim", true);
        boolean canAuto = caps == null || caps.optBoolean("automate", true);

        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(14), dp(12), dp(14), dp(8));
        ScrollView sv = new ScrollView(a);
        sv.addView(box);

        String msg = d.optString("message", "");
        if (!msg.isEmpty()) {
            TextView m = new TextView(a);
            m.setText(msg);
            m.setTextSize(12f);
            m.setTextColor(WARN);
            m.setPadding(0, 0, 0, dp(8));
            box.addView(m);
        }
        JSONArray arr = d.optJSONArray("tasks");
        if (arr == null || arr.length() == 0) {
            box.addView(empty(msg.isEmpty() ? "该账号暂无任务" : msg));
        } else {
            for (int i = 0; i < arr.length(); i++) {
                final JSONObject t = arr.optJSONObject(i);
                if (t == null) continue;
                LinearLayout row = new LinearLayout(a);
                row.setOrientation(LinearLayout.VERTICAL);
                row.setPadding(0, dp(6), 0, dp(8));
                TextView title = new TextView(a);
                title.setText(t.optString("title", t.optString("task_code")));
                title.setTextSize(13.5f);
                title.setTextColor(INK);
                row.addView(title);
                int cur = t.optInt("current"), tgt = t.optInt("target");
                String state = t.optBoolean("claimed") ? "已领取"
                        : t.optBoolean("claimable") ? "可领取"
                        : t.optBoolean("locked") ? "未解锁"
                        : "accepted".equals(t.optString("accept_status")) ? "进行中" : "未接受";
                TextView meta = new TextView(a);
                meta.setText((tgt > 0 ? cur + " / " + tgt : "—") + " · " + state
                        + (t.optInt("credit") > 0 ? " · +" + t.optInt("credit") + " 分" : ""));
                meta.setTextSize(11.5f);
                meta.setTextColor(INK3);
                row.addView(meta);

                if (!t.optBoolean("claimed") && !t.optBoolean("locked")) {
                    LinearLayout bar = new LinearLayout(a);
                    bar.setOrientation(LinearLayout.HORIZONTAL);
                    if (t.optBoolean("claimable") && canClaim) {
                        bar.addView(taskAction("领取", uid, "/tasks/claim", t.optString("task_code")));
                    } else if (!canClaim && !canAuto) {
                        TextView ro = new TextView(a);
                        ro.setText("只读");
                        ro.setTextSize(11.5f);
                        ro.setTextColor(INK3);
                        bar.addView(ro);
                    } else if (canAccept && !"accepted".equals(t.optString("accept_status"))) {
                        bar.addView(taskAction("接受", uid, "/tasks/accept", t.optString("task_code")));
                    }
                    if (bar.getChildCount() > 0) {
                        LinearLayout.LayoutParams blp = new LinearLayout.LayoutParams(
                                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
                        blp.topMargin = dp(5);
                        row.addView(bar, blp);
                    }
                }
                box.addView(row);
            }
        }
        AlertDialog.Builder bld = new AlertDialog.Builder(a)
                .setTitle("积分任务")
                .setView(sv)
                .setNegativeButton("关闭", null);
        if (canAccept) {
            bld.setNeutralButton("全部接受", new android.content.DialogInterface.OnClickListener() {
                public void onClick(android.content.DialogInterface d2, int w) { acceptAll(uid); }
            });
        }
        if (canAuto) {
            bld.setPositiveButton("一键完成全部", new android.content.DialogInterface.OnClickListener() {
                public void onClick(android.content.DialogInterface d2, int w) { autoAll(uid); }
            });
        }
        bld.show();
    }

    private View taskAction(String label, final String uid, final String path, final String code) {
        Button b = btn(label, true);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(final View v) {
                v.setEnabled(false);
                try {
                    JSONObject body = new JSONObject();
                    if (path.endsWith("/accept")) {
                        JSONArray codes = new JSONArray();
                        codes.put(code);
                        body.put("task_codes", codes);
                    } else {
                        body.put("task_code", code);
                    }
                    PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + path, body, new PanelApi.Cb() {
                        public void ok(JSONObject o) { toast(label + "：完成"); v.setEnabled(true); }
                        public void fail(String msg) { toast(label + "失败：" + msg); v.setEnabled(true); }
                    });
                } catch (Throwable t) {
                    toast("请求构造失败：" + t);
                    v.setEnabled(true);
                }
            }
        });
        return b;
    }

    private void acceptAll(final String uid) {
        PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/tasks/accept_all", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                toast("已接受 " + o.optInt("accepted") + " 个任务");
            }
            public void fail(String msg) { toast("全部接受失败：" + msg); }
        });
    }

    private void autoAll(final String uid) {
        toast("开始执行可自动任务（含真实对话，可能 1–2 分钟）…");
        PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/tasks/auto_all", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONArray rs = o.optJSONArray("results");
                int ok = 0, skip = 0, err = 0;
                for (int i = 0; rs != null && i < rs.length(); i++) {
                    String st = rs.optJSONObject(i) == null ? "" : rs.optJSONObject(i).optString("status");
                    if ("done".equals(st)) ok++;
                    else if ("skipped".equals(st)) skip++;
                    else err++;
                }
                toast("执行完成：成功 " + ok + "，跳过 " + skip + "，失败 " + err);
            }
            public void fail(String msg) { toast("一键完成失败：" + msg); }
        });
    }

    // ── 日志 ───────────────────────────────────────────────────────────────
    private void buildLogs() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackgroundColor(BG);

        LinearLayout bar = new LinearLayout(a);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setPadding(dp(10), dp(8), dp(10), dp(6));
        bar.addView(chip("全部", "all"));
        bar.addView(chip("任务", "task"));
        bar.addView(chip("对话", "chat"));
        bar.addView(chip("系统", "sys"));
        bar.addView(spacerH());
        Button auto = btn(logAuto ? "自动刷新：开" : "自动刷新：关", false);
        auto.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                logAuto = !logAuto;
                buildLogs();
            }
        });
        bar.addView(auto);
        box.addView(bar);

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

    private View chip(String label, final String ch) {
        TextView t = new TextView(a);
        t.setText(label);
        t.setTextSize(12f);
        t.setPadding(dp(10), dp(6), dp(10), dp(6));
        t.setTextColor(ch.equals(logCh) ? ACCENT : INK3);
        t.setBackground(card());
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { logCh = ch; buildLogs(); }
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = dp(6);
        t.setLayoutParams(lp);
        return t;
    }

    /** 日志自动刷新：单循环（切换频道/开关不会叠加出多个 3 秒定时器）。 */
    private boolean logLoop;
    private void startLogLoop() {
        if (logLoop) return;
        logLoop = true;
        ui.postDelayed(new Runnable() {
            public void run() {
                if (!"logs".equals(page)) { logLoop = false; return; }
                if (logAuto) loadLogs();
                ui.postDelayed(this, 3000);
            }
        }, 3000);
    }

    private void loadLogs() {
        PanelApi.get(a, "/panel/api/logs", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                TextView tv = (TextView) pageBox.findViewById(android.R.id.text1);
                if (tv == null) return;
                JSONArray arr = o.optJSONArray("entries");
                StringBuilder sb = new StringBuilder();
                int start = arr == null ? 0 : Math.max(0, arr.length() - 400);
                for (int i = start; arr != null && i < arr.length(); i++) {
                    JSONObject e = arr.optJSONObject(i);
                    if (e == null) continue;
                    if (!"all".equals(logCh) && !logCh.equals(e.optString("ch"))) continue;
                    String ts = e.optString("ts", "");
                    int t = ts.indexOf('T');
                    if (t > 0 && ts.length() > t + 9) ts = ts.substring(t + 1, t + 9);
                    sb.append(ts).append(' ').append(e.optString("text")).append('\n');
                }
                tv.setText(sb.length() == 0 ? "（暂无日志）" : sb.toString());
            }
            public void fail(String msg) {
                TextView tv = (TextView) pageBox.findViewById(android.R.id.text1);
                if (tv != null) tv.setText("日志加载失败：" + msg);
            }
        });
    }

    // ── 设置 ───────────────────────────────────────────────────────────────
    private void buildSettings() {
        PanelApi.get(a, "/panel/api/config", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONObject cfg = o.optJSONObject("config");
                if (cfg == null) { dialogText("设置", "配置接口不可用"); return; }
                renderSettings(cfg);
            }
            public void fail(String msg) { dialogText("设置", "读取配置失败：" + msg); }
        });
    }

    private void renderSettings(final JSONObject cfg) {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(12), dp(12), dp(12), dp(16));

        LinearLayout sched = section("定时任务");
        addSwitch(sched, cfg, "schedule", "checkin_enabled", "每日签到");
        addSwitch(sched, cfg, "schedule", "travel_enabled", "猫猫旅行");
        addSwitch(sched, cfg, "schedule", "activity_enabled", "活跃上报");
        addSwitch(sched, cfg, "schedule", "web_activity_enabled", "国际版网页活跃腿");
        addSwitch(sched, cfg, "schedule", "keepalive_enabled", "保活");
        box.addView(sched);

        LinearLayout pool = section("选号与日志");
        final Spinner sp = new Spinner(a);
        String[] opts = new String[]{"weighted（加权随机）", "round_robin（轮询）", "sequential（顺序耗尽）"};
        sp.setAdapter(new ArrayAdapter<String>(a, android.R.layout.simple_spinner_dropdown_item, opts));
        JSONObject p = cfg.optJSONObject("pool");
        String cur = p == null ? "weighted" : p.optString("selection_strategy", "weighted");
        sp.setSelection("round_robin".equals(cur) ? 1 : ("sequential".equals(cur) ? 2 : 0));
        pool.addView(label("选号策略"));
        pool.addView(sp);
        addSwitch(pool, cfg, "logging", "panel_log_enabled", "运行日志落盘");
        box.addView(pool);

        Button save = btn("保存配置", true);
        save.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { saveConfig(cfg, sp.getSelectedItemPosition()); }
        });
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(14);
        box.addView(save, lp);

        TextView note = new TextView(a);
        note.setText("只覆盖这里显示的字段，其余配置原样保留。改动按网关的热生效规则执行，"
                + "个别字段（listen / data 目录等）仍需重启网关。");
        note.setTextSize(11.5f);
        note.setTextColor(INK3);
        note.setPadding(0, dp(10), 0, 0);
        box.addView(note);

        sv.addView(box);
        pageBox.removeAllViews();
        pageBox.addView(sv);
    }

    private LinearLayout section(String title) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackground(card());
        box.setPadding(dp(14), dp(12), dp(14), dp(12));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(10);
        box.setLayoutParams(lp);
        TextView t = new TextView(a);
        t.setText(title);
        t.setTextSize(13f);
        t.setTextColor(INK2);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setPadding(0, 0, 0, dp(8));
        box.addView(t);
        return box;
    }

    private void addSwitch(LinearLayout parent, final JSONObject cfg, final String sec,
                           final String key, String label) {
        JSONObject o = cfg.optJSONObject(sec);
        // 键缺席时按网关的**配置缺省**显示，而不是一律 false：
        // checkin/travel/activity/keepalive/panel_log 缺省都是开，显示成关会让人以为被关了。
        boolean dft = !"web_activity_enabled".equals(key);
        boolean on = o != null ? o.optBoolean(key, dft) : dft;
        Switch sw = new Switch(a);
        sw.setText(label);
        sw.setTextSize(13.5f);
        sw.setTextColor(INK);
        sw.setChecked(on);
        sw.setOnCheckedChangeListener(new CompoundButton.OnCheckedChangeListener() {
            public void onCheckedChanged(CompoundButton b, boolean checked) {
                try {
                    JSONObject o = cfg.optJSONObject(sec);
                    if (o == null) { o = new JSONObject(); cfg.put(sec, o); }
                    o.put(key, checked);
                } catch (Throwable ignored) {}
            }
        });
        parent.addView(sw);
    }

    private void saveConfig(final JSONObject cfg, int strategyIdx) {
        try {
            JSONObject p = cfg.optJSONObject("pool");
            if (p == null) { p = new JSONObject(); cfg.put("pool", p); }
            p.put("selection_strategy", strategyIdx == 1 ? "round_robin" : (strategyIdx == 2 ? "sequential" : "weighted"));
            PanelApi.post(a, "/panel/api/config", cfg, new PanelApi.Cb() {
                public void ok(JSONObject o) {
                    JSONArray need = o.optJSONArray("restart_required");
                    String extra = need == null || need.length() == 0 ? ""
                            : "（需重启：" + need.toString() + "）";
                    toast("配置已保存" + extra);
                }
                public void fail(String msg) { toast("保存失败：" + msg); }
            });
        } catch (Throwable t) {
            toast("保存失败：" + t);
        }
    }

    // ── 数据加载 ───────────────────────────────────────────────────────────
    private void loadOverview(final boolean quiet) {
        PanelApi.get(a, "/panel/api/overview", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                overviewCache = o;
                if ("overview".equals(page)) renderOverview(o);
                else if ("accounts".equals(page)) renderAccounts(o);
            }
            public void fail(String msg) {
                if (!quiet) toast("加载失败：" + msg);
            }
        });
    }

    // ── 小工具 ─────────────────────────────────────────────────────────────
    private int statusColor(JSONObject s) {
        if (s.optBoolean("disabled")) return BAD;
        if (s.optBoolean("paused") || s.optBoolean("cooling")) return WARN;
        return OK;
    }

    private String statusText(JSONObject s) {
        if (s.optBoolean("disabled")) return "已禁用";
        if (s.optBoolean("paused")) return "已暂停选号";
        if (s.optBoolean("cooling")) return "冷却中";
        return "可用";
    }

    private TextView label(String text) {
        TextView t = new TextView(a);
        t.setText(text);
        t.setTextSize(12.5f);
        t.setTextColor(INK3);
        t.setPadding(0, dp(6), 0, dp(2));
        return t;
    }

    private TextView kv(String k, String v) {
        TextView t = new TextView(a);
        t.setText(k + "：" + v);
        t.setTextSize(12.5f);
        t.setTextColor(INK2);
        t.setPadding(0, dp(2), 0, dp(2));
        return t;
    }

    private TextView empty(String msg) {
        TextView t = new TextView(a);
        t.setText(msg);
        t.setTextSize(12.5f);
        t.setTextColor(INK3);
        t.setPadding(dp(6), dp(24), dp(6), dp(24));
        t.setGravity(Gravity.CENTER);
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

    private GradientDrawable card() {
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

    private View spacerH() {
        return new View(a) {{ setLayoutParams(new LinearLayout.LayoutParams(0, 1, 1f)); }};
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

    private static String enc(String s) {
        try { return java.net.URLEncoder.encode(s, "UTF-8"); } catch (Throwable t) { return s; }
    }
}
