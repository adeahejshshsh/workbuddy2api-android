package com.workbuddy.api;

import android.graphics.Bitmap;
import android.graphics.BitmapShader;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.LinearGradient;
import android.graphics.Matrix;
import android.graphics.Paint;
import android.graphics.Path;
import android.graphics.Rect;
import android.graphics.RectF;
import android.graphics.RuntimeShader;
import android.graphics.Shader;
import android.graphics.drawable.Drawable;
import android.os.Build;
import android.view.View;

import java.util.ArrayList;
import java.util.List;

/**
 * 液态玻璃表面（Android View 版，无 Compose / 无第三方库）。
 *
 * 移植的是 Kyant0/AndroidLiquidGlass 的三条核心结论，而不是它的 API：
 *   1. 玻璃 = **折射 + 边缘高光**，不是「加个模糊的半透明色块」；
 *   2. 玻璃下面必须有明暗突变，否则参数怎么调都像塑料 —— 所以采样源是极光网格
 *      ({@link Aurora})，它自带色斑与缓慢漂移；
 *   3. 玻璃只做「浮层」（导航条 / 顶栏 / 弹层），内容卡片用同一套材质但更省：
 *      只采样 + 上高光，不做逐帧模糊（20 张卡各自模糊一次必掉帧）。
 *
 * 采样方式：极光位图是 1/6 渲染再双线性放大，本身就是一次廉价模糊；浮层再采样
 * 「更糊的一档」（1/24）来模拟大半径模糊。**没有用 RenderEffect**：它只挂在
 * View / RenderNode / SurfaceControl 上，`Paint` 没有这个方法；而为一个背景
 * drawable 专门开 RenderNode 离屏通道，在滚动页面上换来的帧率损失不值当。
 *
 * 能力降级（不是「不支持就崩」）：
 *   - RenderEffect 需要 API 31，RuntimeShader(lens) 需要 API 33；
 *   - 任一不可用 → 自动退回「采样 + 高光」的磨砂玻璃，观感一致但无折射；
 *   - AGSL 编译失败只发生一次，之后永久走降级（不会每帧抛异常）。
 */
final class Glass {

    private Glass() {}

    /**
     * 棱边折射：靠近边缘的像素向内取样，形成「玻璃厚度把画面折弯」的错觉。
     * 与 Backdrop 的 lens(refractionHeight, refractionAmount) 同构，量级按像素给。
     */
    static final String LENS_AGSL =
            "uniform shader content;\n"
            + "uniform float2 size;\n"
            + "uniform float edge;\n"
            + "uniform float amount;\n"
            + "half4 main(float2 p) {\n"
            + "  float d = min(min(p.x, size.x - p.x), min(p.y, size.y - p.y));\n"
            + "  float t = clamp(1.0 - d / max(edge, 1.0), 0.0, 1.0);\n"
            + "  float2 c = size * 0.5;\n"
            + "  float2 dir = normalize(c - p + float2(0.0001, 0.0001));\n"
            + "  return content.eval(p + dir * (t * t) * amount);\n"
            + "}\n";


    /** 当前极光底（由 Console/Activity 绑定）；没有它就退回纯半透明。 */
    private static volatile Aurora source;

    /** 需要在极光换帧时重绘的玻璃表面（只存弱引用语义：表面自己去重）。 */
    private static final List<GlassSurface> LIVE = new ArrayList<GlassSurface>();

    private static Boolean lensOk;

    static void bind(Aurora aurora) {
        source = aurora;
        invalidateAll();
    }

    static Aurora source() { return source; }

    /**
     * API 33+ 才有 RuntimeShader；AGSL 编译失败一次即永久降级。
     *
     * 判定放在 {@link Lens} 里：RuntimeShader 是 API 33 才有的类型，如果它的构造
     * 直接写在 GlassSurface 的同名字段/方法里，API 24~32 的 ART 在做类校验时就有
     * 机会碰到这个不存在的类型（NoClassDefFoundError 发生在最不该发生的地方）。
     * 单独一个门面类只在 SDK>=33 时被加载，老设备永远看不到它。
     */
    static boolean lensSupported() {
        if (lensOk == null) {
            lensOk = Build.VERSION.SDK_INT >= 33 && Lens.available();
        }
        return lensOk;
    }

    /**
     * 造一块玻璃。
     *
     * @param radiusDp 圆角
     * @param tint     覆盖色（含 alpha）：玻璃本体亮度/色相，深色界面用深蓝半透明
     * @param blurDp   模糊档：0 = 采样极光原档（内容卡片）；>0 = 采样「更糊的一档」
     *                 （浮层：导航条/顶栏/弹层）
     * @param lens     是否加棱边折射（只给浮层开，贵）
     */
    static Drawable surface(android.content.Context c, float radiusDp, int tint,
                            float blurDp, boolean lens) {
        return new GlassSurface(c, radiusDp, tint, blurDp, lens);
    }

    /** 极光换帧：让所有玻璃重新采样（4fps 足够，漂移是几十秒级的）。 */
    static void invalidateAll() {
        for (int i = LIVE.size() - 1; i >= 0; i--) {
            GlassSurface s = LIVE.get(i);
            // 已经脱离视图的（弹层关闭、页面重建）直接剔掉，避免静态表只增不减。
            if (s == null || !(s.getCallback() instanceof View)) { LIVE.remove(i); continue; }
            s.refresh();
        }
    }

    // ── 表面 ───────────────────────────────────────────────────────────────
    static final class GlassSurface extends Drawable implements Drawable.Callback {

        private final android.content.Context ctx;
        private final float radiusDp;
        private final int tint;
        private final float blurDp;
        private final boolean wantLens;

        private final Paint body = new Paint(Paint.ANTI_ALIAS_FLAG | Paint.FILTER_BITMAP_FLAG);
        private final Paint tintPaint = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final Paint rim = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final Paint shade = new Paint(Paint.ANTI_ALIAS_FLAG);
        private final Path path = new Path();
        private final RectF rect = new RectF();
        private final int[] hostLoc = new int[2];
        private final int[] srcLoc = new int[2];
        private final Matrix matrix = new Matrix();

        private Lens.Handle lensHandle;
        private boolean lensDead;

        GlassSurface(android.content.Context c, float radiusDp, int tint, float blurDp, boolean lens) {
            this.ctx = c.getApplicationContext();
            this.radiusDp = radiusDp;
            this.tint = tint;
            this.blurDp = blurDp;
            this.wantLens = lens;
            rim.setStyle(Paint.Style.STROKE);
            rim.setStrokeWidth(Theme.dp(ctx, 1.1f));
            shade.setStyle(Paint.Style.STROKE);
            shade.setStrokeWidth(Theme.dp(ctx, 1f));
            synchronized (LIVE) { LIVE.add(this); }
        }

        void refresh() {
            if (getCallback() instanceof View) ((View) getCallback()).invalidate();
        }

        @Override
        public void draw(Canvas canvas) {
            Rect b = getBounds();
            if (b.isEmpty()) return;
            rect.set(b);
            path.reset();
            path.addRoundRect(rect, Theme.dp(ctx, radiusDp), Theme.dp(ctx, radiusDp), Path.Direction.CW);

            boolean hasBackdrop = drawBackdrop(canvas);
            drawTint(canvas, hasBackdrop);
            drawShade(canvas);
            drawRim(canvas);
        }

        /** 采样极光：按宿主在屏幕上的位置对齐；返回是否真的采到了底图。 */
        private boolean drawBackdrop(Canvas canvas) {
            Aurora src = source;
            Bitmap frame = src == null ? null : (blurDp > 0 ? src.frameSoft() : src.frame());
            if (frame == null || !(getCallback() instanceof View)) return false;

            View v = (View) getCallback();
            // 屏幕坐标：与极光同口径（弹层/下拉是独立 window，窗口坐标会错位）。
            v.getLocationOnScreen(hostLoc);
            src.location(srcLoc);
            float sx = frame.getWidth() / (float) Math.max(1, src.windowWidth());
            float sy = frame.getHeight() / (float) Math.max(1, src.windowHeight());
            matrix.setScale(sx, sy);
            matrix.postTranslate((hostLoc[0] - srcLoc[0]) * sx, (hostLoc[1] - srcLoc[1]) * sy);

            BitmapShader sample = new BitmapShader(frame, Shader.TileMode.CLAMP, Shader.TileMode.CLAMP);
            sample.setLocalMatrix(matrix);

            body.reset();
            body.setAntiAlias(true);
            body.setFilterBitmap(true);
            body.setShader(sample);

            if (wantLens && !lensDead && lensSupported()) {
                try {
                    // 折射高度按短边取（约占 16%），强度取高度的一半：与 Backdrop 的
                    // lens(24f, 24f) 在 44dp 控件上的比例同构。
                    float edge = Math.max(Math.min(b().width(), b().height()) * 0.16f, 1f);
                    if (lensHandle == null) lensHandle = Lens.create();
                    body.setShader(lensHandle.bind(sample, b().width(), b().height(),
                            edge, edge * 0.5f));
                } catch (Throwable t) {
                    lensDead = true;
                    body.setShader(sample);
                }
            }
            canvas.drawPath(path, body);
            return true;
        }

        private RectF b() { return rect; }

        /**
         * 玻璃本体：轻微上亮下暗（受光方向），保证深色底上的文字对比度。
         *
         * 没有极光可采时（配置页、独立弹窗等不属于控制台壳的界面）把底色提到不透明，
         * 否则一块 45% 的玻璃叠在自定义背景上会读不出来 —— 降级要能看，不能靠运气。
         */
        private void drawTint(Canvas canvas, boolean hasBackdrop) {
            Paint p = tintPaint;
            p.setShader(null);
            int top = hasBackdrop ? 0x1AFFFFFF : 0x14FFFFFF;
            int bottom = hasBackdrop ? 0x08FFFFFF : 0x0AFFFFFF;
            int base = hasBackdrop ? tint : (tint | 0xFF000000);
            p.setShader(new LinearGradient(0, rect.top, 0, rect.bottom,
                    new int[]{top, bottom, base}, new float[]{0f, 0.62f, 1f},
                    Shader.TileMode.CLAMP));
            canvas.drawPath(path, p);
        }

        /** 内侧压暗：让玻璃「有厚度」，否则边缘会显得是贴纸。 */
        private void drawShade(Canvas canvas) {
            RectF inset = new RectF(rect);
            float d = Theme.dp(ctx, 1f);
            inset.inset(d, d);
            shade.setShader(new LinearGradient(0, inset.top, 0, inset.bottom,
                    new int[]{0x00000000, 0x00000000, 0x33000000},
                    new float[]{0f, 0.55f, 1f}, Shader.TileMode.CLAMP));
            canvas.drawRoundRect(inset, Theme.dp(ctx, radiusDp - 1f), Theme.dp(ctx, radiusDp - 1f), shade);
        }

        /** 边缘光：顶部亮、底部几乎透明 —— 玻璃感主要来自这一笔。 */
        private void drawRim(Canvas canvas) {
            rim.setShader(new LinearGradient(0, rect.top, 0, rect.bottom,
                    new int[]{0x8CFFFFFF, 0x24FFFFFF, 0x0AFFFFFF},
                    new float[]{0f, 0.35f, 1f}, Shader.TileMode.CLAMP));
            canvas.drawPath(path, rim);
        }

        @Override
        public void setAlpha(int alpha) {
            body.setAlpha(alpha);
        }

        @Override
        public void setColorFilter(android.graphics.ColorFilter colorFilter) {
            body.setColorFilter(colorFilter);
        }

        @Override
        public int getOpacity() { return android.graphics.PixelFormat.TRANSLUCENT; }

        public void invalidateDrawable(Drawable who) {
            if (getCallback() != null) getCallback().invalidateDrawable(this);
        }

        public void scheduleDrawable(Drawable who, Runnable what, long when) {
            if (getCallback() != null) getCallback().scheduleDrawable(who, what, when);
        }

        public void unscheduleDrawable(Drawable who, Runnable what) {
            if (getCallback() != null) getCallback().unscheduleDrawable(who, what);
        }
    }

    /** 材质片：玻璃**内部**的选中态/徽标（不能再叠一层玻璃，只能用光做出材质感）。 */
    static Drawable material(android.content.Context c, float radiusDp, boolean on) {
        android.graphics.drawable.GradientDrawable g = new android.graphics.drawable.GradientDrawable(
                android.graphics.drawable.GradientDrawable.Orientation.TOP_BOTTOM,
                on ? new int[]{0x5722C55E, 0x1F22C55E} : new int[]{0x14FFFFFF, 0x0AFFFFFF});
        g.setCornerRadius(Theme.dp(c, radiusDp));
        g.setStroke(Theme.dp(c, 0.9f), on ? 0x9922C55E : 0x22FFFFFF);
        return g;
    }
}

/**
 * RuntimeShader 门面：**只有 API 33+ 才会被加载**（见 {@link Glass#lensSupported()}）。
 *
 * 把 API 33 的类型集中在这一个类里，老设备在类校验阶段就不会碰到它 —— 这是
 * 「同一个文件里 import 了新 API 类型，结果在旧系统上一启动就 NoClassDefFoundError」
 * 这类经典事故的唯一可靠规避方式。
 */
final class Lens {

    private Lens() {}

    static boolean available() {
        try {
            android.graphics.RuntimeShader s = new android.graphics.RuntimeShader(Glass.LENS_AGSL);
            return s != null;
        } catch (Throwable t) {
            return false;
        }
    }

    static Handle create() { return new Handle(); }

    /**
     * 一个可复用的折射器：AGSL 编译一次，之后每帧只换输入位图与 uniform。
     *
     * 为什么不每次 new：`new RuntimeShader(agsl)` 会即时编译着色器，放在 draw()
     * 里就是「每帧编译一次 AGSL」—— 滚动时直接掉到个位数帧率。
     */
    static final class Handle {
        private final android.graphics.RuntimeShader shader;

        Handle() { shader = new android.graphics.RuntimeShader(Glass.LENS_AGSL); }

        Shader bind(Shader content, float w, float h, float edge, float amount) {
            shader.setInputShader("content", content);
            shader.setFloatUniform("size", Math.max(w, 1f), Math.max(h, 1f));
            shader.setFloatUniform("edge", Math.max(edge, 1f));
            shader.setFloatUniform("amount", Math.max(amount, 0f));
            return shader;
        }
    }
}
