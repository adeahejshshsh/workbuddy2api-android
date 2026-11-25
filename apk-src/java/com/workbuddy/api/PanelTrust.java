package com.workbuddy.api;

import android.content.Context;
import android.os.Handler;
import android.os.Looper;
import android.util.AtomicFile;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 「对面这个 127.0.0.1:PORT 到底是不是本进程拉起的网关」的唯一判定入口（W6-H-02）。
 *
 * <p>为什么需要独立一层：注入 {@code admin_api_key} 的地方不止一处（{@link WebPanelActivity}
 * 与 {@link MainActivity} 的 fallback WebView），而「校验」如果各写一遍，早晚只修一处。
 * 这里只暴露 {@link #verify}：成功即代表对方的 instance_id 与本机
 * {@code files/gateway/data/instance_id} 一致。
 *
 * <p>判定为什么可信：instance_id 是应用私有目录里的 32 hex（0700），别的 App 读不到；
 * 校验时**不把自己的 id 发过去**，所以占位进程也没法靠回显蒙混。
 *
 * <p>失败一律 fail-closed：调用方拿到 {@code ok=false} 就不许注入，只提示原因。
 */
final class PanelTrust {

    interface Cb {
        /**
         * 在**主线程**回调。reason 见 {@link #message(String)}。
         *
         * @param actualId 对面报出的实例标识（解析失败为空串）—— 用于「信任记忆」与
         *                 chip 上的短指纹，让「为什么没自动登录」一眼可见。
         */
        void done(boolean ok, String reason, String actualId);
    }

    static final String OK = "ok";
    static final String NO_ID = "no-id";
    static final String OLD_GATEWAY = "old-gateway";
    static final String HTTP_ERROR = "http";
    /**
     * 对面确实报了一个合法的 32 hex 实例标识，但与本机那份不同。
     *
     * <p>这与「端口被第三方占用」是**两件不同的事**：能正确应答
     * {@code /panel/api/instance} 并给出 32 hex 的，只可能是 workbuddy2api 网关
     * （这个接口是本项目独有的），所以对面是**同一项目的另一个实例** ——
     * 例如命令行侧/容器里跑的那份、旧版本残留的孤儿进程、或者本 App 早先拉起后
     * 换了 instance_id 的那份。它的 admin_api_key 通常也与本 App 配置里的不同，
     * 这才是「注入了密钥却仍然 401」的真实原因。
     */
    static final String SAME_KIND = "same-kind";
    static final String MISMATCH = "mismatch";
    static final String NO_CONN = "no-conn";

    private PanelTrust() {}

    /**
     * 后台线程探测，结果切回主线程。
     *
     * <p><b>主判据是「密钥被服务端接受」，不是 instance_id 比对</b>。原因（真机反馈）：
     * 本应用记录的实例与端口实际返回明明一致，面板探测却拿到空串 —— 探测受时序、
     * WebView 网络占用影响偶发失败，一旦把它当唯一判据，用户填对密钥也会被拦在门外，
     * 而且 verify 失败时压根不注入，记住的密钥永远没机会被用，表现就是「每次都要填」。
     *
     * <p>能接受这把 32-hex admin key 的只可能是本项目的网关（key 只存在本 App 私有
     * 目录与网关 config.json），所以「keyStatus==200」是比 instance 探测更强的身份证明。
     * instance_id 降级为**辅助信息**（chip 上显示指纹），不再单独决定是否放行。
     *
     * <p>判定顺序：
     * <ol>
     *   <li>手上有 key（keyOverride 或 config.json）且服务端接受 → OK，直接注入；</li>
     *   <li>否则按 instance 探测给原因：连不上 / 旧网关 / id 匹配 / 信任记忆命中；</li>
     *   <li>都不命中 → SAME_KIND 或 MISMATCH，交给用户判断（不自动注入）。</li>
     * </ol>
     */
    static void verify(final Context c, final Cb cb) {
        if (c == null || cb == null) return;
        final int port = GwConfig.port(c);
        final String expect = InstanceId.read(c);
        final String trustedId = trustedInstanceId(c, port);
        // 注入用的同一把 key：用户为「另一个实例」手输的优先，回落 config.json。
        String k = keyOverride(c);
        if (k.isEmpty()) k = PanelApi.key(c);
        final String key = k;
        new Thread(new Runnable() {
            public void run() {
                final String reason;
                String actual = "";
                Probe p = probe(port);
                actual = p.code == 200 ? InstanceId.parseId(p.body) : "";

                // ① 密钥被对面接受 = 自家网关，直接放行（最强判据，绕开探测时序问题）。
                //    只有 keyStatus 明确 200 才算；401/403/连不上都不算，继续走下面的
                //    instance 判据，避免把「对面恰好也认这把 key」当成万能通行证。
                if (!key.isEmpty() && keyAccepted(keyStatus(port, key))) {
                    reason = OK;
                } else if (expect.isEmpty()) {
                    reason = NO_ID;
                } else if (p.code == 0) {
                    // 连不上：网关没起、或被别的进程占着但不 HTTP 应答。
                    reason = NO_CONN;
                } else if (p.code == 404) {
                    reason = OLD_GATEWAY;
                } else if (p.code != 200) {
                    reason = HTTP_ERROR + ":" + p.code;
                } else if (InstanceId.matches(expect, actual)) {
                    reason = OK;
                } else if (!actual.isEmpty() && actual.equals(trustedId)) {
                    // 用户确认过这个实例：它的密钥是「密钥覆盖」里那份（见 WebPanelActivity）
                    reason = OK;
                } else if (hasTrustedKey(c, port) && !key.isEmpty()) {
                    // 用户为这个端口确认并保存过密钥（探测偶尔报空，不能让它反复拦人）。
                    // 注入后 confirmKeyAccepted 会再验一次；若密钥失效会引导重填。
                    reason = OK;
                } else {
                    // 能解析出合法 32 hex = 对面是本项目的另一个实例（见 SAME_KIND）；
                    // 解析不出 = 应答形状不对，按「身份不明的占用者」处理（更严）。
                    reason = actual.isEmpty() ? MISMATCH : SAME_KIND;
                }
                final String actualOut = actual;
                new Handler(Looper.getMainLooper()).post(new Runnable() {
                    public void run() { cb.done(OK.equals(reason), reason, actualOut); }
                });
            }
        }, "wb2api-instance-check").start();
    }

    // ---------- 密钥是否真的被对面接受 ----------

    /**
     * 用一个**指定**密钥去打一个需要鉴权的只读接口，判断这把钥匙对面认不认。
     *
     * <p>为什么必须有这一步：注入成功 ≠ 登录成功。注入只证明「JS 把字符串写进了
     * sessionStorage」，而面板首屏要的是「服务端接受这把 key」。当端口上跑的是
     * {@link #SAME_KIND}（同一项目的另一个实例）时，本 App config.json 里的密钥
     * 它根本不认 —— 于是「写入成功」之后照样 401、照样弹密钥门，用户看到的就是
     * 「点了信任还是要填密钥」。
     *
     * <p>端点选 {@code /panel/api/error_summary}：只读、聚合已有日志环、不触发上游
     * 请求，比 {@code /overview} 轻。它不在免鉴权表里，所以 401/403 就是「key 不对」。
     *
     * @return HTTP 状态码；0 = 连不上（此时不能断言 key 错，调用方按「未知」处理）
     */
    static int keyStatus(int port, String key) {
        if (key == null || key.isEmpty()) return 401;
        HttpURLConnection conn = null;
        try {
            URL u = new URL("http://127.0.0.1:" + port + "/panel/api/error_summary");
            conn = (HttpURLConnection) u.openConnection();
            conn.setRequestMethod("GET");
            conn.setConnectTimeout(1500);
            conn.setReadTimeout(2500);
            conn.setInstanceFollowRedirects(false);
            conn.setUseCaches(false);
            conn.setRequestProperty("Authorization", "Bearer " + key);
            conn.setRequestProperty("Accept", "application/json");
            return conn.getResponseCode();
        } catch (Throwable t) {
            return 0;
        } finally {
            if (conn != null) try { conn.disconnect(); } catch (Throwable ignored) {}
        }
    }

    /** 状态码 → 结论：true 只有一种，401/403 是明确的「不认这把钥匙」。 */
    static boolean keyAccepted(int code) {
        return code >= 200 && code < 300;
    }

    /** 明确的「密钥不对」（区别于「连不上/网关没起」）。 */
    static boolean keyRejected(int code) {
        return code == 401 || code == 403;
    }

    // ---------- 信任记忆 + 密钥覆盖（都在应用私有目录，0600） ----------

    /**
     * 「我确认过这个端口的实例」的持久化标记：{@code data/panel_trust.json}。
     *
     * <p>为什么需要：{@link #SAME_KIND} 在真机上很常见（命令行侧/容器里也跑了一份、
     * 旧版本残留的孤儿进程、App 重装后换了 instance_id 而网关还是旧进程），而原来的
     * 处理是「每次打开面板弹一次确认，确认了也只对本次会话有效」—— 用户感受就是
     * 「始终提示端口被占用，永远要手输密钥」。把确认结果按 (port, instance_id) 记住，
     * 同一个实例就不再打扰；id 一变自动失效，安全性不打折。
     */
    private static File trustFile(Context c) {
        return new File(new File(c.getFilesDir(), "gateway/data"), "panel_trust.json");
    }

    /**
     * 该端口上被用户确认过的实例标识；没有或读不出返回空串。
     *
     * <p>注意：探测偶尔报空（真机反馈：本应用记录的 id 与端口返回一致，探测却拿到空）。
     * 所以「有没有确认过」的权威判据是 {@link #keyOverride} 是否非空 —— 用户填过并通过
     * 服务端验证的密钥已经记住了，那这个端口就是用户点过头的。这里返回的 id 只用于
     * chip 显示与「id 一致」的快速路径，不作为唯一门禁。
     */
    static String trustedInstanceId(Context c, int port) {
        String txt = readSmall(trustFile(c), 4096);
        if (txt.isEmpty()) return "";
        Matcher m = Pattern.compile("\"port\"\\s*:\\s*(\\d+)").matcher(txt);
        if (!m.find()) return "";
        try {
            if (Integer.parseInt(m.group(1)) != port) return "";
        } catch (Throwable t) { return ""; }
        Matcher id = Pattern.compile("\"instance_id\"\\s*:\\s*\"([0-9a-f]{32})\"").matcher(txt);
        return id.find() ? id.group(1) : "";
    }

    /** 用户是否已经为某端口确认并保存了密钥（不依赖 instance 探测）。 */
    static boolean hasTrustedKey(Context c, int port) {
        return !keyOverride(c).isEmpty() && !trustedInstanceId(c, port).isEmpty();
    }

    /** 记住「这个端口的这个实例我认了」。写不进去只影响下次要不要再问，不影响本次注入。 */
    static void rememberTrust(Context c, int port, String instanceId) {
        if (c == null || instanceId == null || instanceId.isEmpty()) return;
        File f = trustFile(c);
        File dir = f.getParentFile();
        if (dir != null && !dir.exists() && !dir.mkdirs()) return;
        String body = "{\"port\":" + port + ",\"instance_id\":\"" + instanceId
                + "\",\"at\":" + (System.currentTimeMillis() / 1000) + "}";
        AtomicFile af = new AtomicFile(f);
        FileOutputStream os = null;
        try {
            os = af.startWrite();
            os.write(body.getBytes("UTF-8"));
            os.flush();
            af.finishWrite(os);
        } catch (Throwable t) {
            if (os != null) try { af.failWrite(os); } catch (Throwable ignored) {}
        }
    }

    /** 忘掉信任（用户在「强制注入」对话框里选了取消/密钥不对时清掉）。 */
    static void forgetTrust(Context c) {
        try { trustFile(c).delete(); } catch (Throwable ignored) {}
    }

    /**
     * 密钥覆盖：{@code data/panel_key}（0600，单行）。
     *
     * <p>{@link SAME_KIND} 场景下，本 App 的 config.json 里的密钥**不是**对面实例的密钥，
     * 注入了也只会 401。这里存用户手输的那一份，优先级高于 config.json ——
     * 否则「信任了这个实例」之后仍然每次 401，等于没修。
     */
    private static File keyOverrideFile(Context c) {
        return new File(new File(c.getFilesDir(), "gateway/data"), "panel_key");
    }

    static String keyOverride(Context c) {
        return readSmall(keyOverrideFile(c), 4096).trim();
    }

    static void rememberKey(Context c, String key) {
        if (c == null || key == null) return;
        String k = key.trim();
        if (k.isEmpty()) return;
        File f = keyOverrideFile(c);
        File dir = f.getParentFile();
        if (dir != null && !dir.exists() && !dir.mkdirs()) return;
        AtomicFile af = new AtomicFile(f);
        FileOutputStream os = null;
        try {
            os = af.startWrite();
            os.write(k.getBytes("UTF-8"));
            os.flush();
            af.finishWrite(os);
        } catch (Throwable t) {
            if (os != null) try { af.failWrite(os); } catch (Throwable ignored) {}
        }
    }

    static void forgetKey(Context c) {
        try { keyOverrideFile(c).delete(); } catch (Throwable ignored) {}
    }

    /** 读小文件（上限 bytes，超限或异常返回空串）。 */
    private static String readSmall(File f, int limit) {
        if (f == null || !f.isFile() || f.length() <= 0 || f.length() > limit) return "";
        InputStream in = null;
        try {
            in = new FileInputStream(f);
            ByteArrayOutputStream bo = new ByteArrayOutputStream();
            byte[] buf = new byte[512];
            int n, total = 0;
            while ((n = in.read(buf)) > 0) {
                total += n;
                if (total > limit) break;
                bo.write(buf, 0, n);
            }
            return bo.toString("UTF-8");
        } catch (Throwable t) {
            return "";
        } finally {
            if (in != null) try { in.close(); } catch (Throwable ignored) {}
        }
    }

    /** 给用户看的文案（不注入时的 toast / 状态条都走这里，避免两处口径分叉）。 */
    static String message(String reason) {
        if (reason == null) return "无法确认端口归属，已阻止注入管理密钥";
        if (reason.startsWith(HTTP_ERROR)) {
            return "无法确认端口归属（HTTP " + reason.substring(HTTP_ERROR.length() + 1)
                    + "），已阻止注入";
        }
        if (OLD_GATEWAY.equals(reason)) {
            return "网关未提供实例校验接口（版本较旧），为安全起见不注入管理密钥";
        }
        if (NO_CONN.equals(reason)) {
            return "连不上本机网关（可能未启动），未注入管理密钥";
        }
        if (NO_ID.equals(reason)) {
            return "读不到本机实例标识，已阻止注入";
        }
        if (SAME_KIND.equals(reason)) {
            return "端口上是另一个 workbuddy2api 实例（实例标识不同），"
                    + "它不认本应用配置里的密钥";
        }
        if (MISMATCH.equals(reason)) {
            return "端口被其他应用占用（实例接口应答不合规），已阻止注入";
        }
        return "已阻止注入管理密钥";
    }

    /** http 状态码 + 响应体（只读前 64KB）。 */
    private static final class Probe {
        final int code;
        final String body;

        Probe(int code, String body) {
            this.code = code;
            this.body = body;
        }
    }

    private static Probe probe(int port) {
        HttpURLConnection conn = null;
        try {
            URL u = new URL("http://127.0.0.1:" + port + "/panel/api/instance");
            conn = (HttpURLConnection) u.openConnection();
            conn.setRequestMethod("GET");
            conn.setConnectTimeout(1500);
            conn.setReadTimeout(2000);
            // 不跟随重定向：30x 说明对面不是直接应答的网关，跟过去等于让它把我们导到
            // 一个它控制的地址上再"证明"自己是网关。
            conn.setInstanceFollowRedirects(false);
            conn.setUseCaches(false);
            conn.setRequestProperty("Accept", "application/json");
            conn.setRequestProperty("Cache-Control", "no-store");
            int code = conn.getResponseCode();
            InputStream in = null;
            try {
                in = (code >= 200 && code < 300) ? conn.getInputStream() : conn.getErrorStream();
                if (in == null) return new Probe(code, "");
                ByteArrayOutputStream bo = new ByteArrayOutputStream();
                byte[] buf = new byte[4096];
                int n, total = 0;
                while ((n = in.read(buf)) > 0) {
                    total += n;
                    if (total > 64 * 1024) break;   // 响应体上限：别被大响应拖住
                    bo.write(buf, 0, n);
                }
                return new Probe(code, bo.toString("UTF-8"));
            } finally {
                if (in != null) try { in.close(); } catch (Throwable ignored) {}
            }
        } catch (Throwable t) {
            return new Probe(0, "");
        } finally {
            if (conn != null) try { conn.disconnect(); } catch (Throwable ignored) {}
        }
    }
}
