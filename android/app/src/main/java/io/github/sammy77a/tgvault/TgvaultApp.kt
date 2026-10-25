package io.github.sammy77a.tgvault

import android.app.Application
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import androidx.core.content.ContextCompat

/** Keeps tgvault told about Wi-Fi and charging while the app's process is alive. */
class TgvaultApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Notifications.createChannels(this)
        val cm = getSystemService(ConnectivityManager::class.java)
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            private var current: Network? = null

            override fun onAvailable(network: Network) {
                // Moving to another network: reconnect so no old connection
                // lingers on the previous one (Telegram ends a login used from
                // two addresses at once).
                if (current != null && current != network) Core.networkChanged()
                current = network
                Core.update(this@TgvaultApp)
            }

            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) = Core.update(this@TgvaultApp)
            override fun onLost(network: Network) = Core.update(this@TgvaultApp)
        })
        val power = IntentFilter().apply {
            addAction(Intent.ACTION_POWER_CONNECTED)
            addAction(Intent.ACTION_POWER_DISCONNECTED)
        }
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) = Core.update(context)
        }
        ContextCompat.registerReceiver(this, receiver, power, ContextCompat.RECEIVER_NOT_EXPORTED)
    }
}
