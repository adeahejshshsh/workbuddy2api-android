package com.dsh.wb2api;

import android.app.job.JobInfo;
import android.app.job.JobParameters;
import android.app.job.JobScheduler;
import android.app.job.JobService;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.os.Build;

/**
 * 看门狗第五层：JobScheduler 周期兜底。
 *
 * 为什么还要这一层（已经有 alarm 了）：alarm 和 job 是两套互不相干的调度机制 ——
 * alarm 在 Doze 里会被拖（setAndAllowWhileIdle 最短 ~9 分钟一次），job 走的是系统的
 * 维护窗口，且 setPersisted(true) 让它在**重启手机之后**依然被系统记着。7863 被杀很凶时，
 * 多一条系统自己管的复活路径就多一分活命率。
 *
 * 注意：这只在"服务被系统杀了"时有用；用户在通知里点"停止"是明确意图，
 * GatewayService 会置 userStop 并取消 alarm/job（见 ACTION_STOP 分支）。
 */
public class ReviveJobService extends JobService {
    static final int JOB_ID = 0x7B2A;
    static final long PERIOD_MS = 15 * 60_000L;   // Android 8+ 周期任务最小值就是 15 分钟

    /** 幂等排一次（已排过就不重复排） */
    static void schedule(Context c) {
        try {
            JobScheduler js = (JobScheduler) c.getSystemService(Context.JOB_SCHEDULER_SERVICE);
            if (js == null) return;
            for (JobInfo j : js.getAllPendingJobs()) if (j.getId() == JOB_ID) return;
            JobInfo.Builder b = new JobInfo.Builder(JOB_ID, new ComponentName(c, ReviveJobService.class))
                    .setPersisted(true)
                    .setPeriodic(PERIOD_MS);
            b.setBackoffCriteria(6 * 60_000L, JobInfo.BACKOFF_POLICY_EXPONENTIAL);
            js.schedule(b.build());
            GatewayService.appLog(c, "第五层 JobScheduler 兜底已排（15 分钟周期，重启手机仍在）");
        } catch (Throwable ignored) {}
    }

    static void cancel(Context c) {
        try {
            JobScheduler js = (JobScheduler) c.getSystemService(Context.JOB_SCHEDULER_SERVICE);
            if (js != null) js.cancel(JOB_ID);
            GatewayService.appLog(c, "第五层 JobScheduler 兜底已取消（用户主动停止）");
        } catch (Throwable ignored) {}
    }

    @Override public boolean onStartJob(final JobParameters p) {
        // JobService 回调跑在主线程：portUp() 是 150ms 连接 + appLog() 要开文件写盘，
        // 慢磁盘/冷启动时命中 StrictMode 违规，极端情况挤进 ANR 窗口。全部丢后台。
        new Thread(new Runnable() {
            public void run() {
                try {
                    // 用户已主动停止就不再拉起（BUG-06）：BootReceiver 判了这个标记，
                    // JobScheduler 这条复活路径此前没判 —— cancel() 抛异常被吞、或
                    // 系统已重排时，网关会被重新拉起，与「点了停止就不再自愈」相反。
                    boolean userStopped;
                    try {
                        userStopped = getSharedPreferences(GatewayService.PREF, MODE_PRIVATE)
                                .getBoolean("user_stop", false);
                    } catch (Throwable t) {
                        userStopped = false;
                    }
                    if (userStopped) {
                        GatewayService.appLog(ReviveJobService.this,
                                "JobScheduler 触发：用户已主动停止，不拉起");
                        // 不在这里调 jobFinished（BUG-19）：return 会走 finally，
                        // 那里已经调过一次；重复调用会被系统记错误并按非法收尾处理。
                        return;
                    }
                    boolean up = GatewayService.portUp();
                    // 端口从配置读（review #28）：写死 7863 时，改过 listen 的部署
                    // 日志会指向错误端口，排障被带偏。
                    GatewayService.appLog(ReviveJobService.this,
                            "JobScheduler 触发：" + GwConfig.port(ReviveJobService.this) + " " + (up ? "在服务，不打扰" : "不在服务 → 拉起服务"));
                    if (!up) {                   // 7863 不在服务 → 把服务叫起来（它自己去拉起网关）
                        try {
                            Intent i = new Intent(ReviveJobService.this, GatewayService.class);
                            if (Build.VERSION.SDK_INT >= 26) startForegroundService(i);
                            else startService(i);
                        } catch (Throwable t) {  // 后台起前台服务被拒 → 退回普通 startService
                            GatewayService.appLog(ReviveJobService.this,
                                    "JobScheduler 起前台服务被拒：" + t.getClass().getSimpleName());
                            try { startService(new Intent(ReviveJobService.this, GatewayService.class)); }
                            catch (Throwable t2) {
                                GatewayService.appLog(ReviveJobService.this, "JobScheduler 拉起服务失败：" + t2);
                            }
                        }
                    }
                } catch (Throwable ignored) {
                    // 兜底：绝不让后台线程的异常冒到系统
                } finally {
                    jobFinished(p, false);       // 干完了，告诉系统别再等我
                }
            }
        }, "revive-job").start();
        return true;                             // 有活没干完：由 jobFinished 收尾
    }

    @Override public boolean onStopJob(JobParameters p) { return true; }   // 被中断 → 重排
}
