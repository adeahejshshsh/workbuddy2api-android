package com.workbuddy.api;

import android.content.Context;
import android.util.AtomicFile;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.security.SecureRandom;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 本机网关的实例标识（{@code files/gateway/data/instance_id}）。
 *
 * <p>为什么需要（W6-H-02）：{@code WebPanelActivity} 在 onPageFinished 里把管理员的
 * {@code admin_api_key} 注入 {@code http://127.0.0.1:<port>} 的 localStorage。回环地址在
 * Android 上**跨应用共享**：任何一个持 INTERNET 权限的 App（安装即授予）先 bind 7863，
 * 用户点「更多 → 网页面板」就会把管理员密钥灌进它的页面。仅凭「URL 是 127.0.0.1」无法
 * 证明对面是本进程拉起的网关。
 *
 * <p>判据：本进程在启动网关**之前**生成一个 32 hex 的随机串写到应用私有目录（0700，别的
 * App 读不到）。注入前向 {@code /panel/api/instance} 要一次对方的 instance_id，与本进程
 * 读到的一致才注入 —— 抢占端口的第三方不知道这个串，也就伪造不出来。
 *
 * <p>注意：**不要**把自己的 id 放进请求（header/query）再让对方回显 —— 那样任何占位进程
 * 只要原样回显就能通过校验。只比较「对方主动报出的值」。
 *
 * <p>对应 Go 侧接口（本分片不改 Go，见报告「需要 Go 侧配合」）：
 * {@code GET /panel/api/instance} → {@code {"instance_id":"<32hex>"}}；Go 进程从
 * {@code <工作目录>/data/instance_id} 读同一个文件。
 */
final class InstanceId {

    /** 与 GatewayService 的 gateway 目录同源：files/gateway/data/instance_id。 */
    static final String FILE_NAME = "instance_id";

    /** 只认 32 位小写 hex：写坏/被塞了别的内容时一律当成「没有」，走失败关闭。 */
    private static final Pattern HEX32 = Pattern.compile("^[0-9a-f]{32}$");

    private InstanceId() {}

    static File file(Context c) {
        return new File(new File(c.getFilesDir(), "gateway/data"), FILE_NAME);
    }

    /**
     * 读取已有 id；不存在或内容不合法返回空串（**不生成**）。
     *
     * 注入路径用这个：读不到就等于「本机网关没起过」，宁可不注入。
     */
    static String read(Context c) {
        File f = file(c);
        if (!f.isFile() || f.length() <= 0 || f.length() > 4096) return "";
        InputStream in = null;
        try {
            in = new FileInputStream(f);
            byte[] buf = new byte[128];
            int n = in.read(buf);
            if (n <= 0) return "";
            String s = new String(buf, 0, n, "UTF-8").trim();
            return HEX32.matcher(s).matches() ? s : "";
        } catch (Throwable t) {
            return "";
        } finally {
            if (in != null) try { in.close(); } catch (Throwable ignored) {}
        }
    }

    /**
     * 读取或生成（幂等）。启动网关前调用，保证「壳写下的 id」与「Go 进程读到的 id」是同一个。
     *
     * 用 {@link AtomicFile}：写入被系统杀掉时留下的是旧文件或新文件，不会出现半截串
     * （半截串会让下次启动的校验永远不匹配，且用户完全看不出原因）。
     */
    static synchronized String ensure(Context c) {
        String cur = read(c);
        if (!cur.isEmpty()) return cur;
        String id = randomHex();
        File f = file(c);
        File dir = f.getParentFile();
        if (dir != null && !dir.exists() && !dir.mkdirs()) return "";
        AtomicFile af = new AtomicFile(f);
        FileOutputStream os = null;
        try {
            os = af.startWrite();
            os.write(id.getBytes("UTF-8"));
            os.flush();
            af.finishWrite(os);
            return id;
        } catch (Throwable t) {
            if (os != null) try { af.failWrite(os); } catch (Throwable ignored) {}
            GatewayService.appLog(c, "instance_id 写入失败：" + t);
            return "";
        }
    }

    /** 32 位小写 hex（16 字节 SecureRandom）。 */
    static String randomHex() {
        byte[] b = new byte[16];
        new SecureRandom().nextBytes(b);
        StringBuilder sb = new StringBuilder(32);
        for (byte x : b) sb.append(Character.forDigit((x >> 4) & 0xF, 16))
                .append(Character.forDigit(x & 0xF, 16));
        return sb.toString();
    }

    /**
     * 常量时间比较（长度不同直接 false）。
     *
     * 这里其实没有侧信道压力（攻击者拿不到比较耗时的高精度观测），但用同一套写法
     * 可以避免「以后有人把这段挪到真需要防时序的地方」时留下坏样板。
     */
    static boolean matches(String a, String b) {
        if (a == null || b == null || a.length() != b.length() || a.isEmpty()) return false;
        int diff = 0;
        for (int i = 0; i < a.length(); i++) diff |= a.charAt(i) ^ b.charAt(i);
        return diff == 0;
    }

    /**
     * 从 {@code /panel/api/instance} 的响应里取 id。
     *
     * 容忍两种字段名（instance_id / id），但**只接受 32 hex**：字段缺失、值是空串、
     * 或者是别的形状（比如 HTML 错误页）都返回空串 → 调用方按失败处理。
     */
    static String parseId(String body) {
        if (body == null || body.isEmpty() || body.length() > 65536) return "";
        Matcher m = Pattern.compile("\"instance_id\"\\s*:\\s*\"([0-9a-fA-F]{32})\"").matcher(body);
        if (!m.find()) m = Pattern.compile("\"id\"\\s*:\\s*\"([0-9a-fA-F]{32})\"").matcher(body);
        if (!m.find()) return "";
        return m.group(1).toLowerCase(java.util.Locale.US);
    }
}
