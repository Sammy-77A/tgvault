package io.github.sammy77a.tgvault

import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Intent
import android.os.Bundle
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import android.widget.TextView
import java.io.File
import kotlin.concurrent.thread
import mobile.Mobile

/** Shows tgvault's page, served by the Go code running inside the app. */
class MainActivity : Activity() {
    private lateinit var web: WebView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        web = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            webViewClient = object : WebViewClient() {
                // tgvault's own page stays here; other links (tg://login, websites) open in their apps.
                override fun shouldOverrideUrlLoading(view: WebView, req: WebResourceRequest): Boolean {
                    if (req.url.host == "127.0.0.1") return false
                    try {
                        startActivity(Intent(Intent.ACTION_VIEW, req.url))
                    } catch (e: ActivityNotFoundException) {
                        Toast.makeText(this@MainActivity, "No app can open this link", Toast.LENGTH_SHORT).show()
                    }
                    return true
                }
            }
        }
        setContentView(web)
        val dataDir = File(filesDir, "tgvault").absolutePath
        thread(name = "tgvault-start") {
            try {
                val url = Mobile.start(dataDir)
                runOnUiThread { web.loadUrl(url) }
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

    @Deprecated("Back button for the page's own history")
    override fun onBackPressed() {
        if (web.canGoBack()) web.goBack() else super.onBackPressed()
    }
}
