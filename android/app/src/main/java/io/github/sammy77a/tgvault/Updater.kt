package io.github.sammy77a.tgvault

import android.content.Context
import android.util.Log
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
            val conn = URL(if (test) "$REPO/tags/android-test" else "$REPO/latest").openConnection() as HttpURLConnection
            conn.connectTimeout = 15_000
            conn.readTimeout = 15_000
            conn.setRequestProperty("Accept", "application/vnd.github+json")
            if (conn.responseCode != 200) return null
            val release = JSONObject(conn.inputStream.bufferedReader().use { it.readText() })
            val latest = Regex("versionCode: (\\d+)").find(release.optString("body"))?.groupValues?.get(1)?.toLong() ?: return null
            if (latest <= installed) return null
            val page = release.getString("html_url")
            Notifications.alert(ctx, "A tgvault update is ready", "Tap to download it, then open the APK to install.", page)
            page
        } catch (e: Exception) {
            Log.w("tgvault", "update check: $e")
            null
        }
    }
}
