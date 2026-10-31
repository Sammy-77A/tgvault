package io.github.sammy77a.tgvault

import android.app.Application
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.util.Log
import androidx.core.content.ContextCompat
import mobile.Mobile

/** Keeps tgvault told about Wi-Fi and charging while the app's process is alive. */
class TgvaultApp : Application() {
    private lateinit var cm: ConnectivityManager
    private var wifi: Network? = null
    private var default: Network? = null
    private var pinned: Network? = null

    override fun onCreate() {
        super.onCreate()
        Notifications.createChannels(this)
        cm = getSystemService(ConnectivityManager::class.java)
        cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                default = network
                pin()
                Core.update(this@TgvaultApp)
            }

            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) = Core.update(this@TgvaultApp)

            override fun onLost(network: Network) {
                if (default == network) default = null
                pin()
                Core.update(this@TgvaultApp)
            }
        })
        cm.registerNetworkCallback(
            NetworkRequest.Builder()
                .addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
                .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                .build(),
            object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) {
                    wifi = network
                    pin()
                    Core.update(this@TgvaultApp)
                }

                override fun onLost(network: Network) {
                    if (wifi == network) wifi = null
                    pin()
                    Core.update(this@TgvaultApp)
                }
            },
        )
        val power = IntentFilter().apply {
            addAction(Intent.ACTION_POWER_CONNECTED)
            addAction(Intent.ACTION_POWER_DISCONNECTED)
        }
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(context: Context, intent: Intent) = Core.update(context)
        }
        ContextCompat.registerReceiver(this, receiver, power, ContextCompat.RECEIVER_NOT_EXPORTED)
    }

    /**
     * Phones with Wi-Fi and mobile data both up (and a "Wi-Fi assistant" that
     * hops to mobile data when Wi-Fi is slow) can send connections out by either
     * network, and Telegram ends a login it sees from two addresses at once. The
     * whole app is pinned to one network: Wi-Fi when backups are Wi-Fi only,
     * otherwise whichever the phone is using.
     */
    @Synchronized
    fun pin() {
        val wifiOnly = try {
            Mobile.wifiOnly()
        } catch (e: Exception) {
            true
        }
        val target = if (wifiOnly && wifi != null) wifi else default
        if (target == pinned) return
        val old = pinned
        pinned = target
        Core.pinned = target
        cm.bindProcessToNetwork(target)
        Log.i("tgvault", "network: using ${target ?: "none"} (${describe(target)}); default is ${default ?: "none"}")
        // Reconnect so no old connection lingers on the previous network.
        if (old != null && target != null) Core.networkChanged()
    }

    private fun describe(network: Network?): String {
        val caps = network?.let { cm.getNetworkCapabilities(it) } ?: return "none"
        return when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "Wi-Fi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile data"
            else -> "other"
        }
    }
}
