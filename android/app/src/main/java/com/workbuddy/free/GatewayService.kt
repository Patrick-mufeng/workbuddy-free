package com.workbuddy.free

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.util.Log
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import java.util.TimeZone

/**
 * GatewayService 把 Go 网关（libwb2api.so）作为前台服务常驻运行。
 *
 * 三个 Android 平台要点，缺一不可：
 *
 * 1) **二进制位置**：Android 10 起，targetSdk >= 29 的应用禁止 execve 私有目录
 *    （filesDir/cacheDir）内的文件——这是 W^X 策略（见 Android 10 行为变更
 *    "Removed execute permission for app home directory"）。而 APK 内以 lib*.so
 *    命名的原生库会被安装到 nativeLibraryDir（/data/app/<pkg>/lib/arm64/），
 *    该目录属 SELinux 的 apk_data_file，对所有应用域开放 execute 权限，
 *    安装器还会 chmod 0755。因此把 Go 可执行文件命名为 libwb2api.so 放进
 *    jniLibs，再从 nativeLibraryDir 启动，是唯一稳定可行的路径
 *    （shadowsocks-android / Orbot / ConnectBot / VLC 等成熟应用同此做法）。
 *
 * 2) **时区**：Go 标准库在 Android 上把 time.Local 硬编码成 UTC，
 *    schedule.*_hours 的整点判定会整体偏移 8 小时。Go 侧 tz_android.go 通过
 *    $TZ 读取设备时区，所以这里必须把 TimeZone.getDefault().id 传进子进程环境。
 *
 * 3) **工作目录**：Go 侧 auth_dir 默认 ./auths、state_file 默认 ./data/state.json，
 *    都是相对 cwd 解析。以 filesDir 作为 cwd，凭证与状态即落在应用私有目录，
 *    卸载随应用一起清除，不需要任何额外权限（SAF / MANAGE_EXTERNAL_STORAGE 都不用）。
 */
class GatewayService : Service() {

    companion object {
        private const val TAG = "WBGateway"

        /** 网关监听端口，与 Go 侧 config.json 的 listen 保持一致。 */
        const val PORT = 3012

        const val ACTION_START = "com.workbuddy.free.action.START"
        const val ACTION_STOP = "com.workbuddy.free.action.STOP"

        private const val CHANNEL_ID = "gateway"
        private const val NOTIF_ID = 1

        /** 二进制在 nativeLibraryDir 中的文件名：必须以 lib 开头、.so 结尾。 */
        private const val BINARY_NAME = "libwb2api.so"

        /** 健康检查间隔；Go 网关 /healthz 无鉴权，可直接探活。 */
        private const val WATCHDOG_INTERVAL_MS = 15_000L

        /** 启动后多久算「稳定」——快于此值退出视为启动失败，进入退避。 */
        private const val STABLE_UPTIME_MS = 10_000L

        @Volatile
        var running: Boolean = false
            private set

        /** 最近一次已知的失败原因，供界面展示。 */
        @Volatile
        var lastError: String? = null
            private set

        fun start(context: Context) {
            val i = Intent(context, GatewayService::class.java).setAction(ACTION_START)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(i)
            } else {
                context.startService(i)
            }
        }

        fun stop(context: Context) {
            context.startService(Intent(context, GatewayService::class.java).setAction(ACTION_STOP))
        }
    }

    private var process: Process? = null
    private var watchdog: Thread? = null

    @Volatile
    private var stopping = false

    /** 连续启动失败次数，用于指数退避，避免端口占用等硬错误下疯狂重启。 */
    private var consecutiveFailures = 0

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopping = true
                shutdown()
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                return START_NOT_STICKY
            }
            else -> {
                stopping = false
                startForegroundSafely()
                ensureProcess()
                startWatchdog()
            }
        }
        // START_STICKY：被系统回收后自动重建（配合 BootReceiver 实现重启后恢复）。
        return START_STICKY
    }

    // ── 前台通知 ────────────────────────────────────────────────────────

    private fun startForegroundSafely() {
        createChannel()
        val tap = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )
        val notif: Notification = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.notif_title))
            .setContentText(getString(R.string.notif_text))
            .setSmallIcon(R.drawable.ic_notification)
            .setContentIntent(tap)
            .setOngoing(true)
            .build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIF_ID, notif, android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIF_ID, notif)
        }
    }

    private fun createChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) == null) {
            val ch = NotificationChannel(
                CHANNEL_ID,
                getString(R.string.notif_channel),
                NotificationManager.IMPORTANCE_LOW
            )
            ch.description = getString(R.string.notif_channel_desc)
            ch.setShowBadge(false)
            nm.createNotificationChannel(ch)
        }
    }

    // ── 进程生命周期 ────────────────────────────────────────────────────

    private fun binaryFile(): File = File(applicationInfo.nativeLibraryDir, BINARY_NAME)

    private fun ensureProcess() {
        if (process?.isAlive == true) {
            running = true
            return
        }

        val bin = binaryFile()
        if (!bin.exists()) {
            lastError = getString(R.string.err_bin_missing, bin.absolutePath)
            Log.e(TAG, lastError!!)
            running = false
            return
        }
        if (!bin.canExecute() && !bin.setExecutable(true, false)) {
            lastError = getString(R.string.err_bin_not_exec, bin.absolutePath)
            Log.e(TAG, lastError!!)
            running = false
            return
        }

        val workDir = filesDir
        val logFile = File(workDir, "gateway.log")
        // 日志超 4MB 时截断重来，避免长跑后无限膨胀。
        if (logFile.exists() && logFile.length() > 4L * 1024 * 1024) {
            logFile.delete()
        }

        try {
            val pb = ProcessBuilder(bin.absolutePath, "-config", "config.json")
            pb.directory(workDir)
            // 时区：Go 的 tz_android.go 依赖 $TZ 还原设备时区（见类注释 2）。
            pb.environment()["TZ"] = TimeZone.getDefault().id
            // 子进程与父进程解耦：网关被系统回收后不牵连本服务。
            pb.redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
            pb.redirectError(ProcessBuilder.Redirect.appendTo(logFile))
            val p = pb.start()

            process = p
            startedAt = System.currentTimeMillis()
            running = true
            lastError = null
            Log.i(TAG, "gateway started cwd=$workDir tz=${TimeZone.getDefault().id}")
        } catch (t: Throwable) {
            process = null
            running = false
            lastError = getString(R.string.err_start_failed, t.message ?: t.javaClass.simpleName)
            Log.e(TAG, "start failed", t)
        }
    }

    private fun startWatchdog() {
        if (watchdog?.isAlive == true) return
        watchdog = Thread {
            while (!stopping) {
                try {
                    val p = process
                    if (p == null || !p.isAlive) {
                        // 进程已退出：记录退出码，必要时退避后重启。
                        val code = try {
                            p?.exitValue()
                        } catch (_: IllegalThreadStateException) {
                            null
                        }
                        val aliveMs = p?.let { System.currentTimeMillis() - startedAt } ?: Long.MAX_VALUE
                        running = false
                        if (code != null) {
                            Log.w(TAG, "gateway exited code=$code aliveMs=$aliveMs")
                        }

                        if (stopping) break

                        // 启动即挂 = 配置或端口等硬错误；退避重试，避免热循环。
                        consecutiveFailures = if (aliveMs < STABLE_UPTIME_MS) {
                            consecutiveFailures + 1
                        } else {
                            0
                        }
                        if (consecutiveFailures > 0) {
                            val backoffMs = minOf(5_000L shl minOf(consecutiveFailures - 1, 4), 60_000L)
                            Log.w(TAG, "restart backoff ${backoffMs}ms (failures=$consecutiveFailures)")
                            Thread.sleep(backoffMs)
                        }
                        if (stopping) break
                        ensureProcess()
                    } else {
                        running = true
                        // 存活超过稳定阈值即清零失败计数。
                        if (System.currentTimeMillis() - startedAt > STABLE_UPTIME_MS) {
                            consecutiveFailures = 0
                        }
                        probeHealth()
                    }
                } catch (ie: InterruptedException) {
                    break
                } catch (t: Throwable) {
                    Log.e(TAG, "watchdog error", t)
                }
                try {
                    Thread.sleep(WATCHDOG_INTERVAL_MS)
                } catch (ie: InterruptedException) {
                    break
                }
            }
        }.apply {
            name = "gateway-watchdog"
            isDaemon = true
            start()
        }
    }

    /** 进程启动时刻，用于判断是否属于「启动即挂」。 */
    @Volatile
    private var startedAt: Long = 0

    /**
     * 轻量探活：能建立 HTTP 连接即算服务在线。
     * 注意 /healthz 在「无可用账号」时返回 503——那也是服务正常在跑，故只要求有响应。
     *
     * 进程活着但连续多次探活失败，说明网关已卡死（线程池耗尽 / 死锁等），
     * 此时主动杀掉让看门狗重启它——比一直挂着不干活强。
     */
    private fun probeHealth() {
        val ok = try {
            val conn = URL("http://127.0.0.1:$PORT/healthz").openConnection() as HttpURLConnection
            conn.connectTimeout = 3000
            conn.readTimeout = 3000
            conn.requestMethod = "GET"
            conn.responseCode // 触发请求；状态码不参与判定（503 = 无可用账号，服务本身正常）
            conn.disconnect()
            true
        } catch (t: Throwable) {
            Log.w(TAG, "health probe failed: ${t.message}")
            false
        }

        if (ok) {
            probeFailures = 0
            return
        }
        // 刚启动的头几秒还没监听端口，属正常，不计数。
        if (System.currentTimeMillis() - startedAt < STABLE_UPTIME_MS) return

        probeFailures++
        if (probeFailures >= 4) { // 约 60s 无响应
            Log.w(TAG, "gateway unresponsive ($probeFailures probes), killing for restart")
            probeFailures = 0
            process?.destroyForcibly()
        }
    }

    /** 连续探活失败次数，达阈值则判定卡死并强制重启。 */
    @Volatile
    private var probeFailures = 0

    private fun shutdown() {
        running = false
        val p = process ?: return
        try {
            // 先请求优雅退出（Go 侧 signal.NotifyContext 捕获 SIGTERM 后 Flush 落盘）。
            p.destroy()
            if (!p.waitFor(5, java.util.concurrent.TimeUnit.SECONDS)) {
                p.destroyForcibly()
            }
        } catch (t: Throwable) {
            Log.w(TAG, "shutdown error", t)
        } finally {
            process = null
        }
    }

    override fun onDestroy() {
        stopping = true
        watchdog?.interrupt()
        watchdog = null
        shutdown()
        super.onDestroy()
    }
}
