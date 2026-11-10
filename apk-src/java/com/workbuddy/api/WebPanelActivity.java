package com.workbuddy.api;

import android.app.Activity;
import android.os.Bundle;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.LinearLayout;
import android.widget.TextView;

/**
 * 网页面板（fallback）：原生控制台没覆盖的进阶页仍从这里进。
 *
 * 与旧版 MainActivity 内嵌 WebView 的区别：这里只在**用户主动打开**时创建并加载，
 * 退出即销毁。原生控制台上常驻一个 WebView 是不必要的内存与 CPU 开销。
 *
 * api_key 仍从 config.json 读（与原生控制台同源），注入 localStorage 后重载一次，
 * 用户不需要手输。
 */
public class WebPanelActivity extends Activity {

    private WebView web;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        // 整体背景：与原生控制台同一套暗色，切换时不闪白。
        getWindow().getDecorView().setBackgroundColor(0xFF0F1115);

        // 顶部返回条：网页面板里没有「回原生面板」的天然入口（网页自己不知道宿主是
        // App），硬件返回键又容易被网页的 history 吃掉。给一条明确的返回栏。
        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(0xFF171A20);
        bar.setPadding(dp(10), dp(8), dp(10), dp(8));

        TextView back = new TextView(this);
        back.setText("← 返回原生面板");
        back.setTextSize(13.5f);
        back.setTextColor(0xFF6E8BFF);
        back.setPadding(dp(6), dp(6), dp(6), dp(6));
        back.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { finish(); }
        });
        bar.addView(back);

        TextView title = new TextView(this);
        title.setText("网页面板（进阶页）");
        title.setTextSize(12f);
        title.setTextColor(0xFF6E7683);
        title.setGravity(Gravity.END);
        bar.addView(title, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        TextView reload = new TextView(this);
        reload.setText("刷新");
        reload.setTextSize(13f);
        reload.setTextColor(0xFFE7EAF0);
        reload.setPadding(dp(10), dp(6), dp(6), dp(6));
        reload.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { if (web != null) web.reload(); }
        });
        bar.addView(reload);

        LinearLayout col = new LinearLayout(this);
        col.setOrientation(LinearLayout.VERTICAL);
        col.addView(bar, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(true);
        web.setBackgroundColor(0xFF0F1115);
        web.setWebChromeClient(new WebChromeClient());
        web.setWebViewClient(new WebViewClient() {
            @Override public void onPageFinished(WebView v, String url) {
                // 注入 api_key 后重载一次（与旧壳同口径：只注入一次，避免循环）。
                if (injected) return;
                String key = PanelApi.key(WebPanelActivity.this);
                if (key.isEmpty()) return;
                injected = true;
                String js = "try{localStorage.setItem('" + MainActivity.LS_KEY + "','"
                        + key.replace("\\", "\\\\").replace("'", "\\'") + "');location.reload();}catch(e){}";
                v.evaluateJavascript(js, null);
            }
        });
        col.addView(web, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        setContentView(col);
        web.loadUrl(MainActivity.panelUrl(this));
    }

    private boolean injected;

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                getResources().getDisplayMetrics());
    }

    @Override public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }

    @Override protected void onDestroy() {
        try { if (web != null) { web.destroy(); web = null; } } catch (Throwable ignored) {}
        super.onDestroy();
    }
}
