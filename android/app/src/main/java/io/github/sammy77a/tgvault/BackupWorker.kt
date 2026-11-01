package io.github.sammy77a.tgvault

import android.app.job.JobScheduler
import android.content.Context
import android.content.pm.ServiceInfo
import android.os.Build
import android.provider.MediaStore
import android.util.Log
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.ForegroundInfo
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.Worker
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import java.util.concurrent.TimeUnit
import mobile.Mobile

/**
 * Runs a backup while the app is closed: scans the folders and uploads until
 * nothing is left. Android decides exactly when, within the conditions set
 * (Wi-Fi only, charging only). Uploads resume where they stopped next time.
 */
class BackupWorker(ctx: Context, params: WorkerParameters) : Worker(ctx, params) {
    override fun doWork(): Result {
        val foreground = try {
            setForegroundAsync(foregroundInfo()).get()
            true
        } catch (e: Exception) {
            // Android didn't allow a long-running job right now; do what fits in a short one.
            Log.w(TAG, "running without a foreground notification: $e")
            false
        }
        val began = System.currentTimeMillis()
        try {
            Core.start(applicationContext)
            Core.update(applicationContext)
            WatchService.start(applicationContext)
            val finished = Mobile.runBackup(if (foreground) 50L * 60 else 9L * 60)
            Log.i(TAG, "scheduled run: ${if (finished) "finished" else "ran out of time"} after ${(System.currentTimeMillis() - began) / 1000}s, foreground=$foreground")
            Updater.checkDaily(applicationContext)
        } catch (e: Exception) {
            Log.e(TAG, "backup failed", e)
        } finally {
            if (inputData.getBoolean(KEY_MEDIA_TRIGGER, false)) watchNewMedia(applicationContext)
        }
        return Result.success()
    }

    private fun foregroundInfo(): ForegroundInfo {
        val n = Notifications.backingUp(applicationContext)
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            ForegroundInfo(Notifications.BACKUP_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            ForegroundInfo(Notifications.BACKUP_ID, n)
        }
    }

    companion object {
        private const val TAG = "tgvault"
        private const val PERIODIC = "backup"
        private const val MEDIA = "backup-new-media"
        private const val KEY_MEDIA_TRIGGER = "media"

        private fun constraints(wifiOnly: Boolean, chargingOnly: Boolean, media: Boolean): Constraints {
            val b = Constraints.Builder()
                .setRequiredNetworkType(if (wifiOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
                .setRequiresCharging(chargingOnly)
            if (media) {
                b.addContentUriTrigger(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, true)
                b.addContentUriTrigger(MediaStore.Video.Media.EXTERNAL_CONTENT_URI, true)
                b.setTriggerContentUpdateDelay(1, TimeUnit.MINUTES)
            }
            return b.build()
        }

        /** Schedules background backups with the current settings. Call after Core.start. */
        fun schedule(ctx: Context) {
            val wifi = Mobile.wifiOnly()
            val charging = Mobile.chargingOnly()
            val periodic = PeriodicWorkRequestBuilder<BackupWorker>(15, TimeUnit.MINUTES)
                .setConstraints(constraints(wifi, charging, false))
                .build()
            // After Android drops the periodic job, WorkManager still believes it is
            // scheduled and UPDATE leaves it missing; start it afresh in that case.
            val registered = ctx.getSystemService(JobScheduler::class.java).allPendingJobs.any { it.isPeriodic }
            val policy = if (registered) ExistingPeriodicWorkPolicy.UPDATE else ExistingPeriodicWorkPolicy.CANCEL_AND_REENQUEUE
            WorkManager.getInstance(ctx).enqueueUniquePeriodicWork(PERIODIC, policy, periodic)
            watchNewMedia(ctx, ExistingWorkPolicy.REPLACE)
        }

        /**
         * Android drops an app's scheduled jobs in several situations (an update, a
         * cleaner), and WorkManager doesn't always notice. Puts them back when fewer
         * than the expected two are pending.
         */
        fun ensureScheduled(ctx: Context) {
            val jobs = ctx.getSystemService(JobScheduler::class.java).allPendingJobs
            if (jobs.size < 2 || jobs.none { it.isPeriodic }) {
                Log.w(TAG, "only ${jobs.size} background job(s) were scheduled; scheduling them again")
                schedule(ctx)
            }
        }

        /** Runs a backup soon after new photos or videos appear. Re-armed after each run. */
        private fun watchNewMedia(ctx: Context, policy: ExistingWorkPolicy = ExistingWorkPolicy.APPEND_OR_REPLACE) {
            val req = OneTimeWorkRequestBuilder<BackupWorker>()
                .setConstraints(constraints(Mobile.wifiOnly(), Mobile.chargingOnly(), true))
                .setInputData(workDataOf(KEY_MEDIA_TRIGGER to true))
                .build()
            WorkManager.getInstance(ctx).enqueueUniqueWork(MEDIA, policy, req)
        }
    }
}
