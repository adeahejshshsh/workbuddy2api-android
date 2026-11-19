package com.workbuddy.api;

import android.app.Activity;
import android.content.Intent;
import android.net.Uri;
import android.os.Bundle;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;


/**
 * 网页面板（fallback）：原生控制台没覆盖的进阶页仍从这里进。
 *
 * 与旧版 MainActivity 内嵌 WebView 的区别：这里只在**用户主动打开**时创建并加载，
 * 退出即销毁。原生控制台上常驻一个 WebView 是不必要的内存与 CPU 开销。
 *
 * 面板密钥从 config.json 读（与原生控制台同源），注入 localStorage 后重载一次。
 *
 * <p><b>W6-H-02</b>：注入前必须确认 `http://127.0.0.1:<port>` 背后是**本进程拉起的网关**。
 * 回环地址在 Android 上跨应用共享，任意持 INTERNET 权限的 App 都可以先 bind 该端口；
 * 此时无条件注入就等于把 {@code admin_api_key} 交给对方。判据是本机实例 id
 * （{@link InstanceId}，应用私有目录里的 32 hex）：只有
 * {@code GET /panel/api/instance} 返回的 id 与本进程读到的一致才注入。
 *
 * 失败关闭：接口 404（旧网关还没实现）、非 200、字段缺失、id 不匹配 —— 一律**不注入**并提示。
 * 宁可让用户手输一次密钥，也不能把管理员密钥灌给一个身份不明的端口占用者。
 */
public class WebPanelActivity extends Activity {

    private WebView web;
    /** 已经注入过（避免 reload → onPageFinished → 再注入的死循环）。 */
    private boolean injected;
    /** 正在做实例校验（用户连点刷新时别叠出一堆并发探测）。 */
    private boolean verifying;

    @Override protected void onCreate(Bundle b) {
        Theme.applyActivityTheme(this);
        super.onCreate(b);
        Theme.apply(this);
        // 整体背景：与原生控制台同一套底色，切换时不闪白（深浅色各自取自己的 BG）。
        getWindow().getDecorView().setBackgroundColor(Theme.BG);

        // 自举（幂等）：admin_api_key 与 instance_id 都必须在网关进程起来之前就存在，
        // 否则注入路径的两次校验都会因为「读不到」而失败关闭（功能看着像坏了）。
        AdminKey.ensure(this);
        InstanceId.ensure(this);

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
                if (injected) return;
                // 只对「本机面板地址」走注入：错误页/被重定向到别处时不碰 localStorage。
                if (!isPanelUrl(url)) return;
                verifyThenInject(v);
            }

            // W6-L-11：面板里的外链（文档、上游状态页）一律交给系统浏览器。
            // 否则 WebView 会被导航出面板，地址栏（这里没有地址栏）再也回不来，
            // 而页面本身还是我们注入了管理密钥的那个 WebView 上下文。
            @Override public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest req) {
                if (req == null || req.getUrl() == null) return false;
                return routeExternally(req.getUrl().toString());
            }

            @SuppressWarnings("deprecation")
            @Override public boolean shouldOverrideUrlLoading(WebView v, String url) {
                return routeExternally(url);
            }
        });
        col.addView(web, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));
        setContentView(col);
        web.loadUrl(MainActivity.panelUrl(this));
    }

    /** 面板基址（含端口）：与本 Activity 加载的 URL 同一个来源，避免写死 7863。 */
    private String panelBase() {
        return "http://127.0.0.1:" + GwConfig.port(this) + "/";
    }

    private boolean isPanelUrl(String url) {
        if (url == null) return false;
        String base = panelBase();
        if (url.startsWith(base)) return true;
        // 个别 ROM 会把 127.0.0.1 规范化成 localhost；同一端口仍视为面板内部。
        return url.startsWith("http://localhost:" + GwConfig.port(this) + "/");
    }

    /** true = 已交给系统浏览器（不要在 WebView 里导航）。 */
    private boolean routeExternally(String url) {
        if (url == null || isPanelUrl(url)) return false;
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url)));
        } catch (Throwable ignored) {
            // 没有浏览器/URL 非法：拦下来即可，别让 WebView 把面板顶掉。
        }
        return true;
    }

    /**
     * 注入前的实例认证（W6-H-02）：判定逻辑集中在 {@link PanelTrust}，
     * 这里只负责「过了就注入、没过就提示且不注入」。
     */
    private void verifyThenInject(final WebView v) {
        if (verifying) return;
        verifying = true;
        PanelTrust.verify(this, new PanelTrust.Cb() {
            public void done(boolean ok, String reason) {
                verifying = false;
                if (!ok) {
                    injected = false;      // 允许用户点「刷新」重试（端口占用解除后即可）
                    toast(PanelTrust.message(reason));
                    return;
                }
                if (v != null) injectKey(v);
            }
        });
    }

    private void injectKey(WebView v) {
        String key = PanelApi.key(this);
        if (key.isEmpty()) {
            injected = false;
            toast("读不到 config.json 里的 admin_api_key / api_key");
            return;
        }
        injected = true;   // 先置位：下面 evaluateJavascript 会触发 reload → onPageFinished
        // W3-04（面板侧契约）：密钥不再写 localStorage —— 面板 origin 是明文 HTTP，
        // localStorage 是落盘明文、永不过期。改为：
        //   ① window.__WB2A_ADMIN_KEY —— 面板 authKey() 的最高优先级来源；
        //   ② sessionStorage —— 仅内存/单标签页生命周期，扛得住下面这次 reload；
        // 两个都写是因为 reload 会丢掉 window 变量，而 sessionStorage 在同一浏览
        // 上下文里跨 reload 保留。localStorage 一个字节都不碰。
        StringBuilder js = new StringBuilder("try{");
        js.append("window.__WB2A_ADMIN_KEY=").append(quote(key)).append(';');
        js.append("sessionStorage.setItem('").append(MainActivity.LS_KEY).append("',")
                .append(quote(key)).append(");");
        String tok = AdminKey.bootstrapToken(this);
        if (!tok.isEmpty()) {
            // W3-02：管理员密钥自举现在要求一次性令牌（回环不再是唯一因子）。
            // 令牌文件 0600、启动时生成、用后即删 —— 面板弹窗读这个变量即可。
            js.append("window.__WB2A_BOOTSTRAP_TOKEN=").append(quote(tok)).append(';');
        }
        js.append("location.reload();}catch(e){}");
        v.evaluateJavascript(js.toString(), null);
    }

    /** JS 字符串字面量（与 MainActivity.quote 同口径，避免两处转义漂移）。 */
    private static String quote(String s) {
        return MainActivity.quote(s);
    }

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                getResources().getDisplayMetrics());
    }

    private void toast(String s) {
        try { Toast.makeText(this, s, Toast.LENGTH_LONG).show(); } catch (Throwable ignored) {}
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
