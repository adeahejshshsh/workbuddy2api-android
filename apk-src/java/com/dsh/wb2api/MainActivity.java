package com.dsh.wb2api;

import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.StateListDrawable;
import android.net.Uri;
import android.os.SystemClock;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.text.TextUtils;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.WebChromeClient;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.TextView;
import android.widget.Toast;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 主界面：一薄层原生壳（状态条 + 4 个胶囊按钮 + 加载进度线）+ 网关自带管理面板（WebView）。
 *
 * 配色直接抄面板 internal/panel/index.html 的 :root 暗色令牌 —— 壳和网页同一套色，
 * 免得顶部一条系统蓝压在冷石墨的面板上，看着像两个东西拼起来的。
 *
 * 面板的 api_key 存在 localStorage['wb2api.key']，首次加载后自动注入，免手输。
 */
public class MainActivity extends Activity {
    /**
     * 面板地址：端口取自 config.json 的 listen（报告 #9）。
     *
     * 写死 7863 时，用户在面板里把 listen 改掉，WebView 就永远打不开（panelLoaded
     * 恒 false，重试耗尽后停在空白页），而网关其实是好的。
     */
    static String panelUrl(android.content.Context c) {
        return "http://127.0.0.1:" + GwConfig.port(c) + "/panel/";
    }
    static final String LS_KEY = "wb2api.key";

    // ── 设计令牌（= 面板 :root / [data-theme="dark"]）
    static final int BG       = 0xFF0C0E14;   // --bg
    static final int SURFACE  = 0xFF14171F;   // --surface
    static final int SURFACE2 = 0xFF1A1E28;   // --surface-2
    static final int LINE     = 0xFF262C3A;   // --line
    static final int INK      = 0xFFE8EBF2;   // --ink
    static final int INK2     = 0xFFA8B0C2;   // --ink-2
    static final int INK3     = 0xFF6B7488;   // --ink-3
    static final int ACCENT   = 0xFF5B7CFA;   // --accent
    static final int OK       = 0xFF3DDC97;   // --ok
    static final int WARN     = 0xFFF5B544;   // --warn
    static final int BAD      = 0xFFF0655F;   // --bad

    WebView web;
    TextView info, logLine, stateLabel;
    View dot;
    ProgressBar progress;
    final Handler ui = new Handler(Looper.getMainLooper());
    String apiKey = "";
    boolean injected = false;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        askNotifPermission();
        if (Build.VERSION.SDK_INT >= 21) {          // 系统栏与壳同色，别露一条黑边
            getWindow().setStatusBarColor(SURFACE);
            getWindow().setNavigationBarColor(SURFACE);
        }

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);

        // ── 状态条：三行（标识+状态 / 保活·省电·自愈 / 最后一行日志）
        LinearLayout head = new LinearLayout(this);
        head.setOrientation(LinearLayout.VERTICAL);
        head.setBackgroundColor(SURFACE);
        head.setPadding(dp(13), dp(10), dp(13), dp(9));

        LinearLayout row1 = new LinearLayout(this);
        row1.setOrientation(LinearLayout.HORIZONTAL);
        row1.setGravity(Gravity.CENTER_VERTICAL);
        dot = new View(this);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(dp(8), dp(8));
        dlp.rightMargin = dp(8);
        dot.setLayoutParams(dlp);
        dot.setBackground(oval(INK3));
        row1.addView(dot);
        TextView title = txt("WorkBuddy2API", 14f, INK);
        title.setTypeface(Typeface.DEFAULT_BOLD);
        row1.addView(title);
        row1.addView(spacer());
        stateLabel = txt("启动中", 11.5f, INK3);
        row1.addView(stateLabel);
        head.addView(row1);

        info = txt("正在启动网关…", 11.5f, INK2);
        info.setPadding(0, dp(5), 0, 0);
        head.addView(info);

        logLine = txt("", 10.5f, INK3);
        logLine.setPadding(0, dp(3), 0, 0);
        logLine.setSingleLine(true);
        logLine.setEllipsize(TextUtils.TruncateAt.END);
        head.addView(logLine);
        root.addView(head);

        View div = new View(this);
        div.setBackgroundColor(LINE);
        root.addView(div, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(1)));

        // ── 动作条：4 个胶囊 chip（与面板 button 同圆角/同色）
        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setBackgroundColor(SURFACE);
        bar.setPadding(dp(9), dp(8), dp(9), dp(8));
        bar.addView(chip("重启网关", new View.OnClickListener() {
            public void onClick(View v) { restart(); }
        }));
        bar.addView(chip("复制密钥", new View.OnClickListener() {
            public void onClick(View v) { copyApi(); }
        }));
        bar.addView(chip("保活设置", new View.OnClickListener() {
            public void onClick(View v) { keepAliveWizard(); }
        }));
        bar.addView(chip("重载面板", new View.OnClickListener() {
            public void onClick(View v) {
                injected = false;
                pageHadError = false;   // 手动重载：清错误标记，否则本次也会被判定失败（bug B2）
                panelLoaded = false;
                loadTries = 0;
                web.reload();
            }
        }));
        root.addView(bar);

        // ── 加载进度线（2dp，加载完自动收起）
        progress = new ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal);
        progress.setMax(100);
        progress.setProgressTintList(ColorStateList.valueOf(ACCENT));
        progress.setProgressBackgroundTintList(ColorStateList.valueOf(LINE));
        progress.setVisibility(View.GONE);
        root.addView(progress, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, dp(2)));

        web = new WebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setUseWideViewPort(true);
        // 面板自带 viewport meta，按设备宽度 1:1 渲染即可；overview 模式会把整页缩小
        // （宽表一多就变成"字小得看不清"，手机上看就是"没适配"）。
        s.setLoadWithOverviewMode(false);
        s.setSupportZoom(true);
        s.setBuiltInZoomControls(false);    // 藏掉 +/- 控件，保留双指缩放
        s.setDisplayZoomControls(false);
        s.setTextZoom(100);
        // 不放开混合内容：面板只连 http://127.0.0.1，允许混合内容只会给将来的
        // 外部资源留下降级口子。
        web.setBackgroundColor(BG);
        web.setOverScrollMode(View.OVER_SCROLL_NEVER);
        web.setWebViewClient(new WebViewClient() {
            @Override public void onPageFinished(WebView v, String url) {
                panelLoading = false;   // 本次加载结束（成功或错误页都算结束，bug #1）
                progress.setVisibility(View.GONE);
                // 错误页同样用原 URL 回调本方法（bug B2）：WebView 的顺序是
                // onReceivedError(failingUrl) → onPageFinished(failingUrl)，所以
                // 只判 url 是否以面板地址开头会把「加载失败」记成「加载成功」，
                // tick 的 !panelLoaded 条件从此不成立 → 网关恢复后也不会自动重载，
                // 只能手点「重载面板」。用一次性错误标记与之取与。
                boolean hadError = pageHadError;
                pageHadError = false;   // 每次加载结束复位
                if (url == null || !url.startsWith(panelUrl(MainActivity.this)) || hadError) {
                    panelLoaded = false;      // 让 tick 的下一拍再试
                    return;
                }
                panelLoaded = true;
                // 成功加载就重置重试计数（bug #18）：否则累计到 MAX_LOAD_TRIES 之后，
                // 本次 Activity 生命周期内再也不会自动 loadUrl（WebView 崩回错误页
                // 也只能靠用户手点「重载面板」）。
                loadTries = 0;
                if (!injected) {
                    // 成功后才置 injected（bug-04）：此前先置位再注入，读不到 key
                    // （首次启动 ensureConfig 还没落盘 / 文件被清 / 解析失败）时只弹一次
                    // toast，此后 onPageFinished 不再注入 → 面板所有 /panel/api/* 一直 401，
                    // 用户只能手点「重载面板」再赌一次同样的时序。
                    injectKey();
                }
            }

            // 新签名（bug-15）：旧签名 onReceivedError(WebView,int,String,String) 在部分
            // ROM/WebView 上对**子资源**失败也会回调 —— 图片/脚本 404 就会置
            // pageHadError → panelLoaded=false → tick 反复自动重载面板（界面自己闪）。
            // 新回调带 WebResourceRequest，只在主框架失败时才算"这次加载失败"。
            @Override public void onReceivedError(WebView v, android.webkit.WebResourceRequest req,
                                                 android.webkit.WebResourceError err) {
                if (req != null && !req.isForMainFrame()) return;   // 子资源失败：忽略
                pageHadError = true;
                panelLoading = false;
                panelLoaded = false;
            }

            // 旧签名保留给 API < 23（新回调不会触发）；API 23+ 时它仍可能被调用，
            // 用 SDK 判断避免与上面的主框架判定重复计数。
            @SuppressWarnings("deprecation")
            @Override public void onReceivedError(WebView v, int code, String desc, String failingUrl) {
                if (Build.VERSION.SDK_INT >= 23) return;   // 新回调已处理
                pageHadError = true;
                panelLoading = false;
                panelLoaded = false;
            }

            // 面板里的外链（文档、上游状态页）一律交给系统浏览器（issue B12）：
            // 否则 WebView 会被导航出面板，而 panelLoaded 仍为 true → tick 不再拉回来，
            // 用户只能靠返回键硬退。
            @Override public boolean shouldOverrideUrlLoading(WebView v, String url) {
                if (url == null || url.startsWith(panelUrl(MainActivity.this))) return false;  // 面板内部：照常
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url)));
                } catch (Throwable ignored) {}
                return true;
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override public void onProgressChanged(WebView v, int p) {
                progress.setProgress(p);
                progress.setVisibility(p >= 100 ? View.GONE : View.VISIBLE);
            }
        });
        root.addView(web, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        setContentView(root);

        // 用户主动打开 App = 明确要跑：清掉「用户停止」标记（bug 22），
        // 否则下次开机 BootReceiver 会继续跳过自启，用户以为坏了。
        try {
            getSharedPreferences(GatewayService.PREF, MODE_PRIVATE).edit()
                    .putBoolean("user_stop", false).apply();
        } catch (Throwable ignored) {}
        GatewayService.start(this);
        apiKey = readApiKey();
        // 这里不再 tick()（bug #16）：onStart 会调 tick() + scheduleTick()，
        // onCreate 里再来一次会让首屏连做两次端口探测与两次状态渲染。
        // 不再「固定 1500ms 后硬加载」（H4）：设备慢/网关冷启动/看门狗退避时那一发
        // 必然打空，而 onPageFinished 连 WebView 的错误页都会回调 —— 于是被记成
        // 「已注入」，之后再也不自动重试，用户对着错误页只能手点重载。
        // 改成由 tick 里的真实探测结果驱动（probePort 在后台线程做 connect）。
        // 首帧渲染交给 onStart（见 bug #16）。
    }

    // ── 小控件工厂（纯代码建视图，不用 XML 布局）

    TextView txt(String s, float sp, int color) {
        TextView t = new TextView(this);
        t.setText(s);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, sp);
        t.setTextColor(color);
        return t;
    }

    View spacer() {
        View v = new View(this);
        v.setLayoutParams(new LinearLayout.LayoutParams(0, 1, 1f));
        return v;
    }

    TextView chip(String label, View.OnClickListener l) {
        TextView t = txt(label, 11.5f, INK2);
        t.setGravity(Gravity.CENTER);
        t.setSingleLine(true);
        t.setEllipsize(TextUtils.TruncateAt.END);
        t.setPadding(dp(4), dp(9), dp(4), dp(9));
        t.setClickable(true);
        t.setFocusable(true);
        StateListDrawable sd = new StateListDrawable();
        sd.addState(new int[]{android.R.attr.state_pressed}, box(SURFACE2, ACCENT));
        sd.addState(new int[0], box(SURFACE2, LINE));
        t.setBackground(sd);
        t.setOnClickListener(l);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f);
        lp.setMargins(dp(3), 0, dp(3), 0);
        t.setLayoutParams(lp);
        return t;
    }

    GradientDrawable box(int fill, int stroke) {
        GradientDrawable g = new GradientDrawable();
        g.setShape(GradientDrawable.RECTANGLE);
        g.setCornerRadius(dp(7));               // 与面板 button 的 7px 圆角一致
        g.setColor(fill);
        g.setStroke(dp(1), stroke);
        return g;
    }

    GradientDrawable oval(int color) {
        GradientDrawable g = new GradientDrawable();
        g.setShape(GradientDrawable.OVAL);
        g.setColor(color);
        return g;
    }

    int dp(int v) { return (int) (v * getResources().getDisplayMetrics().density + 0.5f); }

    // ── 状态刷新

    void tick() {
        // 判定按"事实"，不按服务自报的中文串（上一版就是猜串，串一变就误报"已停止"）：
        //   端口在服务            → 运行中
        //   自称在跑但端口不通    → 启动中（第三行给出原因）
        //   服务活着但还没接管    → 启动中
        //   服务没起来            → 已停止（第三行给出原因）
        int st = GatewayService.statusOf();
        String word;
        int c;
        if (!probed) {                                          // 首次探测还没回来，别急着喊"已停止"
            word = "启动中"; c = WARN;
        } else if (portServing) {
            word = (st == GatewayService.ST_EXTERNAL) ? "运行中（外部）" : "运行中";
            c = OK;
        } else if (st == GatewayService.ST_RUNNING || st == GatewayService.ST_EXTERNAL) {
            word = "启动中"; c = WARN;
        } else if (st == GatewayService.ST_STARTING) {
            word = "启动中"; c = WARN;
        } else {
            word = "已停止"; c = BAD;
        }
        stateLabel.setText(word);
        stateLabel.setTextColor(c);
        dot.setBackground(oval(c));

        StringBuilder sb = new StringBuilder(GatewayService.wakeHeld() ? "保活中" : "未保活");
        sb.append(" · ").append(exemptNow() ? "省电✓" : "省电✗");
        sb.append(overlayOk() ? " 图层✓" : " 图层✗");
        if (GatewayService.freezeCount() > 0) sb.append(" · 冻过 ").append(GatewayService.freezeCount());
        if (GatewayService.restartCount() > 0) sb.append(" · 自愈 ").append(GatewayService.restartCount());
        if (GatewayService.appRestartCount() > 0) sb.append(" · 系统重启 ").append(GatewayService.appRestartCount());
        // 常驻图层失败单独提示（bug B15）：不占用 reason（那是上面的「为什么不可用」），
        // 只在这里补一句，避免次要提示盖掉「启动被系统拒」这类关键原因。
        String oerr = GatewayService.overlayError();
        if (oerr != null && !oerr.isEmpty()) sb.append(" · ").append(oerr);
        // 端口与运行模式分开显示（报告 #18）：此前把 mode（"直接 exec" / "linker 代跑"）
        // 直接拼在 `:端口` 的位置上，看起来像个端口号，而端口本身写死 7863。
        sb.append(" · :").append(GwConfig.port(this));
        String mode = GatewayService.modeText();
        if (!mode.isEmpty()) sb.append(" · ").append(mode);
        info.setText(sb.toString());

        // 第三行：通了给最后一行日志，不通给"为什么"（这才是能拿去做事的信息）
        if (!probed) {
            logLine.setText("正在探测 127.0.0.1:" + GwConfig.port(this) + " …");
            logLine.setTextColor(INK3);
        } else if (portServing) {
            String l = GatewayService.lastLogLine();
            logLine.setText(l.isEmpty() ? "等待日志…" : l);
            logLine.setTextColor(INK3);
        } else {
            String why = GatewayService.reasonText();
            String ex = GatewayService.lastExitText();
            if (!ex.isEmpty() && !why.contains(ex)) why = why + " ｜ 上次：" + ex;
            logLine.setText(why);
            logLine.setTextColor(WARN);
        }

        // 面板加载：端口真的通了才加载（tick 每 2 秒一拍，最多试 MAX_LOAD_TRIES 次）。
        // 必须判「上一次还在加载」（bug #1）：低端机上 WebView 冷启动 + 面板大 HTML/JS
        // 普遍超过 2 秒，不判在途的话从第 2 拍起每 2 秒把正在进行的加载打断重来，
        // 30 次（≈60 秒）后 loadTries 用尽 → 永久空白页，只能手点「重载面板」。
        // 加载在途判定加超时兜底（报告 #21）：panelLoading 只在 onPageFinished /
        // onReceivedError 复位，而 WebView 渲染进程崩溃、页面被系统回收时两个回调
        // 都不来 —— panelLoading 永远 true，之后不再自动加载，只能手点「重载面板」。
        boolean loadingFresh = panelLoading
                && (SystemClock.elapsedRealtime() - panelLoadAt) < PANEL_LOAD_TIMEOUT_MS;
        if (!panelLoaded && !loadingFresh && portServing && web != null
                && loadTries < MAX_LOAD_TRIES) {
            panelLoading = true;
            panelLoadAt = SystemClock.elapsedRealtime();
            loadTries++;
            pageHadError = false;   // 新一次加载：清掉上一次的错误标记（bug B2）
            web.loadUrl(panelUrl(this));
        }

        probePort();
    }

    // ── 可见性闸门（M3）：tick 只在界面可见时跑。
    // 此前循环只在 onDestroy 里停，而按 Home 键只走 onStop、Activity 不销毁 ——
    // 进程常驻后台期间仍然每 2 秒发一次 socket connect + 两次主线程 post，
    // 叠加服务端的 partial wakelock 就是实打实的耗电。
    volatile boolean visible = true;
    volatile boolean tickQueued = false;

    void scheduleTick() {
        if (tickQueued) return;
        tickQueued = true;
        ui.postDelayed(new Runnable() {
            public void run() {
                tickQueued = false;
                if (!visible) return;      // 已退到后台：不再自投递，onStart 会重新拉起
                tick();
                scheduleTick();
            }
        }, 2000);
    }

    @Override protected void onStart() {
        super.onStart();
        visible = true;
        tick();
        scheduleTick();
    }

    @Override protected void onStop() {
        visible = false;
        super.onStop();
    }

    // ── 端口探针：事实来源，且只能在后台线程做（主线程连 150ms 也不能赌）
    final java.util.concurrent.ExecutorService probe = java.util.concurrent.Executors.newSingleThreadExecutor();
    volatile boolean portServing = false;
    volatile boolean probing = false;
    volatile boolean probed = false;
    // 面板加载状态（H4）：panelLoaded 只在真落在面板地址上才置 true；
    // loadTries 限制自动重试次数，避免死循环打空。
    volatile boolean panelLoaded = false;
    volatile boolean panelLoading = false;   // 有一次 loadUrl 在途（防反复打断，bug #1）
    // panelLoadAt 本次加载开始时刻；超过 PANEL_LOAD_TIMEOUT_MS 视为「在途但已死」
    // （报告 #21：渲染进程崩溃时 onPageFinished/onReceivedError 都不来）。
    volatile long panelLoadAt = 0;
    static final long PANEL_LOAD_TIMEOUT_MS = 30000;
    volatile boolean pageHadError = false;   // 本次加载是否已报错（bug B2）
    volatile int loadTries = 0;
    static final int MAX_LOAD_TRIES = 30;

    void probePort() {
        if (probing) return;
        probing = true;
        try {
            probe.execute(new Runnable() {
                public void run() {
                    final boolean up = GatewayService.portUp();
                    ui.post(new Runnable() {
                        public void run() { portServing = up; probed = true; probing = false; }
                    });
                }
            });
        } catch (Throwable t) {
            // 线程池已关闭（Activity 正在销毁）：吞掉即可，绝不能让主线程崩。
            probing = false;
        }
    }

    /**
     * 把 api_key 写进面板的 localStorage 再重载。
     *
     * 不能用「写完睡 400ms 就 reload」（issue B5）：低端机冷启动 / WebView 首帧慢时，
     * reload 会抢在写入生效之前，面板拿不到 key → 所有 /panel/api/* 401，用户点
     * 「重载面板」又走同一条竞速，只能反复赌。
     * 现在写入后**回读校验**，确认 localStorage 里确实是目标值才 reload；
     * 不一致就重试（最多 3 次，每次 300ms），仍失败就在状态条上明说。
     */
    void injectKey() { injectKey(0); }

    void injectKey(final int attempt) {
        apiKey = readApiKey();
        if (apiKey.isEmpty()) {
            // 读不到就保持 injected=false（bug-04）：下一次 onPageFinished 还会再试，
            // 而不是"弹一次 toast 然后永久放弃"。
            injected = false;
            toast("读不到 config.json 里的 api_key");
            return;
        }
        if (web == null) return;
        final String want = apiKey;
        String js = "try{localStorage.setItem('" + LS_KEY + "'," + quote(want) + ");"
                + "localStorage.getItem('" + LS_KEY + "');}catch(e){null}";
        web.evaluateJavascript(js, new android.webkit.ValueCallback<String>() {
            public void onReceiveValue(String got) {
                // evaluateJavascript 把字符串结果再包一层 JSON：带引号才算成功写入
                boolean ok = got != null && got.contains(want);
                if (ok) {
                    injected = true;   // 成功才置位（bug-04）
                    web.reload();
                    return;
                }
                if (attempt >= 2) {
                    // 放弃本轮但保持 false：下一次页面加载仍会自动重试（bug-04）。
                    injected = false;
                    logLine.setText("自动登录失败：面板拿不到 api_key，请点「重载面板」重试");
                    logLine.setTextColor(WARN);
                    return;
                }
                ui.postDelayed(new Runnable() {
                    public void run() { injectKey(attempt + 1); }
                }, 300);
            }
        });
    }

    static String quote(String s) {
        StringBuilder sb = new StringBuilder("\"");
        for (char c : s.toCharArray()) {
            if (c == '"' || c == '\\') sb.append('\\').append(c);
            else if (c < 0x20) sb.append(String.format("\\u%04x", (int) c));
            else sb.append(c);
        }
        return sb.append('"').toString();
    }

    String readApiKey() {
        File cfg = new File(getFilesDir(), "gateway/config.json");
        String txt = readAll(cfg);
        Matcher m = Pattern.compile("\"api_key\"\\s*:\\s*\"([^\"]*)\"").matcher(txt);
        if (m.find()) return m.group(1);
        return "";
    }

    static String readAll(File f) {
        try (FileInputStream in = new FileInputStream(f)) {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[4096];
            int n;
            while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            return new String(bos.toByteArray(), "UTF-8");
        } catch (Throwable t) { return ""; }
    }

    void copyApi() {
        String key = apiKey.isEmpty() ? readApiKey() : apiKey;
        String txt = "Base URL: http://127.0.0.1:" + GwConfig.port(this) + "/v1\nAPI Key: " + key +
                "\n(局域网: 把 127.0.0.1 换成手机 IP)";
        try {
            ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
            cm.setPrimaryClip(ClipData.newPlainText("wb2api", txt));
            toast("已复制 API 信息");
        } catch (Throwable t) { toast(txt); }
    }

    void restart() {
        GatewayService.restart(this);
        injected = false;
        ui.postDelayed(new Runnable() { public void run() {
            pageHadError = false;   // 重启后重载：清错误标记（bug B2）
            panelLoaded = false;
            loadTries = 0;
            web.reload();
        } }, 2000);
        toast("网关重启中…");
    }

    /** 有没有被系统排除在省电优化之外（没排除的话 Doze 会掐网络、推后 alarm） */
    boolean exemptNow() {
        try {
            PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
            return pm.isIgnoringBatteryOptimizations(getPackageName());
        } catch (Throwable t) { return false; }
    }

    void requestExempt() {
        try {
            if (exemptNow()) { toast("已在省电白名单 ✓"); return; }
            Intent i = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS);
            i.setData(Uri.parse("package:" + getPackageName()));
            startActivity(i);
        } catch (Throwable t) {
            try { startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)); }
            catch (Throwable t2) { toast("请到 设置→电池 里允许本应用后台运行 / 自启动"); }
        }
    }

    void toast(String s) { Toast.makeText(this, s, Toast.LENGTH_SHORT).show(); }

    /** 「显示在其他应用上层」= 能不能挂常驻图层（挂了系统才不会把我当缓存进程冻住） */
    boolean overlayOk() {
        try { return Build.VERSION.SDK_INT < 23 || Settings.canDrawOverlays(this); }
        catch (Throwable t) { return false; }
    }

    /** 精确 alarm 能用吗（Android 12+ 的「闹钟与提醒」特殊权限） */
    boolean exactOk() {
        try {
            if (Build.VERSION.SDK_INT < 31) return true;
            android.app.AlarmManager am = (android.app.AlarmManager) getSystemService(Context.ALARM_SERVICE);
            return am == null || am.canScheduleExactAlarms();
        } catch (Throwable t) { return true; }   // 判定不了就别拦着用户
    }

    /**
     * 保活设置：缺哪补哪，按顺序一次点一下。
     *
     * 为什么需要三项：它们是**三条不同的系统通道**，缺任何一条都能让"过一会儿连不上"重现 ——
     * ① 省电白名单：不被 Doze 掐网络 / 推后 alarm；
     * ② 常驻图层：让系统认为我有可见窗口 → 不把我判成缓存态 → 不进 app-freezer（本轮主因）；
     * ③ 精确闹钟：兜底 alarm 不被 Doze 从 5 分钟拖到 ~9 分钟。
     */
    void keepAliveWizard() {
        if (!exemptNow()) {
            toast("① 先允许「忽略电池优化」，回来再点一次");
            GatewayService.appLog(this, "保活①：请求忽略电池优化");
            requestExempt();
            return;
        }
        if (!overlayOk()) {
            toast("② 开「显示在其他应用上层」→ 常驻图层让系统不把我冻住");
            GatewayService.appLog(this, "保活②：请求「显示在其他应用上层」（防冻结关键）");
            try {
                startActivity(new Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION,
                        Uri.parse("package:" + getPackageName())));
            } catch (Throwable t) {
                try { startActivity(new Intent(Settings.ACTION_MANAGE_OVERLAY_PERMISSION)); }
                catch (Throwable t2) { toast("请到 设置→应用→特殊权限 里给「显示在其他应用上层」"); }
            }
            return;
        }
        if (!exactOk()) {
            toast("③ 允许「闹钟与提醒」→ 兜底 alarm 才精确（可选，推荐）");
            GatewayService.appLog(this, "保活③：请求「闹钟与提醒」（精确 alarm）");
            try {
                startActivity(new Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM,
                        Uri.parse("package:" + getPackageName())));
            } catch (Throwable t) { toast("请到 设置→应用→特殊权限 里允许「闹钟与提醒」"); }
            return;
        }
        GatewayService.start(this);     // 立刻把锁和图层挂上（onStartCommand → holdWake）
        GatewayService.appLog(this, "保活三项齐：省电白名单 + 常驻图层 + 精确闹钟");
        toast("保活三项齐了：省电白名单 + 常驻图层 + 精确闹钟 ✓");
    }

    void askNotifPermission() {
        if (Build.VERSION.SDK_INT >= 33) {
            try { requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 1001); }
            catch (Throwable ignored) {}
        }
    }

    @Override protected void onDestroy() {
        // 顺序要紧：先停掉主 Looper 上的 tick 自投递，再关线程池。
        // 此前只 shutdownNow()、从不 removeCallbacks —— 已投递的那一发 tick 仍会在
        // Activity 销毁后执行，probePort() 往已关闭的 ExecutorService 里 execute()，
        // 主线程抛 RejectedExecutionException 直接 FATAL EXCEPTION，同进程的
        // GatewayService 一起陪葬（和「常驻网关」的设计前提直接冲突）。
        try { ui.removeCallbacksAndMessages(null); } catch (Throwable ignored) {}
        try { probe.shutdownNow(); } catch (Throwable ignored) {}
        // WebView 必须显式销毁：它持有 Activity 上下文，不 destroy 就永久泄漏。
        try { if (web != null) { web.destroy(); web = null; } } catch (Throwable ignored) {}
        super.onDestroy();
    }

    @Override public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }
}
