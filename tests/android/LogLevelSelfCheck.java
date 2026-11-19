/**
 * 日志级别判据自检（不需要真机）：javac 后直接用宿主 JVM 跑。
 *   javac -cp android.jar:classes -d test-out tests/android/LogLevelSelfCheck.java
 *   java  -cp android.jar:classes:test-out LogLevelSelfCheck
 *
 * 用例逐条对应网页面板 `internal/panel/app.js` 里 `loadLogs()` 的两个正则分支：
 *   /error|err|失败|错误/i   → 'e'
 *   /warn|冷却|熔断/i        → 'w'
 * 这边判据一变，App 的日志着色就会和网页版分叉，所以拿它当闸门。
 *
 * `ERR:` 那条是回归防线：`i` 标志只能解决大小写，解决不了 `ERR` 与 `ERROR` 的拼写
 * 差异 —— 只写 `error` 的话，后端 27 处 `ERR:` 前缀的错误行一条都不会标红。
 */
public class LogLevelSelfCheck {

    private static int fails = 0;

    private static void eq(String text, int want, String why) {
        int got = com.workbuddy.api.LogLevel.of(text);
        if (got != want) {
            System.out.println("FAIL 期望 " + want + " 实得 " + got + " ← " + why
                    + "  [" + text + "]");
            fails++;
        }
    }

    public static void main(String[] a) {
        // ── /error|err|失败|错误/i ─────────────────────────────────────────
        eq("ERR: connect refused", com.workbuddy.api.LogLevel.ERROR, "大写 ERR 前缀（后端 27 处）");
        eq("ERR: pool persist failed", com.workbuddy.api.LogLevel.ERROR, "ERR 前缀（pool）");
        eq("error: timeout", com.workbuddy.api.LogLevel.ERROR, "小写 error");
        eq("Error while dialing", com.workbuddy.api.LogLevel.ERROR, "首字母大写");
        eq("ERROR: config broken", com.workbuddy.api.LogLevel.ERROR, "全大写 ERROR");
        eq("上游返回 500，请求失败", com.workbuddy.api.LogLevel.ERROR, "中文「失败」");
        eq("配置错误：api_key 为空", com.workbuddy.api.LogLevel.ERROR, "中文「错误」");

        // ── /warn|冷却|熔断/i ──────────────────────────────────────────────
        eq("WARN: slow upstream", com.workbuddy.api.LogLevel.WARN, "大写 WARN 前缀");
        eq("warn: retry in 2s", com.workbuddy.api.LogLevel.WARN, "小写 warn");
        eq("账号冷却中，跳过", com.workbuddy.api.LogLevel.WARN, "中文「冷却」");
        eq("熔断触发，暂停 60s", com.workbuddy.api.LogLevel.WARN, "中文「熔断」");

        // ── 普通行不许上色 ────────────────────────────────────────────────
        eq("checkin done", com.workbuddy.api.LogLevel.PLAIN, "普通英文");
        eq("账号登录成功", com.workbuddy.api.LogLevel.PLAIN, "普通中文");
        eq("总 1234 tok · 首字 320ms", com.workbuddy.api.LogLevel.PLAIN, "统计行");

        // ── 边界 ─────────────────────────────────────────────────────────
        eq(null, com.workbuddy.api.LogLevel.PLAIN, "null 不许 NPE");
        eq("", com.workbuddy.api.LogLevel.PLAIN, "空串");

        // ── 优先级：JS 三元表达式 error 分支在前，这里也必须 ERROR 优先 ─────
        eq("WARN: 但正文里写了 error 两个字", com.workbuddy.api.LogLevel.ERROR,
                "error 优先于 warn");

        if (fails > 0) {
            System.out.println("失败 " + fails + " 项");
            System.exit(1);
        }
        System.out.println("OK 日志级别判据 17 项（与网页面板 app.js 正则同口径）");
    }
}
