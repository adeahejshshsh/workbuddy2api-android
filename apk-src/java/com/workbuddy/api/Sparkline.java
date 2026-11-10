package com.workbuddy.api;

import android.content.Context;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.Path;
import android.view.View;

/**
 * 极简折线（余额趋势用）：只画一条 polyline + 末端点，不做坐标轴/交互。
 *
 * 为什么自己画：网页版用 SVG/Canvas 折线图，原生要引图表库就为一个页面不值得。
 * 这里 60 行足够表达「这几天余额是涨还是掉」，而且能跟着卡片宽度自适应。
 */
final class Sparkline extends View {

    private final Paint line = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint fill = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Paint dot = new Paint(Paint.ANTI_ALIAS_FLAG);
    private float[] values = new float[0];

    Sparkline(Context c, int color) {
        super(c);
        line.setStyle(Paint.Style.STROKE);
        line.setStrokeWidth(dp(2));
        line.setColor(color);
        fill.setStyle(Paint.Style.FILL);
        fill.setColor((color & 0x00FFFFFF) | 0x22000000);
        dot.setStyle(Paint.Style.FILL);
        dot.setColor(color);
    }

    void setValues(float[] v) {
        values = v == null ? new float[0] : v;
        invalidate();
    }

    private float dp(float v) {
        return v * getResources().getDisplayMetrics().density;
    }

    @Override protected void onDraw(Canvas canvas) {
        super.onDraw(canvas);
        int n = values.length;
        if (n < 2) return;
        float w = getWidth(), h = getHeight();
        float pad = dp(3);
        float min = values[0], max = values[0];
        for (float v : values) {
            if (v < min) min = v;
            if (v > max) max = v;
        }
        float span = Math.max(1f, max - min);
        Path path = new Path();
        Path area = new Path();
        float lastX = pad, lastY = h - pad;
        for (int i = 0; i < n; i++) {
            float x = pad + (w - 2 * pad) * i / (n - 1);
            float y = h - pad - (h - 2 * pad) * (values[i] - min) / span;
            if (i == 0) {
                path.moveTo(x, y);
                area.moveTo(x, h - pad);
                area.lineTo(x, y);
            } else {
                path.lineTo(x, y);
                area.lineTo(x, y);
            }
            lastX = x;
            lastY = y;
        }
        area.lineTo(lastX, h - pad);
        area.close();
        canvas.drawPath(area, fill);
        canvas.drawPath(path, line);
        canvas.drawCircle(lastX, lastY, dp(3), dot);
    }
}
