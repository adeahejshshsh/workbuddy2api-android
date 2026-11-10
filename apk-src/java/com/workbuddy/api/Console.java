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
    private String logMode = "run";   // run = 运行日志；req = 请求日志
    private String logCh = "all";
    private JSONObject overviewCache;
    /** 当前页的滚动容器：动作后重新渲染时用它把滚动位置接回去（否则每次刷新都弹回顶部）。 */
    private ScrollView pageScroll;
    /**
     * 冷启动重试计数：App 拉起来时 Go 网关可能还没开始监听（实测 1–3 秒，低端机更久）。
     * 这时任何 /panel/api 请求都是 connection refused —— 直接弹「加载失败」是在怪用户
     * 手快，而不是在等网关。这里改成：连接类错误一律视为「启动中」，自动重试（2s 一次），
     * 超过 60 秒才如实报错。
     */
    private int startRetries;
    private static final int MAX_START_RETRIES = 30;

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
        // 6 个 tab 在 360px 屏上排不下：放进可横滑容器，每项固定最小宽度
        HorizontalScrollView tabScroll = new HorizontalScrollView(a);
        tabScroll.setHorizontalScrollBarEnabled(false);
        tabScroll.setBackgroundColor(SURFACE);
        tabScroll.addView(tabs);
        root.addView(tabScroll, new LinearLayout.LayoutParams(
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
        addTab("usage", "用量");
        addTab("models", "模型");
        addTab("data", "数据");
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
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        t.setMinWidth(dp(64));
        tabs.addView(t, lp);
    }

    void show(String id) {
        page = id;
        buildTabs();
        pageBox.removeAllViews();
        if ("overview".equals(id)) buildOverview();
        else if ("usage".equals(id)) buildUsage();
        else if ("models".equals(id)) buildModels();
        else if ("data".equals(id)) buildData();
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
        pageScroll = sv;
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

        restoreInto(sv);
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

    // ── 用量（web 面板「用量」页）──────────────────────────────────────────
    private int usageHours = 72;

    private void buildUsage() {
        pageBox.removeAllViews();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), dp(8), dp(10), dp(4));
        int[] hours = new int[]{24, 72, 168, 720};
        String[] labels = new String[]{"近 24 小时", "近 3 天", "近 7 天", "近 30 天"};
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

        PanelApi.get(a, "/panel/api/usage?hours=" + usageHours, new PanelApi.Cb() {
            public void ok(JSONObject o) { renderUsage(list, o); }
            public void fail(String msg) {
                list.removeAllViews();
                list.addView(empty("用量加载失败：" + msg));
            }
        });
    }

    private void renderUsage(LinearLayout list, JSONObject o) {
        list.removeAllViews();
        JSONObject t = o.optJSONObject("totals");
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackground(card());
        card.setPadding(dp(13), dp(11), dp(13), dp(11));
        list.addView(card);

        String span = o.optString("window_from", "") + " ~ " + o.optString("window_to", "");
        card.addView(kv("统计区间", span.trim()));
        if (t != null) {
            card.addView(kv("请求", t.optInt("requests") + "（失败 " + t.optInt("errors") + "）"));
            card.addView(kv("Token", "总 " + t.optInt("total_tokens")
                    + " · 入 " + t.optInt("prompt_tokens") + " · 出 " + t.optInt("completion_tokens")));
            if (t.optDouble("cache_hit_rate", 0) > 0) {
                card.addView(kv("前缀缓存命中率",
                        String.format(java.util.Locale.US, "%.1f%%", t.optDouble("cache_hit_rate") * 100)));
            }
            card.addView(kv("平均延迟", String.format(java.util.Locale.US, "%.0f ms", t.optDouble("avg_latency_ms"))));
            card.addView(kv("平均吐字", String.format(java.util.Locale.US, "%.1f tok/s", t.optDouble("avg_tokens_per_second"))));
            if (t.optDouble("credits", 0) > 0) {
                card.addView(kv("积分消耗", String.format(java.util.Locale.US, "%.2f c", t.optDouble("credits"))));
            }
            if (t.optDouble("credits_per_1m_tokens", 0) > 0) {
                card.addView(kv("每百万 token 成本",
                        String.format(java.util.Locale.US, "%.2f c", t.optDouble("credits_per_1m_tokens"))));
            }
        }

        addAggSection(list, "按账号", o.optJSONArray("by_account"), true);
        addAggSection(list, "按模型", o.optJSONArray("by_model"), false);
    }

    /** 用量分组行：key/extra + 请求/token/积分。最多 20 行，避免一屏几千行。 */
    private void addAggSection(LinearLayout list, String title, JSONArray arr, boolean byAccount) {
        if (arr == null || arr.length() == 0) return;
        TextView h = new TextView(a);
        h.setText(title);
        h.setTextSize(12.5f);
        h.setTextColor(INK3);
        h.setPadding(dp(2), dp(10), 0, dp(4));
        list.addView(h);
        int n = Math.min(arr.length(), 20);
        for (int i = 0; i < n; i++) {
            JSONObject r = arr.optJSONObject(i);
            if (r == null) continue;
            LinearLayout row = new LinearLayout(a);
            row.setOrientation(LinearLayout.HORIZONTAL);
            row.setPadding(dp(2), dp(5), dp(2), dp(5));
            TextView name = new TextView(a);
            String key = r.optString("key");
            String extra = r.optString("extra", "");
            name.setText((extra.isEmpty() ? key : extra) + (r.optString("realm", "").isEmpty() ? ""
                    : " · " + r.optString("realm")));
            name.setTextSize(12.5f);
            name.setTextColor(INK2);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            row.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            TextView val = new TextView(a);
            val.setText(r.optInt("requests") + " 次 · " + r.optInt("total_tokens") + " tok"
                    + (r.optDouble("credits", 0) > 0
                        ? String.format(java.util.Locale.US, " · %.2fc", r.optDouble("credits")) : ""));
            val.setTextSize(11.5f);
            val.setTextColor(INK3);
            row.addView(val);
            list.addView(row);
        }
    }

    // ── 模型与档位（web 面板「模型与档位」页）──────────────────────────────
    private void buildModels() {
        pageBox.removeAllViews();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;
        list.addView(empty("加载中…"));
        PanelApi.get(a, "/panel/api/models", new PanelApi.Cb() {
            public void ok(JSONObject o) { renderModels(list, o); }
            public void fail(String msg) {
                list.removeAllViews();
                list.addView(empty("模型列表加载失败：" + msg));
            }
        });
    }

    private void renderModels(LinearLayout list, JSONObject o) {
        list.removeAllViews();
        JSONArray arr = o.optJSONArray("models");
        if (arr == null || arr.length() == 0) {
            list.addView(empty("没有可用模型（检查账号可用性或上游目录）"));
            return;
        }
        for (int i = 0; i < arr.length(); i++) {
            JSONObject m = arr.optJSONObject(i);
            if (m == null) continue;
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(card());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(7);
            card.setLayoutParams(lp);

            LinearLayout r1 = new LinearLayout(a);
            r1.setOrientation(LinearLayout.HORIZONTAL);
            r1.setGravity(Gravity.CENTER_VERTICAL);
            TextView name = new TextView(a);
            name.setText(m.optString("name", m.optString("id")));
            name.setTextSize(13.5f);
            name.setTextColor(INK);
            name.setTypeface(Typeface.DEFAULT_BOLD);
            name.setSingleLine(true);
            name.setEllipsize(TextUtils.TruncateAt.END);
            r1.addView(name, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            double credits = m.optDouble("credits", 0);
            double promo = m.optDouble("promo_credits", 0);
            TextView price = new TextView(a);
            if (promo > 0 && promo < credits) {
                price.setText(String.format(java.util.Locale.US, "%.2f c", promo));
                price.setTextColor(OK);
            } else {
                price.setText(credits > 0 ? String.format(java.util.Locale.US, "%.2f c", credits) : "免费");
                price.setTextColor(INK2);
            }
            price.setTextSize(12.5f);
            r1.addView(price);
            card.addView(r1);

            TextView meta = new TextView(a);
            StringBuilder sb = new StringBuilder(m.optString("id"));
            if (promo > 0 && promo < credits) {
                sb.append(" · 原价 ").append(String.format(java.util.Locale.US, "%.2f c", credits));
            }
            String tags = m.optString("tags", "");
            if (!tags.isEmpty()) sb.append(" · ").append(tags);
            sb.append(m.optBoolean("supports_reasoning") ? " · 思考" : "");
            sb.append(m.optBoolean("supports_images") ? " · 图片" : "");
            sb.append(m.optBoolean("supports_tool_call") ? " · 工具" : "");
            meta.setText(sb);
            meta.setTextSize(11f);
            meta.setTextColor(INK3);
            meta.setPadding(0, dp(3), 0, 0);
            card.addView(meta);
            list.addView(card);
        }
    }

    // ── 数据页：积分构成 / 余额趋势 ─────────────────────────────────────────
    private String dataMode = "packages"; // packages | balance
    private int balanceDays = 7;

    private void buildData() {
        pageBox.removeAllViews();
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
        box.addView(seg);

        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        list.setPadding(dp(10), dp(4), dp(10), dp(12));
        box.addView(list);
        pageBox.addView(box);
        pageScroll = null;

        if ("packages".equals(dataMode)) {
            list.addView(empty("加载中…（逐账号向上游查积分包，账号多时要几秒）"));
            PanelApi.get(a, "/panel/api/packages", new PanelApi.Cb() {
                public void ok(JSONObject o) { renderPackages(list, o); }
                public void fail(String msg) {
                    list.removeAllViews();
                    list.addView(empty("积分构成加载失败：" + msg));
                }
            });
        } else {
            buildBalanceControl(box, list);
            loadBalance(list);
        }
    }

    private void buildBalanceControl(LinearLayout box, LinearLayout list) {
        LinearLayout seg = new LinearLayout(a);
        seg.setOrientation(LinearLayout.HORIZONTAL);
        seg.setPadding(dp(10), 0, dp(10), dp(6));
        int[] days = new int[]{7, 30};
        String[] labels = new String[]{"近 7 天", "近 30 天"};
        for (int i = 0; i < days.length; i++) {
            final int d = days[i];
            seg.addView(chipSel(labels[i], d == balanceDays, new Runnable() {
                public void run() { balanceDays = d; buildData(); }
            }));
        }
        box.addView(seg);
    }

    private void loadBalance(final LinearLayout list) {
        list.addView(empty("加载中…"));
        PanelApi.get(a, "/panel/api/balance_history?days=" + balanceDays, new PanelApi.Cb() {
            public void ok(JSONObject o) { renderBalance(list, o); }
            public void fail(String msg) {
                list.removeAllViews();
                list.addView(empty("余额趋势加载失败：" + msg));
            }
        });
    }

    private void renderPackages(LinearLayout list, JSONObject o) {
        list.removeAllViews();
        JSONArray arr = o.optJSONArray("accounts");
        if (arr == null || arr.length() == 0) {
            list.addView(empty("没有账号"));
            return;
        }
        for (int i = 0; i < arr.length(); i++) {
            JSONObject r = arr.optJSONObject(i);
            if (r == null) continue;
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(card());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(8);
            card.setLayoutParams(lp);

            String nick = r.optString("nickname", "");
            TextView head = new TextView(a);
            head.setText((nick.isEmpty() ? r.optString("uid") : nick)
                    + "  " + r.optInt("remain") + " / " + r.optInt("size")
                    + ("global".equals(r.optString("realm")) ? "  · 国际" : ""));
            head.setTextSize(13.5f);
            head.setTextColor(INK);
            head.setTypeface(Typeface.DEFAULT_BOLD);
            card.addView(head);

            String plan = r.optString("plan", "");
            if (!plan.isEmpty() && !"unknown".equals(plan)) {
                TextView pt = new TextView(a);
                pt.setText("套餐：" + plan + (r.optString("plan_evidence", "").isEmpty() ? ""
                        : "（" + r.optString("plan_evidence") + "）"));
                pt.setTextSize(11f);
                pt.setTextColor(INK3);
                card.addView(pt);
            }
            if (!r.optString("error", "").isEmpty()) {
                TextView er = new TextView(a);
                er.setText("查询失败：" + r.optString("error"));
                er.setTextSize(11.5f);
                er.setTextColor(BAD);
                card.addView(er);
                list.addView(card);
                continue;
            }
            JSONArray ps = r.optJSONArray("packages");
            if (ps != null) {
                for (int j = 0; j < ps.length(); j++) {
                    JSONObject p = ps.optJSONObject(j);
                    if (p == null) continue;
                    String end = p.optString("end_time", "");
                    if (end.length() > 10) end = end.substring(0, 10);
                    TextView row = new TextView(a);
                    row.setText("· " + p.optString("name", "积分包") + "  "
                            + p.optInt("remain") + "/" + p.optInt("size")
                            + (end.isEmpty() ? "" : " 到期 " + end));
                    row.setTextSize(11.5f);
                    row.setTextColor(INK2);
                    row.setPadding(0, dp(2), 0, 0);
                    card.addView(row);
                }
            }
            list.addView(card);
        }
    }

    private void renderBalance(LinearLayout list, JSONObject o) {
        list.removeAllViews();
        JSONArray series = o.optJSONArray("series");
        if (series == null || series.length() == 0) {
            list.addView(empty("还没有余额采样（网关每 30 分钟采样一次）"));
            return;
        }
        for (int i = 0; i < series.length(); i++) {
            JSONObject item = series.optJSONObject(i);
            if (item == null) continue;
            LinearLayout card = new LinearLayout(a);
            card.setOrientation(LinearLayout.VERTICAL);
            card.setBackground(card());
            card.setPadding(dp(12), dp(9), dp(12), dp(9));
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.bottomMargin = dp(8);
            card.setLayoutParams(lp);

            // series 每项 = 一个账号：{uid,name,realm,points:[{t,c,d,cons,gain}]}
            String name = item.optString("name", item.optString("uid"));
            JSONArray pts = item.optJSONArray("points");
            float[] vals = new float[pts == null ? 0 : pts.length()];
            float min = Float.MAX_VALUE, max = -Float.MAX_VALUE;
            for (int j = 0; j < vals.length; j++) {
                JSONObject p = pts.optJSONObject(j);
                vals[j] = p == null ? 0 : (float) p.optDouble("c", 0);
                if (vals[j] < min) min = vals[j];
                if (vals[j] > max) max = vals[j];
            }
            TextView head = new TextView(a);
            int cur = item.optInt("current", vals.length == 0 ? 0 : (int) vals[vals.length - 1]);
            int chg = item.optInt("change", 0);
            head.setText(name + "   当前 " + cur
                    + "（窗口内 " + (chg >= 0 ? "+" : "") + chg + "）");
            head.setTextSize(13f);
            head.setTextColor(INK);
            card.addView(head);

            TextView meta = new TextView(a);
            meta.setText("区间 " + item.optInt("min", 0) + " ~ " + item.optInt("max", 0)
                    + " · " + vals.length + " 个采样"
                    + (item.optString("realm", "").isEmpty() ? "" : " · " + item.optString("realm")));
            meta.setTextSize(11f);
            meta.setTextColor(INK3);
            card.addView(meta);

            Sparkline sp = new Sparkline(a, ACCENT);
            sp.setValues(vals);
            LinearLayout.LayoutParams slp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, dp(56));
            slp.topMargin = dp(6);
            card.addView(sp, slp);
            list.addView(card);
        }
    }

    // ── 账号列表 ───────────────────────────────────────────────────────────
    private void buildAccounts() {
        ScrollView sv = new ScrollView(a);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(10), dp(10), dp(12));
        sv.addView(box);
        pageBox.addView(sv);
        pageScroll = sv;
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
        restoreInto(sv);
    }

    /**
     * 换页内容但保留滚动位置：动作（签到/余额/任务…）完成后会整页重渲染，
     * 不接回位置的话每点一次按钮都弹回最顶部 —— 账号多时得重新往下翻。
     */
    private void restoreInto(final ScrollView sv) {
        final int y = pageScroll == null ? 0 : pageScroll.getScrollY();
        pageBox.removeAllViews();
        pageBox.addView(sv);
        pageScroll = sv;
        if (y > 0) {
            sv.post(new Runnable() {
                public void run() { sv.scrollTo(0, y); }
            });
        }
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
            acts.addView(checkinBtn(uid));
        }
        acts.addView(balanceBtn(uid));
        if (personal) acts.addView(taskBtn(uid));
        if (s.optBoolean("paused")) acts.addView(actionBtn("恢复选号", uid, "/resume", null));
        else acts.addView(actionBtn("暂停选号", uid, "/pause", null));
        acts.addView(typeBtn(s));
        acts.addView(moreBtn(s));
        return card;
    }

    /** 签到：回包带 credits/credits_total 与 checkin_message，按「已签/跳过/完成」分档显示。 */
    private View checkinBtn(final String uid) {
        Button b = btn("签到", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(final View v) {
                v.setEnabled(false);
                PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/checkin", null, new PanelApi.Cb() {
                    public void ok(JSONObject o) {
                        v.setEnabled(true);
                        StringBuilder sb = new StringBuilder();
                        if (o.optBoolean("checkin_skipped")) sb.append("签到已跳过");
                        else if (o.optBoolean("checkin_done")) sb.append("签到完成");
                        else sb.append("签到未完成");
                        if (o.has("credits")) sb.append(" · 积分 ").append(o.optInt("credits"));
                        if (o.optInt("credits_total") > 0) sb.append("/").append(o.optInt("credits_total"));
                        if (o.has("checkin_message") && !o.optString("checkin_message").isEmpty())
                            sb.append("（").append(o.optString("checkin_message")).append("）");
                        if (o.has("balance_error")) sb.append(" · 余额查询失败：").append(o.optString("balance_error"));
                        toast(sb.toString());
                        loadOverview(false);
                    }
                    public void fail(String msg) {
                        v.setEnabled(true);
                        toast("签到失败：" + msg);
                    }
                });
            }
        });
        return b;
    }

    /** 余额：回包就是 credits / credits_total —— 此前只报「完成」，数值根本没显示。 */
    private View balanceBtn(final String uid) {
        Button b = btn("余额", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(final View v) {
                v.setEnabled(false);
                PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/balance", null, new PanelApi.Cb() {
                    public void ok(JSONObject o) {
                        v.setEnabled(true);
                        String msg = "余额 " + o.optInt("credits");
                        if (o.optInt("credits_total") > 0) msg += " / " + o.optInt("credits_total");
                        toast(msg);
                        loadOverview(false);
                    }
                    public void fail(String msg) {
                        v.setEnabled(true);
                        toast("余额刷新失败：" + msg);
                    }
                });
            }
        });
        return b;
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

    /** 账号类型：三选一（自动 / 强制企业 / 强制个人），比「点一下循环」明确得多。 */
    private View typeBtn(final JSONObject s) {
        Button b = btn("类型", false);
        b.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { typeSheet(s); }
        });
        return b;
    }

    private void typeSheet(final JSONObject s) {
        final String uid = s.optString("uid");
        final String[] values = new String[]{"", "enterprise", "personal"};
        final String cur = s.optString("enterprise_override", "");
        final String[] labels = new String[]{
                "自动判定" + (cur.isEmpty() ? "（当前）" : ""),
                "强制企业版" + ("enterprise".equals(cur) ? "（当前）" : ""),
                "强制个人版" + ("personal".equals(cur) ? "（当前）" : "")
        };
        int checked = "enterprise".equals(cur) ? 1 : ("personal".equals(cur) ? 2 : 0);
        new AlertDialog.Builder(a)
                .setTitle("账号类型")
                .setSingleChoiceItems(labels, checked, new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int which) {
                        d.dismiss();
                        applyType(uid, values[which]);
                    }
                })
                .setNegativeButton("取消", null)
                .show();
    }

    private void applyType(final String uid, final String value) {
        try {
            JSONObject body = new JSONObject();
            body.put("override", value);
            PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/enterprise", body, new PanelApi.Cb() {
                public void ok(JSONObject o) {
                    String name = value.isEmpty() ? "自动判定"
                            : ("enterprise".equals(value) ? "强制企业版" : "强制个人版");
                    toast("账号类型：" + name + (o.optBoolean("changed") ? "" : "（判定结果未变）"));
                    loadOverview(false);
                }
                public void fail(String msg) { toast("切换失败：" + msg); }
            });
        } catch (Throwable t) {
            toast("切换失败：" + t);
        }
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
                "账号类型：" + typeLabel(s),
                "探活（只读确认是否已解封）",
                "备注",
                "分组",
                "清空该账号的任务跳过名单",
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
                            typeSheet(s);
                        } else if (which == 3) {
                            probeBan(uid);
                        } else if (which == 4) {
                            editText("备注", "只存本机（state.json），不影响上游", s.optString("remark", ""),
                                    new TextCommit() {
                                        public void onText(String text) { sendRemark(uid, text); }
                                    });
                        } else if (which == 5) {
                            editText("分组", "用于面板筛选，仅本机保存", s.optString("group", ""),
                                    new TextCommit() {
                                        public void onText(String text) { sendGroup(uid, text); }
                                    });
                        } else if (which == 6) {
                            clearSkip(uid);
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

    /** 探活：只读探测上游是否已解封（不改池状态），结论要说清下一步。 */
    private void probeBan(final String uid) {
        PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/probe_ban", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                String v = o.optString("verdict", "");
                String detail = o.optString("detail", "");
                String msg;
                if ("lifted".equals(v)) msg = "已解封，可以点「启用账号」恢复使用";
                else if ("still_banned".equals(v)) msg = "仍被封禁，继续等待（勿反复启用，可能加重风控）";
                else if ("blocked".equals(v)) msg = "会话有效但被风控拦截 —— 继续观察";
                else if ("session_dead".equals(v)) msg = "不是封禁问题：登录态失效，需要重新登录";
                else if ("inconclusive".equals(v)) msg = "探测无明确结论（网络/限流），稍后再试";
                else msg = "无法探测：" + v;
                toast(msg + (detail.isEmpty() ? "" : "（" + detail + "）"));
            }
            public void fail(String msg) { toast("探活失败：" + msg); }
        });
    }

    /** 简单文本输入对话框（备注 / 分组共用）。 */
    private interface TextCommit { void onText(String text); }

    private void editText(String title, String hint, String initial, final TextCommit cb) {
        final EditText et = new EditText(a);
        et.setText(initial == null ? "" : initial);
        et.setHint(hint);
        et.setTextSize(14f);
        et.setPadding(dp(14), dp(8), dp(14), dp(8));
        new AlertDialog.Builder(a)
                .setTitle(title)
                .setView(et)
                .setNegativeButton("取消", null)
                .setPositiveButton("保存", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) {
                        cb.onText(et.getText() == null ? "" : et.getText().toString().trim());
                    }
                }).show();
    }

    private void sendRemark(final String uid, String text) {
        try {
            JSONObject body = new JSONObject();
            body.put("remark", text);
            PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/remark", body, new PanelApi.Cb() {
                public void ok(JSONObject o) { toast("备注已保存"); loadOverview(false); }
                public void fail(String msg) { toast("保存失败：" + msg); }
            });
        } catch (Throwable t) { toast("保存失败：" + t); }
    }

    private void sendGroup(final String uid, String text) {
        try {
            JSONObject body = new JSONObject();
            body.put("group", text);
            PanelApi.post(a, "/panel/api/accounts/" + enc(uid) + "/group", body, new PanelApi.Cb() {
                public void ok(JSONObject o) { toast("分组已保存"); loadOverview(false); }
                public void fail(String msg) { toast("保存失败：" + msg); }
            });
        } catch (Throwable t) { toast("保存失败：" + t); }
    }

    /** 清空该账号的「反复无进展」跳过名单（服务端是全局表，按 uid 只清这一号）。 */
    private void clearSkip(final String uid) {
        try {
            JSONObject body = new JSONObject();
            body.put("clear", true);
            body.put("uid", uid);
            PanelApi.post(a, "/panel/api/tasks/skip", body, new PanelApi.Cb() {
                public void ok(JSONObject o) { toast("已清空跳过名单"); }
                public void fail(String msg) { toast("清空失败：" + msg); }
            });
        } catch (Throwable t) { toast("清空失败：" + t); }
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
        // 必须清空再建：此前直接 addView，切频道后新栏叠在旧栏上，看到的永远是
        // 第一次建出来的那份（高亮恒在「全部」）。
        pageBox.removeAllViews();
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackgroundColor(BG);

        // 一级切换：运行日志 / 请求日志（对应网页面板「日志」页的上下两块）
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
                public void onClick(View v) {
                    logAuto = !logAuto;
                    buildLogs();
                }
            });
            bar.addView(auto);
        } else {
            bar.addView(spacerH());
            Button reload = btn("刷新", true);
            reload.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { buildLogs(); }
            });
            bar.addView(reload);
        }
        box.addView(bar);

        if ("req".equals(logMode)) {
            box.addView(requestLogList());
            pageBox.addView(box);
            pageScroll = null;
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

    /** 一级分段（运行/请求）：选中态用实底，与二级 chips 区分开。 */
    /**
     * 请求日志（网页面板「日志」页上半部分）：/panel/api/request_logs 的最近 100 条。
     * 每行一张小卡：时间 / 结果 / 模型（降级后加 ⤳）/ 账号 / token / TTFB / credit。
     */
    private View requestLogList() {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(4), dp(10), dp(12));
        final LinearLayout list = new LinearLayout(a);
        list.setOrientation(LinearLayout.VERTICAL);
        box.addView(list);
        final TextView loading = empty("加载中…");
        list.addView(loading);

        PanelApi.get(a, "/panel/api/request_logs?limit=100", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                list.removeAllViews();
                JSONArray arr = o.optJSONArray("entries");
                if (arr == null || arr.length() == 0) {
                    list.addView(empty("暂无请求记录（检查 logging.request_archive_enabled 是否开启）"));
                    return;
                }
                for (int i = 0; i < arr.length(); i++) {
                    JSONObject e = arr.optJSONObject(i);
                    if (e != null) list.addView(requestRow(e));
                }
            }
            public void fail(String msg) {
                list.removeAllViews();
                list.addView(empty("请求日志加载失败：" + msg));
            }
        });
        return box;
    }

    private View requestRow(JSONObject e) {
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackground(card());
        card.setPadding(dp(11), dp(8), dp(11), dp(8));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(7);
        card.setLayoutParams(lp);

        int status = e.optInt("status");
        LinearLayout line1 = new LinearLayout(a);
        line1.setOrientation(LinearLayout.HORIZONTAL);
        line1.setGravity(Gravity.CENTER_VERTICAL);
        TextView st = new TextView(a);
        st.setText(String.valueOf(status));
        st.setTextSize(12.5f);
        st.setTypeface(Typeface.DEFAULT_BOLD);
        st.setTextColor(status >= 200 && status < 300 ? OK : (status >= 500 ? BAD : WARN));
        line1.addView(st);
        TextView model = new TextView(a);
        String m = e.optString("model", "-");
        String eff = e.optString("model_eff", "");
        if (!eff.isEmpty() && !eff.equals(m)) m = m + " ⤳ " + eff;
        model.setText(m);
        model.setTextSize(12.5f);
        model.setTextColor(INK);
        model.setPadding(dp(8), 0, 0, 0);
        model.setSingleLine(true);
        model.setEllipsize(TextUtils.TruncateAt.MIDDLE);
        line1.addView(model, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        TextView time = new TextView(a);
        String ts = e.optString("time", "");
        int t = ts.indexOf('T');
        time.setText(t > 0 && ts.length() > t + 9 ? ts.substring(t + 1, t + 9) : ts);
        time.setTextSize(11f);
        time.setTextColor(INK3);
        line1.addView(time);
        card.addView(line1);

        StringBuilder sb = new StringBuilder();
        String acc = e.optString("account", "");
        if (!acc.isEmpty()) sb.append(acc).append(" · ");
        sb.append("总 ").append(e.optInt("total_tokens")).append(" tok");
        if (e.optInt("ttfb_ms") > 0) sb.append(" · 首字 ").append(e.optInt("ttfb_ms")).append("ms");
        if (e.optInt("duration_ms") > 0) sb.append(" · 用时 ").append(e.optInt("duration_ms")).append("ms");
        if (e.optBoolean("credit_known")) sb.append(" · ").append(e.optDouble("credit", 0)).append("c");
        String outcome = e.optString("outcome", "");
        if (!outcome.isEmpty()) sb.append(" · ").append(outcome);
        TextView meta = new TextView(a);
        meta.setText(sb.toString());
        meta.setTextSize(11f);
        meta.setTextColor(INK3);
        meta.setPadding(0, dp(3), 0, 0);
        meta.setSingleLine(true);
        meta.setEllipsize(TextUtils.TruncateAt.END);
        card.addView(meta);
        card.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { showRequestDetail(e); }
        });
        return card;
    }

    /** 请求事件详情：把归档里的字段摊平展示（含可选的对话原文，可能很大，单独可滚动）。 */
    private void showRequestDetail(JSONObject e) {
        ScrollView sv = new ScrollView(a);
        TextView tv = new TextView(a);
        tv.setTextSize(12f);
        tv.setTextColor(INK2);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setTextIsSelectable(true);
        tv.setPadding(dp(14), dp(10), dp(14), dp(10));
        StringBuilder sb = new StringBuilder();
        sb.append("时间     ").append(e.optString("time")).append('\n');
        sb.append("请求 ID  ").append(e.optString("request_id")).append('\n');
        sb.append("路径     ").append(e.optString("path")).append('\n');
        sb.append("账号     ").append(e.optString("account")).append('\n');
        sb.append("模型     ").append(e.optString("model"));
        String eff = e.optString("model_eff", "");
        if (!eff.isEmpty() && !eff.equals(e.optString("model"))) sb.append(" ⤳ ").append(eff);
        sb.append('\n');
        sb.append("状态     ").append(e.optInt("status"))
                .append(" · ").append(e.optString("outcome")).append('\n');
        sb.append("耗时     ").append(e.optInt("duration_ms")).append("ms")
                .append(" · 首字 ").append(e.optInt("ttfb_ms")).append("ms\n");
        sb.append("Token    ").append(e.optInt("prompt_tokens")).append(" 入 / ")
                .append(e.optInt("completion_tokens")).append(" 出 / ")
                .append(e.optInt("total_tokens")).append(" 总\n");
        if (e.optBoolean("credit_known")) {
            sb.append("积分     ").append(e.optDouble("credit", 0)).append('\n');
        }
        if (e.optInt("cache_hit_tokens") > 0 || e.optInt("cache_miss_tokens") > 0) {
            sb.append("缓存     ").append(e.optInt("cache_hit_tokens")).append(" 命中 / ")
                    .append(e.optInt("cache_miss_tokens")).append(" 未命中\n");
        }
        if (e.optInt("tools_declared") > 0 || e.optInt("tool_calls") > 0) {
            sb.append("工具     ").append(e.optInt("tools_declared")).append(" 声明 / ")
                    .append(e.optInt("tool_calls")).append(" 调用\n");
        }
        if (!e.optString("client_ip", "").isEmpty()) {
            sb.append("来源     ").append(e.optString("client_ip"))
                    .append("  ").append(e.optString("user_agent", "")).append('\n');
        }
        Object ctx = e.opt("context");
        if (ctx != null) {
            sb.append("\n── 会话上下文 ──\n").append(String.valueOf(ctx));
        }
        tv.setText(sb.toString());
        sv.addView(tv);
        new AlertDialog.Builder(a)
                .setTitle("请求详情")
                .setView(sv)
                .setPositiveButton("关闭", null)
                .show();
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
        pageScroll = sv;
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
                startRetries = 0;
                overviewCache = o;
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

    /** 连接层错误（网关还没监听）与业务错误的区分：只有前者该被当成「启动中」。 */
    private static boolean isConnectErr(String msg) {
        if (msg == null) return false;
        String m = msg.toLowerCase();
        return m.contains("connect") || m.contains("refused") || m.contains("econnrefused")
                || m.contains("timeout") || m.contains("timed out") || m.contains("unreachable")
                || m.contains("sockettimeout") || m.contains("http 502") || m.contains("http 503");
    }

    /** 启动占位：不弹错、不空屏，明确告诉用户在等什么。 */
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
