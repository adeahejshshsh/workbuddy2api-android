package com.workbuddy.api;

import android.content.Context;
import android.os.Handler;
import android.os.Looper;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;

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
        /** 在**主线程**回调。reason 见 {@link #message(String)}。 */
        void done(boolean ok, String reason);
    }

    static final String OK = "ok";
    static final String NO_ID = "no-id";
    static final String OLD_GATEWAY = "old-gateway";
    static final String HTTP_ERROR = "http";
    static final String MISMATCH = "mismatch";
    static final String NO_CONN = "no-conn";

    private PanelTrust() {}

    /** 后台线程探测，结果切回主线程。 */
    static void verify(final Context c, final Cb cb) {
        if (c == null || cb == null) return;
        final int port = GwConfig.port(c);
        final String expect = InstanceId.read(c);
        new Thread(new Runnable() {
            public void run() {
                final String reason;
                if (expect.isEmpty()) {
                    reason = NO_ID;
                } else {
                    Probe p = probe(port);
                    if (p.code == 0) {
                        // 连不上：网关没起、或被别的进程占着但不 HTTP 应答。
                        reason = NO_CONN;
                    } else if (p.code == 404) {
                        reason = OLD_GATEWAY;
                    } else if (p.code != 200) {
                        reason = HTTP_ERROR + ":" + p.code;
                    } else if (InstanceId.matches(expect, InstanceId.parseId(p.body))) {
                        reason = OK;
                    } else {
                        reason = MISMATCH;
                    }
                }
                new Handler(Looper.getMainLooper()).post(new Runnable() {
                    public void run() { cb.done(OK.equals(reason), reason); }
                });
            }
        }, "wb2api-instance-check").start();
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
        if (MISMATCH.equals(reason)) {
            return "端口被其他应用占用，已阻止注入";
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
