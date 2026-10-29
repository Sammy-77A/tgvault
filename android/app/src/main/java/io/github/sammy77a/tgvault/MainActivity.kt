package io.github.sammy77a.tgvault

import android.Manifest
import android.annotation.SuppressLint
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import android.view.View
import android.view.ViewGroup
import android.util.Log
import android.webkit.ConsoleMessage
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.TextView
import android.widget.Toast
import androidx.core.content.FileProvider
import androidx.swiperefreshlayout.widget.SwipeRefreshLayout
import java.io.File
import kotlin.concurrent.thread
import mobile.Mobile

/** Shows tgvault's page, served by the Go code running inside the app. */
class MainActivity : Activity() {
    private lateinit var web: WebView
    private lateinit var swipe: SwipeRefreshLayout
    private var started = false
    private var fullScreen: View? = null // a video playing full screen
    private var fullScreenDone: WebChromeClient.CustomViewCallback? = null
    private lateinit var chrome: WebChromeClient

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        web = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            webViewClient = object : WebViewClient() {
                override fun shouldOverrideUrlLoading(view: WebView, req: WebResourceRequest): Boolean {
                    val url = req.url
                    if (url.host == "127.0.0.1") return false // tgvault's own page
                    if (url.scheme == "tgvault") {
                        handleAppLink(url)
                        return true
                    }
                    openElsewhere(url)
                    return true
                }
            }
        }
        // A previewed video's full-screen button hands the player over here.
        chrome = object : WebChromeClient() {
            // JavaScript errors and console messages go to Android's log (tag
            // tgvault-js), so a page problem can be read over adb.
            override fun onConsoleMessage(m: ConsoleMessage): Boolean {
                val level = when (m.messageLevel()) {
                    ConsoleMessage.MessageLevel.ERROR -> Log.ERROR
                    ConsoleMessage.MessageLevel.WARNING -> Log.WARN
                    else -> Log.INFO
                }
                Log.println(level, "tgvault-js", "${m.message()} (${m.sourceId().substringAfterLast('/')}:${m.lineNumber()})")
                return true
            }

            override fun onShowCustomView(view: View, callback: CustomViewCallback) {
                if (fullScreen != null) {
                    callback.onCustomViewHidden()
                    return
                }
                fullScreen = view
                fullScreenDone = callback
                (window.decorView as ViewGroup).addView(view, ViewGroup.LayoutParams(
                    ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
                @Suppress("DEPRECATION")
                window.decorView.systemUiVisibility = View.SYSTEM_UI_FLAG_FULLSCREEN or
                    View.SYSTEM_UI_FLAG_HIDE_NAVIGATION or View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
            }

            override fun onHideCustomView() {
                val view = fullScreen ?: return
                (window.decorView as ViewGroup).removeView(view)
                @Suppress("DEPRECATION")
                window.decorView.systemUiVisibility = View.SYSTEM_UI_FLAG_VISIBLE
                fullScreen = null
                fullScreenDone?.onCustomViewHidden()
                fullScreenDone = null
            }
        }
        web.webChromeClient = chrome
        // Downloads are saved by tgvault itself into Download/tgvault; a plain
        // browser download would silently do nothing in this view.
        web.setDownloadListener { _, _, _, _, _ ->
            Toast.makeText(this, "Use the Download button; files are saved to Download/tgvault", Toast.LENGTH_LONG).show()
        }
        // Pull down to check Telegram for files from your other devices.
        swipe = SwipeRefreshLayout(this).apply {
            addView(web)
            setColorSchemeColors(0xFF2F6F5E.toInt())
            setOnRefreshListener {
                pageRefresh()
                postDelayed({ isRefreshing = false }, 1500)
            }
        }
        setContentView(swipe)
        thread(name = "tgvault-start") {
            try {
                val url = Core.start(this)
                BackupWorker.schedule(this)
                runOnUiThread {
                    started = true
                    web.loadUrl(url)
                }
                Updater.checkDaily(this)
            } catch (e: Exception) {
                runOnUiThread {
                    setContentView(TextView(this).apply {
                        setPadding(48, 48, 48, 48)
                        text = "tgvault couldn't start:\n\n${e.message}"
                    })
                }
            }
        }
    }

    override fun onResume() {
        super.onResume()
        Core.setForeground(true)
        // Permissions may have changed in Android's settings.
        if (started) {
            pageRefresh() // files from another device may have arrived meanwhile
            thread {
                Core.update(this)
                BackupWorker.schedule(this) // picks up changed Wi-Fi/charging settings
            }
        }
    }

    override fun onPause() {
        super.onPause()
        Core.setForeground(false)
    }

    private fun pageRefresh() {
        if (started) web.evaluateJavascript("window.tgvaultRefresh && window.tgvaultRefresh()", null)
    }

    /** Links of the form tgvault://… come from the page and ask the app to do something. */
    private fun handleAppLink(url: Uri) {
        when (url.host + url.path) {
            "perm/storage" ->
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    openSettings(Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION, Uri.parse("package:$packageName")))
                } else {
                    requestPermissions(arrayOf(Manifest.permission.WRITE_EXTERNAL_STORAGE), 1)
                }
            "perm/battery" ->
                openSettings(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")))
            "perm/notifications" ->
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && shouldAskNotifications()) {
                    requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 2)
                } else {
                    openSettings(Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, packageName))
                }
            "play" -> url.getQueryParameter("u")?.let {
                startActivity(Intent(this, PlayerActivity::class.java).putExtra("url", it))
            }
            "share-log" -> shareLog()
            "check-update" -> thread {
                val page = Updater.check(this)
                runOnUiThread {
                    if (page == null) Toast.makeText(this, "tgvault is up to date", Toast.LENGTH_SHORT).show()
                }
            }
        }
    }

    private fun shouldAskNotifications(): Boolean {
        val prefs = getSharedPreferences("perms", MODE_PRIVATE)
        // Android only shows the request a couple of times; after that, open the settings page.
        val asked = prefs.getInt("notifAsked", 0)
        prefs.edit().putInt("notifAsked", asked + 1).apply()
        return asked < 2
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        thread { Core.update(this) }
    }

    private fun openSettings(intent: Intent) {
        try {
            startActivity(intent)
        } catch (e: ActivityNotFoundException) {
            startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
        }
    }

    private fun openElsewhere(url: Uri) {
        try {
            startActivity(Intent(Intent.ACTION_VIEW, url))
        } catch (e: ActivityNotFoundException) {
            Toast.makeText(this, "No app can open this link", Toast.LENGTH_SHORT).show()
        }
    }

    private fun shareLog() {
        val src = File(Mobile.logPath(Core.dataDir(this)))
        if (!src.exists()) {
            Toast.makeText(this, "There's no log yet", Toast.LENGTH_SHORT).show()
            return
        }
        val dir = File(cacheDir, "shared").apply { mkdirs() }
        val copy = File(dir, "tgvault-log.txt")
        src.copyTo(copy, overwrite = true)
        val uri = FileProvider.getUriForFile(this, "$packageName.files", copy)
        val send = Intent(Intent.ACTION_SEND).apply {
            type = "text/plain"
            putExtra(Intent.EXTRA_STREAM, uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        startActivity(Intent.createChooser(send, "Share tgvault's log"))
    }

    @Deprecated("Back button for the page's own history")
    override fun onBackPressed() {
        when {
            fullScreen != null -> chrome.onHideCustomView()
            // The page closes an open preview (or dialog) first.
            started -> web.evaluateJavascript("window.tgvaultBack ? window.tgvaultBack() : false") { handled ->
                if (handled != "true") {
                    if (web.canGoBack()) web.goBack() else finish()
                }
            }
            web.canGoBack() -> web.goBack()
            else -> super.onBackPressed()
        }
    }
}
