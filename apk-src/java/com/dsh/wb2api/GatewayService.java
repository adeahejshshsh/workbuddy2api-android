package com.dsh.wb2api;

import android.app.AlarmManager;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
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

    // ── 对外状态：机器可读（UI 只读这两个，不解析下面的中文 state）
    public static final int ST_STOPPED = 0, ST_STARTING = 1, ST_RUNNING = 2, ST_EXTERNAL = 3;
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
    public static void stop(Context c) { c.stopService(new Intent(c, GatewayService.class)); }

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
            watching = false;
            cancelAlarm(this);
            killProc();
            releaseWake();
            stopForegroundCompat();
            stopSelf();
            return START_NOT_STICKY;
        }
        ensureChannel();
        String fgsErr = startForegroundCompat();
        if (fgsErr != null) {   // 起不来就别赖着，否则等 5s 被系统判 ANR 崩掉
            setState(ST_STOPPED, "前台服务起不来", fgsErr);
            stopSelf();
            return START_NOT_STICKY;
        }
        holdWake();   // 保活：不持它，后台时看门狗线程与 Go 定时器都会被冻住
        if (intent != null && intent.getBooleanExtra(EXTRA_RESTART, false)) killProc();
        if (!alive()) {
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

    void selfHeal(String why) {
        int n = ++restarts;
        setState(ST_STARTING, "看门狗自愈 #" + n + "：" + why, "看门狗正在重拉网关：" + why);
        killProc();
        refreshNotification();
        sleep(backoffMs(n));            // 退避，别打连环炮
        if (!watching) return;
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
    void startGateway() {
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
            final File logFile = new File(gwDir, "data/server.log");
            reader = new Thread(() -> pump(p.getInputStream(), logFile));
            reader.setDaemon(true);
            reader.start();
            final Process me = p;
            waiter = new Thread(() -> {
                try { int rc = me.waitFor(); setState(ST_STOPPED, "已退出 rc=" + rc,
                        "网关进程退出了（rc=" + rc + "），看门狗会按退避重拉"); }
                catch (Exception e) { setState(ST_STOPPED, "等待异常 " + e, "等待网关进程出错：" + e); }
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

    void pump(InputStream in, File logFile) {
        try (BufferedReader r = new BufferedReader(new InputStreamReader(in, "UTF-8"));
             Writer w = new OutputStreamWriter(new FileOutputStream(logFile, true), "UTF-8")) {
            String line;
            long bytes = logFile.length();
            while ((line = r.readLine()) != null) {
                lastLine = line;
                w.write(line); w.write('\n'); w.flush();
                bytes += line.length() + 1;
                if (bytes > 2 * 1024 * 1024) break;   // 简单限长，超了就不再写
            }
        } catch (Throwable ignored) {}
    }

    void ensureConfig(File gwDir) {
        File cfg = new File(gwDir, "config.json");
        if (cfg.exists() && cfg.length() > 0) return;
        String key = "wb2api-" + UUID.randomUUID().toString().replace("-", "").substring(0, 20);
        try (InputStream is = getAssets().open("config.json")) {
            byte[] buf = new byte[8192];
            int n = is.read(buf);
            String tpl = (n > 0) ? new String(buf, 0, n, "UTF-8") : "{}";
            tpl = tpl.replace("\"api_key\": \"test_key\"", "\"api_key\": \"" + key + "\"");
            if (!tpl.contains(key)) tpl = tpl.replace("test_key", key);
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

    void killProc() {
        Process p = proc;
        proc = null;
        if (p != null) { try { p.destroy(); } catch (Throwable ignored) {} }
    }

    // ---------- 保活：partial wakelock ----------
    /**
     * 为什么必须有：看门狗是用户态定时器。屏幕一灭 SoC 进 suspend，10s 的 tick 直接停摆，
     * Go 里的签到/保活定时器同理 —— 症状就是"只有 App 在前台（屏幕亮着）时才生效"。
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
    }

    static void releaseWake() {
        try { if (wake != null && wake.isHeld()) wake.release(); } catch (Throwable ignored) {}
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
            am.setAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, SystemClock.elapsedRealtime() + ALARM_MS, pi);
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
    @Override public void onTimeout(int startId) {
        setState(ST_STARTING, "前台服务超时被系统回收",
                 "系统按 FGS 时限回收了前台服务，已挂 5 分钟 alarm 兜回来自启");
        armAlarm(this);
    }

    @Override public void onTimeout(int startId, int fgsType) {
        setState(ST_STARTING, "前台服务超时被系统回收",
                 "系统按 FGS 时限回收了前台服务（type=" + fgsType + "），已挂 5 分钟 alarm 兜回来自启");
        armAlarm(this);
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
