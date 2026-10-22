// gwconfig_selftest.java —— GwConfig 端口解析的离线自检（不进 APK）。
//
// 为什么单独留一份：壳里的探针/面板地址全依赖「从 config.json 的 listen 推端口」，
// 解析错了就是「改端口后 App 面板打不开、看门狗反复杀健康网关」。
//
// 跑法（需要 android.jar 与同目录的 GwConfig.java）：
//   T=$PREFIX/tmp/gwtest; mkdir -p $T/src/com/dsh/wb2api $T/out
//   cp apk-src/java/com/dsh/wb2api/GwConfig.java $T/src/com/dsh/wb2api/
//   cp patches/gwconfig_selftest.java $T/src/com/dsh/wb2api/GwSelfTest.java
//   javac -cp $PREFIX/lib/wbapi-tools/android.jar -d $T/out $T/src/com/dsh/wb2api/*.java
//   java -cp $T/out:$PREFIX/lib/wb2api-tools/android.jar com.dsh.wb2api.GwSelfTest
package com.dsh.wb2api;

public class GwSelfTest {
    static int bad = 0;
    static void eq(String what, Object got, Object want) {
        if (!String.valueOf(got).equals(String.valueOf(want))) {
            System.out.println("FAIL " + what + ": got=" + got + " want=" + want);
            bad++;
        }
    }
    public static void main(String[] a) {
        // listen 解析
        eq(":7863", GwConfig.parsePort(":7863"), 7863);
        eq("127.0.0.1:8080", GwConfig.parsePort("127.0.0.1:8080"), 8080);
        eq("[::1]:9000", GwConfig.parsePort("[::1]:9000"), 9000);
        eq("裸端口 7777", GwConfig.parsePort("7777"), 7777);
        eq("空串", GwConfig.parsePort(""), 0);
        eq("null", GwConfig.parsePort(null), 0);
        eq("超范围", GwConfig.parsePort(":70000"), 0);
        eq("非数字", GwConfig.parsePort(":abc"), 0);
        // 顶层键扫描
        eq("顶层 listen", GwConfig.findTopLevelString("{\"listen\":\":8081\",\"api_key\":\"k\"}", "listen"), ":8081");
        eq("嵌套同名不误取", GwConfig.findTopLevelString("{\"upstream\":{\"listen\":\":1\"},\"listen\":\":8082\"}", "listen"), ":8082");
        eq("值里有转义引号", GwConfig.findTopLevelString("{\"api_key\":\"a\\\"b\",\"listen\":\":8083\"}", "listen"), ":8083");
        eq("缺键", GwConfig.findTopLevelString("{\"api_key\":\"k\"}", "listen"), null);
        eq("数组里同名不取", GwConfig.findTopLevelString("{\"arr\":[{\"listen\":\":1\"}]}", "listen"), null);
        System.out.println(bad == 0 ? "ALL PASS" : ("FAILURES=" + bad));
        System.exit(bad == 0 ? 0 : 1);
    }
}
