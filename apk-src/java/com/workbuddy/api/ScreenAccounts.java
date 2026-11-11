package com.workbuddy.api;

import android.app.Activity;
import android.app.Dialog;
import android.content.Intent;
import android.graphics.Typeface;
import android.os.Handler;
import android.text.TextUtils;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.EditText;
import android.widget.HorizontalScrollView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import java.util.List;

/**
 * 账号池（分区：账号卡、行内动作、更多菜单、模型开关、添加/导入/导出/登录）。
 *
 * 由 Console 在切页时实例化并调用 render()；只与 PanelService 打交道，
 * 不含 HTTP，布局全部走 Theme 令牌。
 */
final class ScreenAccounts {

    private final Console c;
    private final Activity a;
    private final PanelService svc;
    private final Handler ui;

    ScreenAccounts(Console c) {
        this.c = c;
        this.a = c.a;
        this.svc = c.svc;
        this.ui = c.ui;
    }

    void render() { renderAccountsShell(); }
    void renderShell() { renderAccountsShell(); }
    void renderBody(PanelService.Overview o) { renderAccounts(o); }

    // ── 与 Console 的共享能力（保持方法体与本类原有写法一致）────────────────
    private void toast(String s) { c.toast(s); }
    private void show(View v) { c.show(v); }
    private void setBar(String t, boolean back, View... acts) { c.setBar(t, back, acts); }
    private ScrollView scrollOf(LinearLayout col) { return c.scrollOf(col); }
    private ScrollView scrollOf(LinearLayout col, boolean sb) { return c.scrollOf(col, sb); }
    private void refresh(LinearLayout col) { c.refresh(col); }
    private void open(String id) { c.open(id); }
    private View iconAction(int icon, String desc, Runnable r) { return c.iconAction(icon, desc, r); }
    private void loadOverview(boolean quiet) { c.loadOverview(quiet); }
    private ScrollView currentScroll() { return c.currentScroll; }
    private void setCurrentScroll(ScrollView sv) { c.currentScroll = sv; }

    // ── 账号 ───────────────────────────────────────────────────────────────
    private void renderAccountsShell() {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));
        LinearLayout col = Theme.column(a);
        col.addView(Theme.empty(a, "加载中…"));
        show(scrollOf(col));
    }

    private void renderAccounts(PanelService.Overview o) {
        setBar("账号池", false, iconAction(R.drawable.ic_refresh, "重载目录", new Runnable() {
            public void run() {
                svc.reloadAccounts(new PanelService.Cb<String>() {
                    public void ok(String s) { toast(s); loadOverview(false); }
                    public void fail(String m) { toast("重载失败：" + m); }
                });
            }
        }), iconAction(R.drawable.ic_add, "添加账号", new Runnable() {
            public void run() { addSheet(); }
        }));

        LinearLayout col = Theme.column(a);

        // 概览条：总数 / 可用 / 冷却 / 禁用
        LinearLayout hero = Theme.card(a);
        LinearLayout row = new LinearLayout(a);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.addView(Theme.kpi(a, String.valueOf(o.total), "账号", Theme.INK),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.healthy), "可用", Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.cooling), "冷却", Theme.WARN),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        row.addView(Theme.kpi(a, String.valueOf(o.disabled), "禁用", Theme.BAD),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        hero.addView(row);
        col.addView(hero, Theme.cardLp(a, Theme.S4));

        if (o.accounts.isEmpty()) {
            col.addView(Theme.empty(a, "还没有账号\n点右上「＋」导入文件或登录添加"));
        } else {
            for (PanelService.Account acc : o.accounts) col.addView(accountCard(acc));
        }
        refresh(col);
    }

    private View accountCard(final PanelService.Account s) {
        LinearLayout card = Theme.card(a);
        card.setLayoutParams(Theme.cardLp(a, Theme.S3));

        // 第一行：状态点 + 昵称 + 标签
        LinearLayout head = new LinearLayout(a);
        head.setOrientation(LinearLayout.HORIZONTAL);
        head.setGravity(Gravity.CENTER_VERTICAL);
        int dotColor = s.disabled ? Theme.BAD : (s.cooling || s.paused ? Theme.WARN : Theme.ACCENT);
        LinearLayout.LayoutParams dlp = new LinearLayout.LayoutParams(
                Theme.dp(a, 8), Theme.dp(a, 8));
        dlp.rightMargin = Theme.dp(a, Theme.S2);
        head.addView(Theme.dot(a, dotColor, 8), dlp);
        TextView name = Theme.text(a, TextUtils.isEmpty(s.nickname) ? "未命名账号" : s.nickname,
                16f, Theme.INK, 2);
        name.setSingleLine(true);
        name.setEllipsize(TextUtils.TruncateAt.END);
        head.addView(name, new LinearLayout.LayoutParams(0,
                ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        if ("global".equals(s.realm)) head.addView(Theme.tag(a, "国际", Theme.INFO));
        if (s.enterprise) head.addView(Theme.tag(a, "企业", Theme.WARN));
        if (s.apiKey) head.addView(Theme.tag(a, "KEY", Theme.INK3));
        if ("personal".equals(s.override)) head.addView(Theme.tag(a, "强制个人", Theme.ACCENT));
        if ("enterprise".equals(s.override)) head.addView(Theme.tag(a, "强制企业", Theme.WARN));
        card.addView(head);

        // 第二行：uid + 状态
        TextView sub = Theme.text(a, s.uid + "  ·  " + s.statusText(), 11.5f, Theme.INK3, 0);
        sub.setPadding(0, Theme.dp(a, 3), 0, 0);
        card.addView(sub);

        // 第三行：积分 / 成功失败 / 在途（数字用等宽）
        LinearLayout stats = new LinearLayout(a);
        stats.setOrientation(LinearLayout.HORIZONTAL);
        stats.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        stats.addView(statBlock("积分", s.credits + (s.creditsTotal > 0 ? "/" + s.creditsTotal : ""),
                Theme.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1.2f));
        stats.addView(statBlock("成功", String.valueOf(s.success), Theme.ACCENT),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("失败", String.valueOf(s.errTotal), s.errTotal > 0 ? Theme.BAD : Theme.INK3),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        stats.addView(statBlock("在途", String.valueOf(s.inFlight), Theme.INFO),
                new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        card.addView(stats);

        // 动作行：主操作（签到/余额/任务）+ 更多
        HorizontalScrollView hs = new HorizontalScrollView(a);
        hs.setHorizontalScrollBarEnabled(false);
        LinearLayout acts = new LinearLayout(a);
        acts.setOrientation(LinearLayout.HORIZONTAL);
        acts.setPadding(0, Theme.dp(a, Theme.S3), 0, 0);
        hs.addView(acts);
        card.addView(hs);

        boolean personal = !s.enterprise && !s.apiKey;
        if (personal) acts.addView(actionPill("签到", new Runnable() {
            public void run() { doCheckin(s); }
        }));
        acts.addView(actionPill("余额", new Runnable() {
            public void run() { doBalance(s); }
        }));
        if (personal) acts.addView(actionPill("任务", new Runnable() {
            public void run() { c.accountTasks(s.uid); }
        }));
        acts.addView(actionPill(s.paused ? "恢复" : "暂停", new Runnable() {
            public void run() { doPause(s); }
        }));
        // 类型 / 探活 / 备注 / 分组 / 模型开关都在「更多」里：动作行保持 5 个，
        // 360px 屏不用横滑就能看全（旧版第 6 个被右边缘切掉）。
        TextView morePill = Theme.pill(a, "更多", false);
        morePill.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { moreSheet(v, s); }
        });
        LinearLayout.LayoutParams mlp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        mlp.rightMargin = Theme.dp(a, Theme.S2);
        acts.addView(morePill, mlp);
        return card;
    }

    private View statBlock(String label, String value, int color) {
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.addView(Theme.label(a, label));
        TextView v = Theme.num(a, value, 13f, color);
        v.setPadding(0, Theme.dp(a, 2), 0, 0);
        box.addView(v);
        return box;
    }

    private View actionPill(String label, final Runnable r) {
        TextView p = Theme.pill(a, label, false);
        p.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { r.run(); }
        });
        LinearLayout.LayoutParams lp = (LinearLayout.LayoutParams) p.getLayoutParams();
        if (lp == null) lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.rightMargin = Theme.dp(a, Theme.S2);
        p.setLayoutParams(lp);
        return p;
    }

    private void doCheckin(PanelService.Account s) {
        toast("签到中…");
        svc.checkin(s.uid, new PanelService.Cb<PanelService.TaskResult>() {
            public void ok(PanelService.TaskResult r) {
                if ("skipped".equals(r.error)) toast("今日已签到");
                else if ("not_done".equals(r.error)) toast("签到未完成，稍后再试");
                else if (!r.error.isEmpty()) toast("签到异常：" + r.error);
                else toast("签到完成" + (r.message.isEmpty() ? "" : "：" + r.message));
                loadOverview(false);
            }
            public void fail(String m) { toast("签到失败：" + m); }
        });
    }

    private void doBalance(PanelService.Account s) {
        svc.balance(s.uid, new PanelService.Cb<long[]>() {
            public void ok(long[] v) {
                toast("余额 " + v[0] + (v[1] > 0 ? " / " + v[1] : ""));
                loadOverview(false);
            }
            public void fail(String m) { toast("余额查询失败：" + m); }
        });
    }

    private void doPause(PanelService.Account s) {
        svc.pause(s.uid, s.paused, new PanelService.Cb<String>() {
            public void ok(String m) { toast("已更新选号状态"); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        });
    }

    private void typeSheet(final PanelService.Account s) {
        final String[] values = {"", "enterprise", "personal"};
        String cur = s.override == null ? "" : s.override;
        String[] labels = {
                "自动判定" + (cur.isEmpty() ? " · 当前" : ""),
                "强制企业版" + ("enterprise".equals(cur) ? " · 当前" : ""),
                "强制个人版" + ("personal".equals(cur) ? " · 当前" : "")
        };
        int checked = "enterprise".equals(cur) ? 1 : ("personal".equals(cur) ? 2 : 0);
        Sheet.single(a, "账号类型", labels, checked, new Sheet.Pick() {
            public void onPick(int which) {
                svc.enterprise(s.uid, values[which], new PanelService.Cb<Boolean>() {
                    public void ok(Boolean changed) {
                        toast("类型已更新" + (changed ? "" : "（判定结果未变）"));
                        loadOverview(false);
                    }
                    public void fail(String m) { toast("切换失败：" + m); }
                });
            }
        });
    }

    private void moreSheet(View anchor, final PanelService.Account s) {
        String[] items = {
                s.paused ? "恢复选号" : "暂停选号",
                s.disabled ? "启用账号" : "禁用账号",
                "账号类型：" + typeLabel(s),
                "探活（只读确认是否已解封）",
                "备注",
                "分组",
                "清空任务跳过名单",
                "模型开关",
                "移除账号"
        };
        Sheet.anchor(anchor, TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname, items, 8,
                new Sheet.Pick() {
                    public void onPick(int which) {
                        if (which == 0) doPause(s);
                        else if (which == 1) svc.disable(s.uid, !s.disabled, after("已更新"));
                        else if (which == 2) typeSheet(s);
                        else if (which == 3) probe(s);
                        else if (which == 4) editText("备注", "仅本机保存", s.remark, new TextCommit() {
                            public void onText(String t) { svc.setRemark(s.uid, t, after("备注已保存")); }
                        });
                        else if (which == 5) editText("分组", "用于面板筛选", s.group, new TextCommit() {
                            public void onText(String t) { svc.setGroup(s.uid, t, after("分组已保存")); }
                        });
                        else if (which == 6) svc.clearSkip(s.uid, after("已清空跳过名单"));
                        else if (which == 7) modelSheet(s);
                        else confirmRemove(s);
                    }
                });
    }

    private String typeLabel(PanelService.Account s) {
        if ("enterprise".equals(s.override)) return "强制企业版";
        if ("personal".equals(s.override)) return "强制个人版";
        return s.enterprise ? "自动 · 企业" : "自动 · 个人";
    }

    private PanelService.Cb<String> after(final String ok) {
        return new PanelService.Cb<String>() {
            public void ok(String m) { toast(ok); loadOverview(false); }
            public void fail(String m) { toast("失败：" + m); }
        };
    }

    private void probe(PanelService.Account s) {
        toast("探活中…");
        svc.probeBan(s.uid, new PanelService.Cb<PanelService.ProbeResult>() {
            public void ok(PanelService.ProbeResult r) {
                String msg;
                if ("lifted".equals(r.verdict)) msg = "已解封，可启用";
                else if ("still_banned".equals(r.verdict)) msg = "仍被封禁，继续等待";
                else if ("blocked".equals(r.verdict)) msg = "被风控拦截，继续观察";
                else if ("session_dead".equals(r.verdict)) msg = "登录态失效，需重新登录";
                else if ("inconclusive".equals(r.verdict)) msg = "无明确结论，稍后再试";
                else msg = "无法探测：" + r.verdict;
                toast(msg + (r.detail.isEmpty() ? "" : "（" + r.detail + "）"));
            }
            public void fail(String m) { toast("探活失败：" + m); }
        });
    }

    private interface TextCommit { void onText(String t); }

    private void editText(String title, String hint, String initial, final TextCommit cb) {
        final EditText et = field(hint);
        et.setText(initial == null ? "" : initial);
        LinearLayout box = new LinearLayout(a);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(Theme.dp(a, Theme.S4), 0, Theme.dp(a, Theme.S4), Theme.dp(a, Theme.S2));
        box.addView(et);
        Sheet.custom(a, title, box, "保存", new Runnable() {
            public void run() {
                cb.onText(et.getText() == null ? "" : et.getText().toString().trim());
            }
        });
    }

    private void confirmRemove(final PanelService.Account s) {
        Sheet.confirm(a, "移除账号",
                "删除本地凭据文件，不可撤销。\n\n"
                        + (TextUtils.isEmpty(s.nickname) ? s.uid : s.nickname) + "\n" + s.uid,
                "移除", true, new Runnable() {
                    public void run() { svc.remove(s.uid, after("已移除")); }
                });
    }

    // ── 模型开关 ───────────────────────────────────────────────────────────
    private void modelSheet(final PanelService.Account s) {
        toast("读取模型目录…");
        svc.models(new PanelService.Cb<List<PanelService.ModelInfo>>() {
            public void ok(final List<PanelService.ModelInfo> models) {
                svc.modelsOff(s.uid, new PanelService.Cb<List<String>>() {
                    public void ok(List<String> off) { showModels(s.uid, models, off); }
                    public void fail(String m) { toast("读取开关失败：" + m); }
                });
            }
            public void fail(String m) { toast("读取模型目录失败：" + m); }
        });
    }

    private void showModels(final String uid, List<PanelService.ModelInfo> models, List<String> off) {
        if (models.isEmpty()) { toast("模型目录为空"); return; }
        LinearLayout box = Theme.column(a);
        TextView hint = Theme.text(a, "勾选 = 该账号可用；取消 = 手动关闭（永久写入 state.json）",
                12f, Theme.INK3, 0);
        box.addView(hint);
        for (final PanelService.ModelInfo m : models) {
            final String bare = m.id.contains(":") ? m.id.substring(m.id.indexOf(':') + 1) : m.id;
            final boolean enabled = !off.contains(bare);
            LinearLayout row = new LinearLayout(a);
            row.setOrientation(LinearLayout.HORIZONTAL);
            row.setGravity(Gravity.CENTER_VERTICAL);
            row.setPadding(0, Theme.dp(a, Theme.S2), 0, Theme.dp(a, Theme.S2));
            LinearLayout info = new LinearLayout(a);
            info.setOrientation(LinearLayout.VERTICAL);
            info.addView(Theme.text(a, m.name, 14f, Theme.INK, 1));
            info.addView(Theme.text(a, m.id, 11f, Theme.INK3, 0));
            row.addView(info, new LinearLayout.LayoutParams(0,
                    ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
            final TextView toggle = Theme.pill(a, enabled ? "可用" : "已关闭",
                    enabled ? true : false);
            toggle.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    final boolean next = !"可用".equals(String.valueOf(toggle.getText()));
                    svc.setModelOff(uid, bare, !next, new PanelService.Cb<String>() {
                        public void ok(String msg) {
                            toggle.setText(next ? "可用" : "已关闭");
                            toggle.setBackground(next ? Theme.round(a, Theme.ACCENT, Theme.R_PILL)
                                    : Theme.outlined(a, 0x0DFFFFFF, Theme.R_PILL, Theme.LINE, 1));
                            toggle.setTextColor(next ? Theme.ON_ACCENT : Theme.INK2);
                        }
                        public void fail(String m) { toast("切换失败：" + m); }
                    });
                }
            });
            row.addView(toggle);
            box.addView(row);
            box.addView(Theme.divider(a));
        }
        Sheet.custom(a, "模型开关", box, "完成", null);
    }

    // ── 添加账号 ───────────────────────────────────────────────────────────
    void addSheet() {
        String[] items = {"导入文件（cockpit / auths JSON）", "导出账号到下载目录",
                "添加 API Key 账号", "登录添加（浏览器授权）"};
        Sheet.menu(a, "添加 / 迁移账号", items, -1, new Sheet.Pick() {
            public void onPick(int which) {
                if (which == 0) {
                    if (a instanceof MainActivity) ((MainActivity) a).requestImportFile();
                    else toast("当前宿主不支持文件选择");
                } else if (which == 1) exportAccounts();
                else if (which == 2) apiKeyDialog();
                else loginSheet();
            }
        });
    }

    void importUri(android.net.Uri uri) {
        try {
            java.io.InputStream in = a.getContentResolver().openInputStream(uri);
            if (in == null) { toast("读不到文件"); return; }
            java.io.ByteArrayOutputStream bos = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
            in.close();
            byte[] data = bos.toByteArray();
            if (data.length == 0) { toast("文件为空"); return; }
            String name = String.valueOf(uri.getLastPathSegment());
            int slash = name.lastIndexOf('/');
            if (slash >= 0) name = name.substring(slash + 1);
            toast("导入中…");
            svc.importFile(name, data, new PanelService.Cb<PanelService.ImportResult>() {
                public void ok(PanelService.ImportResult r) {
                    toast("导入完成：新增 " + r.imported + "，跳过 " + r.skipped);
                    if (!r.errors.isEmpty()) {
                        StringBuilder sb = new StringBuilder();
                        for (int i = 0; i < Math.min(r.errors.size(), 20); i++) {
                            sb.append("· ").append(r.errors.get(i)).append('\n');
                        }
                        Sheet.confirm(a, "导入错误", sb.toString(), "知道了", false, null);
                    }
                    open("accounts");
                }
                public void fail(String m) { toast("导入失败：" + m); }
            });
        } catch (Throwable t) { toast("导入失败：" + t); }
    }

    private void exportAccounts() {
        svc.exportAccounts(new PanelService.FileCb() {
            public void ok(byte[] data, String filename) {
                try {
                    String path;
                    if (android.os.Build.VERSION.SDK_INT >= 29) {
                        android.content.ContentValues cv = new android.content.ContentValues();
                        cv.put(android.provider.MediaStore.MediaColumns.DISPLAY_NAME, filename);
                        cv.put(android.provider.MediaStore.MediaColumns.MIME_TYPE, "application/json");
                        cv.put(android.provider.MediaStore.MediaColumns.RELATIVE_PATH,
                                android.os.Environment.DIRECTORY_DOWNLOADS);
                        android.net.Uri out = a.getContentResolver()
                                .insert(android.provider.MediaStore.Downloads.EXTERNAL_CONTENT_URI, cv);
                        if (out == null) { toast("写入下载目录失败"); return; }
                        java.io.OutputStream os = a.getContentResolver().openOutputStream(out);
                        os.write(data);
                        os.close();
                        path = "下载/" + filename;
                    } else {
                        java.io.File f = new java.io.File(a.getExternalFilesDir(null), filename);
                        java.io.FileOutputStream fos = new java.io.FileOutputStream(f);
                        fos.write(data);
                        fos.close();
                        path = f.getAbsolutePath();
                    }
                    toast("已导出 " + path);
                } catch (Throwable t) { toast("保存失败：" + t); }
            }
            public void fail(String m) { toast("导出失败：" + m); }
        });
    }

    private void apiKeyDialog() {
        LinearLayout box = Theme.column(a);
        final EditText uid = field("uid（手机号或 UUID）");
        final EditText token = field("access token（ck_ / pt_ 开头）");
        final EditText nick = field("昵称（可选）");
        box.addView(uid);
        box.addView(token);
        box.addView(nick);
        Sheet.custom(a, "添加 API Key 账号", box, "添加", new Runnable() {
            public void run() {
                svc.addApiKey(uid.getText().toString().trim(),
                        token.getText().toString().trim(),
                        nick.getText().toString().trim(),
                        new PanelService.Cb<String>() {
                            public void ok(String id) { toast("已添加 " + id); open("accounts"); }
                            public void fail(String m) { toast("添加失败：" + m); }
                        });
            }
        });
    }

    private EditText field(String hint) {
        EditText et = new EditText(a);
        et.setHint(hint);
        et.setTextSize(15f);
        et.setTypeface(Theme.sans(a));
        return et;
    }

    private void loginSheet() {
        Sheet.menu(a, "登录添加账号",
                new String[]{"国内版 · workbuddy.cn", "国际版 · workbuddy.ai"}, -1,
                new Sheet.Pick() {
                    public void onPick(int which) { startLogin(which == 1 ? "global" : "cn"); }
                });
    }

    private void startLogin(String realm) {
        svc.loginStart(realm, new PanelService.Cb<PanelService.LoginStart>() {
            public void ok(final PanelService.LoginStart s) {
                toast("浏览器已打开，完成授权后自动返回（最长 5 分钟）");
                try {
                    a.startActivity(new Intent(Intent.ACTION_VIEW, android.net.Uri.parse(s.url)));
                } catch (Throwable t) { toast("请手动打开：" + s.url); }
                pollLogin(s.state, 0);
            }
            public void fail(String m) { toast("发起登录失败：" + m); }
        });
    }

    private void pollLogin(final String state, final int attempt) {
        if (attempt > 100) { toast("登录超时"); return; }
        svc.loginPoll(state, new PanelService.Cb<PanelService.LoginPoll>() {
            public void ok(PanelService.LoginPoll p) {
                if (p.done) {
                    toast("登录成功" + (p.uid.isEmpty() ? "" : "：" + p.uid));
                    open("accounts");
                    return;
                }
                if (!p.error.isEmpty()) { toast("登录失败：" + p.error); return; }
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
            public void fail(String msg) {
                ui.postDelayed(new Runnable() { public void run() { pollLogin(state, attempt + 1); } }, 3000);
            }
        });
    }
}
