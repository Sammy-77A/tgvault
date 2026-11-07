package io.github.sammy77a.tgvault

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.util.Log
import android.widget.Toast
import androidx.core.content.FileProvider
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import org.json.JSONObject

/**
 * Checks GitHub once a day for a newer APK and shows a notification that
 * opens the release page. Test builds follow the "android-test" pre-release.
 */
object Updater {
    private const val REPO = "https://api.github.com/repos/Sammy-77A/tgvault/releases"
    private const val PREFS = "updates"
    private const val DAY_MS = 24L * 60 * 60 * 1000

    fun checkDaily(ctx: Context) {
        val prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val now = System.currentTimeMillis()
        if (now - prefs.getLong("lastCheck", 0) < DAY_MS) return
        prefs.edit().putLong("lastCheck", now).apply()
        check(ctx)
    }

    /** Returns the release page if a newer build exists, after notifying. Call off the main thread. */
    fun check(ctx: Context): String? {
        return try {
            val info = ctx.packageManager.getPackageInfo(ctx.packageName, 0)
            @Suppress("DEPRECATION")
            val installed = info.versionCode.toLong()
            val test = (info.versionName ?: "").contains("-test")
            // The movie app follows its own release, so the normal app's update notice never sees it.
            val url = when {
                Core.library -> "$REPO/tags/android-library"
                test -> "$REPO/tags/android-test"
                else -> "$REPO/latest"
            }
            val conn = URL(url).openConnection() as HttpURLConnection
            conn.connectTimeout = 15_000
            conn.readTimeout = 15_000
            conn.setRequestProperty("Accept", "application/vnd.github+json")
            if (conn.responseCode != 200) return null
            val release = JSONObject(conn.inputStream.bufferedReader().use { it.readText() })
            val latest = Regex("versionCode: (\\d+)").find(release.optString("body"))?.groupValues?.get(1)?.toLong() ?: return null
            if (latest <= installed) return null
            val page = release.getString("html_url")
            if (Core.library) {
                // the movie app installs its own updates: remember where the file is
                val assets = release.optJSONArray("assets")
                var apk = ""
                for (i in 0 until (assets?.length() ?: 0)) {
                    val a = assets!!.getJSONObject(i)
                    if (a.optString("name").endsWith(".apk")) apk = a.optString("browser_download_url")
                }
                ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putString("apkUrl", apk).putLong("apkVersion", latest).apply()
                Notifications.alert(ctx, "A tgmoviebox update is ready", "Tap to install it. Your downloads and sign-in are kept.")
            } else {
                Notifications.alert(ctx, "A tgvault update is ready", "Tap to download it, then open the APK to install.", page)
            }
            page
        } catch (e: Exception) {
            Log.w("tgvault", "update check: $e")
            null
        }
    }

    private fun toast(ctx: Context, text: String) {
        Handler(Looper.getMainLooper()).post { Toast.makeText(ctx, text, Toast.LENGTH_LONG).show() }
    }

    private const val UPDATE_ID = 4242

    /**
     * Downloads the newest movie-app APK, showing its progress in a notification, and offers it
     * to Android's installer (one tap to confirm). The installer can't be opened from the
     * background, so when the app isn't on screen the finished download waits in a notification.
     * Installing over the app keeps its data. Call off the main thread.
     */
    fun installLatest(ctx: Context) {
        try {
            if (!ctx.packageManager.canRequestPackageInstalls()) {
                toast(ctx, "Allow tgmoviebox to install updates, then check for the update again.")
                ctx.startActivity(
                    Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:${ctx.packageName}")).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                )
                return
            }
            val prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            var url = prefs.getString("apkUrl", "") ?: ""
            if (url.isEmpty()) {
                check(ctx)
                url = prefs.getString("apkUrl", "") ?: ""
            }
            if (url.isEmpty()) {
                toast(ctx, "tgmoviebox is up to date")
                return
            }
            toast(ctx, "Downloading the update. You can leave the app; a notification shows when it is ready.")
            Notifications.post(ctx, UPDATE_ID, "Downloading the update", "Starting…", null, 0, quiet = true)
            val dir = File(ctx.cacheDir, "shared").apply { mkdirs() }
            val file = File(dir, "tgmoviebox-update.apk")
            val conn = URL(url).openConnection() as HttpURLConnection
            conn.connectTimeout = 20_000
            conn.readTimeout = 60_000
            conn.instanceFollowRedirects = true
            if (conn.responseCode != 200) {
                fail(ctx, "GitHub answered ${conn.responseCode}")
                return
            }
            val total = conn.contentLengthLong
            var got = 0L
            var lastPct = -1
            conn.inputStream.use { input ->
                file.outputStream().use { out ->
                    val buf = ByteArray(64 * 1024)
                    while (true) {
                        val n = input.read(buf)
                        if (n < 0) break
                        out.write(buf, 0, n)
                        got += n
                        if (total > 0) {
                            val pct = (got * 100 / total).toInt()
                            if (pct != lastPct) {
                                lastPct = pct
                                Notifications.post(ctx, UPDATE_ID, "Downloading the update", "$pct%", null, pct, quiet = true)
                            }
                        }
                    }
                }
            }
            if (total > 0 && got != total) {
                fail(ctx, "the download was cut short")
                return
            }
            val uri = FileProvider.getUriForFile(ctx, "${ctx.packageName}.files", file)
            val install = Intent(Intent.ACTION_VIEW).setDataAndType(uri, "application/vnd.android.package-archive")
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_ACTIVITY_NEW_TASK)
            val tap = PendingIntent.getActivity(ctx, UPDATE_ID, install, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
            Notifications.cancel(ctx, UPDATE_ID)
            Notifications.post(ctx, UPDATE_ID + 1, "tgmoviebox update ready", "Tap to install it. Your downloads and sign-in are kept.", tap)
            try {
                ctx.startActivity(install) // works when the app is on screen; otherwise the notification carries it
            } catch (e: Exception) {
                Log.w("tgvault", "the installer waits in the notification: $e")
            }
        } catch (e: Exception) {
            Log.w("tgvault", "installing the update: $e")
            fail(ctx, e.message ?: e.toString())
        }
    }

    private fun fail(ctx: Context, why: String) {
        Notifications.cancel(ctx, UPDATE_ID)
        Notifications.post(ctx, UPDATE_ID + 2, "Couldn't download the update", "$why. Try again from Settings, Check for an update.", null)
        toast(ctx, "Couldn't download the update: $why")
    }
}
