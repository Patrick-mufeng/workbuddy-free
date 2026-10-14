package com.workbuddy.free

import android.annotation.SuppressLint
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.util.Log
import android.view.View
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import java.net.HttpURLConnection
import java.net.NetworkInterface
import java.net.URL

/**
 * MainActivity 承载内嵌的 Web 管理面板。
 *
 * 面板本身是 Go 二进制以 go:embed 打进程序的静态资源（含独立的 app.js），
 * CSP 为 default-src 'none' + script-src 'self'，无任何外部 CDN 依赖，
 * 因此用 WebView 直接加载 http://127.0.0.1:3012/panel/ 即可，无需本地资源转存。
 */
class MainActivity : AppCompatActivity() {

    private companion object {
        const val TAG = "WBGateway"
        /** 主文档加载失败后的自动重载上限，避免网关长期不可用时反复重载。 */
        const val MAX_PANEL_RETRIES = 6
    }

    private lateinit var web: WebView
    private lateinit var statusDot: View
    private lateinit var statusText: TextView
    private lateinit var btnToggle: Button
    private lateinit var addrText: TextView
    private lateinit var btnAddr: Button

    private var loaded = false
    private var lastReady = false
    private var portLoading = false

    /** 主文档加载失败的重试计数（成功后归零，见 onPageFinished）。 */
    private var panelRetries = 0

    /**
     * 状态轮询：服务的启停是异步的（startForegroundService → onStartCommand → 拉起进程
     * 需要一两秒），点完按钮立刻读状态必然拿到旧值。这里在前台期间定周期刷新，
     * 让「运行中 / 未运行」始终与真实进程一致。
     */
    private val uiHandler = android.os.Handler(android.os.Looper.getMainLooper())
    private val statusTick = object : Runnable {
        override fun run() {
            refreshStatus()
            uiHandler.postDelayed(this, 1500)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        web = findViewById(R.id.web)
        statusDot = findViewById(R.id.statusDot)
        statusText = findViewById(R.id.statusText)
        btnToggle = findViewById(R.id.btnToggle)
        addrText = findViewById(R.id.addrText)
        btnAddr = findViewById(R.id.btnAddr)

        setupWebView()

        // 「启动 / 停止」按钮：直接以本次点击为准切换，不看缓存的 running 值。
        btnToggle.setOnClickListener {
            if (GatewayService.running) {
                GatewayService.stop(this)
            } else {
                GatewayService.start(this)
            }
            refreshStatus()
        }

        // 点地址即复制，方便粘贴到其它设备。
        btnAddr.setOnClickListener {
            val a = addrText.text.toString()
            if (a.isNotBlank() && a != getString(R.string.addr_unknown)) {
                copyToClipboard(a)
            }
        }

        // 息屏后仍要常驻，先引导用户放行电池优化。
        maybeAskBattery()

        // 首次进入即拉起网关（已运行则为空操作）。
        GatewayService.start(this)
    }

    override fun onResume() {
        super.onResume()
        refreshStatus()
        uiHandler.post(statusTick)
        // 服务起来后加载面板；已在跑就直接刷新。
        maybeLoadPanel()
    }

    override fun onPause() {
        uiHandler.removeCallbacks(statusTick)
        super.onPause()
    }

    // ── WebView ────────────────────────────────────────────────────────

    @SuppressLint("SetJavaScriptEnabled")
    private fun setupWebView() {
        val s: WebSettings = web.settings
        s.javaScriptEnabled = true // 面板是 JS 渲染的（app.js），必须开启
        s.domStorageEnabled = true // 面板用 localStorage 记住 api_key 与主题
        s.cacheMode = WebSettings.LOAD_NO_CACHE // 面板是本地服务，避免缓存住旧前端
        s.useWideViewPort = true
        s.loadWithOverviewMode = false
        s.mediaPlaybackRequiresUserGesture = true
        s.allowFileAccess = false
        s.allowContentAccess = false

        web.setBackgroundColor(0xFF0B0E14.toInt()) // 与面板 --bg 一致，消除加载白闪
        web.isVerticalScrollBarEnabled = true

        web.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(
                view: WebView,
                request: WebResourceRequest
            ): Boolean {
                val url = request.url
                // 只接管真正的外链（OAuth 授权页跳浏览器完成，WebView 内登不了腾讯账号）。
                // 本机地址一律留在 WebView 内。
                if (url.host == "127.0.0.1" || url.host == "localhost") return false
                Log.i(TAG, "open externally: $url")
                return openExternally(url)
            }

            override fun onPageFinished(view: WebView, url: String) {
                // 加载成功即清零重试计数（下次失败可从零再来）。
                panelRetries = 0
                Log.i(TAG, "page finished: $url")
            }

            override fun onReceivedError(
                view: WebView,
                request: WebResourceRequest,
                error: WebResourceError
            ) {
                if (!request.isForMainFrame) return
                Log.w(TAG, "page error: ${error.errorCode} ${error.description} url=${request.url}")
                // 主文档加载失败：网关可能还在启动或刚重启。等端口就绪后重载，
                // 否则 WebView 会停在一个空白/错误页上再也不动。
                // 计数封顶：网关长期不可用时不至于无限重载耗电。
                if (!isFinishing && !isDestroyed && panelRetries < MAX_PANEL_RETRIES) {
                    panelRetries++
                    loadPanel(waitForPort = true)
                }
            }
        }
    }

    private fun openExternally(uri: Uri): Boolean {
        return try {
            startActivity(Intent(Intent.ACTION_VIEW, uri).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            true
        } catch (e: ActivityNotFoundException) {
            Toast.makeText(this, R.string.err_no_browser, Toast.LENGTH_SHORT).show()
            true
        }
    }

    private fun maybeLoadPanel() {
        loadPanel(waitForPort = true)
    }

    /** 面板地址；网关固定监听回环地址的 3012 端口。 */
    private val panelUrl: String get() = "http://127.0.0.1:${GatewayService.PORT}/panel/"

    /**
     * 加载面板。网关进程从拉起至监听需要一两秒，故 waitForPort=true 时
     * 先轮询 /panel/ 再 loadUrl，避免加载出连接失败页。
     * api_key 存在 WebView 的 localStorage 里，重新加载不会丢。
     */
    private fun loadPanel(waitForPort: Boolean) {
        if (!waitForPort) {
            web.loadUrl(panelUrl)
            loaded = true
            return
        }
        if (portLoading) return
        portLoading = true
        Thread {
            try {
                for (i in 0 until 30) {
                    if (isFinishing || isDestroyed) return@Thread
                    if (portReady()) {
                        runOnUiThread {
                            if (!isFinishing && !isDestroyed) {
                                web.loadUrl(panelUrl)
                                loaded = true
                            }
                        }
                        return@Thread
                    }
                    Thread.sleep(500)
                }
            } finally {
                portLoading = false
            }
        }.apply { isDaemon = true; name = "panel-loader" }.start()
    }

    private fun portReady(): Boolean = try {
        val conn = URL(panelUrl).openConnection() as HttpURLConnection
        conn.connectTimeout = 1500
        conn.readTimeout = 1500
        conn.responseCode
        conn.disconnect()
        true
    } catch (t: Throwable) {
        false
    }

    // ── 状态区 ──────────────────────────────────────────────────────────

    private fun refreshStatus() {
        val running = GatewayService.running
        statusText.text = when {
            running -> getString(R.string.status_running)
            GatewayService.lastError != null -> GatewayService.lastError
            else -> getString(R.string.status_stopped)
        }
        statusDot.setBackgroundResource(
            if (running) R.drawable.dot_on else R.drawable.dot_off
        )
        btnToggle.setText(if (running) R.string.btn_stop else R.string.btn_start)
        addrText.text = if (running) localAddress() else getString(R.string.addr_unknown)

        // 状态由「停」翻「运行」：WebView 里可能还停在上次的连接失败页，
        // 重新加载，否则界面是死的。
        if (running && !lastReady) loadPanel(waitForPort = true)
        lastReady = running
    }

    /** 取本机局域网 IPv4，拼成面板地址——给其它设备填写 Base URL 用。 */
    private fun localAddress(): String {
        val port = GatewayService.PORT
        return try {
            val ifaces = NetworkInterface.getNetworkInterfaces()
            var fallback: String? = null
            for (nif in ifaces) {
                if (!nif.isUp || nif.isLoopback) continue
                for (addr in nif.inetAddresses) {
                    val host = addr.hostAddress ?: continue
                    if (addr.isLoopbackAddress || host.contains(':')) continue // 只要 IPv4
                    if (nif.name.startsWith("wlan")) {
                        return "http://$host:$port/v1"
                    }
                    if (fallback == null) fallback = "http://$host:$port/v1"
                }
            }
            fallback ?: getString(R.string.addr_unknown)
        } catch (t: Throwable) {
            getString(R.string.addr_unknown)
        }
    }

    private fun copyToClipboard(text: String) {
        val cm = getSystemService(Context.CLIPBOARD_SERVICE) as android.content.ClipboardManager
        cm.setPrimaryClip(android.content.ClipData.newPlainText("workbuddy-free", text))
        Toast.makeText(this, getString(R.string.addr_copied, text), Toast.LENGTH_SHORT).show()
    }

    // ── 电池优化引导 ────────────────────────────────────────────────────

    /**
     * 不加白名单的话，Doze 会挂起网关的网络访问，定时任务与转发都会静默失效。
     * 这里只弹一次，用户拒绝也不阻断使用（前台服务本身已大幅提升存活率）。
     */
    private fun maybeAskBattery() {
        val prefs = getSharedPreferences("gateway", MODE_PRIVATE)
        if (prefs.getBoolean("asked_battery", false)) return

        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        if (pm.isIgnoringBatteryOptimizations(packageName)) {
            prefs.edit().putBoolean("asked_battery", true).apply()
            return
        }
        prefs.edit().putBoolean("asked_battery", true).apply()
        try {
            startActivity(
                Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
                    .setData(Uri.parse("package:$packageName"))
            )
        } catch (e: ActivityNotFoundException) {
            // 部分 ROM 没有该 Intent，退回到电池优化列表页。
            try {
                startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
            } catch (_: ActivityNotFoundException) {
            }
        }
    }

    override fun onBackPressed() {
        if (web.canGoBack()) {
            web.goBack()
        } else {
            // 退到后台但不杀服务：网关继续跑，从通知栏可回到界面。
            moveTaskToBack(true)
        }
    }

    override fun onDestroy() {
        web.destroy()
        super.onDestroy()
    }
}
