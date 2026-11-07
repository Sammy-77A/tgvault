package io.github.sammy77a.tgvault

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import kotlin.concurrent.thread

/**
 * Puts the background backup schedule back after the phone restarts or the app
 * is updated. Android drops an app's scheduled jobs when it is replaced, and
 * without them nothing wakes tgvault until the app is opened by hand.
 */
class ReviveReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val pending = goAsync()
        if (Core.library && intent.action == DownloadService.ACTION_REVIVE) {
            // the alarm that checks downloads are still going: nothing to do if they finished
            if (!DownloadService.wasActive(context)) {
                pending.finish()
                return
            }
            DownloadService.armRevive(context.applicationContext, 60_000L)
            if (DownloadService.alive) { // still running: nothing to bring back
                pending.finish()
                return
            }
        }
        if (Core.library) {
            // Downloads that were running when the phone restarted carry on from where they stopped.
            thread(name = "tgvault-revive") {
                try {
                    Core.start(context.applicationContext)
                    DownloadService.start(context.applicationContext)
                } catch (e: Exception) {
                    Log.e("tgvault", "couldn't resume the downloads", e)
                } finally {
                    pending.finish()
                }
            }
            return
        }
        thread(name = "tgvault-revive") {
            try {
                Core.start(context.applicationContext)
                BackupWorker.schedule(context.applicationContext)
                WatchService.start(context.applicationContext)
            } catch (e: Exception) {
                Log.e("tgvault", "couldn't restore the backup schedule", e)
            } finally {
                pending.finish()
            }
        }
    }
}
