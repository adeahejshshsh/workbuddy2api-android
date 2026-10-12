package com.dsh.wb2api;

import android.app.Activity;
import android.app.Dialog;
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
import android.widget.ScrollView;
import android.widget.HorizontalScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.RandomAccessFile;
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
    static final String PANEL_URL = "http://127.0.0.1:7863/panel/";
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
            public void onClick(View v) { injected = false; web.reload(); }
        }));
        bar.addView(chip("日志", new View.OnClickListener() {
            public void onClick(View v) { openLog(); }
        }));
        // 动作条改成横向可滚：芯片再多也不会被窄屏裁掉
        HorizontalScrollView hs = new HorizontalScrollView(this);
        hs.setHorizontalScrollBarEnabled(false);
        hs.setBackgroundColor(SURFACE);
        hs.addView(bar);
        root.addView(hs);

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
        if (Build.VERSION.SDK_INT >= 21) s.setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        web.setBackgroundColor(BG);
        web.setOverScrollMode(View.OVER_SCROLL_NEVER);
        web.setWebViewClient(new WebViewClient() {
            @Override public void onPageFinished(WebView v, String url) {
                progress.setVisibility(View.GONE);
                if (!injected && url != null && url.contains("/panel")) {
                    injected = true;
                    injectKey();
                }
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

        GatewayService.start(this);
        apiKey = readApiKey();
        ui.postDelayed(new Runnable() { public void run() { web.loadUrl(PANEL_URL); } }, 1500);
        tick();
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
        String mode = GatewayService.modeText();
        sb.append(" · ").append(mode.isEmpty() ? ":7863" : mode);
        info.setText(sb.toString());

        // 第三行：通了给最后一行日志，不通给"为什么"（这才是能拿去做事的信息）
        if (!probed) {
            logLine.setText("正在探测 127.0.0.1:7863 …");
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

        probePort();
        ui.postDelayed(new Runnable() { public void run() { tick(); } }, 2000);
    }

    // ── 端口探针：事实来源，且只能在后台线程做（主线程连 150ms 也不能赌）
    final java.util.concurrent.ExecutorService probe = java.util.concurrent.Executors.newSingleThreadExecutor();
    volatile boolean portServing = false;
    volatile boolean probing = false;
    volatile boolean probed = false;

    void probePort() {
        if (probing) return;
        probing = true;
        probe.execute(new Runnable() {
            public void run() {
                final boolean up = GatewayService.portUp();
                ui.post(new Runnable() {
                    public void run() { portServing = up; probed = true; probing = false; }
                });
            }
        });
    }

    void injectKey() {
        apiKey = readApiKey();
        if (apiKey.isEmpty()) { toast("读不到 config.json 里的 api_key"); return; }
        String js = "try{localStorage.setItem('" + LS_KEY + "'," + quote(apiKey) + ");}catch(e){}";
        web.evaluateJavascript(js, null);
        ui.postDelayed(new Runnable() { public void run() { web.reload(); } }, 400);
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
        String txt = "Base URL: http://127.0.0.1:7863/v1\nAPI Key: " + key +
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
        ui.postDelayed(new Runnable() { public void run() { web.reload(); } }, 2000);
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

    // ───────────────────────── 详细日志 ─────────────────────────
    /**
     * 日志页：App 事件（系统什么时候冻/杀/拒启动，只有 App 知道）+ 网关 stdout/stderr 尾部
     *（任务中心执行了什么、上游回了什么，全在里面）。
     *
     * 为什么要有这一页：日志其实一直在写（`getFilesDir()/gateway/data/server.log`），
     * 但手机上既没入口也没复制按钮 —— 等于没有日志。
     */
    void openLog() {
        final StringBuilder sb = new StringBuilder();
        sb.append("=== ① App 事件（").append(stat(GatewayService.appLogFile(this))).append("）===\n");
        sb.append(tailOf(GatewayService.appLogFile(this), 256 * 1024, 300)).append('\n');
        sb.append("\n=== ② 网关日志（").append(stat(GatewayService.gwLogFile(this))).append("）===\n");
        sb.append(tailOf(GatewayService.gwLogFile(this), 512 * 1024, 600));
        final String text = mask(sb.toString().trim());

        TextView tv = txt(text.isEmpty() ? "（还没有日志：网关还没跑起来过，或日志刚被清）" : text, 10.5f, INK2);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setTextIsSelectable(true);
        tv.setPadding(dp(10), dp(8), dp(10), dp(8));

        ScrollView sv = new ScrollView(this);
        sv.setBackgroundColor(BG);
        sv.addView(tv, new ViewGroup.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT,
                                                  ViewGroup.LayoutParams.WRAP_CONTENT));

        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackgroundColor(SURFACE);
        box.addView(sv, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        LinearLayout btns = new LinearLayout(this);
        btns.setOrientation(LinearLayout.HORIZONTAL);
        btns.setPadding(dp(9), dp(8), dp(9), dp(8));
        btns.setBackgroundColor(SURFACE);
        btns.addView(chip("复制全部", new View.OnClickListener() {
            public void onClick(View v) { copyText(text); }
        }));
        final Dialog d = new Dialog(this);
        btns.addView(chip("刷新", new View.OnClickListener() {
            public void onClick(View v) { d.dismiss(); openLog(); }
        }));
        btns.addView(chip("关闭", new View.OnClickListener() {
            public void onClick(View v) { d.dismiss(); }
        }));
        box.addView(btns);

        d.setTitle("详细日志（复制后可直接发给作者）");
        d.setContentView(box);
        d.show();
    }

    /** 复制到剪贴板 */
    void copyText(String s) {
        try {
            ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
            cm.setPrimaryClip(ClipData.newPlainText("wb2api", s));
            toast("已复制 " + s.length() + " 字符");
        } catch (Throwable t) { toast("复制失败：" + t); }
    }

    /** 文件摘要（名字 + 大小）：一眼看出日志到底有没有在写 */
    static String stat(File f) {
        try { return f.getName() + "，" + (f.length() / 1024) + " KB"; }
        catch (Throwable t) { return f.getName(); }
    }

    /**
     * 读文件尾部（最多 maxBytes 字节、maxLines 行）。不整个读进内存 ——
     * 网关日志能到几 MB，整读会 OOM。
     */
    static String tailOf(File f, int maxBytes, int maxLines) {
        RandomAccessFile raf = null;
        try {
            if (!f.exists() || f.length() == 0) return "（空）";
            raf = new RandomAccessFile(f, "r");
            long len = raf.length();
            int want = (int) Math.min(len, maxBytes);
            long from = len - want;
            byte[] buf = new byte[want];
            raf.seek(from);
            raf.readFully(buf);
            String s = new String(buf, "UTF-8");
            if (from > 0) {                       // 从半行开始 → 丢掉残行
                int nl = s.indexOf('\n');
                if (nl >= 0) s = s.substring(nl + 1);
            }
            String[] lines = s.split("\n", -1);
            if (lines.length > maxLines) {
                StringBuilder out = new StringBuilder();
                for (int i = lines.length - maxLines; i < lines.length; i++) out.append(lines[i]).append('\n');
                return out.toString();
            }
            return s;
        } catch (Throwable t) {
            return "（读取失败：" + t + "）";
        } finally {
            try { if (raf != null) raf.close(); } catch (Throwable ignored) {}
        }
    }

    /** 脱敏：日志里可能带 api_key / Authorization —— 复制出去前一律打码 */
    static String mask(String s) {
        if (s == null) return "";
        s = s.replaceAll("(?i)(bearer\\s+)[A-Za-z0-9._\\-]{6,}", "$1***");
        s = s.replaceAll("(wb2api-)[A-Za-z0-9]{6,}", "$1***");
        s = s.replaceAll("(sk-)[A-Za-z0-9_\\-]{6,}", "$1***");
        return s;
    }

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
        try { probe.shutdownNow(); } catch (Throwable ignored) {}
        super.onDestroy();
    }

    @Override public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }
}
