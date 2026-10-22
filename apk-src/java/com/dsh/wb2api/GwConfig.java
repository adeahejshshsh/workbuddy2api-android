package com.dsh.wb2api;

import android.content.Context;

import java.io.File;
import java.io.FileInputStream;
import java.io.ByteArrayOutputStream;

/**
 * 读网关配置里的监听端口。
 *
 * 为什么需要：面板配置页把 listen 暴露成可编辑项（改完需重启），但壳里的健康探针、
 * 看门狗判定与 WebView 地址此前全部写死 127.0.0.1:7863。把端口改掉之后：
 *   - 探针恒失败 → watchLoop 超过 45s 宽限就 selfHeal，反复杀进程重启健康网关；
 *   - 面板 WebView 永远打不开（停在空白页）。
 * 这里统一从 config.json 的 listen 推导端口，三处共用。
 */
final class GwConfig {

    static final int DEFAULT_PORT = 7863;

    private GwConfig() {}

    /** 网关工作目录（与 GatewayService 拉起进程时的工作目录一致）。 */
    static File gwDir(Context c) {
        return new File(c.getFilesDir(), "gateway");
    }

    /**
     * 监听端口：解析 listen（支持 ":7863" / "127.0.0.1:8080" / "[::1]:9000"）。
     * 读不到、解析失败、非 1..65535 一律回落默认端口 —— 探针不该因为配置脏了就误判。
     */
    static int port(Context c) {
        String listen = readListen(gwDir(c));
        int p = parsePort(listen);
        return p > 0 ? p : DEFAULT_PORT;
    }

    /** 从 listen 字符串里取端口号；取不到返回 0。 */
    static int parsePort(String listen) {
        if (listen == null) return 0;
        String s = listen.trim();
        if (s.isEmpty()) return 0;
        int idx = s.lastIndexOf(':');
        String digits = idx >= 0 ? s.substring(idx + 1) : s;
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < digits.length(); i++) {
            char ch = digits.charAt(i);
            if (ch < '0' || ch > '9') break;
            sb.append(ch);
        }
        if (sb.length() == 0) return 0;
        try {
            int p = Integer.parseInt(sb.toString());
            return (p > 0 && p <= 65535) ? p : 0;
        } catch (Throwable t) {
            return 0;
        }
    }

    /**
     * 从 config.json 里取 "listen" 的字符串值。
     *
     * 刻意不引 JSON 库：壳里只有这几行需求，为它加依赖不值得。用最小扫描定位顶层
     * "listen" 键（跳过字符串内部），值必须是非转义的简单字符串（端口配置就是这种）。
     */
    static String readListen(File dir) {
        File f = new File(dir, "config.json");
        FileInputStream in = null;
        try {
            in = new FileInputStream(f);
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            return findTopLevelString(bos.toString("UTF-8"), "listen");
        } catch (Throwable t) {
            return null;
        } finally {
            if (in != null) try { in.close(); } catch (Throwable ignored) {}
        }
    }

    /**
     * 在 JSON 文本里找**顶层** key 对应的字符串值。
     * 只在 depth==1 时匹配，避免命中嵌套对象里的同名键；跳过字符串字面量内的干扰。
     */
    static String findTopLevelString(String json, String key) {
        if (json == null) return null;
        int depth = 0;
        boolean inStr = false, esc = false;
        for (int i = 0; i < json.length(); i++) {
            char ch = json.charAt(i);
            if (inStr) {
                if (esc) { esc = false; continue; }
                if (ch == '\\') { esc = true; continue; }
                if (ch == '"') inStr = false;
                continue;
            }
            if (ch == '"') {
                // 读一个字符串 token
                int start = i + 1;
                int j = start;
                boolean e2 = false;
                while (j < json.length()) {
                    char cj = json.charAt(j);
                    if (e2) { e2 = false; j++; continue; }
                    if (cj == '\\') { e2 = true; j++; continue; }
                    if (cj == '"') break;
                    j++;
                }
                if (j >= json.length()) return null;
                String token = json.substring(start, j);
                i = j;
                if (depth == 1 && key.equals(token)) {
                    // 期望后面紧跟 : "value"
                    int k = j + 1;
                    while (k < json.length() && Character.isWhitespace(json.charAt(k))) k++;
                    if (k < json.length() && json.charAt(k) == ':') {
                        k++;
                        while (k < json.length() && Character.isWhitespace(json.charAt(k))) k++;
                        if (k < json.length() && json.charAt(k) == '"') {
                            int vs = k + 1, vj = vs;
                            boolean ve = false;
                            while (vj < json.length()) {
                                char cv = json.charAt(vj);
                                if (ve) { ve = false; vj++; continue; }
                                if (cv == '\\') { ve = true; vj++; continue; }
                                if (cv == '"') break;
                                vj++;
                            }
                            if (vj < json.length()) return json.substring(vs, vj);
                        }
                    }
                }
                continue;
            }
            if (ch == '{' || ch == '[') depth++;
            else if (ch == '}' || ch == ']') depth--;
        }
        return null;
    }
}
