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

    private static final int BG = 0xFF0F1115, SURFACE = 0xFF171A20, SURFACE2 = 0xFF1F232B;
    private static final int LINE = 0xFF2A2F39, INK = 0xFFE7EAF0, INK2 = 0xFFA7AEBB, INK3 = 0xFF6E7683;
    private static final int ACCENT = 0xFF6E8BFF, BAD = 0xFFE5677D;

    private JSONObject cfg;
    private LinearLayout content;
    private TextView status;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);

        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        bar.setBackgroundColor(SURFACE);
        bar.setPadding(dp(10), dp(8), dp(10), dp(8));

        TextView back = new TextView(this);
        back.setText("← 返回");
        back.setTextSize(13.5f);
        back.setTextColor(ACCENT);
        back.setPadding(dp(6), dp(6), dp(6), dp(6));
        back.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { finish(); }
        });
        bar.addView(back);

        TextView title = new TextView(this);
        title.setText("全部配置");
        title.setTextSize(14f);
        title.setTextColor(INK);
        title.setTypeface(Typeface.DEFAULT_BOLD);
        title.setPadding(dp(8), 0, 0, 0);
        bar.addView(title, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        status = new TextView(this);
        status.setTextSize(11f);
        status.setTextColor(INK3);
        bar.addView(status);

        Button save = new Button(this);
        save.setText("保存");
        save.setAllCaps(false);
        save.setTextSize(12.5f);
        save.setTextColor(0xFFFFFFFF);
        GradientDrawable bg = new GradientDrawable();
        bg.setColor(ACCENT);
        bg.setCornerRadius(dp(9));
        save.setBackground(bg);
        save.setPadding(dp(14), dp(6), dp(14), dp(6));
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
        load();
    }

    private void load() {
        content.removeAllViews();
        content.addView(hint("加载中…"));
        PanelApi.get(this, "/panel/api/config", new PanelApi.Cb() {
            public void ok(JSONObject o) {
                cfg = o.optJSONObject("config");
                if (cfg == null) {
                    content.removeAllViews();
                    content.addView(hint("配置接口不可用（panel.LoadConfig 未装配）"));
                    return;
                }
                render();
            }
            public void fail(String msg) {
                content.removeAllViews();
                content.addView(hint("读取配置失败：" + msg));
            }
        });
    }

    private void render() {
        content.removeAllViews();
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
        t.setTextSize(12.5f);
        t.setTextColor(ACCENT);
        t.setTypeface(Typeface.DEFAULT_BOLD);
        t.setPadding(dp(4), dp(14), 0, dp(6));
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
        label.setTextColor(INK2);
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
            EditText et = numberInput(String.valueOf(value));
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    commitNumber(owner, key, ((EditText) v).getText().toString());
                }
            });
            box.addView(et);
            return box;
        }
        if (value instanceof String) {
            EditText et = textInput((String) value);
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    try { owner.put(key, ((EditText) v).getText().toString()); } catch (Throwable ignored) {}
                }
            });
            box.addView(et);
            return box;
        }
        if (value instanceof JSONArray && onlyNumbers((JSONArray) value)) {
            JSONArray arr = (JSONArray) value;
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < arr.length(); i++) {
                if (i > 0) sb.append(',');
                sb.append(arr.optInt(i));
            }
            EditText et = textInput(sb.toString());
            et.setHint("逗号分隔，例如 9,21");
            et.setOnFocusChangeListener(new View.OnFocusChangeListener() {
                public void onFocusChange(View v, boolean has) {
                    if (has) return;
                    commitNumberArray(owner, key, ((EditText) v).getText().toString());
                }
            });
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
        status.setText("保存中…");
        PanelApi.post(this, "/panel/api/config", cfg, new PanelApi.Cb() {
            public void ok(JSONObject o) {
                JSONArray need = o.optJSONArray("restart_required");
                String extra = need == null || need.length() == 0 ? ""
                        : "（需重启：" + need.toString() + "）";
                status.setText("已保存");
                toast("配置已保存" + extra);
            }
            public void fail(String msg) {
                status.setText("保存失败");
                toast("保存失败：" + msg);
            }
        });
    }

    private EditText textInput(String value) {
        EditText et = new EditText(this);
        et.setText(value);
        et.setTextSize(13f);
        et.setTextColor(INK);
        et.setSingleLine(true);
        et.setPadding(dp(10), dp(6), dp(10), dp(6));
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
        g.setColor(SURFACE);
        g.setCornerRadius(dp(10));
        g.setStroke(dp(1), LINE);
        return g;
    }

    private int dp(int v) {
        return (int) TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v,
                getResources().getDisplayMetrics());
    }

    private void toast(String s) { Toast.makeText(this, s, Toast.LENGTH_SHORT).show(); }
}
