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
 * 模型与档位（分区：价格、限时优惠、能力标记）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenModels {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenModels(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderModels(); }

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

    // ── 模型 ───────────────────────────────────────────────────────────────
    private void renderModels() {
        setBar("模型与档位", true, iconAction(R.drawable.ic_refresh, "刷新", new Runnable() {
            public void run() { renderModels(); }
        }));
        LinearLayout col = Theme.column(a);
        final LinearLayout list = Theme.list(a);
        col.addView(list);
        list.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(List<PanelService.ModelInfo> models) {
                list.removeAllViews();
                if (models.isEmpty()) { list.addView(Theme.empty(a, "没有可用模型")); return; }
                for (PanelService.ModelInfo m : models) {
                    LinearLayout card = Theme.card(a);
                    card.setLayoutParams(Theme.cardLp(a, Theme.S2));
                    LinearLayout top = new LinearLayout(a);
                    top.setOrientation(LinearLayout.HORIZONTAL);
                    top.setGravity(Gravity.CENTER_VERTICAL);
                    TextView name = Theme.text(a, m.name, 14.5f, Theme.INK, 2);
                    name.setSingleLine(true);
                    name.setEllipsize(TextUtils.TruncateAt.END);
                    top.addView(name, new LinearLayout.LayoutParams(0,
                            ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
                    TextView price = Theme.num(a, m.hasPromo()
                            ? String.format(java.util.Locale.US, "%.2f", m.promoCredits)
                            : (m.credits > 0 ? String.format(java.util.Locale.US, "%.2f", m.credits) : "0"),
                            14f, m.hasPromo() ? Theme.ACCENT : Theme.INK2);
                    top.addView(price);
                    card.addView(top);

                    LinearLayout tags = new LinearLayout(a);
                    tags.setOrientation(LinearLayout.HORIZONTAL);
                    tags.setPadding(0, Theme.dp(a, Theme.S2), 0, 0);
                    tags.addView(Theme.tag(a, m.id, Theme.INK3));
                    if (m.hasPromo()) tags.addView(Theme.tag(a, "限时价", Theme.ACCENT));
                    if (m.reasoning) tags.addView(Theme.tag(a, "思考", Theme.INFO));
                    if (m.images) tags.addView(Theme.tag(a, "图片", Theme.INFO));
                    if (m.tools) tags.addView(Theme.tag(a, "工具", Theme.INFO));
                    card.addView(tags);
                    list.addView(card);
                }
            }
            public void fail(String m) { list.removeAllViews(); list.addView(Theme.empty(a, "失败：" + m)); }
        });
    }
}
