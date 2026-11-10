package com.workbuddy.api;

import android.app.Activity;
import android.os.Bundle;
import android.view.Gravity;
import android.view.ViewGroup;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;

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
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(0xFF0F1115);

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
        root.addView(web, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        setContentView(root);
        web.loadUrl(MainActivity.panelUrl(this));
    }

    private boolean injected;

    @Override public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }

    @Override protected void onDestroy() {
        try { if (web != null) { web.destroy(); web = null; } } catch (Throwable ignored) {}
        super.onDestroy();
    }
}
