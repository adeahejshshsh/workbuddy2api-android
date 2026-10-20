package com.dsh.wb2api;

import android.app.AlarmManager;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.graphics.PixelFormat;
import android.net.wifi.WifiManager;
import android.provider.Settings;
import android.view.View;
import android.view.WindowManager;
import android.net.ConnectivityManager;
import android.net.LinkProperties;
import android.net.Network;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.os.SystemClock;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStreamWriter;
import java.io.Writer;
import java.net.HttpURLConnection;
import java.net.Inet4Address;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.net.URL;
import java.util.Map;
import java.util.UUID;

/**
 * 前台服务：把随 APK 一起分发的 Go 网关跑起来（lib/arm64-v8a/libwb2api.so）。
 *
 * 为什么二进制叫 .so：Android 只允许 app 执行 nativeLibraryDir 里的东西
 * （自己 data 目录 execve 会被 SELinux 拒）。打包成 lib 前缀的 .so，安装器会把它
 * 解到 nativeLibraryDir；若该文件带 x 位就直接 exec，否则退回系统 linker 代跑。
 *
 * 对外状态：status + reason（**机器可读**）。界面绝不许去猜 state 那串中文 —— 踩过：
 * 串一变（"直接 exec pid=1" 不含任何关键词）UI 就误报"已停止/未知"。state 只给人看，
 * 一切判断读 statusOf()。
 *
 * 看门狗（四层）：
 *   1) 进程级：每 10s 看孩子还在不在，不在就按退避拉起来；
 *   2) 健康级：进程在但 /healthz 超过 45s 没响应（僵死）→ 杀掉重拉；
 *   3) 系统级：AlarmManager 每 5 分钟兜一下，App 被系统整个杀掉后也能自己回来；
 *      + BOOT_COMPLETED 开机自启（BootReceiver）。
 *   4) 保活级：服务在跑就一直持 partial wakelock —— 不持的话屏幕一灭 CPU 就进 suspend，
 *      看门狗线程和 Go 里的定时器全被冻住，症状正是「只有 App 在前台时才生效」。
 */
public class GatewayService extends Service {
    public static final String ACTION_STOP = "com.dsh.wb2api.action.STOP";
    public static final String ACTION_WATCH = "com.dsh.wb2api.action.WATCH";
    public static final String EXTRA_RESTART = "restart";
    static final String CH_ID = "wb2api";
    static final int NOTIF_ID = 0x1001;

    static final long TICK_MS = 10_000L;        // 看门狗心跳
    static final long GRACE_MS = 45_000L;       // 启动宽限：这段时间内不响应不算僵死
    static final long STABLE_MS = 120_000L;     // 稳定这么久 → 退避计数清零
    static final long ALARM_MS = 5 * 60_000L;   // 系统级兜底间隔
    static final int ALARM_REQ = 7;

    static volatile Process proc;
    static volatile String state = "未启动";
    static volatile String lastLine = "";
    static volatile String mode = "";
    static volatile int restarts = 0;
    static volatile boolean external = false;   // 7863 被别的实例占着（比如 DSH 里那个）

    // ── 被杀诊断：7863 老被杀，光说"又挂了"没用，得知道是谁杀的、杀了几次
    static final String PREF = "wb2api";
    static volatile int appRestarts = 0;        // App 进程被系统杀掉后又被拉起来的次数
    // overlayErr 常驻图层失败原因（bug B15）：独立字段，不占用 reason（那是界面上的
    // 「为什么不可用」，关键原因不该被次要提示覆盖）。
    static volatile String overlayErr = "";
    public static String overlayError() { return overlayErr; }
    static volatile String lastExit = "";       // 上一次网关退出的原因（跨 App 重启保留）
    static volatile boolean userStop = false;   // 用户在通知里点了"停止"：那就不再自愈
    static volatile boolean startCounted = false; // 本进程是否已统计过"被杀重启"（bug #5）
    public static int appRestartCount() { return appRestarts; }
    public static String lastExitText() { return lastExit; }

    // ── 保活：三个锁（CPU / Wi-Fi）+ 一个"常驻图层"，还有一个"我被冻过多久"的账
    static volatile WifiManager.WifiLock wifiLock;
    static volatile View overlay;               // 1x1 全透明的常驻窗口（见 installOverlay）
    static volatile int freezes = 0;            // 心跳断了（= 被系统冻过）的次数
    static volatile String freezeText = "";
    static volatile long lastTickAt = 0;
    public static int freezeCount() { return freezes; }
    public static String freezeText() { return freezeText; }

    // ── 详细日志：App 侧事件落盘，和网关自己的 server.log 拼成完整证据链 ──
    /** App 事件日志（超过 512KB 翻成 app.log.1，只留两代） */
    public static File appLogFile(Context c) {
        File dir = new File(c.getFilesDir(), "logs");
        try { dir.mkdirs(); } catch (Throwable ignored) {}
        return new File(dir, "app.log");
    }

    /** 网关自己的日志：App 把它的 stdout+stderr 追加写到这里（redirectErrorStream） */
    public static File gwLogFile(Context c) {
        File dir = new File(c.getFilesDir(), "gateway/data");
        try { dir.mkdirs(); } catch (Throwable ignored) {}
        return new File(dir, "server.log");
    }

    /**
     * 记一条 App 事件。为什么必须单独记：网关日志能看出"任务做了什么、上游回了什么"，
     * 但"系统什么时候冻了我/杀了我、前台服务有没有被拒启动、alarm 精不精确"只有 App 知道。
     */
    static void appLog(Context c, String s) {
        try {
            File f = appLogFile(c);
            if (f.length() > 512 * 1024) {
                File old = new File(f.getParentFile(), "app.log.1");
                if (old.exists()) old.delete();
                // renameTo 的返回值必须看（issue B10）：失败（存储满/被占用）时
                // f 仍指向原文件继续追加，512KB 上限彻底失效，能把 files/ 撑满。
                if (f.renameTo(old)) {
                    f = appLogFile(c);
                } else {
                    // 兜底：原地截断重写，至少不让它无限涨。
                    try (FileOutputStream os = new FileOutputStream(f, false)) {
                        os.write(("[app] 日志翻代失败（rename 被拒），已截断重写\n").getBytes("UTF-8"));
                    } catch (Throwable ignored) {}
                }
            }
            String ts = new java.text.SimpleDateFormat("MM-dd HH:mm:ss", java.util.Locale.US)
                    .format(new java.util.Date());
            try (Writer w = new OutputStreamWriter(new FileOutputStream(f, true), "UTF-8")) {
                w.write(ts + "  " + s + "\n");
            }
        } catch (Throwable ignored) {}
    }

    /** 精确 alarm 权限在不在（Android 12+ 的「闹钟与提醒」特殊权限） */
    static boolean exactAlarmOk(Context c) {
        try {
            if (Build.VERSION.SDK_INT < 31) return true;
            AlarmManager am = (AlarmManager) c.getSystemService(Context.ALARM_SERVICE);
            return am == null || am.canScheduleExactAlarms();
        } catch (Throwable t) { return false; }
    }

    /** 退出码翻成人话：137 基本就是被系统强杀（内存不够或清后台），143 是收到停止信号 */
    static String exitMeaning(int rc) {
        if (rc == 0) return "rc=0 正常退出";
        if (rc == 137) return "rc=137 被强杀 SIGKILL（内存不足或系统清后台）";
        if (rc == 139) return "rc=139 段错误 SIGSEGV";
        if (rc == 143) return "rc=143 收到停止信号 SIGTERM";
        if (rc == 141) return "rc=141 写 stdout 撞 EPIPE（SIGPIPE）：日志读取端被关掉了 —— 网关本身可能是健康的，问题在日志管道";
        return "rc=" + rc + (rc > 128 ? "（信号 " + (rc - 128) + "）" : "");
    }

    // ── 对外状态：机器可读（UI 只读这两个，不解析下面的中文 state）
    public static final int ST_STOPPED = 0, ST_STARTING = 1, ST_RUNNING = 2, ST_EXTERNAL = 3;

    /** server.log 单代上限：pump 超过它就只读空、不落盘（**绝不关读端**，见 pump 注释）。 */
    static final long LOG_PUMP_MAX = 2L * 1024 * 1024;
    static volatile int status = ST_STOPPED;
    static volatile String reason = "服务尚未启动";
    public static int statusOf() { return status; }
    public static String reasonText() { return reason; }
    /** status/state/reason 三件套一起改，免得漏一个（上一个版本就是漏了才误报） */
    static void setState(int st, String text, String why) {
        status = st; state = text;
        if (why != null) reason = why;
    }
    static volatile long startedAt = 0;         // elapsedRealtime，用于算启动宽限
    static volatile PowerManager.WakeLock wake; // 保活：持着它 CPU 才不会睡

    Thread reader, waiter, watchdog;
    volatile boolean watching = false;

    public static void start(Context c) { kick(c, false); }
    public static void restart(Context c) { kick(c, true); }

    static void kick(Context c, boolean restart) {
        Intent i = new Intent(c, GatewayService.class);
        if (restart) i.putExtra(EXTRA_RESTART, true);
        try {
            if (Build.VERSION.SDK_INT >= 26) c.startForegroundService(i); else c.startService(i);
        } catch (Throwable t) {   // 后台起前台服务被系统拒：把原因留下来，别静默
            reason = "服务启动被系统拒：" + t.getClass().getSimpleName();
        }
    }
    /** 用户主动停止：必须与 ACTION_STOP 分支同语义（bug #10）。
     *  只 stopService 的话，onDestroy 里 `if (!userStop)` 会重挂 alarm + JobScheduler，
     *  服务 5 分钟内自己回来 —— 用户以为停了其实没停。 */
    public static void stop(Context c) {
        userStop = true;
        try {
            c.getSharedPreferences(PREF, MODE_PRIVATE).edit().putBoolean("service_up", false).apply();
        } catch (Throwable ignored) {}
        cancelAlarm(c);
        ReviveJobService.cancel(c);
        // 状态复位（bug B3）：与 ACTION_STOP 分支同语义，否则界面永远「启动中」。
        setState(ST_STOPPED, "已停止", "用户点了停止，不再自愈");
        try { c.stopService(new Intent(c, GatewayService.class)); } catch (Throwable ignored) {}
    }

    /** 读整个文本文件（UTF-8）；失败返回 null。 */
    static String readTextFile(File f) {
        try (java.io.InputStream is = new java.io.FileInputStream(f)) {
            java.io.ByteArrayOutputStream bo = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = is.read(buf)) > 0) bo.write(buf, 0, n);
            return new String(bo.toByteArray(), "UTF-8");
        } catch (Throwable t) {
            return null;
        }
    }

    /** 指定进程是否还活着（bug P1-1：静态 alive() 只看 proc，而 killProc 已把它置 null）。 */
    static boolean isAlive(Process p) {
        if (p == null) return false;
        try { p.exitValue(); return false; } catch (IllegalThreadStateException e) { return true; }
    }

    public static boolean alive() {
        Process p = proc;
        if (p == null) return false;
        try { p.exitValue(); return false; } catch (IllegalThreadStateException e) { return true; }
    }
    public static String stateText() { return state; }
    public static String lastLogLine() { return lastLine; }
    public static String modeText() { return mode; }
    public static int restartCount() { return restarts; }
    public static boolean externalInstance() { return external; }

    @Override public IBinder onBind(Intent i) { return null; }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {   // 用户主动停：别再自愈
            userStop = true;
            try { getSharedPreferences(PREF, MODE_PRIVATE).edit().putBoolean("service_up", false).apply(); } catch (Throwable ignored) {}
            watching = false;
            cancelAlarm(this);
            ReviveJobService.cancel(this);
            killProc();
            releaseWake();
            stopForegroundCompat();
            // 状态必须复位（bug B3）：此前只关通知/关端口，静态 status 仍停在
            // ST_RUNNING，而 MainActivity.tick 把 ST_RUNNING 读成「启动中」——
            // 用户在通知栏点了停止，界面上却一直是「启动中」，还继续每 2s 探测、
            // 每 2s 尝试加载面板。
            setState(ST_STOPPED, "已停止", "用户在通知栏点了停止，不再自愈");
            appRestarts = 0;
            refreshNotification();
            appLog(this, "用户主动停止 → 释放保活、取消 alarm/job，不再自愈");
            stopSelf();
            return START_NOT_STICKY;
        }
        ensureChannel();
        // 上一次实例没清"运行中"标记，说明是 App 进程被系统杀掉之后又被拉起来的 → 记一笔
        try {
            SharedPreferences sp = getSharedPreferences(PREF, MODE_PRIVATE);
            // 只统计「进程被系统杀掉后重建」，且每个进程只记一次（bug #5）。
            // onStartCommand 不只在进程重建时调用：MainActivity 每次 onCreate、
            // BootReceiver、JobScheduler、5 分钟一次的 ACTION_WATCH alarm 都会进来，
            // 而 service_up 一直是 true —— 此前等于统计"onStartCommand 被调用次数"，
            // 界面上的「系统重启 N」会随每次打开 App / 每次 alarm 无限增长。
            if (!startCounted) {
                startCounted = true;
                if (sp.getBoolean("service_up", false)) {
                    appRestarts = sp.getInt("app_restarts", 0) + 1;
                    sp.edit().putInt("app_restarts", appRestarts).apply();
                }
            }
            lastExit = sp.getString("last_exit", "");
            sp.edit().putBoolean("service_up", true).apply();
            if (!lastExit.isEmpty())
                reason = "上次网关退出：" + lastExit + (appRestarts > 0 ? "（服务被系统杀过 " + appRestarts + " 次）" : "");
        } catch (Throwable ignored) {}
        userStop = false;
        appLog(this, "服务启动 startId=" + startId
                + (appRestarts > 0 ? "，App 进程已被系统杀过 " + appRestarts + " 次" : "")
                + (lastExit.isEmpty() ? "" : "，上次网关退出：" + lastExit));
        ReviveJobService.schedule(this);   // 第五层：JobScheduler，系统托管，跨重启仍在
        String fgsErr = startForegroundCompat();
        if (fgsErr != null) {   // 起不来就别赖着，否则等 5s 被系统判 ANR 崩掉
            setState(ST_STOPPED, "前台服务起不来", fgsErr);
            appLog(this, "前台服务起不来：" + fgsErr);
            stopSelf();
            return START_NOT_STICKY;
        }
        holdWake();   // 保活：不持它，后台时看门狗线程与 Go 定时器都会被冻住
        appLog(this, "保活状态：wakelock=" + wakeHeld() + "，常驻图层=" + (overlay != null)
                + "，alarm=" + (exactAlarmOk(this) ? "精确" : "非精确（未授予「闹钟与提醒」）") + "，任务列表=划掉也不停");
        // 重启：必须等旧进程真的退出再拉新的（destroy 是异步的，句柄清空 ≠ 进程已死）。
        // 否则新进程 bind: address already in use → 直接退出 → 只能等看门狗 10s 一拍
        // 兜底；有在途 SSE 时（Go 侧要排空最多 5 秒）尤其明显。
        boolean restarting = false;
        if (intent != null && intent.getBooleanExtra(EXTRA_RESTART, false)) {
            restartGateway();
            restarting = true;
        }
        if (!restarting && !alive()) {
            if (portUp()) {               // 本机已有实例在跑（比如 DSH 里那个），复用不抢
                external = true;
                mode = "复用";
                setState(ST_EXTERNAL, "端口 7863 已被占用，复用现有实例",
                         "7863 上已有别的实例在服务，本服务不接管（不抢端口、不杀进程）");
                refreshNotification();
            } else {
                external = false;
                startGateway();
            }
        }
        armAlarm(this);
        startWatchdog();
        return START_STICKY;
    }

    @Override public void onDestroy() {
        super.onDestroy();
        watching = false;      // 系统杀我时 alarm 还挂着 → 5 分钟内自己回来
        killProc();
        releaseWake();
        if (!userStop) {       // 不是用户点的停止 → alarm + job 两条路把服务拉回来
            appLog(this, "服务被销毁（非用户操作）→ 已重挂 alarm + JobScheduler 兜底");
            armAlarm(this);
            ReviveJobService.schedule(this);
        }
    }

    /** 任务列表被划掉：stopWithTask="false" 时服务还活着，但兜底要重挂一遍 */
    @Override public void onTaskRemoved(Intent root) {
        setState(status, "任务被划掉，服务继续",
                 "App 任务被划掉（stopWithTask=false 服务继续跑），已重挂 alarm/job 兜底");
        appLog(this, "任务列表被划掉：服务继续跑（stopWithTask=false），已重挂兜底");
        armAlarm(this);
        ReviveJobService.schedule(this);
        super.onTaskRemoved(root);
    }

    // ---------- 看门狗 ----------
    void startWatchdog() {
        if (watching) return;
        watching = true;
        watchdog = new Thread(this::watchLoop);
        watchdog.setDaemon(true);
        watchdog.start();
    }

    void watchLoop() {
        while (watching) {
            sleep(TICK_MS);
            if (!watching) return;
            try {
                long now = SystemClock.elapsedRealtime();
                // 心跳断了 30s 以上 = 这期间整个进程（连看门狗线程一起）被系统冻住了。
                // 症状正是"过一会儿连不上，把 App 拉回前台立刻就好" —— 记下来，别让人猜。
                if (lastTickAt > 0 && now - lastTickAt > 3 * TICK_MS) {
                    freezes++;
                    long gap = (now - lastTickAt) / 1000;
                    freezeText = "系统冻了我 " + gap + " 秒（"
                            + new java.text.SimpleDateFormat("HH:mm", java.util.Locale.US)
                                    .format(new java.util.Date()) + " 恢复）";
                    setState(status, freezeText, freezeText + " —— 拉回前台/放进白名单后已恢复");
                    appLog(this, freezeText);
                }
                lastTickAt = now;
                if (!alive()) {
                    if (portUp()) {        // 端口有主但没有我的孩子 → 外部实例，不插手
                        external = true;
                        mode = "复用";
                        setState(ST_EXTERNAL, "外部实例在跑（复用）",
                                 "本服务没持有子进程，7863 由外部实例服务");
                        continue;
                    }
                    external = false;
                    selfHeal("网关进程已退出");
                    continue;
                }
                if (!httpUp() && now - startedAt > GRACE_MS) {
                    selfHeal("健康检查 45s 无响应（僵死）");
                    continue;
                }
                if (restarts > 0 && now - startedAt > STABLE_MS) {   // 自愈后稳住 → 退避清零
                    restarts = 0;
                    setState(ST_RUNNING, mode + " 运行正常（自愈后已稳定）", null);
                    refreshNotification();
                }
                armAlarm(this);
            } catch (Throwable ignored) {
                // 看门狗自己绝不能死：吞掉一切异常继续下一轮
            }
        }
    }

    /** 重启用的后台线程：等待旧进程退出不能放在主线程（阻塞 3~4 秒会挤进 ANR 窗口）。 */
    static final java.util.concurrent.ExecutorService boot =
            java.util.concurrent.Executors.newSingleThreadExecutor();

    /**
     * 重启网关：先拿到旧句柄、杀掉，再在后台等它真的退出（有界），最后拉起新的。
     * 与 startGateway 同锁，避免和看门狗线程交叉执行。
     */
    void restartGateway() {
        final Process old;
        synchronized (this) {
            old = proc;                 // 先捕获：killProc 会把静态 proc 置 null
        }
        killProc();
        boot.execute(new Runnable() {
            public void run() {
                if (old != null) {
                    // 等旧进程真正退出，窗口要 ≥ 网关的优雅停机预算（bug P1-1）。
                    // Go 侧 srv.Shutdown 用的是 5s：有在途 SSE 时必然跑满，而这里原先
                    // 只等 4s 就拉新进程 → 新进程 bind: address already in use 立刻退出
                    // → 看门狗退避 2s 再拉一次，用户看到"重启了两次"、自愈计数 +1。
                    // 轮询 isAlive 而不是 waitFor 固定时长：旧进程提前退出就不用干等。
                    long deadline = System.currentTimeMillis() + 8000; // ≥ 5s 停机预算
                    while (System.currentTimeMillis() < deadline && isAlive(old)) {
                        try { Thread.sleep(150); }
                        catch (InterruptedException e) { Thread.currentThread().interrupt(); break; }
                    }
                    // 窗口结束仍活着 → 强杀再走（bug #4）：否则新进程必然撞端口。
                    if (isAlive(old)) {
                        try {
                            if (Build.VERSION.SDK_INT >= 26) old.destroyForcibly();
                            else old.destroy();
                        } catch (Throwable ignored) {}
                        try { Thread.sleep(300); } catch (InterruptedException ignored) {}
                    }
                }
                // 等待窗口里用户点了「停止」（userStop=true 且已 stopSelf）：
                // 这时再拉起就是一个没人看门狗、没 wakelock、没 alarm 的孤儿进程，
                // 占着 7863 直到系统杀 App —— 用户还会以为"停了"。
                if (userStop) {
                    appLog(GatewayService.this, "重启任务被取消：等待期间用户已停止服务");
                    return;
                }
                startGateway();
            }
        });
    }

    void selfHeal(String why) {
        int n = ++restarts;
        setState(ST_STARTING, "看门狗自愈 #" + n + "：" + why, "看门狗正在重拉网关：" + why);
        appLog(this, "看门狗自愈 #" + n + "：" + why + "（退避 " + (backoffMs(n) / 1000) + "s）");
        killProc();
        refreshNotification();
        sleep(backoffMs(n));            // 退避，别打连环炮
        if (!watching) return;
        // 退避窗口里主线程可能已经手动重启过（onStartCommand 的 killProc + startGateway）：
        // 这时再拉起就是第二个进程 —— P1 变孤儿占着 7863，P2 bind 失败退出，看门狗下一拍
        // 把「端口有人但 proc 已死」判成外部实例，界面永久停在「运行中（外部）」，点停止
        // 也杀不掉孤儿。二次检查（startGateway 内另有锁 + 同样的检查，双保险）。
        if (alive()) return;
        startGateway();
    }

    /** 自愈退避：2s,4s,8s,16s,32s,60s 封顶（public 只为 apk-src/test 的自检能调） */
    public static long backoffMs(int n) {
        long d = 2000L << Math.min(5, Math.max(0, n - 1));
        return Math.min(d, 60_000L);
    }

    static void sleep(long ms) {
        try { Thread.sleep(ms); } catch (InterruptedException e) { Thread.currentThread().interrupt(); }
    }

    // ---------- 启动网关 ----------
    /**
     * 拉起网关进程。
     *
     * synchronized + 入口 alive() 二次检查是必需的：调用方有两个线程 —— 主线程
     * （onStartCommand）与看门狗线程（selfHeal）。此前没有任何互斥，看门狗在
     * selfHeal 的 sleep 退避里时用户点「重启网关」，主线程先拉起 P1，看门狗醒来
     * 再拉起 P2 并覆盖静态 proc → P1 成孤儿占着 7863，P2 bind 失败退出，之后
     * killProc() 只作用在已死的 P2 上，孤儿再也杀不掉。
     */
    synchronized void startGateway() {
        if (userStop) return;           // 用户已点停止：任何路径都不许再拉起（B2）
        if (alive()) return;            // 已有存活进程：不重复拉起（锁内二次检查）
        try {
            File gwDir = new File(getFilesDir(), "gateway");
            new File(gwDir, "auths").mkdirs();
            new File(gwDir, "data").mkdirs();
            ensureConfig(gwDir);
            File resolv = writeResolvConf(gwDir);

            String bin = getApplicationInfo().nativeLibraryDir + "/libwb2api.so";
            boolean canExec = new File(bin).canExecute();
            ProcessBuilder pb;
            if (canExec) {
                mode = "直接 exec";
                pb = new ProcessBuilder(bin);
            } else {
                mode = "linker 代跑";
                // bionic 手动模式: argv[1] 必须是程序的绝对路径
                pb = new ProcessBuilder("/system/bin/linker64", bin, bin);
            }
            pb.directory(gwDir);
            pb.redirectErrorStream(true);
            Map<String, String> env = pb.environment();
            env.put("WB2API_RESOLV_CONF", resolv.getAbsolutePath());  // Go 纯 resolver 读它
            env.put("HOME", gwDir.getAbsolutePath());
            env.put("TMPDIR", getCacheDir().getAbsolutePath());

            Process p = pb.start();
            proc = p;
            startedAt = SystemClock.elapsedRealtime();
            String pid = "?";
            try { pid = String.valueOf(p.pid()); } catch (Throwable ignored) {}
            setState(ST_RUNNING,
                     mode + " pid=" + pid + " 运行中" + (restarts > 0 ? "（自愈 " + restarts + " 次）" : ""),
                     null);
            appLog(this, "拉起网关：" + mode + " pid=" + pid + "，工作目录 " + gwDir.getAbsolutePath());
            File lf = new File(gwDir, "data/server.log");
            if (lf.length() > LOG_PUMP_MAX) {           // 与 pump 上限同口径：超了先翻代，重启从小文件起算
                File old = new File(gwDir, "data/server.log.1");
                if (old.exists()) old.delete();
                lf.renameTo(old);
            }
            final File logFile = lf;
            reader = new Thread(() -> pump(p.getInputStream(), logFile));
            reader.setDaemon(true);
            reader.start();
            final Process me = p;
            waiter = new Thread(() -> {
                try {
                    int rc = me.waitFor();
                    String why = exitMeaning(rc);
                    // 先判「我还是不是当前进程」（bug #4）：看门狗自愈先 SIGTERM 旧进程
                    // 再退避 2s 拉新的，而 Go 侧优雅停机最长 5s —— 旧进程的 waitFor
                    // 可能在新进程 setState(RUNNING) **之后**才返回，把状态和 lastExit
                    // 一起覆盖成「已停止 / rc=143」，而 7863 其实正在服务。
                    if (proc != me) {
                        appLog(this, "旧网关进程退出（已被新实例接管）：" + why);
                        return;
                    }
                    lastExit = why;
                    appLog(this, "网关进程退出：" + why);
                    try { getSharedPreferences(PREF, MODE_PRIVATE).edit().putString("last_exit", lastExit).apply(); } catch (Throwable ignored) {}
                    setState(ST_STOPPED, "已退出 " + why,
                             "网关进程退出：" + why + "，看门狗按退避重拉");
                } catch (Exception e) {
                    if (proc != me) return;       // 同上：被接管了就别写全局状态
                    setState(ST_STOPPED, "等待异常 " + e, "等待网关进程出错：" + e);
                }
                if (proc == me) proc = null;      // 只清自己那个句柄，别把看门狗刚拉起的顶掉
                refreshNotification();
            });
            waiter.setDaemon(true);
            waiter.start();
            refreshNotification();
        } catch (Throwable t) {
            setState(ST_STOPPED, "启动失败: " + t, "拉起网关失败：" + t);
            refreshNotification();
        }
    }

    /**
     * 把网关的 stdout/stderr 灌进 server.log。
     *
     * ⚠ 铁律（rc=141 老 bug 的根因）：**任何时候都不能关掉这个管道的读端**。
     * 旧实现写满 2MB 就 `break` 跳出 try-with-resources → BufferedReader 被关闭 →
     * 读端消失 → 网关下一次打日志写入 EPIPE，而 Go 运行时对 fd 1/2 的 EPIPE 会
     * **直接以 SIGPIPE 退出（exit 141）**。更毒的是：重启时 bytes 从 logFile.length()
     * 起算，文件早已 >2MB → 第一行就 break → **每次拉起几秒内必死**；而日志轮转要到
     * 4MB 才做，2~4MB 那段就是"看门狗怎么拉都是 rc141"。
     *
     * 现在的做法：超限（或写文件出错）之后只**继续把管道读空**、不再落盘；循环只在
     * EOF（子进程退出/主动关闭 stdout）时结束 —— 那才是唯一安全的关闭时机。
     */
    void pump(InputStream in, File logFile) {
        BufferedReader r = null;
        Writer w = null;
        try {
            r = new BufferedReader(new InputStreamReader(in, "UTF-8"));
            long bytes = logFile.length();
            boolean writeOn = bytes <= LOG_PUMP_MAX;
            if (writeOn) {
                try { w = new OutputStreamWriter(new FileOutputStream(logFile, true), "UTF-8"); }
                catch (Throwable t) { writeOn = false; w = null; }
            }
            String line;
            while ((line = r.readLine()) != null) {
                lastLine = line;
                if (!writeOn) continue;              // 只读空，保持管道通畅
                try {
                    w.write(line); w.write('\n'); w.flush();
                    // 按**实际写出的字节数**累计（issue B7）：String.length() 是
                    // UTF-16 字符数，中文日志每字符写 3 字节 UTF-8 —— 用字符数当字节数，
                    // LOG_PUMP_MAX=2MB 实际能落 6MB，而翻代判断用的是 logFile.length()
                    // （真实字节），两处口径不一致，2MB/4MB 边界行为会跳变。
                    bytes += line.getBytes(java.nio.charset.StandardCharsets.UTF_8).length + 1;
                    if (bytes > LOG_PUMP_MAX) {      // 超限：停写，但**不关读端**
                        writeOn = false;
                        try { w.close(); } catch (Throwable ignored) {}
                        w = null;
                    }
                } catch (Throwable t) {
                    writeOn = false;
                    try { if (w != null) w.close(); } catch (Throwable ignored) {}
                    w = null;
                }
            }
        } catch (Throwable ignored) {
            // 读取线程自身异常：也要把管道读空，别让子进程撞 EPIPE。
            try { byte[] buf = new byte[8192]; while (in.read(buf) > 0) { /* drain */ } } catch (Throwable ignored2) {}
        } finally {
            try { if (w != null) w.close(); } catch (Throwable ignored) {}
            try { if (r != null) r.close(); } catch (Throwable ignored) {}
        }
    }

    void ensureConfig(File gwDir) {
        File cfg = new File(gwDir, "config.json");
        String key = "wb2api-" + UUID.randomUUID().toString().replace("-", "").substring(0, 20);
        // 文件已存在也要检查 api_key（bug-03）：首次生成被打断、或用户手改留下
        // "CHANGE_ME"/"test_key"/空串时，网关的 checkListenKey 会直接 log.Fatalf 退出
        // （rc=1），看门狗每 10s 一拍按退避无限重拉也永远起不来，界面只有「启动中」。
        // 这里**只**在 api_key 仍是占位/空时原地替换，绝不碰用户其它配置。
        if (cfg.exists() && cfg.length() > 0) {
            String cur = readTextFile(cfg);
            if (cur == null || cur.isEmpty()) return;
            java.util.regex.Matcher m = java.util.regex.Pattern
                    .compile("\"api_key\"\\s*:\\s*\"([^\"]*)\"")
                    .matcher(cur);
            if (!m.find()) return;                 // 没有该键：交给网关自己的默认值
            String curKey = m.group(1).trim();
            boolean bad = curKey.isEmpty()
                    || "CHANGE_ME".equalsIgnoreCase(curKey)
                    || "test_key".equalsIgnoreCase(curKey)
                    || "sk-xxx".equalsIgnoreCase(curKey);
            if (!bad) return;                      // 正常 key：一个字都不改
            String fixed = m.replaceFirst("\"api_key\": \"" + key + "\"");
            try (java.io.FileOutputStream os = new java.io.FileOutputStream(cfg)) {
                os.write(fixed.getBytes("UTF-8"));
            } catch (Throwable t) {
                appLog(this, "config.json 的占位 api_key 替换失败：" + t);
                return;
            }
            appLog(this, "config.json 里是占位/空 api_key，已替换为随机 key（否则网关拒绝启动）");
            return;
        }
        try (InputStream is = getAssets().open("config.json")) {
            // 必须读到 EOF：单次 read() 只保证「至少读到 1 字节」，assets 一旦涨过缓冲
            // 上限就被截成半截 JSON，网关读配置失败起不来；按字节切 UTF-8 还会切出乱码。
            java.io.ByteArrayOutputStream bo = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = is.read(buf)) > 0) bo.write(buf, 0, n);
            String tpl = (bo.size() > 0) ? new String(bo.toByteArray(), "UTF-8") : "{}";
            // 模板里的 api_key 是占位符 CHANGE_ME（旧版是 test_key，两个都换）。
            // 换成随机 key 后网关才肯在非回环监听下启动（config.normalize 的守卫）。
            tpl = tpl.replace("\"api_key\": \"CHANGE_ME\"", "\"api_key\": \"" + key + "\"");
            tpl = tpl.replace("\"api_key\": \"test_key\"", "\"api_key\": \"" + key + "\"");
            if (!tpl.contains(key)) tpl = tpl.replace("CHANGE_ME", key).replace("test_key", key);
            try (FileOutputStream os = new FileOutputStream(cfg)) { os.write(tpl.getBytes("UTF-8")); }
        } catch (Throwable t) {
            try (FileOutputStream os = new FileOutputStream(cfg)) {
                os.write(("{\"listen\":\":7863\",\"api_key\":\"" + key + "\",\"auth_dir\":\"./auths\",\"state_file\":\"./data/state.json\"}").getBytes("UTF-8"));
            } catch (Throwable ignored) {}
        }
    }

    /** 写 resolv.conf：优先用系统当前 DNS，取不到则回落公共 DNS（Go 不做系统解析，只能读文件） */
    File writeResolvConf(File gwDir) {
        StringBuilder sb = new StringBuilder();
        try {
            ConnectivityManager cm = (ConnectivityManager) getSystemService(Context.CONNECTIVITY_SERVICE);
            for (Network n : cm.getAllNetworks()) {
                LinkProperties lp = cm.getLinkProperties(n);
                if (lp == null) continue;
                for (InetAddress a : lp.getDnsServers()) {
                    if (a instanceof Inet4Address) sb.append("nameserver ").append(a.getHostAddress()).append('\n');
                }
            }
        } catch (Throwable ignored) {}
        if (sb.length() == 0) sb.append("nameserver 8.8.8.8\nnameserver 223.5.5.5\n");
        File f = new File(gwDir, "resolv.conf");
        try (FileOutputStream os = new FileOutputStream(f)) { os.write(sb.toString().getBytes("UTF-8")); }
        catch (Throwable ignored) {}
        return f;
    }

    /** 杀掉当前网关句柄。与 startGateway 同锁：proc 的读改写必须串行，否则两个线程
     *  交叉执行会出现「拉起后立刻被另一个线程的 killProc 清空句柄」（孤儿进程）。 */
    synchronized void killProc() {
        Process p = proc;
        proc = null;
        if (p == null) return;
        try { p.destroy(); } catch (Throwable ignored) {}
        // 强杀兜底（bug #4）：只发 SIGTERM 的话，Go 侧一旦卡在优雅停机（在途 SSE 排空 /
        // 锁竞争）就会留下一个**占着 7863 的旧进程** —— 新进程 bind 失败立刻退出，
        // 看门狗反复自愈也救不回来，只有系统杀掉 App 才恢复。
        forceKillLater(p, 3000);
    }

    /** 延时强杀：到点仍活着就 destroyForcibly（API 26+；更低版本只能再 destroy 一次）。 */
    static void forceKillLater(final Process p, final long delayMs) {
        if (p == null) return;
        Thread t = new Thread(new Runnable() {
            public void run() {
                try { Thread.sleep(delayMs); } catch (InterruptedException e) { return; }
                if (!isAlive(p)) return;
                try {
                    if (Build.VERSION.SDK_INT >= 26) p.destroyForcibly();
                    else p.destroy();
                } catch (Throwable ignored) {}
            }
        }, "wb2api-forcekill");
        t.setDaemon(true);
        t.start();
    }

    // ---------- 保活三件套 ----------
    /**
     * 为什么必须有 partial wakelock：看门狗是用户态定时器。屏幕一灭 SoC 进 suspend，
     * 10s 的 tick 直接停摆，Go 里的签到/保活定时器同理 —— 症状就是"只有 App 在前台时才生效"。
     * 代价是耗电（CPU 不睡）。不想常驻就在通知栏点「停止」，会一并释放。
     */
    void holdWake() {
        try {
            if (wake == null) {
                PowerManager pm = (PowerManager) getSystemService(Context.POWER_SERVICE);
                wake = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "wb2api:gateway");
                wake.setReferenceCounted(false);   // 反复 acquire 不叠加，release 一次就放
            }
            if (!wake.isHeld()) wake.acquire();
        } catch (Throwable ignored) {}
        holdWifi();
        installOverlay();
    }

    /**
     * Wi-Fi 高性能锁：从电脑/别的设备经局域网连过来时，屏幕一灭 Wi-Fi 就进省电甚至休眠，
     * 表现同样是"连不上"。127.0.0.1 本机连不受影响，所以这条是给局域网场景的。
     */
    void holdWifi() {
        try {
            if (wifiLock == null) {
                WifiManager wm = (WifiManager) getApplicationContext().getSystemService(Context.WIFI_SERVICE);
                if (wm == null) return;
                try { wifiLock = wm.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "wb2api:wifi"); }
                catch (Throwable t) { wifiLock = wm.createWifiLock(WifiManager.WIFI_MODE_FULL, "wb2api:wifi"); }
                wifiLock.setReferenceCounted(false);
            }
            if (!wifiLock.isHeld()) wifiLock.acquire();
        } catch (Throwable ignored) {}
    }

    /**
     * 「常驻图层」：一个 1×1、全透明、不接收触摸的悬浮窗。
     *
     * 为什么它可以治"过一会儿连不上"：系统按进程的**可见性**分级调度 —— 有可见窗口的进程
     * 不会被判定为缓存态，也就不会被 app-freezer 冻住（被冻住的进程连线程都停摆，
     * 看门狗自然也救不了自己）。代价是需要在系统设置里给「显示在其他应用上层」权限；
     * 没给就静默跳过，不影响任何其它功能。
     */
    /**
     * 常驻 1x1 全透明图层。**必须 synchronized**（issue B6）：
     * 判空 → addView → 赋值不是原子操作，而 onStartCommand（主线程）与 watchLoop
     * （看门狗线程）都会经 holdWake() 走到这里 —— 最坏情况加两个悬浮窗，静态字段
     * 只记住后一个，前一个再也 removeView 不掉（WindowManager token 泄漏，且系统
     * 仍认为该进程有可见窗口，保活语义被放大）。
     */
    // 一把显式静态锁（bug #17）：installOverlay 原来是 synchronized（锁 Service 实例），
    // removeOverlay 是 static 且无同步 —— 两者不是同一把锁。releaseWake（主线程）与
    // installOverlay（看门狗线程）交错时，overlay=null 之后另一线程才 addView 并覆盖
    // 静态字段，前一个 View 永远 remove 不掉（系统仍认为进程有可见窗口）。
    static final Object OVERLAY_LOCK = new Object();

    void installOverlay() {
        synchronized (OVERLAY_LOCK) {
            installOverlayLocked();
        }
    }

    private void installOverlayLocked() {
        if (overlay != null) return;
        try {
            if (Build.VERSION.SDK_INT < 23 || !Settings.canDrawOverlays(this)) return;
            int type = Build.VERSION.SDK_INT >= 26
                    ? WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY
                    : WindowManager.LayoutParams.TYPE_PHONE;
            WindowManager.LayoutParams lp = new WindowManager.LayoutParams(
                    1, 1, type,
                    WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE
                            | WindowManager.LayoutParams.FLAG_NOT_TOUCHABLE
                            | WindowManager.LayoutParams.FLAG_NOT_TOUCH_MODAL,
                    PixelFormat.TRANSLUCENT);
            View v = new View(this);
            v.setAlpha(0f);
            WindowManager wm = (WindowManager) getSystemService(Context.WINDOW_SERVICE);
            if (wm == null) return;
            wm.addView(v, lp);
            overlay = v;
        } catch (Throwable t) {
            // 不要覆盖 reason（bug B15）：reason 是界面第三行展示的「为什么不可用」，
            // 例如「启动被系统拒」这类关键原因会被这条次要提示盖掉，用户看到的解释
            // 与真实故障无关。改用独立字段，由 MainActivity 拼在保活状态串里。
            overlayErr = "常驻图层没加上（" + t.getClass().getSimpleName() + "）";
            appLog(this, "常驻图层失败：" + t + "（系统可能仍会冻结本进程）");
        }
    }

    static void removeOverlay() {
        synchronized (OVERLAY_LOCK) {
            removeOverlayLocked();
        }
    }

    private static void removeOverlayLocked() {
        final View v = overlay;
        overlay = null;
        if (v == null) return;
        try {
            WindowManager wm = (WindowManager) v.getContext().getSystemService(Context.WINDOW_SERVICE);
            if (wm != null) wm.removeView(v);
        } catch (Throwable ignored) {}
    }

    static void releaseWake() {
        try { if (wake != null && wake.isHeld()) wake.release(); } catch (Throwable ignored) {}
        try { if (wifiLock != null && wifiLock.isHeld()) wifiLock.release(); } catch (Throwable ignored) {}
        removeOverlay();
    }

    static boolean wakeHeld() {
        try { return wake != null && wake.isHeld(); } catch (Throwable t) { return false; }
    }

    // ---------- 系统级兜底：alarm ----------
    static void armAlarm(Context c) {
        try {
            AlarmManager am = (AlarmManager) c.getSystemService(Context.ALARM_SERVICE);
            Intent i = new Intent(c, GatewayService.class).setAction(ACTION_WATCH);
            int fl = PendingIntent.FLAG_UPDATE_CURRENT;
            if (Build.VERSION.SDK_INT >= 23) fl |= PendingIntent.FLAG_IMMUTABLE;
            PendingIntent pi = PendingIntent.getService(c, ALARM_REQ, i, fl);
            long due = SystemClock.elapsedRealtime() + ALARM_MS;
            // 能精确就精确：Doze 里 setAndAllowWhileIdle 会被从 5 分钟拖到 ~9 分钟一次
            boolean exact = Build.VERSION.SDK_INT < 31 || am.canScheduleExactAlarms();
            if (exact) am.setExactAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, due, pi);
            else am.setAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, due, pi);
        } catch (Throwable ignored) {}
    }

    static void cancelAlarm(Context c) {
        try {
            AlarmManager am = (AlarmManager) c.getSystemService(Context.ALARM_SERVICE);
            Intent i = new Intent(c, GatewayService.class).setAction(ACTION_WATCH);
            int fl = PendingIntent.FLAG_UPDATE_CURRENT;
            if (Build.VERSION.SDK_INT >= 23) fl |= PendingIntent.FLAG_IMMUTABLE;
            am.cancel(PendingIntent.getService(c, ALARM_REQ, i, fl));
        } catch (Throwable ignored) {}
    }

    // ---------- 探针 ----------
    /** 端口探测（带超时，避免主线程卡顿） */
    static boolean portUp() {
        Socket s = new Socket();
        try {
            s.connect(new InetSocketAddress("127.0.0.1", 7863), 150);
            return true;
        } catch (Throwable t) {
            return false;
        } finally {
            try { s.close(); } catch (Throwable ignored) {}
        }
    }

    /** 健康探针：拿到任何 HTTP 响应都算活着（还没绑账号时 /healthz 是 503，也算活着） */
    static boolean httpUp() {
        HttpURLConnection c = null;
        try {
            c = (HttpURLConnection) new URL("http://127.0.0.1:7863/healthz").openConnection();
            c.setConnectTimeout(2000);
            c.setReadTimeout(2000);
            return c.getResponseCode() > 0;
        } catch (Throwable t) {
            return false;
        } finally {
            if (c != null) c.disconnect();
        }
    }

    boolean isUp() { return httpUp(); }

    // ---------- 通知 ----------
    void ensureChannel() {
        if (Build.VERSION.SDK_INT < 26) return;
        NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        if (nm.getNotificationChannel(CH_ID) != null) return;
        NotificationChannel ch = new NotificationChannel(CH_ID, "网关服务", NotificationManager.IMPORTANCE_LOW);
        ch.setShowBadge(false);
        nm.createNotificationChannel(ch);
    }

    Notification buildNotification() {
        Intent open = new Intent(this, MainActivity.class);
        int fl = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= 23) fl |= PendingIntent.FLAG_IMMUTABLE;
        PendingIntent pi = PendingIntent.getActivity(this, 0, open, fl);

        Intent stopI = new Intent(this, GatewayService.class);
        stopI.setAction(ACTION_STOP);
        PendingIntent stopPi = PendingIntent.getService(this, 1, stopI, fl);

        Notification.Builder b = (Build.VERSION.SDK_INT >= 26)
                ? new Notification.Builder(this, CH_ID)
                : new Notification.Builder(this);
        b.setSmallIcon(android.R.drawable.stat_notify_sync)
         .setContentTitle("WorkBuddy2API 网关")
         .setContentIntent(pi)
         .setOngoing(true)
         .addAction(android.R.drawable.ic_menu_close_clear_cancel, "停止", stopPi);
        // 不在这里发网络探针：调用方常是主线程（onStartCommand 里要 5s 内进前台），
        // 探测最长拖 2s 会把自己拖成 ANR。只看缓存状态。
        boolean up = (statusOf() == ST_RUNNING || statusOf() == ST_EXTERNAL);
        String s = (up ? "运行中 · " : "启动中 · ") + "127.0.0.1:7863"
                + (restarts > 0 ? " · 自愈 " + restarts + " 次" : "");
        b.setContentText(s);
        return b.build();
    }

    /**
     * 进前台。@return null = 成功；非 null = 失败原因（**别吞异常**，界面要显示它）。
     *
     * 类型用 specialUse 而不是 dataSync：dataSync 在 Android 15+ 有「6 小时/天」上限，
     * 对一个要 7×24 常驻的自托管网关是致命的（到点被系统掐，且只在 onTimeout 里说一声）。
     */
    String startForegroundCompat() {
        Notification n = buildNotification();
        try {
            if (Build.VERSION.SDK_INT >= 34) {
                try {
                    startForeground(NOTIF_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
                    return null;
                } catch (Throwable t34) {
                    // 个别 ROM/权限组合不认显式类型 → 退回不带类型的老调用，别直接死
                }
            }
            if (Build.VERSION.SDK_INT >= 26) startForeground(NOTIF_ID, n);
            else { n.flags |= Notification.FLAG_ONGOING_EVENT; startForeground(NOTIF_ID, n); }
            return null;
        } catch (Throwable t) {
            return "startForeground 被拒：" + t.getClass().getSimpleName()
                    + (t.getMessage() == null ? "" : "（" + t.getMessage() + "）");
        }
    }

    /** 前台服务被系统判超时回收（Android 14+ 的 onTimeout / 15+ 的 FGS 时限）→ 立刻挂兜底 */
    // onTimeout 的语义是「你自己停下来，几秒内不停就 ANR / 直接打死进程」（bug #4）。
    // 此前只 setState + armAlarm、不 stopSelf，本该靠 alarm 兜底自愈的路径变成进程
    // 被系统当场杀掉。服务是 START_STICKY，停掉后 alarm / JobScheduler 照常拉回。
    @Override public void onTimeout(int startId) {
        setState(ST_STARTING, "前台服务超时被系统回收",
                 "系统按 FGS 时限回收了前台服务，已挂 5 分钟 alarm 兜回来自启");
        appLog(this, "前台服务被系统按 FGS 时限回收，已挂 alarm 兜底");
        armAlarm(this);
        try { stopSelf(startId); } catch (Throwable ignored) {}
    }

    @Override public void onTimeout(int startId, int fgsType) {
        setState(ST_STARTING, "前台服务超时被系统回收",
                 "系统按 FGS 时限回收了前台服务（type=" + fgsType + "），已挂 5 分钟 alarm 兜回来自启");
        appLog(this, "前台服务被系统按 FGS 时限回收（type=" + fgsType + "），已挂 alarm 兜底");
        armAlarm(this);
        try { stopSelf(startId); } catch (Throwable ignored) {}
    }

    void stopForegroundCompat() {
        try { stopForeground(true); } catch (Throwable ignored) {}
    }

    void refreshNotification() {
        try {
            NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
            nm.notify(NOTIF_ID, buildNotification());
        } catch (Throwable ignored) {}
    }
}
