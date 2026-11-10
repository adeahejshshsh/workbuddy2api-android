package com.workbuddy.api;

import android.content.Context;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.RippleDrawable;
import android.content.res.ColorStateList;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.TextView;

/**
 * 设计令牌与组件工厂（Glassmorphism / 深色数据台）。
 *
 * 令牌来自 ui-ux-pro-max 的 design-system 输出（产品：API 网关运维台）：
 *   背景 #0F172A · 卡片 #1B2336 · 前景 #F8FAFC · 次级文字 #94A3B8
 *   强调/主操作 #22C55E · 危险 #EF4444 · 边界 #475569(40%)
 *   字体 Fira Sans（正文）+ Fira Mono（数字/日志/ID）
 *   效果 半透明卡片 + 1px 冷色描边 + 柔和径向高光
 *
 * 全部界面文件只从这里取颜色/字号/圆角与控件，避免各页面各写一套（旧版的毛病）。
 */
final class Theme {

    // ── 颜色 ───────────────────────────────────────────────────────────────
    static final int BG = 0xFF0F172A;
    static final int BG_TOP = 0xFF141E36;
    static final int CARD = 0xF21B2336;      // 卡片略透明，叠在渐变上像磨砂
    static final int CARD_SOLID = 0xFF1B2336;
    static final int MUTED = 0xFF272F42;
    static final int INK = 0xFFF8FAFC;
    static final int INK2 = 0xFFA9B4C7;
    static final int INK3 = 0xFF94A3B8;
    static final int LINE = 0x66475569;
    static final int ACCENT = 0xFF22C55E;
    static final int ON_ACCENT = 0xFF07240F;
    static final int INFO = 0xFF38BDF8;
    static final int WARN = 0xFFF59E0B;
    static final int BAD = 0xFFEF4444;
    static final int ON_BAD = 0xFFFFFFFF;

    // ── 尺度 ───────────────────────────────────────────────────────────────
    static final int R_CARD = 18, R_CTRL = 14, R_PILL = 999;
    static final int S1 = 4, S2 = 8, S3 = 12, S4 = 16, S5 = 20, S6 = 28;

    private Theme() {}

    // ── 尺寸 ───────────────────────────────────────────────────────────────
    static int dp(Context c, float v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                c.getResources().getDisplayMetrics());
    }
    static int sp(Context c, float v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_SP, v,
                c.getResources().getDisplayMetrics());
    }

    // ── 字体 ───────────────────────────────────────────────────────────────
    private static Typeface sansR, sansM, sansSB, mono;

    static Typeface sans(Context c) { return sans(c, 0); }

    /** weight: 0 regular / 1 medium / 2 semibold / 3 bold */
    static Typeface sans(Context c, int weight) {
        try {
            if (weight >= 3) {
                if (sansSB == null) sansSB = Typeface.createFromAsset(c.getAssets(), "fonts/FiraSans-Bold.ttf");
                return sansSB;
            }
            if (weight == 2) {
                if (sansSB == null) sansSB = Typeface.createFromAsset(c.getAssets(), "fonts/FiraSans-SemiBold.ttf");
                return sansSB;
            }
            if (weight == 1) {
                if (sansM == null) sansM = Typeface.createFromAsset(c.getAssets(), "fonts/FiraSans-Medium.ttf");
                return sansM;
            }
            if (sansR == null) sansR = Typeface.createFromAsset(c.getAssets(), "fonts/FiraSans-Regular.ttf");
            return sansR;
        } catch (Throwable t) {
            return weight >= 2 ? Typeface.DEFAULT_BOLD : Typeface.DEFAULT;
        }
    }

    static Typeface mono(Context c) {
        try {
            if (mono == null) mono = Typeface.createFromAsset(c.getAssets(), "fonts/FiraMono-Regular.ttf");
            return mono;
        } catch (Throwable t) {
            return Typeface.MONOSPACE;
        }
    }

    // ── 文本 ───────────────────────────────────────────────────────────────
    static TextView text(Context c, String s, float sizeSp, int color, int weight) {
        TextView t = new TextView(c);
        t.setText(s);
        t.setTextSize(sizeSp);
        t.setTextColor(color);
        t.setTypeface(sans(c, weight));
        t.setLineSpacing(dp(c, 2), 1f);
        return t;
    }

    static TextView num(Context c, String s, float sizeSp, int color) {
        TextView t = new TextView(c);
        t.setText(s);
        t.setTextSize(sizeSp);
        t.setTextColor(color);
        t.setTypeface(mono(c));
        return t;
    }

    /** 全大写小标签（KPI 名称、分组标题）。 */
    static TextView label(Context c, String s) {
        TextView t = text(c, s.toUpperCase(), 10.5f, INK3, 2);
        t.setLetterSpacing(0.12f);
        return t;
    }

    // ── 形状 ───────────────────────────────────────────────────────────────
    static GradientDrawable round(Context c, int color, float radiusDp) {
        GradientDrawable g = new GradientDrawable();
        g.setColor(color);
        g.setCornerRadius(dp(c, radiusDp));
        return g;
    }

    static GradientDrawable outlined(Context c, int color, float radiusDp, int strokeColor, float strokeDp) {
        GradientDrawable g = round(c, color, radiusDp);
        g.setStroke(dp(c, strokeDp), strokeColor);
        return g;
    }

    /** 玻璃卡：半透明填充 + 1px 冷色描边 + 大圆角。 */
    static GradientDrawable glass(Context c, float radiusDp) {
        return outlined(c, CARD, radiusDp, LINE, 1);
    }

    /** 顶部微光背景（从略亮的蓝黑渐变到背景色）。 */
    static GradientDrawable backdrop(Context c) {
        GradientDrawable g = new GradientDrawable(GradientDrawable.Orientation.TOP_BOTTOM,
                new int[]{BG_TOP, BG, BG});
        g.setGradientType(GradientDrawable.LINEAR_GRADIENT);
        return g;
    }

    static GradientDrawable ring(Context c, int color, float sizeDp) {
        GradientDrawable g = new GradientDrawable();
        g.setShape(GradientDrawable.OVAL);
        g.setColor(color);
        return g;
    }

    static View dot(Context c, int color, float sizeDp) {
        View v = new View(c);
        v.setBackground(ring(c, color, sizeDp));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(dp(c, sizeDp), dp(c, sizeDp));
        v.setLayoutParams(lp);
        return v;
    }

    // ── 组件 ───────────────────────────────────────────────────────────────
    /** 玻璃卡容器（纵向，带 16dp 内边距）。 */
    static LinearLayout card(Context c) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackground(glass(c, R_CARD));
        box.setPadding(dp(c, S4), dp(c, S3), dp(c, S4), dp(c, S3));
        return box;
    }

    static LinearLayout.LayoutParams cardLp(Context c, int bottomMarginDp) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(c, bottomMarginDp);
        return lp;
    }

    /** 主按钮（实心强调色，最小 44dp 高）。 */
    static TextView primaryButton(Context c, String label) {
        TextView b = text(c, label, 13.5f, ON_ACCENT, 2);
        b.setGravity(Gravity.CENTER);
        b.setBackground(ripple(c, round(c, ACCENT, R_CTRL), 0x3322C55E));
        b.setMinHeight(dp(c, 48));
        b.setPadding(dp(c, S4), dp(c, 12), dp(c, S4), dp(c, 12));
        b.setClickable(true);
        b.setFocusable(true);
        return b;
    }

    /** 次按钮（描边幽灵按钮）。 */
    static TextView ghostButton(Context c, String label) {
        TextView b = text(c, label, 13f, INK, 1);
        b.setGravity(Gravity.CENTER);
        b.setBackground(ripple(c, outlined(c, 0x14FFFFFF, R_CTRL, LINE, 1), 0x1FFFFFFF));
        b.setMinHeight(dp(c, 48));
        b.setPadding(dp(c, S4), dp(c, 12), dp(c, S4), dp(c, 12));
        b.setClickable(true);
        b.setFocusable(true);
        return b;
    }

    /** 小胶囊按钮（行内次要动作）。 */
    static TextView pill(Context c, String label, boolean on) {
        TextView b = text(c, label, 12f, on ? ON_ACCENT : INK2, 1);
        b.setGravity(Gravity.CENTER);
        b.setBackground(on ? round(c, ACCENT, R_PILL)
                : ripple(c, outlined(c, 0x0DFFFFFF, R_PILL, LINE, 1), 0x1FFFFFFF));
        // 视觉高度 40dp，靠上下 padding 把命中区撑到 ~48dp
        b.setMinHeight(dp(c, 40));
        b.setPadding(dp(c, S3), dp(c, 9), dp(c, S3), dp(c, 9));
        b.setClickable(true);
        return b;
    }

    static RippleDrawable ripple(Context c, GradientDrawable bg, int rippleColor) {
        return new RippleDrawable(ColorStateList.valueOf(rippleColor), bg, null);
    }

    /** 图标按钮（40dp 命中区，图标本身按 tint 着色）。 */
    static ImageView iconButton(Context c, int drawableRes, int tint, String contentDesc) {
        ImageView iv = new ImageView(c);
        iv.setImageResource(drawableRes);
        iv.setColorFilter(tint);
        iv.setContentDescription(contentDesc);
        iv.setBackground(ripple(c, round(c, 0x14FFFFFF, R_CTRL), 0x1FFFFFFF));
        int pad = dp(c, 11);
        iv.setPadding(pad, pad, pad, pad);
        iv.setLayoutParams(new LinearLayout.LayoutParams(dp(c, 44), dp(c, 44)));
        iv.setClickable(true);
        iv.setFocusable(true);
        return iv;
    }

    /** 状态胶囊：小圆点 + 文字。 */
    static LinearLayout statusChip(Context c, String label, int color) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.HORIZONTAL);
        box.setGravity(Gravity.CENTER_VERTICAL);
        box.setPadding(dp(c, S2), dp(c, 3), dp(c, S3), dp(c, 3));
        GradientDrawable g = round(c, (color & 0x00FFFFFF) | 0x26000000, R_PILL);
        g.setStroke(dp(c, 1), (color & 0x00FFFFFF) | 0x55000000);
        box.setBackground(g);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(dp(c, 6), dp(c, 6));
        dlp.rightMargin = dp(c, 6);
        box.addView(dot(c, color, 6), dlp);
        box.addView(text(c, label, 11f, color, 1));
        return box;
    }

    /** 迷你标签（企业 / 国际 / Key）。 */
    static TextView tag(Context c, String label, int color) {
        TextView t = text(c, label, 10f, color, 2);
        GradientDrawable g = round(c, (color & 0x00FFFFFF) | 0x22000000, 6);
        g.setStroke(dp(c, 1), (color & 0x00FFFFFF) | 0x44000000);
        t.setBackground(g);
        t.setPadding(dp(c, 5), dp(c, 1), dp(c, 5), dp(c, 1));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.leftMargin = dp(c, 6);
        t.setLayoutParams(lp);
        return t;
    }

    /** 分隔线。 */
    static View divider(Context c) {
        View v = new View(c);
        v.setBackgroundColor(LINE);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, Math.max(1, dp(c, 0.6f)));
        v.setLayoutParams(lp);
        return v;
    }

    /** key/value 行（左侧标签，右侧值）。 */
    static LinearLayout kvRow(Context c, String k, String v) {
        LinearLayout row = new LinearLayout(c);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(c, 5), 0, dp(c, 5));
        TextView key = text(c, k, 12.5f, INK3, 0);
        row.addView(key, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        TextView val = num(c, v, 12.5f, INK);
        val.setGravity(Gravity.END);
        row.addView(val);
        return row;
    }

    /** KPI 块（数值在上，标签在下）。 */
    static LinearLayout kpi(Context c, String value, String label, int color) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.VERTICAL);
        TextView v = num(c, value, 22f, color);
        box.addView(v);
        box.addView(label(c, label));
        return box;
    }

    /** 空状态。 */
    static TextView empty(Context c, String msg) {
        TextView t = text(c, msg, 13f, INK3, 0);
        t.setGravity(Gravity.CENTER);
        t.setPadding(dp(c, S4), dp(c, 36), dp(c, S4), dp(c, 36));
        return t;
    }

    /** 页面统一内边距容器。 */
    static LinearLayout column(Context c) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(c, S4), dp(c, S4), dp(c, S4), dp(c, S6));
        return box;
    }
}
