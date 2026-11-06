package io.github.sammy77a.tgvault

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat

object Notifications {
    private const val ALERTS = "alerts"
    private const val BACKUP = "backup"
    private const val WATCH = "watch"
    const val DOWNLOADS = "downloads"
    const val DOWNLOAD_ID = 3
    const val BACKUP_ID = 1
    const val WATCH_ID = 2
    private var nextId = 100

    fun createChannels(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(ALERTS, "Needs your attention", NotificationManager.IMPORTANCE_DEFAULT).apply {
                description = "Logged out, files failing for a day, a folder missing, or an update"
            },
        )
        nm.createNotificationChannel(
            NotificationChannel(WATCH, "Watching for new files", NotificationManager.IMPORTANCE_MIN).apply {
                description = "Always shown while tgvault keeps your backup going; hide it here if you like"
            },
        )
        nm.createNotificationChannel(
            NotificationChannel(DOWNLOADS, "Downloads", NotificationManager.IMPORTANCE_LOW).apply {
                description = "Progress of movies and shows being downloaded"
            },
        )
        nm.createNotificationChannel(
            NotificationChannel(BACKUP, "Backing up", NotificationManager.IMPORTANCE_LOW).apply {
                description = "Shown while tgvault uploads in the background"
            },
        )
    }

    fun watching(ctx: Context) = NotificationCompat.Builder(ctx, WATCH)
        .setSmallIcon(R.drawable.ic_notification)
        .setContentTitle("tgvault is watching for new files")
        .setContentText("Keeps your backup going in the background")
        .setContentIntent(openApp(ctx))
        .setOngoing(true)
        .setSilent(true)
        .setPriority(NotificationCompat.PRIORITY_MIN)
        .build()

    /** Opens the app, on [open] ("downloads" or "library") when given. */
    fun openApp(ctx: Context, open: String? = null): PendingIntent = PendingIntent.getActivity(
        ctx, if (open == null) 0 else open.hashCode(),
        Intent(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK).apply { if (open != null) putExtra("open", open) },
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    /** Shows a notification; tapping it opens tgvault, or [link] if given. */
    fun alert(ctx: Context, title: String, body: String, link: String? = null) {
        if (!Core.hasNotifications(ctx)) return
        val tap = if (link == null) {
            // the movie app's messages open the page they are about
            openApp(ctx, when {
                title.startsWith("Downloaded:") || title.startsWith("Download failed:") -> "downloads"
                title.startsWith("Ready to download:") || title.startsWith("Not available:") -> "library"
                else -> null
            })
        } else {
            PendingIntent.getActivity(ctx, 1, Intent(Intent.ACTION_VIEW, Uri.parse(link)), PendingIntent.FLAG_IMMUTABLE)
        }
        val n = NotificationCompat.Builder(ctx, ALERTS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            .setContentIntent(tap)
            .setAutoCancel(true)
            .build()
        try {
            NotificationManagerCompat.from(ctx).notify(nextId++, n)
        } catch (e: SecurityException) {
            // Notifications are off; the page shows the same problems.
        }
    }

    fun backingUp(ctx: Context) = NotificationCompat.Builder(ctx, BACKUP)
        .setSmallIcon(R.drawable.ic_notification)
        .setContentTitle("Backing up to Telegram")
        .setContentText("tgvault is uploading new and changed files")
        .setContentIntent(openApp(ctx))
        .setOngoing(true)
        .setSilent(true)
        .build()
}
