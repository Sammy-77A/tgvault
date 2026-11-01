package io.github.sammy77a.tgvault

import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.database.ContentObserver
import android.net.Uri
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.provider.MediaStore
import android.util.Log
import androidx.core.app.ServiceCompat
import kotlin.concurrent.thread
import mobile.Mobile

/**
 * Keeps tgvault running in the background with a quiet notification, so the
 * phone's power manager doesn't freeze it between the 15-minute checks. Scheduled
 * jobs alone are held back on some phones (Infinix's Hiber freezes the app
 * seconds after each wake). Every few minutes, or soon after a new photo or
 * video appears, it asks for a scan and lets the uploads run.
 */
class WatchService : Service() {
    private val lock = Object()
    private var wake = false
    @Volatile private var running = false
    private val handler = Handler(Looper.getMainLooper())
    private val observer = object : ContentObserver(handler) {
        override fun onChange(selfChange: Boolean, uri: Uri?) = nudge()
    }

    override fun onCreate() {
        super.onCreate()
        Notifications.createChannels(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE else 0
        try {
            ServiceCompat.startForeground(this, Notifications.WATCH_ID, Notifications.watching(this), type)
        } catch (e: Exception) {
            // Android won't allow a foreground service right now; the scheduled jobs still run.
            Log.w(TAG, "watch service couldn't go to the foreground: $e")
            stopSelf()
            return START_NOT_STICKY
        }
        if (!running) {
            running = true
            contentResolver.registerContentObserver(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, true, observer)
            contentResolver.registerContentObserver(MediaStore.Video.Media.EXTERNAL_CONTENT_URI, true, observer)
            thread(name = "tgvault-watch") { loop() }
            thread(name = "tgvault-schedule") { keepScheduled() }
        }
        return START_STICKY
    }

    private fun nudge() = synchronized(lock) {
        wake = true
        lock.notifyAll()
    }

    private fun loop() {
        try {
            Core.start(applicationContext)
            BackupWorker.schedule(applicationContext)
        } catch (e: Exception) {
            Log.e(TAG, "watch service couldn't start tgvault", e)
        }
        while (running) {
            val began = System.currentTimeMillis()
            try {
                Core.update(applicationContext)
                val finished = Mobile.runBackup(10L * 60)
                Log.i(TAG, "watch: scan and upload ${if (finished) "finished" else "ran out of time"} after ${(System.currentTimeMillis() - began) / 1000}s")
            } catch (e: Exception) {
                Log.e(TAG, "watch: backup failed", e)
            }
            synchronized(lock) {
                if (!wake && running) lock.wait(CHECK_EVERY_MS)
                wake = false
            }
            // A burst of new photos settles before the next scan.
            if (running) Thread.sleep(20_000)
        }
    }

    /** A scan can run for minutes, so the job check has its own thread. */
    private fun keepScheduled() {
        while (running) {
            try {
                BackupWorker.ensureScheduled(applicationContext)
            } catch (e: Exception) {
                Log.e(TAG, "watch: couldn't check the schedule", e)
            }
            Thread.sleep(3 * 60 * 1000L)
        }
    }

    override fun onDestroy() {
        running = false
        contentResolver.unregisterContentObserver(observer)
        nudge()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    companion object {
        private const val TAG = "tgvault"
        private const val CHECK_EVERY_MS = 5 * 60 * 1000L

        /** Starts the watch service; quietly does nothing if Android doesn't allow it now. */
        fun start(ctx: Context) {
            try {
                ctx.startForegroundService(Intent(ctx, WatchService::class.java))
            } catch (e: Exception) {
                Log.w(TAG, "couldn't start the watch service: $e")
            }
        }
    }
}
