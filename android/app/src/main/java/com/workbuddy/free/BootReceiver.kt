package com.workbuddy.free

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * BootReceiver 开机自启。
 *
 * 只在用户「上次是运行状态」时拉起——用户主动停止过就别自作主张开机再跑起来。
 * 注意 Android 15+ 禁止从 BOOT_COMPLETED 启动 dataSync 等类型的前台服务，
 * 但 specialUse 不在受限列表内，可以正常启动。
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        val prefs = context.getSharedPreferences("gateway", Context.MODE_PRIVATE)
        if (prefs.getBoolean("autostart", true)) {
            GatewayService.start(context)
        }
    }
}
