package com.workbuddy.api;

import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.LinearGradient;
import android.graphics.Paint;
import android.graphics.RadialGradient;
import android.graphics.Rect;
import android.graphics.Shader;
import android.graphics.drawable.Drawable;
import android.os.SystemClock;
import android.view.Choreographer;
import android.view.View;

/**
 * 「液态玻璃」的底：一层缓慢漂移的极光网格，同时是玻璃表面的采样源。
 *
 * 为什么要有它：玻璃的观感来自**折射背后的内容**，而背后如果是一块纯色/单渐变，
 * 无论参数怎么调都会像半透明塑料（Kyant0 Backdrop 的「铁律 2」）。本应用的页面
 * 底色原本就是一块静态渐变，直接在上面做玻璃必然发灰 —— 所以先把底做成有明暗
 * 突变、并且会动的极光网格，玻璃才有东西可弯。
 *
 * 实现取舍：
 *   - 渲染在 1/6 分辨率的小位图上再放大绘制：双线性放大本身就是一次廉价模糊，
 *     于是玻璃采样时不必再对每张卡片做 RenderEffect（20 张卡 × 每帧模糊会掉帧）；
 *   - 位置相位由系统时钟算，Choreographer 只负责按 ~15fps 触发重绘，页面不可见
 *     或没有 callback 时自动停摆（后台不烧 CPU）。
 */
final class Aurora extends Drawable implements Drawable.Callback, Choreographer.FrameCallback {

    /** 极光渲染缩放：越小越糊、越省。1/6 在手机上肉眼已看不出像素块。 */
    private static final int SCALE = 6;
    /** 目标帧率（漂移很慢，15fps 足够；再高只是白烧电）。 */
    private static final long FRAME_MS = 400;

    private final Paint paint = new Paint(Paint.FILTER_BITMAP_FLAG | Paint.DITHER_FLAG);
    private Bitmap frame;
    private Bitmap soft;
    private int winW = 1, winH = 1;
    private boolean scheduled;
    private int sampleTick;
    /** 极光宿主在窗口里的位置（玻璃采样时用来对齐坐标）。 */
    private final int[] origin = new int[2];

    /** 供玻璃采样：当前这一帧的极光位图。 */
    Bitmap frame() { return frame; }

    /**
     * 「更糊的一档」：再降采样 4 倍后放大，等价于一次半径较大的模糊。
     *
     * 为什么不用 RenderEffect：它只挂在 View/RenderNode 上，`Paint` 没有；而背景
     * drawable 为此开一条离屏 RenderNode 通道，在滚动列表上是纯亏。降采样一次
     * 小位图（几百像素）的成本可以忽略，观感差别肉眼几乎不可见。
     */
    Bitmap frameSoft() { return soft; }

    /** 极光位图对应的窗口宽度（用于把位图像素映射回窗口坐标）。 */
    int windowWidth() { return winW; }

    int windowHeight() { return winH; }

    /** 极光位图左上角对应的窗口坐标（即宿主 View 的窗口位置）。 */
    void location(int[] out) {
        out[0] = origin[0];
        out[1] = origin[1];
    }

    @Override
    public void draw(Canvas canvas) {
        Rect b = getBounds();
        if (b.width() <= 0 || b.height() <= 0) return;
        if (frame == null || winW != b.width() || winH != b.height()) {
            winW = b.width();
            winH = b.height();
            render();
        }
        // 用屏幕坐标而不是窗口坐标：底部弹层、下拉菜单各自是独立 window，
        // 只有屏幕坐标能让所有玻璃表面采样到同一张底图。
        if (getCallback() instanceof View) ((View) getCallback()).getLocationOnScreen(origin);
        canvas.drawBitmap(frame, null, b, paint);
        schedule();
    }

    /** 把极光渲染进小位图：底色渐变 + 四团漂移的高饱和色斑。 */
    private void render() {
        int fw = Math.max(48, winW / SCALE);
        int fh = Math.max(48, winH / SCALE);
        if (frame == null || frame.getWidth() != fw || frame.getHeight() != fh) {
            frame = Bitmap.createBitmap(fw, fh, Bitmap.Config.ARGB_8888);
        }
        if (soft == null || soft.getWidth() != Math.max(8, fw / 4) || soft.getHeight() != Math.max(8, fh / 4)) {
            soft = Bitmap.createBitmap(Math.max(8, fw / 4), Math.max(8, fh / 4), Bitmap.Config.ARGB_8888);
        }
        Canvas c = new Canvas(frame);
        c.drawColor(Color.TRANSPARENT);

        // 底色：从略亮的蓝黑压到最暗，制造上亮下暗的受光方向。
        Paint bg = new Paint();
        bg.setShader(new LinearGradient(0, 0, 0, fh,
                new int[]{0xFF16213C, 0xFF0F172A, 0xFF0A101C},
                new float[]{0f, 0.55f, 1f}, Shader.TileMode.CLAMP));
        c.drawRect(0, 0, fw, fh, bg);

        long t = SystemClock.uptimeMillis();
        // 四团色斑：色相拉开（绿/青/蓝紫/暖橙），周期互质，看起来不会同步摆动。
        blob(c, fw, fh, t, 41000, Theme.ACCENT, 0.34f, 0.18f, 0.42f, 26);
        blob(c, fw, fh, t, 53000, Theme.INFO, 0.62f, 0.10f, 0.50f, 22);
        blob(c, fw, fh, t, 67000, 0xFF8B5CF6, 0.24f, 0.74f, 0.46f, 20);
        blob(c, fw, fh, t, 79000, Theme.WARN, 0.78f, 0.62f, 0.40f, 14);

        // 软档：把这一帧再缩到 1/4（双线性），玻璃浮层采样它就有大半径模糊的观感。
        Canvas sc = new Canvas(soft);
        sc.drawBitmap(frame, null,
                new android.graphics.Rect(0, 0, soft.getWidth(), soft.getHeight()), paint);
    }

    private void blob(Canvas c, int fw, int fh, long t, long period,
                      int color, float cx, float cy, float radius, int alpha) {
        double ph = (t % period) / (double) period * Math.PI * 2;
        // 椭圆轨迹：中心在基准点附近绕小圈漂移，半径按屏幕短边取，避免方屏/长屏比例失衡。
        float dx = (float) Math.cos(ph) * fw * 0.10f;
        float dy = (float) Math.sin(ph * 1.3) * fh * 0.07f;
        float x = cx * fw + dx;
        float y = cy * fh + dy;
        float r = radius * Math.min(fw, fh);
        Paint p = new Paint(Paint.ANTI_ALIAS_FLAG);
        p.setShader(new RadialGradient(x, y, r,
                (alpha << 24) | (color & 0x00FFFFFF),
                (color & 0x00FFFFFF),
                Shader.TileMode.CLAMP));
        c.drawCircle(x, y, r, p);
    }

    /** 按 FRAME_MS 的节奏调度下一帧；只在有宿主且可见时跑（后台不烧电）。 */
    private void schedule() {
        if (scheduled) return;
        if (getCallback() == null) return;
        if (getCallback() instanceof View && !((View) getCallback()).isShown()) return;
        scheduled = true;
        Choreographer.getInstance().postFrameCallbackDelayed(this, FRAME_MS);
    }

    @Override
    public void doFrame(long frameTimeNanos) {
        scheduled = false;
        // 每一帧都换相位没有意义（漂移周期是几十秒），隔几帧重画一次即可。
        render();
        invalidateSelf();
        // 玻璃表面隔一帧再重采样（≈0.8s 一次）：极光漂移是几十秒级的，采样跟得上
        // 肉眼就完全看不出来，而每 1.6s 让全屏重绘一次远比每帧重绘省。
        if ((++sampleTick & 1) == 0) Glass.invalidateAll();
        schedule();   // 自续：否则只有第一次重绘，极光会停在原地
    }

    @Override
    public void setAlpha(int alpha) { }

    @Override
    public void setColorFilter(android.graphics.ColorFilter colorFilter) { }

    @Override
    public int getOpacity() { return android.graphics.PixelFormat.OPAQUE; }

    // Drawable.Callback：作为背景时会拿到宿主 View，转给自身的失效逻辑。
    public void invalidateDrawable(Drawable who) { invalidateSelf(); }

    public void scheduleDrawable(Drawable who, Runnable what, long when) {
        if (getCallback() != null) getCallback().scheduleDrawable(who, what, when);
    }

    public void unscheduleDrawable(Drawable who, Runnable what) {
        if (getCallback() != null) getCallback().unscheduleDrawable(who, what);
    }
}
