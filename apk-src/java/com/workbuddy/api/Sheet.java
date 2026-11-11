package com.workbuddy.api;

import android.app.Activity;
import android.app.Dialog;
import android.graphics.Rect;
import android.graphics.drawable.ColorDrawable;
import android.graphics.drawable.GradientDrawable;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.view.WindowManager;
import android.widget.LinearLayout;
import android.widget.PopupWindow;
import android.widget.ScrollView;
import android.widget.TextView;

/**
 * 主题化底部面板（替代系统 AlertDialog 的列表框）。
 *
 * 为什么不用 AlertDialog.setItems：那是系统的灰底列表，和玻璃深色台完全两套材质 ——
 * 截图里就能看到：面板是浅灰方块、圆角与字体都不对，像是从别的应用里弹出来的。
 *
 * 这里统一：底部锚定 + 玻璃卡 + 抓取条 + Fira 字体 + 行分隔线 + 强调色选中 + 危险项红色，
 * 点遮罩关闭。四种形态：menu（列表）/ single（单选）/ confirm（确认）/ custom（自定义内容）。
 */
final class Sheet {

    interface Pick { void onPick(int index); }

    private interface Body { LinearLayout build(Activity a, Dialog d); }

    private Sheet() {}

    // ── 列表 ───────────────────────────────────────────────────────────────
    static Dialog menu(Activity a, String title, final String[] items, final int dangerIndex,
                       final Pick cb) {
        return show(a, title, new Body() {
            public LinearLayout build(Activity act, final Dialog d) {
                LinearLayout body = new LinearLayout(act);
                body.setOrientation(LinearLayout.VERTICAL);
                for (int i = 0; i < items.length; i++) {
                    final int idx = i;
                    body.addView(row(act, items[i], i == dangerIndex ? Theme.BAD : Theme.INK, null,
                            new Runnable() {
                                public void run() { d.dismiss(); cb.onPick(idx); }
                            }));
                    if (i < items.length - 1) body.addView(divider(act));
                }
                return body;
            }
        }, null, null);
    }

    /**
     * 下拉菜单：锚在触发控件下方（"更多"这类行内动作），同一套玻璃材质。
     *
     * 与底部面板的分工：锚定菜单用于「就地操作」（行内按钮），底部面板用于跨屏选择
     * （添加账号、登录、确认）。两者共用 row/divider，视觉一致。
     */
    static PopupWindow anchor(View anchorView, String title, final String[] items,
                              final int dangerIndex, final Pick cb) {
        final Activity a = (Activity) anchorView.getContext();

        // 内容卡（行）+ 外层滚动壳：条目多、屏幕放不下时必须能滑，不能截断。
        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        if (title != null && !title.isEmpty()) {
            TextView t = Theme.text(a, title, 12f, Theme.INK3, 2);
            t.setPadding(Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S2),
                    Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S1));
            card.addView(t);
        }
        final PopupWindow pw = new PopupWindow(a);
        for (int i = 0; i < items.length; i++) {
            final int idx = i;
            card.addView(row(a, items[i], i == dangerIndex ? Theme.BAD : Theme.INK, null,
                    new Runnable() {
                        public void run() { pw.dismiss(); cb.onPick(idx); }
                    }));
            if (i < items.length - 1) card.addView(divider(a));
        }

        ScrollView sc = new ScrollView(a);
        sc.setVerticalScrollBarEnabled(false);
        sc.setFillViewport(false);
        sc.addView(card);
        sc.setBackground(glassBg(a, 16));

        int margin = Theme.dp(a, Theme.S2);
        int screenW = a.getResources().getDisplayMetrics().widthPixels;
        int width = Math.min(Theme.dp(a, 236), screenW - margin * 2);
        pw.setContentView(sc);
        pw.setWidth(width);
        pw.setBackgroundDrawable(new ColorDrawable(0));
        pw.setOutsideTouchable(true);
        pw.setElevation(Theme.dp(a, 14));

        // 量一次内容高度：决定「向下弹还是向上弹」以及要不要限高滚动。
        int wSpec = View.MeasureSpec.makeMeasureSpec(width, View.MeasureSpec.EXACTLY);
        sc.measure(wSpec, View.MeasureSpec.makeMeasureSpec(0, View.MeasureSpec.UNSPECIFIED));
        int contentH = sc.getMeasuredHeight();

        Rect r = new Rect();
        boolean visible = anchorView.getGlobalVisibleRect(r);
        int screenH = anchorView.getRootView().getHeight();
        if (!visible || screenH <= 0) {
            pw.setHeight(ViewGroup.LayoutParams.WRAP_CONTENT);
            pw.showAsDropDown(anchorView, 0, margin, Gravity.END);
            return pw;
        }
        int below = screenH - r.bottom - margin * 2;
        int above = r.top - margin * 2;
        boolean up = contentH > below && above > below;
        int space = Math.max(Theme.dp(a, 132), up ? above : below);
        int height = Math.min(contentH, Math.min(space, (int) (screenH * 0.62f)));
        pw.setHeight(height);

        // 向上弹：yoff 取负（锚点高度 + 弹层高度 + 间距），否则会盖住锚点本身。
        int yoff = up ? -(r.height() + height + margin) : margin;
        pw.showAsDropDown(anchorView, 0, yoff, Gravity.END);
        return pw;
    }

    /** 下拉菜单底：同一套玻璃材质（采样极光 + 光边），不做真实模糊（PopupWindow 太碎）。 */
    private static android.graphics.drawable.Drawable glassBg(Activity a, float radiusDp) {
        return Theme.chrome(a, radiusDp, 0xB3101A2E, 0, false);
    }

    // ── 单选 ───────────────────────────────────────────────────────────────
    static Dialog single(Activity a, String title, final String[] items, final int checked,
                         final Pick cb) {
        return show(a, title, new Body() {
            public LinearLayout build(Activity act, final Dialog d) {
                LinearLayout body = new LinearLayout(act);
                body.setOrientation(LinearLayout.VERTICAL);
                for (int i = 0; i < items.length; i++) {
                    final int idx = i;
                    body.addView(row(act, items[i], Theme.INK, i == checked ? "✓" : null,
                            new Runnable() {
                                public void run() { d.dismiss(); cb.onPick(idx); }
                            }));
                    if (i < items.length - 1) body.addView(divider(act));
                }
                return body;
            }
        }, null, null);
    }

    // ── 确认 ───────────────────────────────────────────────────────────────
    static Dialog confirm(Activity a, String title, final String message, String okLabel,
                          boolean danger, final Runnable ok) {
        return show(a, title, new Body() {
            public LinearLayout build(Activity act, Dialog d) {
                LinearLayout body = new LinearLayout(act);
                body.setOrientation(LinearLayout.VERTICAL);
                TextView msg = Theme.text(act, message, 13f, Theme.INK3, 0);
                msg.setPadding(Theme.dp(act, Theme.S4), Theme.dp(act, Theme.S2),
                        Theme.dp(act, Theme.S4), Theme.dp(act, Theme.S3));
                body.addView(msg);
                return body;
            }
        }, okLabel, ok);
    }

    // ── 自定义内容 ─────────────────────────────────────────────────────────
    static Dialog custom(Activity a, String title, final View content, String okLabel,
                         final Runnable ok) {
        return show(a, title, new Body() {
            public LinearLayout build(Activity act, Dialog d) {
                LinearLayout body = new LinearLayout(act);
                body.setOrientation(LinearLayout.VERTICAL);
                body.addView(content, new LinearLayout.LayoutParams(
                        ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
                return body;
            }
        }, okLabel, ok);
    }

    // ── 骨架 ───────────────────────────────────────────────────────────────
    private static Dialog show(final Activity a, String title, Body bodyBuilder,
                               String okLabel, final Runnable ok) {
        final Dialog d = new Dialog(a, android.R.style.Theme_Translucent_NoTitleBar);
        Window w = d.getWindow();
        if (w != null) {
            w.setBackgroundDrawable(new ColorDrawable(0));
            w.addFlags(WindowManager.LayoutParams.FLAG_DIM_BEHIND);
            w.setDimAmount(0.66f);
            w.setGravity(Gravity.BOTTOM);
            w.setLayout(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        }

        LinearLayout root = new LinearLayout(a);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2), Theme.dp(a, Theme.S2));

        LinearLayout card = new LinearLayout(a);
        card.setOrientation(LinearLayout.VERTICAL);
        // 弹层玻璃：采样极光 + 真模糊(24dp) + 棱边折射。一屏同时只有导航条与它开
        // lens —— 玻璃是这个体系里最贵的东西（每帧采样 + 模糊 + shader）。
        card.setBackground(Theme.chrome(a, 22, 0xA6101A2E, 24, true));
        card.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));

        // 抓取条
        LinearLayout grabWrap = new LinearLayout(a);
        grabWrap.setGravity(Gravity.CENTER);
        grabWrap.setPadding(0, Theme.dp(a, Theme.S1), 0, Theme.dp(a, Theme.S2));
        View grab = new View(a);
        grab.setBackground(Theme.round(a, 0x55FFFFFF, Theme.R_PILL));
        grabWrap.addView(grab, new LinearLayout.LayoutParams(Theme.dp(a, 36), Theme.dp(a, 4)));
        card.addView(grabWrap);

        if (title != null && !title.isEmpty()) {
            TextView t = Theme.text(a, title, 17f, Theme.INK, 2);
            t.setPadding(Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S1),
                    Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2));
            card.addView(t);
        }

        final ScrollView bodyScroll = new ScrollView(a);
        bodyScroll.setVerticalScrollBarEnabled(false);
        bodyScroll.addView(bodyBuilder.build(a, d));
        card.addView(bodyScroll, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        bodyScroll.post(new Runnable() {
            public void run() {
                View child = bodyScroll.getChildAt(0);
                int max = (int) (a.getResources().getDisplayMetrics().heightPixels * 0.56f);
                if (child != null && child.getHeight() > max) {
                    ViewGroup.LayoutParams lp = bodyScroll.getLayoutParams();
                    lp.height = max;
                    bodyScroll.setLayoutParams(lp);
                }
            }
        });

        if (okLabel != null) {
            TextView okBtn = Theme.primaryButton(a, okLabel);
            LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
            lp.setMargins(Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2),
                    Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S1));
            card.addView(okBtn, lp);
            okBtn.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    d.dismiss();
                    if (ok != null) ok.run();
                }
            });
        }

        // 取消行
        TextView cancel = Theme.text(a, "取消", 13.5f, Theme.INK3, 1);
        cancel.setGravity(Gravity.CENTER);
        cancel.setPadding(0, Theme.dp(a, Theme.S3), 0, Theme.dp(a, Theme.S3));
        cancel.setBackground(Theme.ripple(a,
                Theme.outlined(a, 0x0DFFFFFF, Theme.R_CTRL, Theme.LINE, 1), 0x1FFFFFFF));
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        clp.setMargins(Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S1),
                Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S1));
        card.addView(cancel, clp);
        cancel.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { d.dismiss(); }
        });

        root.addView(card, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        d.setContentView(root);
        d.setCanceledOnTouchOutside(true);
        d.show();
        return d;
    }

    /** 一行：标题 +（可选）右侧勾选，点击后面板先关再回调。 */
    private static View row(final Activity a, String label, int color, String trailing,
                            final Runnable onClick) {
        final Dialog[] holder = new Dialog[1];
        LinearLayout line = new LinearLayout(a);
        line.setOrientation(LinearLayout.HORIZONTAL);
        line.setGravity(Gravity.CENTER_VERTICAL);
        line.setPadding(Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S3),
                Theme.dp(a, Theme.S3), Theme.dp(a, Theme.S3));
        line.setBackground(Theme.ripple(a, Theme.round(a, 0x00000000, 0), 0x22FFFFFF));
        TextView t = Theme.text(a, label, 15f, color, 0);
        line.addView(t, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if (trailing != null) line.addView(Theme.text(a, trailing, 15f, Theme.ACCENT, 2));
        line.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { onClick.run(); }
        });
        return line;
    }

    private static View divider(Activity a) {
        View v = new View(a);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, Math.max(1, Theme.dp(a, 0.6f)));
        v.setLayoutParams(lp);
        v.setBackgroundColor(0x22FFFFFF);
        return v;
    }
}
