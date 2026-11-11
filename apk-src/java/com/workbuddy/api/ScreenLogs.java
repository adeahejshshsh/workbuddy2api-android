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
 * 日志（分区：运行日志（频道筛选/自动刷新/贴最新）与请求日志（详情/清空归档））。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenLogs {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;
    private String logMode = "run", logCh = "all";
    private boolean logAuto = true, logLoop;
    /** 日志是否「跟随最新」：贴在最新一端时不打断，用户手动上滑后停止跟随。 */
    private boolean logPinned = true;
    /** 最新一排在最上面（true）还是最下面（false，默认，与原行为一致）。 */
    private boolean logNewestFirst = false;

    ScreenLogs(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderLogs(); }

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
            // 右侧：自动刷新 / 排序 / 一键到底。频道 + 这组按钮在窄屏会超宽，整组可横滑。
            HorizontalScrollView chipScroll = new HorizontalScrollView(a);
            chipScroll.setHorizontalScrollBarEnabled(false);
            chipScroll.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
            LinearLayout chipRow = new LinearLayout(a);
            chipRow.setOrientation(LinearLayout.HORIZONTAL);
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
                chipRow.addView(p, lp);
            }
            final TextView auto = Theme.pill(a, logAuto ? "自动" : "暂停", logAuto);
            auto.setTextSize(11.5f);
            auto.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { logAuto = !logAuto; renderLogs(); }
            });
            LinearLayout.LayoutParams alp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            alp.leftMargin = Theme.dp(a, Theme.S2);
            alp.rightMargin = Theme.dp(a, Theme.S1);
            chipRow.addView(auto, alp);

            final TextView order = Theme.pill(a, logNewestFirst ? "最新在上" : "最新在下", false);
            order.setTextSize(11.5f);
            order.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    logNewestFirst = !logNewestFirst;
                    // 换端后自动贴到「最新」那一端，省得手滑
                    logPinned = true;
                    renderLogs();
                }
            });
            LinearLayout.LayoutParams olp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            olp.rightMargin = Theme.dp(a, Theme.S1);
            chipRow.addView(order, olp);

            // 「最新」比「到底」准确：切到「最新在上」时它跳的是顶端
            final TextView tail = Theme.pill(a, "最新", true);
            tail.setTextSize(11.5f);
            tail.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    logPinned = true;
                    scrollLogToNewest();
                }
            });
            chipRow.addView(tail);
            chipScroll.addView(chipRow);
            col.addView(chipScroll);

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

            // 日志页显示右侧滚动条：几千行时要能看出当前位置
            ScrollView sv = scrollOf(col, true);
            show(sv);
            watchLogScroll(sv);
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

            final LinearLayout list = Theme.list(a);
            list.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
            col.addView(list);
            list.addView(Theme.empty(a, "加载中…"));
            show(scrollOf(col, true));
            svc.requestLogs(new PanelService.Cb<List<PanelService.ReqEvent>>() {
                public void ok(List<PanelService.ReqEvent> rows) { bodyRequests(list, rows); }
                public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
            });
        }
    }

    private void loadLogs(final TextView tv) {
        svc.logs(new PanelService.Cb<List<PanelService.LogRow>>() {
            public void ok(List<PanelService.LogRow> rows) {
                // 先收集成行再决定顺序：最新在下（默认，等同于终端输出）或最新在上。
                List<String> lines = new java.util.ArrayList<String>();
                int start = Math.max(0, rows.size() - 400);
                for (int i = start; i < rows.size(); i++) {
                    PanelService.LogRow r = rows.get(i);
                    if (!"all".equals(logCh) && !logCh.equals(r.channel)) continue;
                    String ts = r.time;
                    int t = ts.indexOf('T');
                    if (t > 0 && ts.length() > t + 9) ts = ts.substring(t + 1, t + 9);
                    lines.add(ts + "  " + r.text);
                }
                if (logNewestFirst) java.util.Collections.reverse(lines);
                StringBuilder sb = new StringBuilder();
                for (String line : lines) sb.append(line).append('\n');
                tv.setText(lines.isEmpty() ? "（暂无日志）" : sb.toString());
                if (logPinned) scrollLogToNewest();
            }
            public void fail(String msg) { tv.setText("日志加载失败：" + msg); }
        });
    }

    /** 滚到「最新」那一端：最新在下 → 到底；最新在上 → 到顶。 */
    private void scrollLogToNewest() {
        final ScrollView sv = currentScroll();
        if (sv == null) return;
        sv.post(new Runnable() {
            public void run() {
                if (logNewestFirst) sv.fullScroll(View.FOCUS_UP);
                else sv.fullScroll(View.FOCUS_DOWN);
            }
        });
    }

    /**
     * 跟随最新：用户手动滑离最新一端就停止跟随（否则刷一次把人顶回去），
     * 滑回端头再自动恢复。用 ViewTreeObserver 监听（兼容 minSdk 21）。
     */
    private void watchLogScroll(final ScrollView sv) {
        sv.getViewTreeObserver().addOnScrollChangedListener(
                new android.view.ViewTreeObserver.OnScrollChangedListener() {
                    public void onScrollChanged() {
                        if (!"logs".equals(c.screen) || !"run".equals(logMode)) return;
                        View child = sv.getChildAt(0);
                        if (child == null) return;
                        int max = Math.max(0, child.getHeight() - sv.getHeight());
                        if (logNewestFirst) logPinned = sv.getScrollY() <= Theme.dp(a, 24);
                        else logPinned = sv.getScrollY() >= max - Theme.dp(a, 24);
                    }
                });
    }

    private void startLogLoop() {
        if (logLoop) return;
        logLoop = true;
        ui.postDelayed(new Runnable() {
            public void run() {
                if (!"logs".equals(c.screen) || !"run".equals(logMode)) { logLoop = false; return; }
                if (logAuto) {
                    View v = c.findLogView();
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
        Sheet.custom(a, "请求详情", col, "关闭", null);
    }

    private void confirmClear() {
        Sheet.confirm(a, "清空请求归档",
                "删除磁盘上的 requests-*.jsonl，不可恢复。\n运行日志（内存）不受影响。",
                "清空", true, new Runnable() {
                    public void run() {
                        svc.clearArchive(new PanelService.Cb<String>() {
                            public void ok(String m) { toast("已清空"); renderLogs(); }
                            public void fail(String msg) { toast("失败：" + msg); }
                        });
                    }
                });
    }

    private View space() {
        View v = new View(a);
        v.setLayoutParams(new LinearLayout.LayoutParams(0, 1, 1f));
        return v;
    }
}
