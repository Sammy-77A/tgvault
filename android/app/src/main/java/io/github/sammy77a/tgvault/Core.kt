package io.github.sammy77a.tgvault

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.media.MediaScannerConnection
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.BatteryManager
import android.os.Build
import android.os.Environment
import android.os.PowerManager
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.FileProvider
import java.io.File
import mobile.Host
import mobile.Mobile

/** Starts the Go side once per process and keeps it informed about the phone. */
object Core {
    private var url: String? = null

    fun dataDir(ctx: Context) = File(ctx.filesDir, "tgvault").absolutePath

    /** Starts tgvault if needed and returns its page URL. Call off the main thread. */
    @Synchronized
    fun start(ctx: Context): String {
        url?.let { return it }
        val app = ctx.applicationContext
        val storage = Environment.getExternalStorageDirectory().absolutePath
        val u = Mobile.start(dataDir(app), storage, AndroidHost(app))
        url = u
        update(app)
        return u
    }

    /** Passes current permissions, network and charging state to tgvault. */
    fun update(ctx: Context) {
        if (url == null) return
        Mobile.setPermissions(hasStorage(ctx), hasNotifications(ctx), hasBattery(ctx))
        Mobile.setConditions(isUnmetered(ctx), isCharging(ctx))
    }

    /** Tells tgvault the phone switched networks, so it reconnects to Telegram. */
    fun networkChanged() {
        if (url != null) Mobile.networkChanged()
    }

    fun hasStorage(ctx: Context): Boolean =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            Environment.isExternalStorageManager()
        } else {
            ctx.checkSelfPermission(Manifest.permission.WRITE_EXTERNAL_STORAGE) == PackageManager.PERMISSION_GRANTED
        }

    fun hasNotifications(ctx: Context) = NotificationManagerCompat.from(ctx).areNotificationsEnabled()

    fun hasBattery(ctx: Context): Boolean =
        ctx.getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(ctx.packageName)

    fun isUnmetered(ctx: Context): Boolean {
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
    }

    fun isCharging(ctx: Context): Boolean {
        val status = ctx.registerReceiver(null, IntentFilter(Intent.ACTION_BATTERY_CHANGED)) ?: return false
        return status.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0) != 0
    }
}

/** What the Go side asks the app to do. */
class AndroidHost(private val ctx: Context) : Host {
    override fun showNotification(title: String, body: String) = Notifications.alert(ctx, title, body)

    override fun fileChanged(path: String) {
        // Keeps the Gallery in step with restored and freed-up files.
        MediaScannerConnection.scanFile(ctx, arrayOf(path), null, null)
    }

    override fun shareFiles(paths: String) {
        val uris = ArrayList(paths.split("\n").filter { it.isNotBlank() }.map {
            FileProvider.getUriForFile(ctx, "${ctx.packageName}.files", File(it))
        })
        if (uris.isEmpty()) return
        val send = if (uris.size == 1) {
            Intent(Intent.ACTION_SEND).putExtra(Intent.EXTRA_STREAM, uris[0])
        } else {
            Intent(Intent.ACTION_SEND_MULTIPLE).putParcelableArrayListExtra(Intent.EXTRA_STREAM, uris)
        }
        send.type = "*/*"
        send.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        ctx.startActivity(Intent.createChooser(send, "Share from tgvault").addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
    }
}
