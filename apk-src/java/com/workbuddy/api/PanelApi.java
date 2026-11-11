package com.workbuddy.api;

import android.content.Context;
import android.os.Handler;
import android.os.Looper;

import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 原生控制台用的最小 HTTP 客户端：只跟本机网关的 /panel/api 说话。
 *
 * 为什么不引 OkHttp/Gson：壳里只有 GET/POST + 十来个接口，多两个依赖只为了少写 150 行，
 * 却把 APK 体积和混淆面一起放大。org.json 是 Android 自带的，HttpURLConnection 也是。
 *
 * 线程模型：普通请求走 4 线程池，长动作（一键完成这类跑几十秒到十几分钟的）走
 * 单独的串行通道，回调切回主线程。
 *
 * 为什么不能全塞进一条单线程：旧版所有请求共用一个单线程池，点一次「一键完成」
 * 就把整条通道占住 —— 期间账号列表、用量、日志全部排在后面，界面看上去就是
 * 「点了之后一个数据都刷不出来，直到那个任务跑完」。串行只对同一类动作有意义
 * （避免并发改配置/并发打同一个账号），不能拿来做全局无差别串行。
 */
final class PanelApi {

    interface Cb {
        /** 2xx 且 JSON 可解析。 */
        void ok(JSONObject body);
        /** 网络错误或非 2xx；msg 已包含状态码与上游文案（尽量可直接显示）。 */
        void fail(String msg);
    }

    /** 普通请求（读列表/日志/配置等，秒级返回）。 */
    private static final ExecutorService IO = Executors.newFixedThreadPool(4);
    /** 长动作通道：一条线程，保证两次长动作不会叠在一起，也不挤占普通请求。 */
    private static final ExecutorService SLOW = Executors.newSingleThreadExecutor();
    private static final Handler UI = new Handler(Looper.getMainLooper());
    private static final Pattern API_KEY =
            Pattern.compile("\"api_key\"\\s*:\\s*\"([^\"]*)\"");

    private PanelApi() {}

    /** 网关基址（端口从 config.json 的 listen 推导，改端口后无需改代码）。 */
    static String base(Context c) {
        return "http://127.0.0.1:" + GwConfig.port(c);
    }

    /** 面板 api_key：与网页面板 localStorage 里那份同源（config.json）。 */
    static String key(Context c) {
        File f = new File(GwConfig.gwDir(c), "config.json");
        FileInputStream in = null;
        try {
            in = new FileInputStream(f);
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            Matcher m = API_KEY.matcher(bos.toString("UTF-8"));
            return m.find() ? m.group(1) : "";
        } catch (Throwable t) {
            return "";
        } finally {
            if (in != null) try { in.close(); } catch (Throwable ignored) {}
        }
    }

    static void get(Context c, String path, Cb cb) {
        req(c, "GET", path, null, false, cb);
    }

    static void post(Context c, String path, JSONObject body, Cb cb) {
        req(c, "POST", path, body, false, cb);
    }

    /** 长动作（一键完成等）：单独通道 + 长读超时，期间普通请求不受影响。 */
    static void postSlow(Context c, String path, JSONObject body, Cb cb) {
        req(c, "POST", path, body, true, cb);
    }

    private static void req(final Context app, final String method, final String path,
                            final JSONObject body, final boolean slow, final Cb cb) {
        final Context ctx = app.getApplicationContext();
        (slow ? SLOW : IO).execute(new Runnable() {
            public void run() {
                HttpURLConnection conn = null;
                try {
                    URL u = new URL(base(ctx) + path);
                    conn = (HttpURLConnection) u.openConnection();
                    conn.setRequestMethod(method);
                    conn.setConnectTimeout(6000);
                    // 长通道给足时间：服务端对整条流水线兜 15 分钟，这里只留 10 分钟
                    // 上限，避免客户端比服务端先断、把「还在跑」误报成失败。
                    conn.setReadTimeout(slow ? 600000 : 30000);
                    String k = key(ctx);
                    if (!k.isEmpty()) conn.setRequestProperty("Authorization", "Bearer " + k);
                    conn.setRequestProperty("Accept", "application/json");
                    if (body != null) {
                        conn.setDoOutput(true);
                        conn.setRequestProperty("Content-Type", "application/json");
                        byte[] raw = body.toString().getBytes("UTF-8");
                        conn.setFixedLengthStreamingMode(raw.length);
                        OutputStream os = conn.getOutputStream();
                        os.write(raw);
                        os.close();
                    }
                    int code = conn.getResponseCode();
                    InputStream is = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
                    String text = is == null ? "" : readAll(is);
                    if (code < 200 || code >= 300) {
                        final String msg = "HTTP " + code + (text.isEmpty() ? "" : "：" + brief(text));
                        UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                        return;
                    }
                    JSONObject obj;
                    try {
                        obj = text.isEmpty() ? new JSONObject() : new JSONObject(text);
                    } catch (Throwable t) {
                        final String msg = "响应不是 JSON：" + brief(text);
                        UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                        return;
                    }
                    final JSONObject out = obj;
                    UI.post(new Runnable() { public void run() { cb.ok(out); } });
                } catch (final Throwable t) {
                    final String msg = t.getClass().getSimpleName() + (t.getMessage() == null ? "" : "：" + t.getMessage());
                    UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                } finally {
                    if (conn != null) try { conn.disconnect(); } catch (Throwable ignored) {}
                }
            }
        });
    }

    /** 二进制下载（导出账号）：回调拿到字节与建议文件名（取 Content-Disposition）。 */
    interface FileCb {
        void ok(byte[] data, String filename);
        void fail(String msg);
    }

    static void download(final Context app, final String path, final FileCb cb) {
        final Context ctx = app.getApplicationContext();
        IO.execute(new Runnable() {
            public void run() {
                HttpURLConnection conn = null;
                try {
                    URL u = new URL(base(ctx) + path);
                    conn = (HttpURLConnection) u.openConnection();
                    conn.setConnectTimeout(6000);
                    conn.setReadTimeout(60000);
                    String k = key(ctx);
                    if (!k.isEmpty()) conn.setRequestProperty("Authorization", "Bearer " + k);
                    int code = conn.getResponseCode();
                    InputStream is = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
                    final byte[] data = is == null ? new byte[0] : readBytes(is);
                    if (code < 200 || code >= 300) {
                        final String msg = "HTTP " + code + "：" + brief(new String(data, "UTF-8"));
                        UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                        return;
                    }
                    String name = "accounts.json";
                    String cd = conn.getHeaderField("Content-Disposition");
                    if (cd != null) {
                        int i = cd.indexOf("filename=\"");
                        if (i >= 0) {
                            int j = cd.indexOf('"', i + 10);
                            if (j > i) name = cd.substring(i + 10, j);
                        }
                    }
                    final String fname = name;
                    UI.post(new Runnable() { public void run() { cb.ok(data, fname); } });
                } catch (final Throwable t) {
                    final String msg = t.getClass().getSimpleName() + (t.getMessage() == null ? "" : "：" + t.getMessage());
                    UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                } finally {
                    if (conn != null) try { conn.disconnect(); } catch (Throwable ignored) {}
                }
            }
        });
    }

    /** 上传单个文件（导入 cockpit / auths）：multipart/form-data，字段名固定为 file。 */
    static void upload(final Context app, final String path, final String filename,
                       final byte[] data, final Cb cb) {
        final Context ctx = app.getApplicationContext();
        IO.execute(new Runnable() {
            public void run() {
                HttpURLConnection conn = null;
                try {
                    String boundary = "----wb2api" + System.currentTimeMillis();
                    URL u = new URL(base(ctx) + path);
                    conn = (HttpURLConnection) u.openConnection();
                    conn.setRequestMethod("POST");
                    conn.setDoOutput(true);
                    conn.setConnectTimeout(6000);
                    conn.setReadTimeout(120000);
                    conn.setRequestProperty("Content-Type", "multipart/form-data; boundary=" + boundary);
                    String k = key(ctx);
                    if (!k.isEmpty()) conn.setRequestProperty("Authorization", "Bearer " + k);
                    java.io.ByteArrayOutputStream body = new java.io.ByteArrayOutputStream();
                    String head = "--" + boundary + "\r\n"
                            + "Content-Disposition: form-data; name=\"file\"; filename=\"" + filename + "\"\r\n"
                            + "Content-Type: application/json\r\n\r\n";
                    body.write(head.getBytes("UTF-8"));
                    body.write(data);
                    body.write(("\r\n--" + boundary + "--\r\n").getBytes("UTF-8"));
                    byte[] raw = body.toByteArray();
                    conn.setFixedLengthStreamingMode(raw.length);
                    OutputStream os = conn.getOutputStream();
                    os.write(raw);
                    os.close();

                    int code = conn.getResponseCode();
                    InputStream is = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
                    String text = is == null ? "" : readAll(is);
                    if (code < 200 || code >= 300) {
                        final String msg = "HTTP " + code + "：" + brief(text);
                        UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                        return;
                    }
                    JSONObject obj;
                    try { obj = text.isEmpty() ? new JSONObject() : new JSONObject(text); }
                    catch (Throwable t) { obj = new JSONObject(); }
                    final JSONObject out = obj;
                    UI.post(new Runnable() { public void run() { cb.ok(out); } });
                } catch (final Throwable t) {
                    final String msg = t.getClass().getSimpleName() + (t.getMessage() == null ? "" : "：" + t.getMessage());
                    UI.post(new Runnable() { public void run() { cb.fail(msg); } });
                } finally {
                    if (conn != null) try { conn.disconnect(); } catch (Throwable ignored) {}
                }
            }
        });
    }

    private static byte[] readBytes(InputStream is) throws Exception {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = is.read(buf)) > 0) bos.write(buf, 0, n);
        is.close();
        return bos.toByteArray();
    }

    private static String readAll(InputStream is) throws Exception {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = is.read(buf)) > 0) bos.write(buf, 0, n);
        is.close();
        return bos.toString("UTF-8");
    }

    /** 错误文案截断：上游 502 的 HTML 整段塞进 TextView 会把界面撑爆。 */
    private static String brief(String s) {
        String t = s.replaceAll("\\s+", " ").trim();
        // 优先取 {"error":"..."} 这种结构化文案
        try {
            JSONObject o = new JSONObject(t);
            if (o.has("error")) return o.getString("error");
            if (o.has("message")) return o.getString("message");
        } catch (Throwable ignored) {}
        return t.length() > 160 ? t.substring(0, 160) + "…" : t;
    }
}
