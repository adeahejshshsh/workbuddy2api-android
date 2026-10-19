package com.dsh.wb2api;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** 开机 / App 更新后自启网关（看门狗的第三层：系统级复活） */
public class BootReceiver extends BroadcastReceiver {
    @Override public void onReceive(Context c, Intent i) {
        String a = (i == null) ? null : i.getAction();
        // 只认受保护广播：BOOT_COMPLETED 与 MY_PACKAGE_REPLACED 只有系统能发。
        // QUICKBOOT_POWERON 不是受保护广播 —— exported=true 下任何应用都能发，
        // 等于给了外部一个「随时拉起本 App 前台服务」的开关。
        if (Intent.ACTION_BOOT_COMPLETED.equals(a)
                || Intent.ACTION_MY_PACKAGE_REPLACED.equals(a)) {
            GatewayService.start(c);
        }
    }
}
