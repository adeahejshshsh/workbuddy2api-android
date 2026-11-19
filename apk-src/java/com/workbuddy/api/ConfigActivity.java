package com.workbuddy.api;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.DialogInterface;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.os.Bundle;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.CompoundButton;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.Collections;
import java.util.Iterator;
import java.util.List;

/**
 * 全部配置（原生）：把 config.json 的每一个键摊平成可编辑列表，保存时整份回传。
 *
 * 为什么做成「按 JSON 自动生成」而不是手写 100 个控件：配置有十几节、字段还在增加，
 * 手写一份必然与后端 schema 漂移（漏一个键就是「保存后这个字段被重置」）。这里按
 * 类型生成控件：
 *   bool            → Switch
 *   number          → 数字输入
 *   string          → 文本输入
 *   number 数组     → 「9,21」这种逗号分隔（schedule.*_hours 就是这种）
 *   数组/对象       → 「编辑 JSON」弹窗（结构复杂的少数几项，比如 pool.account_priority）
 * 未识别的类型一律只读展示，避免把不认识的形状写坏。
 *
 * 只改**用户动过的字段**：编辑器挂在同一个 JSONObject 上，保存时原样回传其余键。
 */
public class ConfigActivity extends Activity {

    // 与 Theme 同一套令牌。**可变**：Theme 的调色板随深浅色切换，复制成 final 会把
    // 颜色钉死在类初始化那一刻（issue #167 第 1 条）。每次 onCreate 前 refresh 一次。
    private static int BG, SURFACE, SURFACE2, LINE, INK, INK2, INK3, ACCENT, BAD;

    private static void refreshThemeAliases() {
        BG = Theme.BG;
        SURFACE = Theme.CARD_SOLID;
        SURFACE2 = Theme.MUTED;
        LINE = Theme.LINE;
        INK = Theme.INK;
        INK2 = Theme.INK2;
        INK3 = Theme.INK3;
        ACCENT = Theme.ACCENT;
        BAD = Theme.BAD;
    }

    private JSONObject cfg;
    private LinearLayout content;
    private TextView status;
    private PanelService svc;

    /**
     * 每个可编辑控件 → 「把控件当前文本写回 cfg」的动作（W6-M-02）。
     *
     * 为什么需要：编辑值原先只在**失焦**时写回。用户改完最后一个输入框直接点顶部
     * 「保存」时，那个控件通常还没失焦（点击按钮不一定先触发 focus change，部分 ROM
     * 上更是完全不触发），提交的就是旧值 —— 表现为「改了、保存了、重进又变回去」。
     * 现在保存前先 clearFocus() 再按登记表逐个提交，任何输入路径都不会漏。
     */
    private final java.util.IdentityHashMap<EditText, Runnable> commits =
            new java.util.IdentityHashMap<EditText, Runnable>();

    @Override protected void onCreate(Bundle b) {
        Theme.applyActivityTheme(this);
        super.onCreate(b);
        Theme.apply(this);
        refreshThemeAliases();
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);

        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(SURFACE);
        bar.setPadding(dp(10), dp(8), dp(10), dp(8));

        TextView back = new TextView(this);
        back.setText("‹ 返回");
        back.setTextSize(15f);
        back.setTextColor(ACCENT);
        back.setTypeface(Theme.sans(this, 1));
        back.setPadding(dp(8), dp(10), dp(8), dp(10));
        back.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { finish(); }
        });
        bar.addView(back);

        TextView title = new TextView(this);
        title.setText("全部配置");
        title.setTextSize(16f);
        title.setTextColor(INK);
        title.setTypeface(Theme.sans(this, 2));
        title.setPadding(dp(8), 0, 0, 0);
        bar.addView(title, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        status = new TextView(this);
        status.setTextSize(11f);
        status.setTextColor(INK3);
        bar.addView(status);

        Button save = new Button(this);
        save.setText("保存");
        save.setAllCaps(false);
        save.setTextSize(13.5f);
        save.setTextColor(Theme.ON_ACCENT);
        save.setTypeface(Theme.sans(this, 2));
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(ACCENT);
        bg.setCornerRadius(dp(Theme.R_CTRL));
        save.setBackground(bg);
        save.setPadding(dp(18), dp(8), dp(18), dp(8));
        save.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { saveAll(); }
        });
        bar.addView(save);
        root.addView(bar);

        ScrollView sv = new ScrollView(this);
        content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        content.setPadding(dp(10), dp(10), dp(10), dp(24));
        sv.addView(content);
        root.addView(sv, new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, 0, 1f));

        setContentView(root);
        svc = new PanelService(this);
        load();
    }

    private void load() {
        content.removeAllViews();
        content.addView(hint("加载中…"));
        svc.loadConfig(new PanelService.ConfigCb() {
            public void ok(JSONObject c, String path) { cfg = c; render(); }
            public void fail(String msg) {
                content.removeAllViews();
                content.addView(hint("读取配置失败：" + msg));
            }
        });
    }

    private void render() {
        content.removeAllViews();
        commits.clear();   // 每次重建都会重新登记，旧控件不能留在表里（会写回已废弃的 owner）
        List<String> keys = new ArrayList<String>();
        for (Iterator<String> it = cfg.keys(); it.hasNext(); ) keys.add(it.next());
        Collections.sort(keys);
        for (String k : keys) {
            Object v = cfg.opt(k);
            if (v instanceof JSONObject) {
                JSONObject sec = (JSONObject) v;
                content.addView(sectionHeader(k));
                List<String> sk = new ArrayList<String>();
                for (Iterator<String> it = sec.keys(); it.hasNext(); ) sk.add(it.next());
                Collections.sort(sk);
                if (sk.isEmpty()) content.addView(hint("（空）"));
                for (String key : sk) {
                    View row = row(sec, key, sec.opt(key));
                    if (row != null) content.addView(row);
                }
            } else {
                // 顶层标量（listen / api_key / state_file ...）
                View row = row(cfg, k, v);
                if (row != null) content.addView(row);
            }
        }
        content.addView(hint("提示：保存会整份回传。需要重启的字段会在保存后提示（listen / data 目录等）。"));
    }

    private View sectionHeader(String name) {
        TextView t = new TextView(this);
        t.setText(name);
        t.setTextSize(12f);
        t.setTextColor(ACCENT);
        t.setTypeface(Theme.sans(this, 2));
        t.setLetterSpacing(0.08f);
        t.setPadding(dp(4), dp(16), 0, dp(6));
        return t;
    }

    private View hint(String text) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextSize(11.5f);
        t.setTextColor(INK3);
        t.setPadding(dp(4), dp(6), dp(4), dp(6));
        return t;
    }

    /** 一行配置：标签 + 类型控件。未识别的类型退化成只读文本。 */
    private View row(final JSONObject owner, final String key, Object value) {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setBackground(card());
        box.setPadding(dp(12), dp(8), dp(12), dp(8));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.bottomMargin = dp(6);
        box.setLayoutParams(lp);

        TextView label = new TextView(this);
        label.setText(key);
        label.setTextSize(12f);
        label.setTextColor(INK3);
        label.setTypeface(Theme.sans(this));
        box.addView(label);

        if (value instanceof Boolean) {
            Switch sw = new Switch(this);
            sw.setChecked((Boolean) value);
            sw.setTextSize(12.5f);
            sw.setTextColor(INK);
            sw.setOnCheckedChangeListener(new CompoundButton.OnCheckedChangeListener() {
                public void onCheckedChanged(CompoundButton b, boolean checked) {
                    try { owner.put(key, checked); } catch (Throwable ignored) {}
                }
            });
            box.addView(sw);
            return box;
        }
        if (value instanceof Number) {
            final EditText et = numberInput(String.valueOf(value));
            final Runnable commit = new Runnable() {
                public void run() { commitNumber(owner, key, et.getText().toString()); }
            };
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    commit.run();
                }
            });
            commits.put(et, commit);
            box.addView(et);
            return box;
        }
        if (value instanceof String) {
            final EditText et = isSecretKey(key) ? secretInput((String) value) : textInput((String) value);
            final Runnable commit = new Runnable() {
                public void run() {
                    try { owner.put(key, et.getText().toString()); } catch (Throwable ignored) {}
                }
            };
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    commit.run();
                }
            });
            commits.put(et, commit);
            box.addView(et);
            if (isSecretKey(key)) {
                // W6-M-01：密钥默认掩码；想核对时点「显示」（再点「隐藏」）。
                // 掩码只影响显示，EditText 里始终是真实值，保存拿到的也是真实值。
                LinearLayout tools = new LinearLayout(this);
                tools.setOrientation(LinearLayout.HORIZONTAL);
                tools.setGravity(Gravity.END);
                tools.addView(revealToggle(et));
                box.addView(tools);
            }
            return box;
        }
        if (value instanceof JSONArray && onlyNumbers((JSONArray) value)) {
            JSONArray arr = (JSONArray) value;
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < arr.length(); i++) {
                if (i > 0) sb.append(',');
                sb.append(arr.optInt(i));
            }
            final EditText et = textInput(sb.toString());
            et.setHint("逗号分隔，例如 9,21");
            final Runnable commit = new Runnable() {
                public void run() { commitNumberArray(owner, key, et.getText().toString()); }
            };
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    commit.run();
                }
            });
            commits.put(et, commit);
            box.addView(et);
            return box;
        }
        // 复杂结构：JSON 弹窗
        final String raw = String.valueOf(value);
        TextView tv = new TextView(this);
        tv.setText(raw.length() > 120 ? raw.substring(0, 120) + "…" : raw);
        tv.setTextSize(11f);
        tv.setTextColor(INK3);
        tv.setTypeface(Typeface.MONOSPACE);
        tv.setPadding(0, dp(3), 0, dp(3));
        box.addView(tv);
        Button edit = new Button(this);
        edit.setText("编辑 JSON");
        edit.setAllCaps(false);
        edit.setTextSize(11.5f);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(SURFACE2);
        bg.setCornerRadius(dp(8));
        edit.setBackground(bg);
        edit.setTextColor(INK);
        edit.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { editJson(owner, key, raw); }
        });
        box.addView(edit);
        return box;
    }

    private static boolean onlyNumbers(JSONArray arr) {
        for (int i = 0; i < arr.length(); i++) {
            Object o = arr.opt(i);
            if (!(o instanceof Number)) return false;
        }
        return true;
    }

    private void commitNumber(JSONObject owner, String key, String text) {
        try {
            if (text.trim().isEmpty()) return;
            double d = Double.parseDouble(text.trim());
            if (d == Math.rint(d) && Math.abs(d) < Long.MAX_VALUE) owner.put(key, (long) d);
            else owner.put(key, d);
        } catch (Throwable t) {
            toast("「" + key + "」不是合法数字，未保存该字段");
        }
    }

    private void commitNumberArray(JSONObject owner, String key, String text) {
        try {
            JSONArray arr = new JSONArray();
            for (String part : text.split(",")) {
                String s = part.trim();
                if (s.isEmpty()) continue;
                arr.put((long) Double.parseDouble(s));
            }
            owner.put(key, arr);
        } catch (Throwable t) {
            toast("「" + key + "」不是合法的数字列表（用逗号分隔）");
        }
    }

    private void editJson(final JSONObject owner, final String key, String raw) {
        final EditText et = new EditText(this);
        et.setText(raw);
        et.setTextSize(12f);
        et.setTypeface(Typeface.MONOSPACE);
        et.setMinLines(6);
        et.setGravity(Gravity.TOP | Gravity.START);
        et.setPadding(dp(12), dp(10), dp(12), dp(10));
        new AlertDialog.Builder(this)
                .setTitle(key)
                .setView(et)
                .setNegativeButton("取消", null)
                .setPositiveButton("应用", new DialogInterface.OnClickListener() {
                    public void onClick(DialogInterface d, int w) {
                        String text = et.getText() == null ? "" : et.getText().toString().trim();
                        if (text.isEmpty()) return;
                        try {
                            if (text.startsWith("[")) owner.put(key, new JSONArray(text));
                            else owner.put(key, new JSONObject(text));
                        } catch (Throwable t) {
                            toast("JSON 解析失败：" + t.getMessage());
                        }
                    }
                }).show();
    }

    private void saveAll() {
        if (cfg == null) return;
        // W6-M-02：先把焦点从输入框上摘下来（触发一次失焦提交），再把所有登记过的
        // 编辑器的最新文本无一遗漏地同步进 cfg —— 直接点保存时提交旧值的问题到此为止。
        View focused = getCurrentFocus();
        if (focused != null) focused.clearFocus();
        for (Runnable r : commits.values()) {
            try { r.run(); } catch (Throwable ignored) {}
        }
        status.setText("保存中…");
        svc.saveConfig(cfg, new PanelService.Cb<String>() {
            public void ok(String extra) {
                status.setText("已保存");
                toast("配置已保存" + extra);
            }
            public void fail(String msg) {
                status.setText("保存失败");
                toast("保存失败：" + msg);
            }
        });
    }

    /**
     * 哪些键要当密钥处理（W6-M-01）。
     *
     * 判据是「键名以 _key / token 结尾」，而不是只写死 api_key/admin_api_key：
     * upstash.token 之类同样是凭据，漏掉一个就是同源的第二个洞。排除 *_file / *_dir /
     * *_url —— 那些是路径与地址，掩码只会让人没法核对。
     */
    private static boolean isSecretKey(String key) {
        if (key == null) return false;
        String k = key.toLowerCase(java.util.Locale.US);
        if (k.endsWith("_file") || k.endsWith("_dir") || k.endsWith("_url")) return false;
        return k.equals("token") || k.endsWith("_token")
                || k.equals("key") || k.endsWith("_key");
    }

    /** 掩码输入框：显示成圆点，值本身不变。 */
    private EditText secretInput(String value) {
        EditText et = textInput(value);
        et.setSingleLine(true);
        et.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD);
        return et;
    }

    /** 「显示 / 隐藏」切换：只改显示方式，不碰 EditText 里的值。 */
    private TextView revealToggle(final EditText et) {
        final TextView t = new TextView(this);
        t.setText("显示");
        t.setTextSize(11.5f);
        t.setTextColor(ACCENT);
        t.setPadding(dp(8), dp(4), dp(2), dp(4));
        t.setClickable(true);
        t.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                boolean masked = (et.getInputType() & InputType.TYPE_TEXT_VARIATION_PASSWORD) != 0;
                if (masked) {
                    et.setInputType(InputType.TYPE_CLASS_TEXT
                            | InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD);
                    t.setText("隐藏");
                } else {
                    et.setInputType(InputType.TYPE_CLASS_TEXT
                            | InputType.TYPE_TEXT_VARIATION_PASSWORD);
                    t.setText("显示");
                }
                // setInputType 会把 typeface 重置成默认字体，重新挂等宽；
                // 光标也回到末尾，不然切换到可见后光标停在开头很别扭。
                et.setTypeface(Theme.mono(ConfigActivity.this));
                try { et.setSelection(et.getText().length()); } catch (Throwable ignored) {}
            }
        });
        return t;
    }

    private EditText textInput(String value) {
        EditText et = new EditText(this);
        et.setText(value);
        et.setTextSize(14f);
        et.setTextColor(INK);
        et.setTypeface(Theme.mono(this));
        et.setSingleLine(true);
        et.setPadding(dp(10), dp(8), dp(10), dp(8));
        return et;
    }

    private EditText numberInput(String value) {
        EditText et = textInput(value);
        et.setInputType(InputType.TYPE_CLASS_NUMBER | InputType.TYPE_NUMBER_FLAG_DECIMAL
                | InputType.TYPE_NUMBER_FLAG_SIGNED);
        return et;
    }

    private GradientDrawable card() {
        GradientDrawable g = new GradientDrawable();
        g.setColor(Theme.CARD);
        g.setCornerRadius(dp(Theme.R_CARD));
        g.setStroke(dp(1), LINE);
        return g;
    }

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                getResources().getDisplayMetrics());
    }

    private void toast(String s) { Toast.makeText(this, s, Toast.LENGTH_SHORT).show(); }
}
