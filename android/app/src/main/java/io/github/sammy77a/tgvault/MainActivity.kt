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
        if (web.canGoBack()) web.goBack() else super.onBackPressed()
    }
}
