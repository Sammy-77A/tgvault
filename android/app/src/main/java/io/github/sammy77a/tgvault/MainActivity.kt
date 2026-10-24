package io.github.sammy77a.tgvault

import android.app.Activity
import android.os.Bundle
import android.webkit.WebView
import android.webkit.WebViewClient
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
            webViewClient = WebViewClient()
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
