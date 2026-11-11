package com.workbuddy.api;

import android.content.Context;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.List;

/**
 * 网关数据层：**只负责取数与解析**，不含任何 android.view / widget 引用。
 *
 * UI（Console.java / ConfigActivity.java）只与本类打交道，拿到的是这里定义的模型对象，
 * 不再自己读 JSONObject —— 这样「界面长什么样」与「接口返回什么形状」彻底解耦：
 *   - 接口加字段：改本类；
 *   - 界面改版：改 Console；
 *   - 两者都不需要动另一边的代码。
 *
 * 传输层仍是 PanelApi（鉴权/超时/错误文案），本类只做「路径 + 解析 + 模型」。
 */
final class PanelService {

    private final Context ctx;

    PanelService(Context c) { this.ctx = c.getApplicationContext(); }

    // ── 回调 ───────────────────────────────────────────────────────────────
    interface Cb<T> {
        void ok(T value);
        void fail(String msg);
    }

    /** 二进制下载回调（导出账号）。 */
    interface FileCb {
        void ok(byte[] data, String filename);
        void fail(String msg);
    }

    // ── 模型 ───────────────────────────────────────────────────────────────
    static final class Account {
        String uid = "", nickname = "", remark = "", group = "", realm = "", override = "";
        long credits, creditsTotal;
        int success, errTotal, inFlight;
        boolean cooling, disabled, paused, enterprise, apiKey;
        String disabledReason = "";
        String statusText() {
            if (disabled) return "已禁用";
            if (paused) return "已暂停选号";
            if (cooling) return "冷却中";
            return "可用";
        }
    }

    static final class Overview {
        int total, healthy, cooling, disabled, paused, sticky;
        String version = "", redisMode = "";
        boolean authRequired;
        long uptimeSec;
        final List<Account> accounts = new ArrayList<Account>();
    }

    static final class Usage {
        long requests, errors, promptTokens, completionTokens, totalTokens;
        double cacheHitRate, avgLatencyMs, avgTps, credits, creditsPer1M;
        String from = "", to = "";
        static final class Row { String key = "", extra = "", realm = ""; long requests, tokens; double credits; }
        final List<Row> byAccount = new ArrayList<Row>(), byModel = new ArrayList<Row>();
    }

    static final class ModelInfo {
        String id = "", name = "", tags = "";
        double credits, promoCredits;
        boolean reasoning, images, tools;
        boolean hasPromo() { return promoCredits > 0 && promoCredits < credits; }
    }

    static final class Queue {
        boolean running, started;
        int total, conc;
        static final class Item { String uid = "", nickname = "", code = "", status = "", message = ""; }
        final List<Item> items = new ArrayList<Item>();
    }

    static final class Job {
        String uid = "", status = "", message = "", error = "", updatedAt = "";
        String jobId = "", currentTask = "";
        int total, completed, failed, skipped;

        static Job from(JSONObject j) {
            Job x = new Job();
            if (j == null) return x;
            x.uid = j.optString("uid"); x.status = j.optString("status");
            x.message = j.optString("message"); x.error = j.optString("error");
            x.updatedAt = j.optString("updated_at");
            x.jobId = j.optString("job_id"); x.currentTask = j.optString("current_task");
            x.total = j.optInt("total"); x.completed = j.optInt("completed");
            x.failed = j.optInt("failed"); x.skipped = j.optInt("skipped");
            return x;
        }

        /** 已处理项数（作业总项数未知时用它当分母的下界）。 */
        int handled() { return completed + failed + skipped; }
    }

    static final class Pkg {
        String name = "", endTime = "";
        long remain, size;
    }
    static final class PkgAccount {
        String uid = "", nickname = "", realm = "", plan = "", planEvidence = "", error = "";
        long remain, size;
        final List<Pkg> packages = new ArrayList<Pkg>();
    }

    static final class BalanceSeries {
        String uid = "", name = "", realm = "";
        long current, min, max, change;
        final List<Float> values = new ArrayList<Float>();
    }

    static final class Voucher {
        String prize = "", code = "", validTo = "";
    }
    static final class VoucherAccount {
        String uid = "", nickname = "", error = "";
        final List<Voucher> vouchers = new ArrayList<Voucher>();
    }

    static final class ErrStat {
        String account = "", kind = "", code = "", model = "", last = "", sev = "warn",
                until = "", sample = "";
        int count;
    }
    static final class ErrSummary {
        int scanned;
        String updatedAt = "";
        final List<ErrStat> rows = new ArrayList<ErrStat>();
    }

    static final class Metrics {
        long completed, inFlight, succeeded, failed;
        double successRate, httpSuccessRate, avgDurationMs;
        boolean archiveEnabled;
        int archiveFiles;
        double archiveBytes;
    }

    static final class LogRow { String time = "", channel = "", text = ""; }

    static final class ReqEvent {
        String time = "", requestId = "", path = "", account = "", model = "", modelEff = "",
                outcome = "", clientIp = "", userAgent = "", context = "";
        int status;
        long durationMs, ttfbMs, promptTokens, completionTokens, totalTokens;
        double credit;
        boolean creditKnown;
        long cacheHit, cacheMiss;
        int toolsDeclared, toolCalls;
    }

    static final class TaskItem {
        String code = "", title = "", acceptStatus = "";
        int current, target, credit;
        boolean claimable, claimed, locked;
    }
    static final class TasksView {
        String realm = "", message = "";
        boolean canAccept = true, canClaim = true, canAuto = true;
        final List<TaskItem> tasks = new ArrayList<TaskItem>();
    }

    static final class TaskResult {
        String message = "", error = "";
        int accepted, done, skipped, failed;
    }

    static final class ProbeResult { String verdict = "", detail = ""; }

    static final class ModelSwitch { String model = ""; boolean off; }

    static final class ImportResult {
        int imported, skipped, total;
        final List<String> errors = new ArrayList<String>();
    }

    static final class LoginStart { String state = "", url = ""; }
    static final class LoginPoll { boolean done; String uid = "", error = ""; }

    // ── 账号池 ─────────────────────────────────────────────────────────────
    void overview(final Cb<Overview> cb) {
        PanelApi.get(ctx, "/panel/api/overview", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                Overview v = new Overview();
                v.total = o.optInt("total"); v.healthy = o.optInt("healthy");
                v.cooling = o.optInt("cooling"); v.disabled = o.optInt("disabled");
                v.paused = o.optInt("paused"); v.sticky = o.optInt("sticky_sessions");
                v.version = o.optString("version", ""); v.redisMode = o.optString("redis_mode", "");
                v.authRequired = o.optBoolean("auth_required");
                v.uptimeSec = o.optLong("uptime_sec");
                JSONArray arr = o.optJSONArray("accounts");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject s = arr.optJSONObject(i);
                    if (s == null) continue;
                    Account a = new Account();
                    a.uid = s.optString("uid"); a.nickname = s.optString("nickname");
                    a.remark = s.optString("remark"); a.group = s.optString("group");
                    a.realm = s.optString("realm");
                    a.override = s.optString("enterprise_override");
                    a.credits = s.optLong("credits"); a.creditsTotal = s.optLong("credits_total");
                    a.success = s.optInt("success_count"); a.errTotal = s.optInt("err_total");
                    a.inFlight = s.optInt("in_flight");
                    a.cooling = s.optBoolean("cooling"); a.disabled = s.optBoolean("disabled");
                    a.paused = s.optBoolean("paused"); a.enterprise = s.optBoolean("enterprise");
                    a.apiKey = s.optBoolean("api_key");
                    a.disabledReason = s.optString("disabled_reason");
                    v.accounts.add(a);
                }
                cb.ok(v);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void reloadAccounts(final Cb<String> cb) {
        PanelApi.post(ctx, "/panel/api/accounts/reload", new JSONObject(), new PanelApi.Cb() {
            public void ok(JSONObject o) { cb.ok("目录已重载"); }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void checkin(String uid, final Cb<TaskResult> cb) {
        PanelApi.post(ctx, account(uid) + "/checkin", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                TaskResult r = new TaskResult();
                r.message = o.optString("checkin_message");
                if (o.has("credits")) r.message = r.message + "|" + o.optLong("credits")
                        + "|" + o.optLong("credits_total");
                if (o.optBoolean("checkin_skipped")) r.error = "skipped";
                else if (!o.optBoolean("checkin_done")) r.error = "not_done";
                if (o.has("balance_error")) r.error = o.optString("balance_error");
                cb.ok(r);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void balance(String uid, final Cb<long[]> cb) {
        PanelApi.post(ctx, account(uid) + "/balance", null, new PanelApi.Cb() {
            public void ok(JSONObject o) { cb.ok(new long[]{o.optLong("credits"), o.optLong("credits_total")}); }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void pause(String uid, boolean paused, final Cb<String> cb) {
        post(account(uid) + (paused ? "/resume" : "/pause"), null, cb);
    }

    void disable(String uid, boolean disable, final Cb<String> cb) {
        post(account(uid) + (disable ? "/disable" : "/revive"), null, cb);
    }

    void remove(String uid, final Cb<String> cb) { post(account(uid) + "/remove", null, cb); }

    void enterprise(String uid, String override, final Cb<Boolean> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("override", override);
            PanelApi.post(ctx, account(uid) + "/enterprise", body, new PanelApi.Cb() {
                public void ok(JSONObject o) { cb.ok(o.optBoolean("changed")); }
                public void fail(String msg) { cb.fail(msg); }
            });
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void probeBan(String uid, final Cb<ProbeResult> cb) {
        PanelApi.post(ctx, account(uid) + "/probe_ban", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                ProbeResult r = new ProbeResult();
                r.verdict = o.optString("verdict");
                r.detail = o.optString("detail");
                cb.ok(r);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void setRemark(String uid, String text, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("remark", text);
            post(account(uid) + "/remark", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void setGroup(String uid, String text, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("group", text);
            post(account(uid) + "/group", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void clearSkip(String uid, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("clear", true);
            body.put("uid", uid);
            post("/panel/api/tasks/skip", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    // ── 模型开关 ───────────────────────────────────────────────────────────
    void models(final Cb<List<ModelInfo>> cb) {
        PanelApi.get(ctx, "/panel/api/models", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<ModelInfo> out = new ArrayList<ModelInfo>();
                JSONArray arr = o.optJSONArray("models");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject m = arr.optJSONObject(i);
                    if (m == null) continue;
                    ModelInfo mi = new ModelInfo();
                    mi.id = m.optString("id"); mi.name = m.optString("name", mi.id);
                    mi.tags = m.optString("tags");
                    mi.credits = m.optDouble("credits", 0);
                    mi.promoCredits = m.optDouble("promo_credits", 0);
                    mi.reasoning = m.optBoolean("supports_reasoning");
                    mi.images = m.optBoolean("supports_images");
                    mi.tools = m.optBoolean("supports_tool_call");
                    out.add(mi);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void modelsOff(String uid, final Cb<List<String>> cb) {
        PanelApi.get(ctx, account(uid) + "/models_off", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<String> out = new ArrayList<String>();
                JSONArray arr = o.optJSONArray("list");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject it = arr.optJSONObject(i);
                    if (it != null) out.add(it.optString("model"));
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void setModelOff(String uid, String model, boolean off, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("model", model);
            body.put("off", off);
            post(account(uid) + "/model_toggle", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    // ── 任务 ───────────────────────────────────────────────────────────────
    void tasks(String uid, final Cb<TasksView> cb) {
        PanelApi.get(ctx, account(uid) + "/tasks", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                TasksView v = new TasksView();
                v.realm = o.optString("realm"); v.message = o.optString("message");
                JSONObject caps = o.optJSONObject("capabilities");
                if (caps != null) {
                    v.canAccept = caps.optBoolean("accept", true);
                    v.canClaim = caps.optBoolean("claim", true);
                    v.canAuto = caps.optBoolean("automate", true);
                }
                JSONArray arr = o.optJSONArray("tasks");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject t = arr.optJSONObject(i);
                    if (t == null) continue;
                    TaskItem it = new TaskItem();
                    it.code = t.optString("task_code");
                    it.title = t.optString("title", it.code);
                    it.current = t.optInt("current"); it.target = t.optInt("target");
                    it.credit = t.optInt("credit");
                    it.claimable = t.optBoolean("claimable");
                    it.claimed = t.optBoolean("claimed");
                    it.locked = t.optBoolean("locked");
                    it.acceptStatus = t.optString("accept_status");
                    v.tasks.add(it);
                }
                cb.ok(v);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void acceptTask(String uid, String code, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            JSONArray codes = new JSONArray();
            codes.put(code);
            body.put("task_codes", codes);
            post(account(uid) + "/tasks/accept", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void claimTask(String uid, String code, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("task_code", code);
            post(account(uid) + "/tasks/claim", body, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void acceptAll(String uid, final Cb<TaskResult> cb) {
        PanelApi.post(ctx, account(uid) + "/tasks/accept_all", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                TaskResult r = new TaskResult();
                r.accepted = o.optInt("accepted");
                cb.ok(r);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    /**
     * 一键完成（异步）：立刻返回受理的作业，进度靠 {@link #taskJob} 轮询。
     *
     * 以前这里是同步 POST，服务端要跑完整条流水线才回（几十秒到十几分钟），
     * 客户端那条连接一直被占住 —— 表现就是「点了一键完成，界面一个数据都刷不出来」。
     * 现在服务端 202 立即返回作业 ID，客户端拿着它轮询作业记录。
     *
     * 兼容旧网关：若响应里没有 async 标记（说明对面还是同步实现），把 results
     * 直接折算成一个终态作业交回去，不再轮询。
     */
    void autoAllAsync(String uid, final Cb<Job> cb) {
        PanelApi.postSlow(ctx, account(uid) + "/tasks/auto_all?async=1", null, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                Job j = new Job();
                j.uid = uid;
                j.jobId = o.optString("job_id");
                if (!o.optBoolean("async")) {
                    JSONArray rs = o.optJSONArray("results");
                    for (int i = 0; rs != null && i < rs.length(); i++) {
                        JSONObject it = rs.optJSONObject(i);
                        String st = it == null ? "" : it.optString("status");
                        if ("done".equals(st)) j.completed++;
                        else if ("skipped".equals(st)) j.skipped++;
                        else j.failed++;
                    }
                    j.total = j.handled();
                    j.status = j.failed > 0 && j.completed > 0 ? "partial"
                            : j.failed > 0 ? "error" : "done";
                    j.message = "同步形态已完成";
                } else {
                    j.status = "running";
                    j.message = o.optString("message");
                }
                cb.ok(j);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    /** 单账号最新作业（异步一键完成的进度来源）。没有记录时 job 为 null。 */
    void taskJob(String uid, final Cb<Job> cb) {
        PanelApi.get(ctx, account(uid) + "/tasks/job", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONObject j = o.optJSONObject("job");
                cb.ok(j == null ? null : Job.from(j));
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void queue(final Cb<Queue> cb) {
        PanelApi.get(ctx, "/panel/api/queue", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                Queue q = new Queue();
                q.running = o.optBoolean("running"); q.started = o.optBoolean("started");
                q.total = o.optInt("total"); q.conc = o.optInt("conc");
                JSONArray arr = o.optJSONArray("items");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject it = arr.optJSONObject(i);
                    if (it == null) continue;
                    Queue.Item x = new Queue.Item();
                    x.uid = it.optString("uid"); x.nickname = it.optString("nickname");
                    x.code = it.optString("code"); x.status = it.optString("status");
                    x.message = it.optString("message");
                    q.items.add(x);
                }
                cb.ok(q);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void jobs(final Cb<List<Job>> cb) {
        PanelApi.get(ctx, "/panel/api/tasks/jobs", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<Job> out = new ArrayList<Job>();
                JSONArray arr = o.optJSONArray("jobs");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject j = arr.optJSONObject(i);
                    if (j == null) continue;
                    out.add(Job.from(j));
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    // ── 统计 ───────────────────────────────────────────────────────────────
    void usage(int hours, final Cb<Usage> cb) {
        PanelApi.get(ctx, "/panel/api/usage?hours=" + hours, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                Usage u = new Usage();
                u.from = o.optString("window_from"); u.to = o.optString("window_to");
                JSONObject t = o.optJSONObject("totals");
                if (t != null) {
                    u.requests = t.optLong("requests"); u.errors = t.optLong("errors");
                    u.promptTokens = t.optLong("prompt_tokens");
                    u.completionTokens = t.optLong("completion_tokens");
                    u.totalTokens = t.optLong("total_tokens");
                    u.cacheHitRate = t.optDouble("cache_hit_rate", 0);
                    u.avgLatencyMs = t.optDouble("avg_latency_ms", 0);
                    u.avgTps = t.optDouble("avg_tokens_per_second", 0);
                    u.credits = t.optDouble("credits", 0);
                    u.creditsPer1M = t.optDouble("credits_per_1m_tokens", 0);
                }
                readUsageRows(o.optJSONArray("by_account"), u.byAccount);
                readUsageRows(o.optJSONArray("by_model"), u.byModel);
                cb.ok(u);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    private static void readUsageRows(JSONArray arr, List<Usage.Row> out) {
        for (int i = 0; arr != null && i < arr.length(); i++) {
            JSONObject r = arr.optJSONObject(i);
            if (r == null) continue;
            Usage.Row x = new Usage.Row();
            x.key = r.optString("key"); x.extra = r.optString("extra");
            x.realm = r.optString("realm");
            x.requests = r.optLong("requests"); x.tokens = r.optLong("total_tokens");
            x.credits = r.optDouble("credits", 0);
            out.add(x);
        }
    }

    void packages(final Cb<List<PkgAccount>> cb) {
        PanelApi.get(ctx, "/panel/api/packages", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<PkgAccount> out = new ArrayList<PkgAccount>();
                JSONArray arr = o.optJSONArray("accounts");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject r = arr.optJSONObject(i);
                    if (r == null) continue;
                    PkgAccount a = new PkgAccount();
                    a.uid = r.optString("uid"); a.nickname = r.optString("nickname");
                    a.realm = r.optString("realm"); a.plan = r.optString("plan");
                    a.planEvidence = r.optString("plan_evidence");
                    a.error = r.optString("error");
                    a.remain = r.optLong("remain"); a.size = r.optLong("size");
                    JSONArray ps = r.optJSONArray("packages");
                    for (int j = 0; ps != null && j < ps.length(); j++) {
                        JSONObject p = ps.optJSONObject(j);
                        if (p == null) continue;
                        Pkg x = new Pkg();
                        x.name = p.optString("name"); x.endTime = p.optString("end_time");
                        x.remain = p.optLong("remain"); x.size = p.optLong("size");
                        a.packages.add(x);
                    }
                    out.add(a);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void balanceHistory(int days, final Cb<List<BalanceSeries>> cb) {
        PanelApi.get(ctx, "/panel/api/balance_history?days=" + days, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<BalanceSeries> out = new ArrayList<BalanceSeries>();
                JSONArray arr = o.optJSONArray("series");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject s = arr.optJSONObject(i);
                    if (s == null) continue;
                    BalanceSeries b = new BalanceSeries();
                    b.uid = s.optString("uid"); b.name = s.optString("name", b.uid);
                    b.realm = s.optString("realm");
                    b.current = s.optLong("current");
                    b.min = s.optLong("min"); b.max = s.optLong("max");
                    b.change = s.optLong("change");
                    JSONArray pts = s.optJSONArray("points");
                    for (int j = 0; pts != null && j < pts.length(); j++) {
                        JSONObject p = pts.optJSONObject(j);
                        if (p != null) b.values.add((float) p.optDouble("c", 0));
                    }
                    out.add(b);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void vouchers(final Cb<List<VoucherAccount>> cb) {
        PanelApi.get(ctx, "/panel/api/school/vouchers", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<VoucherAccount> out = new ArrayList<VoucherAccount>();
                JSONArray arr = o.optJSONArray("accounts");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject r = arr.optJSONObject(i);
                    if (r == null) continue;
                    VoucherAccount a = new VoucherAccount();
                    a.uid = r.optString("uid"); a.nickname = r.optString("nickname");
                    a.error = r.optString("error");
                    JSONArray vs = r.optJSONArray("vouchers");
                    for (int j = 0; vs != null && j < vs.length(); j++) {
                        JSONObject v = vs.optJSONObject(j);
                        if (v == null) continue;
                        Voucher x = new Voucher();
                        x.prize = v.optString("prize_name", v.optString("sku_code", "券"));
                        x.code = v.optString("code"); x.validTo = v.optString("valid_to");
                        a.vouchers.add(x);
                    }
                    out.add(a);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void errors(final Cb<ErrSummary> cb) {
        PanelApi.get(ctx, "/panel/api/error_summary", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                ErrSummary s = new ErrSummary();
                s.scanned = o.optInt("scanned"); s.updatedAt = o.optString("updated_at");
                JSONArray arr = o.optJSONArray("rows");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject r = arr.optJSONObject(i);
                    if (r == null) continue;
                    ErrStat e = new ErrStat();
                    e.account = r.optString("account"); e.kind = r.optString("kind");
                    e.code = r.optString("code"); e.model = r.optString("model");
                    e.last = r.optString("last"); e.sev = r.optString("sev", "warn");
                    e.until = r.optString("until");
                    e.sample = r.optString("sample_full", r.optString("sample"));
                    e.count = r.optInt("count");
                    s.rows.add(e);
                }
                cb.ok(s);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void metrics(final Cb<Metrics> cb) {
        PanelApi.get(ctx, "/panel/api/request_metrics", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                Metrics m = new Metrics();
                m.completed = o.optLong("completed"); m.inFlight = o.optLong("in_flight");
                m.succeeded = o.optLong("succeeded"); m.failed = o.optLong("failed");
                m.successRate = o.optDouble("success_rate", 0);
                m.httpSuccessRate = o.optDouble("http_success_rate", 0);
                m.avgDurationMs = o.optDouble("avg_duration_ms", 0);
                JSONObject a = o.optJSONObject("archive");
                if (a != null) {
                    m.archiveEnabled = a.optBoolean("enabled");
                    m.archiveFiles = a.optInt("files");
                    m.archiveBytes = a.optDouble("bytes", 0);
                }
                cb.ok(m);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    // ── 日志 ───────────────────────────────────────────────────────────────
    void logs(final Cb<List<LogRow>> cb) {
        PanelApi.get(ctx, "/panel/api/logs", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<LogRow> out = new ArrayList<LogRow>();
                JSONArray arr = o.optJSONArray("entries");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject e = arr.optJSONObject(i);
                    if (e == null) continue;
                    LogRow r = new LogRow();
                    r.time = e.optString("ts"); r.channel = e.optString("ch");
                    r.text = e.optString("text");
                    out.add(r);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void requestLogs(final Cb<List<ReqEvent>> cb) {
        PanelApi.get(ctx, "/panel/api/request_logs?limit=100", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                List<ReqEvent> out = new ArrayList<ReqEvent>();
                JSONArray arr = o.optJSONArray("entries");
                for (int i = 0; arr != null && i < arr.length(); i++) {
                    JSONObject e = arr.optJSONObject(i);
                    if (e == null) continue;
                    ReqEvent r = new ReqEvent();
                    r.time = e.optString("time"); r.requestId = e.optString("request_id");
                    r.path = e.optString("path"); r.account = e.optString("account");
                    r.model = e.optString("model"); r.modelEff = e.optString("model_eff");
                    r.status = e.optInt("status"); r.outcome = e.optString("outcome");
                    r.durationMs = e.optLong("duration_ms"); r.ttfbMs = e.optLong("ttfb_ms");
                    r.promptTokens = e.optLong("prompt_tokens");
                    r.completionTokens = e.optLong("completion_tokens");
                    r.totalTokens = e.optLong("total_tokens");
                    r.credit = e.optDouble("credit", 0); r.creditKnown = e.optBoolean("credit_known");
                    r.cacheHit = e.optLong("cache_hit_tokens"); r.cacheMiss = e.optLong("cache_miss_tokens");
                    r.toolsDeclared = e.optInt("tools_declared"); r.toolCalls = e.optInt("tool_calls");
                    r.clientIp = e.optString("client_ip"); r.userAgent = e.optString("user_agent");
                    Object ctxObj = e.opt("context");
                    r.context = ctxObj == null ? "" : String.valueOf(ctxObj);
                    out.add(r);
                }
                cb.ok(out);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void clearArchive(final Cb<String> cb) {
        post("/panel/api/request_logs/clear", null, cb);
    }

    // ── 配置 ───────────────────────────────────────────────────────────────
    interface ConfigCb { void ok(JSONObject cfg, String path); void fail(String msg); }

    /** 设置页那几个常用开关（值 + 保存时用到的原始配置）。 */
    static final class Settings {
        boolean checkin = true, travel = true, activity = true, webActivity = false,
                keepalive = true, panelLog = true;
        String path = "";
    }

    private JSONObject rawConfig;

    void loadSettings(final Cb<Settings> cb) {
        loadConfig(new ConfigCb() {
            public void ok(JSONObject cfg, String path) {
                rawConfig = cfg;
                Settings st = new Settings();
                st.path = path;
                JSONObject sch = cfg.optJSONObject("schedule");
                if (sch != null) {
                    st.checkin = sch.optBoolean("checkin_enabled", true);
                    st.travel = sch.optBoolean("travel_enabled", true);
                    st.activity = sch.optBoolean("activity_enabled", true);
                    st.webActivity = sch.optBoolean("web_activity_enabled", false);
                    st.keepalive = sch.optBoolean("keepalive_enabled", true);
                }
                JSONObject lg = cfg.optJSONObject("logging");
                if (lg != null) st.panelLog = lg.optBoolean("panel_log_enabled", true);
                cb.ok(st);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    /** 只把设置页那几个开关写回原始配置，其余字段原样保留。 */
    void saveSettings(Settings st, final Cb<String> cb) {
        if (rawConfig == null) { cb.fail("配置尚未加载"); return; }
        try {
            JSONObject sch = rawConfig.optJSONObject("schedule");
            if (sch == null) { sch = new JSONObject(); rawConfig.put("schedule", sch); }
            sch.put("checkin_enabled", st.checkin);
            sch.put("travel_enabled", st.travel);
            sch.put("activity_enabled", st.activity);
            sch.put("web_activity_enabled", st.webActivity);
            sch.put("keepalive_enabled", st.keepalive);
            JSONObject lg = rawConfig.optJSONObject("logging");
            if (lg == null) { lg = new JSONObject(); rawConfig.put("logging", lg); }
            lg.put("panel_log_enabled", st.panelLog);
            saveConfig(rawConfig, cb);
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void loadConfig(final ConfigCb cb) {
        PanelApi.get(ctx, "/panel/api/config", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONObject cfg = o.optJSONObject("config");
                if (cfg == null) { cb.fail("配置接口不可用"); return; }
                cb.ok(cfg, o.optString("path"));
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void saveConfig(JSONObject cfg, final Cb<String> cb) {
        PanelApi.post(ctx, "/panel/api/config", cfg, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONArray need = o.optJSONArray("restart_required");
                String extra = need == null || need.length() == 0 ? ""
                        : "（需重启：" + need.toString() + "）";
                cb.ok(extra);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    // ── 导入 / 导出 / 登录 ─────────────────────────────────────────────────
    void importFile(String name, byte[] data, final Cb<ImportResult> cb) {
        PanelApi.upload(ctx, "/panel/api/import/cockpit", name, data, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                ImportResult r = new ImportResult();
                r.imported = o.optInt("imported"); r.skipped = o.optInt("skipped");
                r.total = o.optInt("total");
                JSONArray errs = o.optJSONArray("errors");
                for (int i = 0; errs != null && i < errs.length(); i++) r.errors.add(errs.optString(i));
                cb.ok(r);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void exportAccounts(final FileCb cb) {
        PanelApi.download(ctx, "/panel/api/export/accounts", new PanelApi.FileCb() {
            public void ok(byte[] data, String filename) { cb.ok(data, filename); }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    void addApiKey(String uid, String token, String nickname, final Cb<String> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("uid", uid);
            body.put("access_token", token);
            body.put("nickname", nickname);
            PanelApi.post(ctx, "/panel/api/accounts/add_apikey", body, new PanelApi.Cb() {
                public void ok(JSONObject o) { cb.ok(o.optString("uid")); }
                public void fail(String msg) { cb.fail(msg); }
            });
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void loginStart(String realm, final Cb<LoginStart> cb) {
        try {
            JSONObject body = new JSONObject();
            body.put("realm", realm);
            PanelApi.post(ctx, "/panel/api/login/start", body, new PanelApi.Cb() {
                public void ok(JSONObject o) {
                    LoginStart s = new LoginStart();
                    s.state = o.optString("state");
                    s.url = o.optString("url");
                    if (s.state.isEmpty() || s.url.isEmpty()) { cb.fail("缺少 state / url"); return; }
                    cb.ok(s);
                }
                public void fail(String msg) { cb.fail(msg); }
            });
        } catch (Throwable t) { cb.fail(String.valueOf(t)); }
    }

    void loginPoll(String state, final Cb<LoginPoll> cb) {
        PanelApi.get(ctx, "/panel/api/login/poll?state=" + enc(state), new PanelApi.Cb() {
            public void ok(JSONObject o) {
                LoginPoll p = new LoginPoll();
                p.done = o.optBoolean("done");
                p.uid = o.optString("uid");
                p.error = o.optString("error");
                cb.ok(p);
            }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    // ── 内部 ───────────────────────────────────────────────────────────────
    private void post(String path, JSONObject body, final Cb<String> cb) {
        PanelApi.post(ctx, path, body, new PanelApi.Cb() {
            public void ok(JSONObject o) { cb.ok(o.optString("message", "完成")); }
            public void fail(String msg) { cb.fail(msg); }
        });
    }

    private static String account(String uid) {
        return "/panel/api/accounts/" + enc(uid);
    }

    private static String enc(String s) {
        try { return java.net.URLEncoder.encode(s, "UTF-8"); } catch (Throwable t) { return s; }
    }
}
