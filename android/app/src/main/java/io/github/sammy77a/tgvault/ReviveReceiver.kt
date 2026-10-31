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
        thread(name = "tgvault-revive") {
            try {
                Core.start(context.applicationContext)
                BackupWorker.schedule(context.applicationContext)
            } catch (e: Exception) {
                Log.e("tgvault", "couldn't restore the backup schedule", e)
            } finally {
                pending.finish()
            }
        }
    }
}
