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
    /** 顶部状态 chip：已注入 / 未注入（原因），点一下可重试。 */
    private TextView statusChip;
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

        // 状态 chip：把「有没有自动登录」直接写在标题位上。
        // 此前注入失败只弹一次 toast（几秒就没了），用户看到的是密钥输入框，
        // 于是以为「每次都得输」——真实原因是注入被实例校验拦下了，而那行提示早消失了。
        statusChip = new TextView(this);
        statusChip.setText("正在确认网关身份…");
        statusChip.setTextSize(11.5f);
        statusChip.setTextColor(0xFF6E7683);
        statusChip.setGravity(Gravity.END);
        statusChip.setPadding(dp(6), dp(6), dp(6), dp(6));
        bar.addView(statusChip, new LinearLayout.LayoutParams(0,
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
            public void done(boolean ok, String reason, String actualId) {
                verifying = false;
                if (!ok) {
                    injected = false;      // 允许用户点「刷新」重试（端口占用解除后即可）
                    toast(PanelTrust.message(reason));
                    offerManualInject(reason, actualId);
                    return;
                }
                if (v != null) injectKey(v);
            }
        });
    }

    private void injectKey(WebView v) { injectKey(v, 0); }

    /**
     * 写密钥 + **回读确认**，成功才 reload。
     *
     * 旧实现在 evaluateJavascript 之前就把 injected 置 true、reload 也写在 JS 里，
     * 于是「写入没成功」和「成功」长得一模一样：页面刷新回来还是没有 key，面板弹密钥门，
     * 而 onPageFinished 因为 injected==true 不再重试 —— 用户看到的就是「每次都要输」。
     * 现在与 MainActivity 同一口径（bug-04）：回读 sessionStorage 命中才算成功，
     * 失败最多重试 3 次，未成功则保持 injected=false，下次页面加载还会再来。
     */
    /** 实例标识的短指纹（前 8 位）：chip 与对话框里用来区分「哪个实例」。 */
    private static String fp(String id) {
        if (id == null || id.isEmpty()) return "—";
        return id.length() <= 8 ? id : id.substring(0, 8);
    }

    private void injectKey(final WebView v, final int attempt) {
        if (v == null) return;
        // 密钥优先级：用户为「另一个实例」手输的那份 > 本 App config.json。
        // 不这样排，「信任了这个实例」之后仍然每次 401 —— 因为两个实例的
        // admin_api_key 本来就不同（见 PanelTrust.SAME_KIND）。
        String k = PanelTrust.keyOverride(this);
        if (k.isEmpty()) k = PanelApi.key(this);
        if (k.isEmpty()) {
            // config.json 里没有可用密钥：让面板自己生成一把（幂等），再读一次。
            try { AdminKey.ensure(this); } catch (Throwable ignored) {}
            k = PanelApi.key(this);
        }
        final String key = k;
        if (key.isEmpty()) {
            injected = false;
            setChip("未自动登录 · 读不到 admin_api_key（去「全部配置」检查）", 0xFFE0A030, null);
            toast("读不到 config.json 里的 admin_api_key / api_key");
            return;
        }
        // W3-04（面板侧契约）：密钥不写 localStorage —— 面板 origin 是明文 HTTP，
        // localStorage 是落盘明文、永不过期。改为：
        //   ① window.__WB2A_ADMIN_KEY —— 面板 authKey() 的最高优先级来源；
        //   ② sessionStorage —— 仅内存/单标签页生命周期，扛得住下面这次 reload。
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
        js.append("sessionStorage.getItem('").append(MainActivity.LS_KEY).append("');}catch(e){null}");
        v.evaluateJavascript(js.toString(), new android.webkit.ValueCallback<String>() {
            public void onReceiveValue(String got) {
                // evaluateJavascript 把字符串结果再包一层 JSON：带引号才算真写进去。
                if (got != null && got.contains(key)) {
                    injected = true;
                    setChip("已自动登录（无需输入密钥）", 0xFF4CAF7D, null);
                    v.reload();
                    return;
                }
                if (attempt < 2) {
                    v.postDelayed(new Runnable() {
                        public void run() { injectKey(v, attempt + 1); }
                    }, 400);
                    return;
                }
                injected = false;   // 保持 false：下次 load 仍会自动重试
                // 写入被拒与「实例校验没过」是两件事：这里 WebView 明确没接受写入
                // （页面还没到可执行 JS 的状态、或被 CSP/策略拦下）。给同一个处置入口，
                // 但文案要说清是「写入」这一步失败，别让用户以为是端口的问题。
                setChip("未自动登录 · 写入密钥被拒（点此处理）", 0xFFE0A030,
                        new View.OnClickListener() {
                            public void onClick(View x) { confirmTrustAndInject(""); }
                        });
            }
        });
    }

    /** JS 字符串字面量（与 MainActivity.quote 同口径，避免两处转义漂移）。 */
    private static String quote(String s) {
        return MainActivity.quote(s);
    }

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                getResources().getDisplayMetrics());
    }

    /**
     * 校验失败时把 chip 变成可点的入口（默认失败关闭，注入要用户确认一次）。
     *
     * <p>chip 上带期望/实际实例的短指纹：原来只写「端口被其他应用占用」，用户完全
     * 看不出到底是第三方占了端口、还是自己另开了一份网关 —— 这两种情况的处置相反。
     */
    private void offerManualInject(String reason, final String actualId) {
        setChip("未自动登录 · " + shortReason(reason) + "（点此处理）", 0xFFE0A030,
                new View.OnClickListener() {
                    public void onClick(View x) { confirmTrustAndInject(actualId); }
                });
    }

    /** 状态 chip 的统一样式入口（null 监听 = 只显示）。 */
    private void setChip(final String text, final int color, View.OnClickListener click) {
        runOnUiThread(new Runnable() {
            public void run() {
                if (statusChip == null) return;
                statusChip.setText(text);
                statusChip.setTextColor(color);
            }
        });
        if (statusChip != null) statusChip.setOnClickListener(click);
    }

    /** 校验失败的短文案（chip 放不下 PanelTrust 的整句）。 */
    private static String shortReason(String reason) {
        if (reason == null) return "未确认端口归属";
        if (reason.startsWith(PanelTrust.HTTP_ERROR)) return "实例接口异常";
        if (PanelTrust.OLD_GATEWAY.equals(reason)) return "网关版本较旧";
        if (PanelTrust.NO_CONN.equals(reason)) return "连不上网关";
        if (PanelTrust.NO_ID.equals(reason)) return "读不到实例标识";
        if (PanelTrust.SAME_KIND.equals(reason)) return "端口上是另一个同类实例";
        if (PanelTrust.MISMATCH.equals(reason)) return "端口被其他应用占用";
        return "未确认端口归属";
    }

    /**
     * 实例校验没过时的处置入口（默认 fail-closed，任何注入都要用户明确确认一次）。
     *
     * <p>为什么给这个口子：校验的初衷是不把 admin_api_key 交给占位进程；但它的失败
     * 模式在真机上很常见 —— 端口上跑着**同一项目的另一个实例**（命令行侧/容器里那份、
     * 旧版本残留的孤儿、App 重装后换了 instance_id 而网关还是旧进程）。原来的处理是
     * 每次打开面板都弹一次确认、且只在本次会话有效，用户感受就是「始终提示端口被占用、
     * 永远要手输密钥」。
     *
     * <p>现在分成三档，按 (端口, 实例标识) 记忆：
     * <ul>
     *   <li><b>信任这个实例</b> —— 记下 (port, instance_id)，下次同实例直接注入；</li>
     *   <li><b>填它自己的 admin_api_key</b> —— 存进 {@code data/panel_key}，优先级高于
     *       config.json（两个实例的密钥不同，这是 401 的真因）；</li>
     *   <li><b>取消</b> —— 保持不注入，并清掉旧的信任标记。</li>
     * </ul>
     */
    private void confirmTrustAndInject(final String actualId) {
        final String port = String.valueOf(GwConfig.port(this));
        final boolean sameKind = actualId != null && !actualId.isEmpty();
        final String expect = InstanceId.read(this);
        String msg = "实例校验没通过（端口 " + port + "）。\n\n"
                + "本应用记录的实例：" + fp(expect) + "…\n"
                + "该端口报出的实例：" + fp(actualId) + "…\n\n";
        if (sameKind) {
            msg += "对面确实是一个 workbuddy2api 网关，只是**不是本应用这次拉起的那个**"
                    + "（实例标识不同）。这种情况常见于：命令行/容器里也跑了一份、"
                    + "或旧版本网关进程还在跑。\n\n"
                    + "· 选「信任这个实例」：以后这个实例不再校验，但仍要用**它自己的**"
                    + " admin_api_key —— 两个实例的密钥通常不同，否则面板照样 401。\n"
                    + "· 选「填它的密钥」：把那个实例的 admin_api_key 粘进来，会记住。";
        } else {
            msg += "对面没有按预期报出实例标识 —— 可能是别的应用占着这个端口。\n\n"
                    + "如果你确认这就是本应用的网关（例如刚升级、网关刚被看门狗拉起），"
                    + "可以强制注入；否则请点「取消」，注入等于把管理员密钥交给对方。";
        }
        new android.app.AlertDialog.Builder(this)
                .setTitle(sameKind ? "端口上是另一个 workbuddy2api 实例" : "确认这是本应用的网关？")
                .setMessage(msg)
                .setPositiveButton("信任这个实例", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) {
                        if (sameKind) PanelTrust.rememberTrust(WebPanelActivity.this,
                                GwConfig.port(WebPanelActivity.this), actualId);
                        if (web != null) injectKey(web);
                    }
                })
                .setNeutralButton("填它的密钥", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) {
                        askKeyOverride(actualId);
                    }
                })
                .setNegativeButton("取消", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) {
                        PanelTrust.forgetTrust(WebPanelActivity.this);
                    }
                })
                .show();
    }

    /** 让用户粘贴「该实例自己的」admin_api_key：记住后立即重试注入。 */
    private void askKeyOverride(final String actualId) {
        final android.widget.EditText input = new android.widget.EditText(this);
        input.setHint("该实例的 admin_api_key");
        input.setTextSize(13f);
        input.setSingleLine(true);
        new android.app.AlertDialog.Builder(this)
                .setTitle("填入该实例的 admin_api_key")
                .setMessage("面板只认它自己那份管理员密钥。留空并确定 = 清掉已记住的密钥。")
                .setView(input)
                .setPositiveButton("保存并重试", new android.content.DialogInterface.OnClickListener() {
                    public void onClick(android.content.DialogInterface d, int w) {
                        String v = input.getText() == null ? "" : input.getText().toString().trim();
                        if (v.isEmpty()) PanelTrust.forgetKey(WebPanelActivity.this);
                        else PanelTrust.rememberKey(WebPanelActivity.this, v);
                        if (actualId != null && !actualId.isEmpty()) {
                            PanelTrust.rememberTrust(WebPanelActivity.this,
                                    GwConfig.port(WebPanelActivity.this), actualId);
                        }
                        if (web != null) injectKey(web);
                    }
                })
                .setNegativeButton("取消", null)
                .show();
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
