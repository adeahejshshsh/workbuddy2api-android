/**
 * 看门狗纯逻辑自检（不需要真机）：javac 后直接用宿主 JVM 跑。
 *   javac -cp android.jar:classes -d test-out test/WatchdogSelfCheck.java
 *   java  -cp android.jar:classes:test-out WatchdogSelfCheck
 */
public class WatchdogSelfCheck {
    public static void main(String[] a) {
        long[] want = {2000, 4000, 8000, 16000, 32000, 60000, 60000, 60000};
        for (int n = 1; n <= want.length; n++) {
            long got = com.dsh.wb2api.GatewayService.backoffMs(n);
            if (got != want[n - 1]) {
                System.out.println("FAIL n=" + n + " got=" + got + " want=" + want[n - 1]);
                System.exit(1);
            }
        }
        if (com.dsh.wb2api.GatewayService.backoffMs(0) != 2000) {
            System.out.println("FAIL n=0 应退化为 2s");
            System.exit(1);
        }
        System.out.println("OK 退避序列 2/4/8/16/32/60s 封顶，n<=0 退化 2s");
    }
}
