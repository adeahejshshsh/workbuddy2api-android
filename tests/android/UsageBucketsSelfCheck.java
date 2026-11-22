import com.workbuddy.api.UsageBuckets;
import com.workbuddy.api.UsageBuckets.Pt;
import com.workbuddy.api.UsageBuckets.Result;

import java.util.ArrayList;
import java.util.List;

/**
 * issue #205 的宿主 JVM 自检：3D/7D 恒空白的取点规则必须按「按日聚合 + 单日回落」来。
 *
 * 不依赖 Android：UsageBuckets 是纯数据变换，构建脚本会 javac + java 直接跑这个类
 * （见 patches/build_apk.sh 第 3 步）。任一断言失败即构建失败。
 */
public final class UsageBucketsSelfCheck {

    private static int failed = 0;

    public static void main(String[] args) {
        caseHourlyWithin24h();
        caseThreeDaysAggregated();
        caseSingleDayFallsBackToHours();
        caseServerDayOverridesAggregated();
        caseEmptySeries();
        caseDayKeyRejectsGarbage();
        if (failed > 0) {
            System.out.println("UsageBucketsSelfCheck: FAILED " + failed);
            System.exit(1);
        }
        System.out.println("UsageBucketsSelfCheck: OK");
    }

    /** ≤24H：原样返回小时点，标记 hourly。 */
    private static void caseHourlyWithin24h() {
        List<Pt> series = new ArrayList<Pt>();
        for (int h = 10; h < 15; h++) {
            series.add(new Pt(String.format("2026-11-22T%02d", h), "hour", 1, 100, 0.5));
        }
        Result r = UsageBuckets.bucket(series, 24);
        check(r.pts.size() == 5, "24H 应有 5 根小时柱，实际 " + r.pts.size());
        check(r.hourly, "24H 的 hourly 标记应为 true");
        check(!r.aggregated, "24H 不该标「由小时点聚合」");
    }

    /** 3D：跨三天的小时点必须聚成 3 根，且三项指标都累加。 */
    private static void caseThreeDaysAggregated() {
        List<Pt> series = new ArrayList<Pt>();
        series.add(new Pt("2026-11-20T08", "hour", 2, 200, 1.0));
        series.add(new Pt("2026-11-20T09", "hour", 3, 300, 2.0));
        series.add(new Pt("2026-11-21T10", "hour", 4, 400, 3.0));
        series.add(new Pt("2026-11-22T11", "hour", 5, 500, 4.0));
        Result r = UsageBuckets.bucket(series, 72);
        check(r.pts.size() == 3, "3D 应聚成 3 根日柱，实际 " + r.pts.size());
        check(!r.hourly && r.aggregated, "3D 应标「按日聚合」");
        Pt d0 = r.pts.get(0);
        check("2026-11-20".equals(d0.t), "首桶日期应为 2026-11-20，实际 " + d0.t);
        check(d0.requests == 5 && d0.tokens == 500 && Math.abs(d0.credits - 3.0) < 1e-9,
                "首桶应累加 5 请求 / 500 tok / 3.0 积分，实际 "
                        + d0.requests + "/" + d0.tokens + "/" + d0.credits);
        check(r.pts.get(2).requests == 5, "末桶请求数应为 5，实际 " + r.pts.get(2).requests);
    }

    /** 刚装几小时：只有一天的小时点，应退回小时点（用户看到 5 根，而不是 1 根）。 */
    private static void caseSingleDayFallsBackToHours() {
        List<Pt> series = new ArrayList<Pt>();
        for (int h = 9; h < 14; h++) {
            series.add(new Pt(String.format("2026-11-22T%02d", h), "hour", 1, 10, 0.1));
        }
        Result r = UsageBuckets.bucket(series, 72);
        check(r.pts.size() == 5, "单日数据应退回 5 根小时柱，实际 " + r.pts.size());
        check(r.hourly, "单日回落时 hourly 标记应为 true");
        check(!r.aggregated, "单日回落时不该标聚合");
    }

    /** 服务端日桶覆盖同一天，且更早的历史（>7 天）也保留。 */
    private static void caseServerDayOverridesAggregated() {
        List<Pt> series = new ArrayList<Pt>();
        series.add(new Pt("2026-11-01", "day", 100, 1000, 9.0));   // 7 天前，只可能来自服务端
        series.add(new Pt("2026-11-21T23", "hour", 1, 10, 0.1));
        series.add(new Pt("2026-11-22T00", "hour", 1, 10, 0.1));
        series.add(new Pt("2026-11-22", "day", 7, 70, 0.7));       // 同一天：日桶更权威
        Result r = UsageBuckets.bucket(series, 720);
        check(r.pts.size() == 3, "应得 11-01 / 11-21 / 11-22 三根，实际 " + r.pts.size());
        Pt last = r.pts.get(r.pts.size() - 1);
        check("2026-11-22".equals(last.t) && last.requests == 7,
                "同一天应由服务端日桶覆盖（7 请求），实际 " + last.t + "/" + last.requests);
    }

    /** 空 series：空结果、不抛异常。 */
    private static void caseEmptySeries() {
        Result r = UsageBuckets.bucket(new ArrayList<Pt>(), 72);
        check(r.pts.isEmpty(), "空输入应返回空柱子");
        check(UsageBuckets.bucket(null, 72).pts.isEmpty(), "null 输入应返回空柱子（不 NPE）");
    }

    /** dayKey：合法取前 10 位，非法返回空串（不造 0001-01-01 那种桶）。 */
    private static void caseDayKeyRejectsGarbage() {
        check("2026-11-22".equals(UsageBuckets.dayKey("2026-11-22T13")), "dayKey 应截取日期段");
        check("".equals(UsageBuckets.dayKey("")), "空串应返回空");
        check("".equals(UsageBuckets.dayKey("2026-1")), "过短输入应返回空");
        check("".equals(UsageBuckets.dayKey(null)), "null 应返回空");
    }

    private static void check(boolean cond, String msg) {
        if (cond) {
            System.out.println("  OK   " + msg);
        } else {
            failed++;
            System.out.println("  FAIL " + msg);
        }
    }
}
