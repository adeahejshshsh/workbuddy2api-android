package com.workbuddy.api;

/**
 * 日志级别判据（纯逻辑，零 Android 依赖 —— 便于在宿主 JVM 上跑自检）。
 *
 * **必须与网页面板 `internal/panel/app.js` 的 `loadLogs()` 逐字对应**：
 *
 * <pre>
 *   const lvl = /error|err|失败|错误/i.test(e.text) ? 'e'
 *             : /warn|冷却|熔断/i.test(e.text) ? 'w' : '';
 * </pre>
 *
 * 两边判据一旦漂开，同一条网关日志会在网页上标红、在 App 里不标，等于同一份数据
 * 有两套事实。后端日志前缀是大写 `WARN:` / `ERR:` / `INFO:`，JS 那边靠 `i` 标志兜住
 * 大小写，这里统一转小写再比。
 *
 * `err` 这一支是必需的：后端有 27 处 `ERR:` 前缀，而 `i` 标志只能解决大小写，
 * 解决不了 `ERR` 与 `ERROR` 的拼写差异 —— 只写 `error` 的话那些错误行一条都不会标红。
 *
 * 为什么单独成 public 类：`ScreenLogs` 是包内 final class，默认包的自检看不到它。
 * 与 `GatewayService.backoffMs()` 同一个理由 —— 判据要能被 `tests/android` 直接测。
 */
public final class LogLevel {

    public static final int PLAIN = 0, WARN = 1, ERROR = 2;

    private LogLevel() {}

    /** 判定级别。同时命中 error 与 warn 时按 ERROR（与 JS 三元表达式的短路顺序一致）。 */
    public static int of(String text) {
        if (text == null) return PLAIN;
        String t = text.toLowerCase(java.util.Locale.ROOT);
        if (t.contains("error") || t.contains("err") || t.contains("失败") || t.contains("错误")) {
            return ERROR;
        }
        if (t.contains("warn") || t.contains("冷却") || t.contains("熔断")) return WARN;
        return PLAIN;
    }
}
