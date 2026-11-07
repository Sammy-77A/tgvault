package io.github.sammy77a.tgvault

import android.app.AlarmManager
import android.app.Notification
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.app.ServiceCompat
import kotlin.concurrent.thread
import mobile.Mobile
import org.json.JSONObject

/**
 * Keeps the movie app's downloads going with the app closed. While anything is
 * queued or running it is a foreground service with a progress notification
 * (with Pause and Cancel), so the phone's power manager leaves the app alone;
 * when the queue is empty it stops by itself.
 */
class DownloadService : Service() {
    @Volatile private var running = false

    override fun onCreate() {
        super.onCreate()
        alive = true
        Notifications.createChannels(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_PAUSE -> thread { Mobile.downloadAction("pause") }
            ACTION_CANCEL -> thread { Mobile.downloadAction("cancel-all") }
        }
        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC else 0
        try {
            ServiceCompat.startForeground(this, Notifications.DOWNLOAD_ID, build("Starting the download…", 0, true), type)
        } catch (e: Exception) {
            Log.w(TAG, "download service couldn't go to the foreground: $e")
            stopSelf()
            return START_NOT_STICKY
        }
        if (!running) {
            running = true
            thread(name = "tgvault-downloads") { watch() }
        }
        return START_STICKY
    }

    private fun watch() {
        var idle = 0
        try {
            Core.start(applicationContext)
        } catch (e: Exception) {
            Log.e(TAG, "download service couldn't start tgvault", e)
        }
        while (running) {
            val j = try { JSONObject(Mobile.downloadStatus()) } catch (e: Exception) { JSONObject() }
            if (!j.optBoolean("active")) {
                // give a download that is about to be queued a moment before stopping
                if (++idle >= 4) break
            } else {
                idle = 0
                // If Android ends the app for memory, this alarm brings the downloads back.
                markActive(applicationContext, true)
                val size = j.optLong("size")
                val done = j.optLong("done")
                val pct = if (size > 0) (done * 100 / size).toInt().coerceIn(0, 100) else 0
                val speed = j.optDouble("speed")
                val count = j.optInt("count")
                val title = j.optString("title")
                val text = buildString {
                    append(if (count > 1) "$count files: $title" else title)
                    append(" · ").append(pct).append("%")
                    if (speed > 1) append(" · ").append(human(speed)).append("/s")
                    if (speed > 1 && size > done) {
                        val eta = ((size - done) / speed).toLong()
                        append(" · ").append(if (eta < 90) "${eta}s" else "${eta / 60} min").append(" left")
                    }
                }
                try {
                    NotificationManagerCompat.from(this).notify(Notifications.DOWNLOAD_ID, build(text, pct, false))
                } catch (e: SecurityException) {
                    // notifications are off; the downloads carry on
                }
            }
            Thread.sleep(1000)
        }
        running = false
        markActive(applicationContext, false)
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    private fun build(text: String, pct: Int, indeterminate: Boolean): Notification {
        fun action(act: String, req: Int) = PendingIntent.getService(
            this, req, Intent(this, DownloadService::class.java).setAction(act), PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return NotificationCompat.Builder(this, Notifications.DOWNLOADS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Downloading")
            .setContentText(text)
            .setContentIntent(Notifications.openApp(this, "downloads"))
            .setProgress(100, pct, indeterminate)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .addAction(0, "Pause", action(ACTION_PAUSE, 1))
            .addAction(0, "Cancel", action(ACTION_CANCEL, 2))
            .build()
    }

    private fun human(b: Double): String = when {
        b >= 1e9 -> "%.1f GB".format(b / 1e9)
        b >= 1e6 -> "%.1f MB".format(b / 1e6)
        else -> "%.0f KB".format(b / 1e3)
    }

    override fun onDestroy() {
        alive = false
        running = false
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onTaskRemoved(rootIntent: Intent?) {
        // swiped away from the recent apps: keep going, and make sure of it
        if (running) armRevive(applicationContext, 5_000)
        super.onTaskRemoved(rootIntent)
    }

    companion object {
        private const val TAG = "tgvault"
        const val ACTION_PAUSE = "tgvault.download.pause"
        const val ACTION_CANCEL = "tgvault.download.cancel"

        const val ACTION_REVIVE = "tgvault.download.revive"
        private const val PREFS = "dlservice"
        private const val CHECK_EVERY_MS = 60_000L

        /** Remembers whether downloads are running, and keeps an alarm set while they are. */
        fun markActive(ctx: Context, active: Boolean) {
            val now = System.currentTimeMillis()
            if (active && now - lastArm < 30_000L) return // the loop calls this every second
            lastArm = now
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean("active", active).apply()
            if (active) armRevive(ctx, CHECK_EVERY_MS) else disarmRevive(ctx)
        }

        @Volatile var alive = false
        @Volatile private var lastArm = 0L

        fun wasActive(ctx: Context): Boolean = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getBoolean("active", false)

        private fun revivePending(ctx: Context): PendingIntent = PendingIntent.getBroadcast(
            ctx, 77, Intent(ctx, ReviveReceiver::class.java).setAction(ACTION_REVIVE),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )

        /**
         * An alarm that wakes the app if it has been ended for memory (a game can force that);
         * an alarm may start the service when the app is in the background.
         */
        fun armRevive(ctx: Context, inMs: Long) {
            val am = ctx.getSystemService(Context.ALARM_SERVICE) as AlarmManager
            val at = System.currentTimeMillis() + inMs
            try {
                am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, revivePending(ctx))
            } catch (e: SecurityException) {
                am.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at, revivePending(ctx))
            }
        }

        private fun disarmRevive(ctx: Context) {
            (ctx.getSystemService(Context.ALARM_SERVICE) as AlarmManager).cancel(revivePending(ctx))
        }

        /** Starts the service; quietly does nothing if Android doesn't allow it now. */
        fun start(ctx: Context) {
            try {
                ctx.startForegroundService(Intent(ctx, DownloadService::class.java))
            } catch (e: Exception) {
                Log.w(TAG, "couldn't start the download service: $e")
            }
        }
    }
}
