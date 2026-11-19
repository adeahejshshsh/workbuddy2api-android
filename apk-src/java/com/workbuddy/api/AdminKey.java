package com.workbuddy.api;

import android.content.Context;
import android.util.AtomicFile;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.security.SecureRandom;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 管理员密钥（{@code admin_api_key}）的首次启动自举（W6-H-03）。
 *
 * <p>为什么要自举：出厂配置里 {@code admin_api_key} 为空时，面板会把「客户端 api_key」
 * 当成管理员凭据用（服务端 {@code withAdminAuth} / panel 的 dedicated 判定都是这个口径）。
 * 而客户端 key 按设计要发给每一个下游使用者，只读面（账号清单 uid/昵称/积分/到期、用量、
 * 日志、请求归档含 client_ip/UA）因此对每个客户端开放。出厂配置又是 {@code listen: ":7863"}
 * 全网卡 —— 安装即对局域网暴露，等于把这些面直接放到同网段。
 *
 * <p>做法：配置里没有可用的 admin_api_key 时生成 32 hex 落盘（{@link AtomicFile}，掉电不会
 * 留半截 JSON）。首次安装（配置还不存在）走 {@link #bootstrapFromAssets}：先把 assets 模板
 * 落成 config.json 并填好 admin key，再让网关启动 —— 否则网关会先落一份 admin 为空的模板
 * 并立刻用它起来，那个窗口正是本条要找的洞。
 * **不改 listen**：局域网访问是用户明确要的用法（README「局域网访问」），这里只把
 * 「唯一防线」从共享的客户端 key 换成独立的管理员 key。
 *
 * <p>为什么放在 GatewayService 之外：本分片不允许改 GatewayService.java（另一个代理在改）。
 * 启动顺序上是等价的 —— MainActivity.onCreate / BootReceiver 都在拉起网关进程**之前**调用
 * {@link #ensure(Context)}，Go 进程读到的就是已补齐的配置。headless 路径（看门狗自愈重拉）
 * 不经过这里，见报告「需要 Go 侧配合」：{@code ensureConfig} 里应补一行
 * {@code AdminKey.ensure(this)}。
 */
final class AdminKey {

    static final String KEY_NAME = "admin_api_key";

    private static final Pattern ADMIN_KEY =
            Pattern.compile("\"" + KEY_NAME + "\"\\s*:\\s*\"([^\"]*)\"");

    private AdminKey() {}

    /** {@link #ensure} 的结果：调用方需要知道「这次是不是刚生成、网关要不要重启」。 */
    static final class Result {
        final String key;
        /** 本次调用新生成并写盘了。 */
        final boolean generated;
        /**
         * 配置是在**网关可能已经起来之后**才改的 → 必须重启才生效（Go 进程只在启动时读）。
         * 首次安装那条路径是在 start 之前落盘，所以 generated=true 但 needsRestart=false。
         */
        final boolean needsRestart;

        Result(String key, boolean generated, boolean needsRestart) {
            this.key = key;
            this.generated = generated;
            this.needsRestart = needsRestart;
        }
    }

    static File configFile(Context c) {
        return new File(GwConfig.gwDir(c), "config.json");
    }

    /** 读现有 admin_api_key；没有或不可用返回空串（不生成）。 */
    static String read(Context c) {
        String txt = readText(configFile(c));
        if (txt.isEmpty()) return "";
        Matcher m = ADMIN_KEY.matcher(txt);
        if (!m.find()) return "";
        String v = m.group(1).trim();
        return usable(v) ? v : "";
    }

    /**
     * 读管理员密钥自举用的一次性令牌（W3-02）。
     *
     * <p>面板侧的契约变了：`POST /panel/api/admin_key` 不再「回环即信」，必须带上
     * 进程启动时生成的 128 位随机令牌 —— Android 上回环跨 App 共享，任何本地应用先
     * 抢占端口就能自举出一个管理员密钥，随后导出全池 token。
     *
     * <p>令牌落在 &lt;网关目录&gt;/panel_bootstrap_token（0600，面板启动时生成，
     * 用后即删，重启换新）。这里只读不写：文件不存在说明「已经自举过」或「面板还没
     * 起来」，两种情况都不该注入，返回空串让调用方跳过。
     */
    static String bootstrapToken(Context c) {
        try {
            File f = new File(GwConfig.gwDir(c), "panel_bootstrap_token");
            if (!f.isFile()) return "";
            String v = readText(f).trim();
            // 128 位随机 = 32 hex；形状不对就当没有（半截文件/被写坏）。
            if (v.length() != 32) return "";
            for (int i = 0; i < v.length(); i++) {
                char ch = v.charAt(i);
                boolean hex = (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')
                        || (ch >= 'A' && ch <= 'F');
                if (!hex) return "";
            }
            return v;
        } catch (Throwable t) {
            return "";
        }
    }

    /**
     * 幂等自举。
     *
     * <p>首次安装时配置还不存在：这里**先把 assets/config.json 落成 config.json**，
     * 并把 admin_api_key 填好，再让网关启动 —— 否则顺序是「网关先落模板（admin 为空）→
     * 网关用空 admin 起来 → 面板回退用客户端 api_key 鉴权」，那个窗口正是 W6-H-03 的洞。
     * 只替换 admin_api_key 的值，api_key 的 CHANGE_ME 留给 GatewayService.ensureConfig
     * 处理（少复制一份别人的逻辑，就少一处将来会漂移的口径）。
     *
     * <p>配置已存在时只补 admin_api_key，其余一个字节都不动。
     */
    static synchronized Result ensure(Context c) {
        File cfg = configFile(c);
        if (!cfg.isFile() || cfg.length() <= 0) return bootstrapFromAssets(c, cfg);
        String cur = readText(cfg);
        if (cur.isEmpty()) return new Result("", false, false);

        Matcher m = ADMIN_KEY.matcher(cur);
        if (m.find()) {
            String v = m.group(1).trim();
            if (usable(v)) return new Result(v, false, false);
            // 有键但为空/占位：原地替换值，其余一个字节都不动。
            String key = generate();
            String fixed = m.replaceFirst("\"" + KEY_NAME + "\": \"" + key + "\"");
            if (!writeAtomic(cfg, fixed)) {
                GatewayService.appLog(c, "admin_api_key 补写失败（存储不可写？），本次未生成");
                return new Result("", false, false);
            }
            GatewayService.appLog(c, "admin_api_key 原为空/占位，已生成随机值（32 hex）。"
                    + "面板管理接口不再回退到客户端 api_key；见面板「设置」页。");
            return new Result(key, true, true);
        }

        // 没有这个键：插到最后一个 } 之前。分隔符按前缀末字符决定 —— 对空对象 `{}` 无条件
        // 拼前导逗号会写出非法 JSON（GatewayService.ensureConfig 踩过同一个坑，见其 bug #4）。
        String key = generate();
        int brace = cur.lastIndexOf('}');
        if (brace <= 0) return new Result("", false, false);   // 不是对象：别乱改
        String head = cur.substring(0, brace).replaceAll("\\s+$", "");
        String sep = (head.isEmpty() || head.endsWith("{")) ? "" : ",";
        String injected = head + sep + " \"" + KEY_NAME + "\": \"" + key + "\"\n" + cur.substring(brace);
        if (!writeAtomic(cfg, injected)) {
            GatewayService.appLog(c, "config.json 补写 admin_api_key 失败（存储不可写？）");
            return new Result("", false, false);
        }
        GatewayService.appLog(c, "config.json 缺 " + KEY_NAME + "，已补写随机值（32 hex）。"
                + "面板管理接口不再回退到客户端 api_key；见面板「设置」页。");
        return new Result(key, true, true);
    }

    /**
     * 首次安装：把 assets 模板落盘，并把 admin_api_key 直接填成随机值。
     *
     * 任何一步不符合预期（assets 读不到、模板里没有 admin_api_key 占位）就**不写**，
     * 交回给 GatewayService.ensureConfig 的既有路径 —— 宁可晚 3 秒由补偿任务补上，
     * 也不要写出一份形状不明的配置把网关卡在启动失败。
     */
    private static Result bootstrapFromAssets(Context c, File cfg) {
        java.io.InputStream is = null;
        try {
            is = c.getAssets().open("config.json");
            java.io.ByteArrayOutputStream bo = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = is.read(buf)) > 0) bo.write(buf, 0, n);
            String tpl = bo.toString("UTF-8");
            if (!tpl.contains("\"" + KEY_NAME + "\"")) return new Result("", false, false);
            String key = generate();
            String out = tpl.replace("\"" + KEY_NAME + "\": \"\"",
                    "\"" + KEY_NAME + "\": \"" + key + "\"");
            if (!out.contains(key)) return new Result("", false, false);   // 占位形状不认识
            File dir = cfg.getParentFile();
            if (dir != null && !dir.exists() && !dir.mkdirs()) return new Result("", false, false);
            if (!writeAtomic(cfg, out)) return new Result("", false, false);
            GatewayService.appLog(c, "首次启动：已生成 " + KEY_NAME + "（32 hex）并落盘。"
                    + "面板管理接口不再回退到客户端 api_key；见面板「设置」页。");
            return new Result(key, true, false);   // 发生在网关启动之前，不需要重启
        } catch (Throwable t) {
            GatewayService.appLog(c, "首次启动落配置失败（交给 ensureConfig）：" + t);
            return new Result("", false, false);
        } finally {
            if (is != null) try { is.close(); } catch (Throwable ignored) {}
        }
    }

    /** 启动日志用：把「面板在局域网上」这件事说清楚，别让用户以为只有本机能连。 */
    static void logLanExposure(Context c) {
        String listen = GwConfig.readListen(GwConfig.gwDir(c));
        boolean lan = listen == null || listen.isEmpty()
                || listen.startsWith(":") || listen.startsWith("0.0.0.0")
                || listen.startsWith("[::]");
        if (lan) {
            GatewayService.appLog(c, "面板/网关已监听全网卡（listen="
                    + (listen == null ? "?" : listen) + "）：局域网内可访问，"
                    + "admin_api_key 是管理面的唯一防线，不要把它发给客户端。");
        }
    }

    private static boolean usable(String v) {
        if (v == null || v.isEmpty()) return false;
        if ("CHANGE_ME".equalsIgnoreCase(v) || "test_key".equalsIgnoreCase(v)
                || "sk-xxx".equalsIgnoreCase(v)) return false;
        return true;
    }

    /** 32 位小写 hex（16 字节 SecureRandom），与「32 hex」的运维口径一致。 */
    static String generate() {
        byte[] b = new byte[16];
        new SecureRandom().nextBytes(b);
        StringBuilder sb = new StringBuilder(32);
        for (byte x : b) sb.append(Character.forDigit((x >> 4) & 0xF, 16))
                .append(Character.forDigit(x & 0xF, 16));
        return sb.toString();
    }

    private static String readText(File f) {
        if (f == null || !f.isFile() || f.length() <= 0 || f.length() > 4 * 1024 * 1024) return "";
        InputStream in = null;
        try {
            in = new FileInputStream(f);
            ByteArrayOutputStream bo = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) bo.write(buf, 0, n);
            return bo.toString("UTF-8");
        } catch (Throwable t) {
            return "";
        } finally {
            if (in != null) try { in.close(); } catch (Throwable ignored) {}
        }
    }

    /** 原子写（同目录 tmp + rename），与 GatewayService.writeFileAtomic 同语义。 */
    private static boolean writeAtomic(File target, String text) {
        AtomicFile af = new AtomicFile(target);
        FileOutputStream os = null;
        try {
            os = af.startWrite();
            os.write(text.getBytes("UTF-8"));
            os.flush();
            af.finishWrite(os);
            return true;
        } catch (Throwable t) {
            if (os != null) try { af.failWrite(os); } catch (Throwable ignored) {}
            return false;
        }
    }
}
