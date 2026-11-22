package com.workbuddy.api;

import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.List;
import java.util.TreeMap;

/**
 * 用量柱状图的取点规则（issue #205）：把 /panel/api/usage 的 series 折算成「要画的那几根」。
 *
 * <p>为什么单独成类：这段规则错一次就是「3D/7D 恒空白」，而它只跟数据形状有关、与
 * Android 框架无关 —— 放在 {@link ScreenUsage} 里就只能靠读代码保证，放这里可以在
 * 宿主 JVM 上直接跑用例（tests/android/UsageBucketsSelfCheck.java）。
 *
 * <p>规则（与 issue #205 及其评论区的取舍一致）：
 * <ul>
 *   <li>≤24H：直接用小时点（原行为）；</li>
 *   <li>&gt;24H：先把小时点按<b>日期</b>聚合成日柱，再用服务端日桶覆盖同一天
 *       （日桶是权威聚合，且覆盖 7 天以前的历史）；3D → 3 根、7D → 7 根；</li>
 *   <li>聚合后只剩一天（刚装几小时）：退回实际存在的<b>小时点</b>，让用户看到那几根，
 *       而不是一根柱子或空图；</li>
 *   <li>两者都没有才返回空。</li>
 * </ul>
 */
public final class UsageBuckets {

    /** 一个时间点（与 PanelService.Usage.Point 同形，去掉了 Android 依赖）。 */
    public static final class Pt {
        public String t = "", scope = "";
        public long requests, tokens;
        public double credits;

        public Pt() {}

        public Pt(String t, String scope, long requests, long tokens, double credits) {
            this.t = t;
            this.scope = scope;
            this.requests = requests;
            this.tokens = tokens;
            this.credits = credits;
        }
    }

    /** 取点结果：柱子本身 + 两个口径标记（脚注文案要用）。 */
    public static final class Result {
        public final List<Pt> pts = new ArrayList<Pt>();
        /** 画的是小时点（脚注写「小时 / 粒度 1 小时」）。 */
        public boolean hourly;
        /** 日柱由小时点聚合而来（脚注补一句「由小时点按日聚合」）。 */
        public boolean aggregated;
    }

    private UsageBuckets() {}

    /** 见类注释。hours = 当前选中的区间小时数；series 为后端原样返回的那一条。 */
    public static Result bucket(List<Pt> series, int hours) {
        Result r = new Result();
        if (series == null) return r;
        List<Pt> hoursPts = new ArrayList<Pt>();
        for (Pt p : series) {
            if (p != null && "hour".equals(p.scope)) hoursPts.add(p);
        }
        if (hours <= 24) {
            r.pts.addAll(hoursPts);
            r.hourly = true;
            return r;
        }
        // ① 小时点按日期聚合（服务端只保留 7 天小时桶，3D/7D 正好全覆盖）。
        TreeMap<String, Pt> byDay = new TreeMap<String, Pt>(new Comparator<String>() {
            public int compare(String a, String b) { return a.compareTo(b); }
        });
        for (Pt p : hoursPts) {
            String d = dayKey(p.t);
            if (d.isEmpty()) continue;
            Pt acc = byDay.get(d);
            if (acc == null) {
                acc = new Pt();
                acc.t = d;
                acc.scope = "day";
                byDay.put(d, acc);
            }
            acc.requests += p.requests;
            acc.tokens += p.tokens;
            acc.credits += p.credits;
        }
        // ② 服务端日桶覆盖同一天：它是权威聚合，而且覆盖 7 天以前的历史。
        for (Pt p : series) {
            if (p == null || !"day".equals(p.scope)) continue;
            String d = dayKey(p.t);
            if (d.isEmpty()) continue;
            byDay.put(d, p);
        }
        r.pts.addAll(byDay.values());
        r.aggregated = r.pts.size() > 1;
        // ③ 只剩一天（刚装几小时）：退回实际小时点，别让用户看一根柱子。
        if (r.pts.size() <= 1 && !hoursPts.isEmpty()) {
            r.pts.clear();
            r.pts.addAll(hoursPts);
            r.hourly = true;
            r.aggregated = false;
        }
        return r;
    }

    /** "2026-11-16T13" / "2026-11-16" → "2026-11-16"；非法输入返回空串（不造桶）。 */
    public static String dayKey(String t) {
        if (t == null || t.length() < 10) return "";
        return t.substring(0, 10);
    }

    /** 按时间升序稳定排序（后端已排好，这里只兜住异常输入；相等时保持原序）。 */
    public static void sortByTime(List<Pt> pts) {
        Collections.sort(pts, new Comparator<Pt>() {
            public int compare(Pt a, Pt b) {
                String x = a == null ? "" : a.t, y = b == null ? "" : b.t;
                return x.compareTo(y);
            }
        });
    }
}
