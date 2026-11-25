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
     * <p><b>信任记忆</b>：若此前用户明确确认过「这个端口上那个实例就是我自己的」，
     * 且对面现在报的 id 与当初确认的**完全一致**，直接判 OK。判据是 id 而不是端口 ——
     * 端口可能换主，id 换不了（换了就说明是另一个实例，要重新确认）。
     * 这样「强制注入一次」不再只是本次会话有效，不会每次打开面板都重来一遍。
     */
    static void verify(final Context c, final Cb cb) {
        if (c == null || cb == null) return;
        final int port = GwConfig.port(c);
        final String expect = InstanceId.read(c);
        final String trustedId = trustedInstanceId(c, port);
        new Thread(new Runnable() {
            public void run() {
                final String reason;
                String actual = "";
                if (expect.isEmpty()) {
                    reason = NO_ID;
                } else {
                    Probe p = probe(port);
                    actual = p.code == 200 ? InstanceId.parseId(p.body) : "";
                    if (p.code == 0) {
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
                    } else {
                        // 能解析出合法 32 hex = 对面是本项目的另一个实例（见 SAME_KIND）；
                        // 解析不出 = 应答形状不对，按「身份不明的占用者」处理（更严）。
                        reason = actual.isEmpty() ? MISMATCH : SAME_KIND;
                    }
                }
                final String actualOut = actual;
                new Handler(Looper.getMainLooper()).post(new Runnable() {
                    public void run() { cb.done(OK.equals(reason), reason, actualOut); }
                });
            }
        }, "wb2api-instance-check").start();
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

    /** 该端口上被用户确认过的实例标识；没有或读不出返回空串。 */
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
            os = new AtomicFile(f).startWrite();
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
