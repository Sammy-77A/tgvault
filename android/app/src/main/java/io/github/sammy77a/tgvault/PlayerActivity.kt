package io.github.sammy77a.tgvault

import android.app.Activity
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.view.View
import android.view.WindowManager
import android.widget.Toast
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView

/**
 * Plays a video or song from tgvault's own server with the phone's media
 * decoders. The page's video element can't play everything (MKV files,
 * x265/HEVC video); this can, and it seeks by asking for parts of the file.
 */
@OptIn(UnstableApi::class)
class PlayerActivity : Activity() {
    private var player: ExoPlayer? = null
    private lateinit var view: PlayerView
    private var url: String = ""
    private var position = 0L
    private var playing = true

    companion object {
        /** True while the player is on screen (the main page waits for it before closing itself). */
        @Volatile var showing = false
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val link = intent.getStringExtra("url")
        // Only links to tgvault's own server on this phone.
        if (link == null || Uri.parse(link).host != "127.0.0.1") {
            finish()
            return
        }
        url = link
        position = savedInstanceState?.getLong("position") ?: 0L
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        view = PlayerView(this).apply {
            setBackgroundColor(Color.BLACK)
            setShowSubtitleButton(true)
            controllerShowTimeoutMs = 3500
        }
        setContentView(view)
    }

    override fun onStart() {
        super.onStart()
        showing = true
        if (url.isEmpty()) return
        // A decoder that fails to start hands over to the next one that can play it.
        val p = ExoPlayer.Builder(this, DefaultRenderersFactory(this).setEnableDecoderFallback(true)).build()
        p.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                Toast.makeText(
                    this@PlayerActivity,
                    "This can't be played here (${error.errorCodeName}). In tgvault, Download or Open with… lets another app try.",
                    Toast.LENGTH_LONG,
                ).show()
            }
        })
        p.setMediaItem(MediaItem.fromUri(url))
        p.seekTo(position)
        p.playWhenReady = playing
        p.prepare()
        view.player = p
        player = p
    }

    override fun onStop() {
        super.onStop()
        showing = false
        player?.let {
            position = it.currentPosition
            playing = it.playWhenReady
            it.release()
        }
        player = null
        view.player = null
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putLong("position", player?.currentPosition ?: position)
    }

    @Suppress("DEPRECATION")
    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (hasFocus) {
            window.decorView.systemUiVisibility = View.SYSTEM_UI_FLAG_FULLSCREEN or
                View.SYSTEM_UI_FLAG_HIDE_NAVIGATION or View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
        }
    }
}
