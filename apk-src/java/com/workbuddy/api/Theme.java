package com.workbuddy.api;

import android.app.Activity;
import android.content.Context;
import android.content.res.Configuration;
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

    // ── 颜色（按主题切换） ─────────────────────────────────────────────────
    //
    // 为什么是**可变的 static** 而不是「按 uiMode 返回调色板」的方法：全仓有 700+ 处
    // Theme.XXX 引用（13 个文件），改成方法要动每一处；改成非 final static 只需要在
    // setDark() 里换一套赋值，调用点一行都不用改。
    //
    // 代价有两条，都得守住：
    //   ① 必须在 Activity.onCreate 里、**建视图之前**调 apply(ctx)，否则拿到的是上一次
    //      的调色板（进程刚起来时是深色默认值）；
    //   ② 谁要是把颜色复制成自己的 static final（MainActivity/ConfigActivity 就这么干过），
    //      切换主题时那份副本不会跟着变 —— 见 refreshThemeAliases()。
    static int BG, BG_TOP, CARD, CARD_SOLID, MUTED, INK, INK2, INK3, LINE,
            ACCENT, ON_ACCENT, INFO, WARN, BAD, ON_BAD, MINT, SLATE,
            NAVBAR, OVERLAY, OVERLAY_SOFT, RIPPLE, RIPPLE_ACCENT, TRACK;

    /** 主题模式：auto（跟随系统）/ dark / light。与设置页、SharedPreferences 同口径。 */
    static final String PREF_THEME = "ui_theme";
    static final String MODE_AUTO = "auto", MODE_DARK = "dark", MODE_LIGHT = "light";
    static String mode = MODE_AUTO;

    /**
     * 按当前模式装载调色板。**必须在建任何视图之前调用**。
     *
     * 读 SharedPreferences 失败（极少见）时退回跟随系统：宁可跟系统，也不要卡在一个
     * 用户改不动的状态里。
     */
    static void apply(Context c) {
        String m = MODE_AUTO;
        try {
            m = c.getSharedPreferences(GatewayService.PREF, Context.MODE_PRIVATE)
                    .getString(PREF_THEME, MODE_AUTO);
        } catch (Throwable ignored) {}
        if (!MODE_DARK.equals(m) && !MODE_LIGHT.equals(m)) m = MODE_AUTO;
        mode = m;
        setDark(MODE_DARK.equals(m) || (MODE_AUTO.equals(m) && isSystemDark(c)));
    }

    /** 系统是否处于深色模式（auto 模式下用它）。 */
    static boolean isSystemDark(Context c) {
        try {
            int ui = c.getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
            return ui == Configuration.UI_MODE_NIGHT_YES;
        } catch (Throwable t) {
            return true;   // 读不到就按深色（本 App 的原始观感）
        }
    }

    /**
     * 换一套调色板。
     *
     * 浅色不是深色的简单反色：玻璃卡（半透明 + 1px 冷色描边 + 径向高光）在浅底上会发灰，
     * 所以浅色的卡片用**半透明白**、描边与叠加层改成低透明度**黑**，强调绿也压深一档
     * （#22C55E 在白底上对比度只有 2.3:1，做前景字读不清）。
     */
    static void setDark(boolean dark) {
        if (dark) {
            BG = 0xFF0F172A;
            BG_TOP = 0xFF141E36;
            CARD = 0xF21B2336;          // 卡片略透明，叠在渐变上像磨砂
            CARD_SOLID = 0xFF1B2336;
            MUTED = 0xFF272F42;
            INK = 0xFFF8FAFC;
            INK2 = 0xFFA9B4C7;
            INK3 = 0xFF94A3B8;
            LINE = 0x66475569;
            ACCENT = 0xFF22C55E;
            ON_ACCENT = 0xFF07240F;
            INFO = 0xFF38BDF8;
            WARN = 0xFFF59E0B;
            BAD = 0xFFEF4444;
            ON_BAD = 0xFFFFFFFF;
            MINT = 0xFF34D399;          // 已完成（比强调色淡一档：亮绿=可操作，这种绿=已收下）
            SLATE = 0xFF64748B;         // 置灰 / 不可用 / 未解锁
            NAVBAR = 0xF20B1220;
            OVERLAY = 0x14FFFFFF;       // 卡片内浅色叠加（图标底、次级块）
            OVERLAY_SOFT = 0x0DFFFFFF;  // 更淡一档（未选中的胶囊）
            RIPPLE = 0x1FFFFFFF;
            RIPPLE_ACCENT = 0x3322C55E;
            TRACK = 0x1FFFFFFF;         // 开关滑轨底
        } else {
            BG = 0xFFF2F5FA;
            BG_TOP = 0xFFFFFFFF;
            CARD = 0xF2FFFFFF;
            CARD_SOLID = 0xFFFFFFFF;
            MUTED = 0xFFE7ECF4;
            INK = 0xFF0F172A;
            INK2 = 0xFF3D4A5C;
            INK3 = 0xFF5B6B80;
            LINE = 0x33475569;
            ACCENT = 0xFF15A34A;        // 压深一档：#22C55E 在白底上做前景只有 2.3:1
            ON_ACCENT = 0xFFFFFFFF;
            INFO = 0xFF0369A1;
            WARN = 0xFFB45309;
            BAD = 0xFFD92D20;
            ON_BAD = 0xFFFFFFFF;
            MINT = 0xFF0F9D63;
            SLATE = 0xFF8A97A8;
            NAVBAR = 0xF2FFFFFF;
            OVERLAY = 0x0F000000;
            OVERLAY_SOFT = 0x0A000000;
            RIPPLE = 0x1F000000;
            RIPPLE_ACCENT = 0x3315A34A;
            TRACK = 0x14000000;
        }
    }

    /** 主题模式的中文名（设置页展示用）。 */
    static String modeLabel(String m) {
        if (MODE_DARK.equals(m)) return "深色";
        if (MODE_LIGHT.equals(m)) return "浅色";
        return "跟随系统";
    }

    /** 保存模式并立即装载（调用方负责重建界面）。 */
    static void saveMode(Context c, String m) {
        if (!MODE_DARK.equals(m) && !MODE_LIGHT.equals(m)) m = MODE_AUTO;
        try {
            c.getSharedPreferences(GatewayService.PREF, Context.MODE_PRIVATE)
                    .edit().putString(PREF_THEME, m).apply();
        } catch (Throwable ignored) {}
        mode = m;
        setDark(MODE_DARK.equals(m) || (MODE_AUTO.equals(m) && isSystemDark(c)));
    }

    /**
     * 给 Activity 选窗口主题（在 super.onCreate 之前调）。
     *
     * 手动覆盖必须走 setTheme：资源里的 AppTheme 由 values-night 决定，用户选了「浅色」
     * 而系统是深色时，光改我们的调色板会让系统绘制的部分（对话框、Toast、滚动条）还是
     * 深色，看起来像坏了一半。
     */
    static void applyActivityTheme(Activity a) {
        String m = MODE_AUTO;
        try {
            m = a.getSharedPreferences(GatewayService.PREF, Context.MODE_PRIVATE)
                    .getString(PREF_THEME, MODE_AUTO);
        } catch (Throwable ignored) {}
        if (MODE_DARK.equals(m)) a.setTheme(R.style.AppTheme_Dark);
        else if (MODE_LIGHT.equals(m)) a.setTheme(R.style.AppTheme_Light);
        // auto：不调 setTheme，让清单里的 AppTheme 跟着 values-night 走
    }

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
        b.setBackground(ripple(c, round(c, ACCENT, R_CTRL), RIPPLE_ACCENT));
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
        b.setBackground(ripple(c, outlined(c, OVERLAY, R_CTRL, LINE, 1), RIPPLE));
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
                : ripple(c, outlined(c, OVERLAY_SOFT, R_PILL, LINE, 1), RIPPLE));
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
        iv.setBackground(ripple(c, round(c, OVERLAY, R_CTRL), RIPPLE));
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

    /**
     * 细进度条（5dp 高胶囊）：按 cur/target 比例填充，未达部分留轨道底色。
     *
     * 用于任务/作业这类「有明确目标值」的条目——纯文字 "3 / 5" 在列表里几乎看不出差别，
     * 一条填充比例不同的横条可以让人一眼扫出哪些已经走完。
     */
    static LinearLayout progress(Context c, int cur, int target, int color) {
        LinearLayout track = new LinearLayout(c);
        track.setOrientation(LinearLayout.HORIZONTAL);
        track.setBackground(round(c, TRACK, R_PILL));
        int total = target > 0 ? target : 1;
        int done = Math.max(0, Math.min(cur, total));
        if (done > 0) {
            View fill = new View(c);
            fill.setBackground(round(c, color, R_PILL));
            track.addView(fill, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.MATCH_PARENT, done));
        }
        if (done < total) {
            View rest = new View(c);
            rest.setBackground(round(c, 0x00000000, R_PILL));
            track.addView(rest, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.MATCH_PARENT, total - done));
        }
        track.setLayoutParams(new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, dp(c, 5)));
        return track;
    }

    /** 空状态。 */
    static TextView empty(Context c, String msg) {
        TextView t = text(c, msg, 13f, INK3, 0);
        t.setGravity(Gravity.CENTER);
        t.setPadding(dp(c, S4), dp(c, 36), dp(c, S4), dp(c, 36));
        return t;
    }

    /**
     * 纵向列表容器。
     *
     * 专门包一层的原因：Android 的 LinearLayout 默认是 **HORIZONTAL**，忘一行
     * setOrientation 就会出现「列表只显示第一条、其余排到屏幕右侧」这种极难一眼看出的
     * 布局 bug（本次请求日志就踩了）。所有「往里 addView 多条卡片」的容器都改用这个。
     */
    static LinearLayout list(Context c) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.VERTICAL);
        return box;
    }

    /** 页面统一内边距容器。 */
    static LinearLayout column(Context c) {
        LinearLayout box = new LinearLayout(c);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(c, S4), dp(c, S4), dp(c, S4), dp(c, S6));
        return box;
    }
}
