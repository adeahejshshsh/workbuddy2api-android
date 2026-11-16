package com.workbuddy.api;

import android.content.Context;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.RectF;
import android.view.View;

/**
 * 极简柱状图（用量页的时间维度，issue #167 第 2 条）。
 *
 * 为什么自己画而不是引图表库：整个 App 是纯 framework、零依赖，为一个页面引 MPAndroidChart
 * 要多背几百 KB 的 dex；而这里要表达的东西很简单 —— 每小时/每天有多少请求、token 或积分。
 * 与 Sparkline 同一套路子（Canvas 自绘 + 跟着卡片宽度自适应），区别是柱状 + 坐标基线 +
 * 稀疏的 x 轴标签。
 *
 * 两个刻意的取舍：
 *   · **不画 y 轴刻度线**，只在左上角标一个最大值 —— 柱状图的用途是看形状与相对高低，
 *     精确读数由上面的 KPI 卡与下面的排行表负责，多画一套刻度只会让 40dp 高的图变糊。
 *   · **x 轴标签按宽度抽稀**（最多 maxLabels 个），否则 24 根柱子会把「13」「14」「15」
 *     挤成一团黑。抽稀规则见 labelStride()。
 */
final class BarChart extends View {

    private final Paint bar = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint baseline = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint label = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint maxText = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final RectF rect = new RectF();

    private float[] values = new float[0];
    private String[] labels = new String[0];
    private String unit = "";
    private int accent;

    /** 一根柱子上限 26dp、下限 2dp：数据少时不要变成几根柱子糊满整屏。 */
    private static final float MAX_BAR_DP = 26f;

    BarChart(Context c) {
        super(c);
        accent = Theme.ACCENT;
        bar.setStyle(Paint.Style.FILL);
        baseline.setStyle(Paint.Style.STROKE);
        baseline.setStrokeWidth(dp(1));
        label.setTextSize(dp(9.5f));
        label.setTextAlign(Paint.Align.CENTER);
        maxText.setTextSize(dp(9.5f));
        maxText.setTextAlign(Paint.Align.LEFT);
        setWillNotDraw(false);
    }

    void setData(float[] v, String[] l, String unitText) {
        values = v == null ? new float[0] : v;
        labels = l == null ? new String[0] : l;
        unit = unitText == null ? "" : unitText;
        invalidate();
    }

    /** 换主题后重新取色（调色板是进程级 static，已建好的 View 不会自己变）。 */
    void refreshColors() {
        accent = Theme.ACCENT;
        invalidate();
    }

    private float dp(float v) {
        return v * getResources().getDisplayMetrics().density;
    }

    @Override protected void onDraw(Canvas canvas) {
        super.onDraw(canvas);
        int n = values.length;
        if (n == 0) return;

        float w = getWidth();
        float h = getHeight();
        float padTop = dp(14);          // 给左上角的最大值留位置
        float padBottom = dp(13);       // 给 x 轴标签留位置
        float baseY = h - padBottom;
        float chartH = Math.max(1f, baseY - padTop);

        float max = 0f;
        for (float v : values) if (v > max) max = v;
        if (max <= 0f) max = 1f;

        baseline.setColor(Theme.LINE);
        canvas.drawLine(0, baseY, w, baseY, baseline);

        float slot = w / n;
        float barW = Math.min(dp(MAX_BAR_DP), Math.max(dp(2), slot * 0.62f));
        bar.setColor(accent);
        for (int i = 0; i < n; i++) {
            float v = values[i];
            if (v <= 0f) continue;      // 零值不画：留白比一条 1px 的灰线更好读
            float bh = chartH * (v / max);
            if (bh < dp(1.5f)) bh = dp(1.5f);   // 有数据就要看得见
            float cx = slot * i + slot / 2f;
            rect.set(cx - barW / 2f, baseY - bh, cx + barW / 2f, baseY);
            canvas.drawRoundRect(rect, dp(2), dp(2), bar);
        }

        // 左上角最大值（带单位），比画满 y 轴刻度省地方
        maxText.setColor(Theme.INK3);
        canvas.drawText(fmt(max) + (unit.isEmpty() ? "" : " " + unit), 0, dp(10), maxText);

        // x 轴标签：按宽度抽稀，首尾必留
        label.setColor(Theme.INK3);
        int stride = labelStride(n, slot);
        for (int i = 0; i < n; i++) {
            if (i % stride != 0 && i != n - 1) continue;
            if (i != n - 1 && (n - 1 - i) < stride && i != 0) continue;  // 别和末尾标签叠
            String t = i < labels.length && labels[i] != null ? labels[i] : "";
            if (t.isEmpty()) continue;
            float cx = slot * i + slot / 2f;
            canvas.drawText(t, cx, h - dp(2), label);
        }
    }

    /** 每根柱子占的宽度放不下一个标签时抽稀：至少 34dp 才画一个。 */
    private int labelStride(int n, float slot) {
        float need = dp(34);
        int stride = 1;
        while (stride < n && slot * stride < need) stride++;
        return Math.max(1, stride);
    }

    private static String fmt(float v) {
        if (v >= 1_000_000f) return String.format(java.util.Locale.US, "%.1fM", v / 1_000_000f);
        if (v >= 1_000f) return String.format(java.util.Locale.US, "%.1fk", v / 1_000f);
        return String.valueOf(Math.round(v));
    }
}
