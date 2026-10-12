package com.dsh.wb2api;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** 开机 / App 更新后自启网关（看门狗的第三层：系统级复活） */
public class BootReceiver extends BroadcastReceiver {
    @Override public void onReceive(Context c, Intent i) {
        String a = (i == null) ? null : i.getAction();
        if (Intent.ACTION_BOOT_COMPLETED.equals(a)
                || Intent.ACTION_MY_PACKAGE_REPLACED.equals(a)
                || "android.intent.action.QUICKBOOT_POWERON".equals(a)) {
            GatewayService.start(c);
        }
    }
}
