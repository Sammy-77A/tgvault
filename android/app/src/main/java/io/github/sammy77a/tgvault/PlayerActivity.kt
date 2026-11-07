package io.github.sammy77a.tgvault

import android.app.Activity
import android.app.AlertDialog
import android.app.PictureInPictureParams
import android.content.Context
import android.content.Intent
import android.content.pm.ActivityInfo
import android.content.pm.PackageManager
import android.graphics.Color
import android.graphics.Typeface
import android.media.AudioManager
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Rational
import android.util.TypedValue
import android.view.Gravity
import android.view.GestureDetector
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.view.WindowManager
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.MediaMetadata
import androidx.media3.common.MimeTypes
import androidx.media3.common.PlaybackException
import androidx.media3.datasource.DefaultDataSource
import androidx.media3.datasource.DefaultHttpDataSource
import androidx.media3.exoplayer.source.DefaultMediaSourceFactory
import androidx.media3.exoplayer.upstream.DefaultLoadErrorHandlingPolicy
import io.github.anilbeesetti.nextlib.media3ext.ffdecoder.NextRenderersFactory
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.CaptionStyleCompat
import androidx.media3.ui.PlayerView
import java.net.HttpURLConnection
import java.net.URL
import kotlin.concurrent.thread
import kotlin.math.abs
import org.json.JSONArray
import org.json.JSONObject

/**
 * Plays videos from tgvault's own server with the phone's media decoders.
 *
 * Two ways in: a single link (the page's preview of a stored file), or a
 * playlist (the movie app: downloaded episodes in order, with subtitles, where
 * playback left off, and a way to search for subtitles). The page's video
 * element can't play everything (MKV, x265/HEVC); this can, and it seeks by
 * asking for parts of the file.
 */
@OptIn(UnstableApi::class)
class PlayerActivity : Activity() {
    private var player: ExoPlayer? = null
    private lateinit var view: PlayerView
    private lateinit var title: TextView
    private lateinit var overlay: LinearLayout
    private lateinit var hud: TextView
    private val ui = Handler(Looper.getMainLooper())

    private var single: String? = null          // a plain link
    private var listUrl: String? = null         // the movie app's playlist
    private var items: List<Item> = emptyList()
    private var startIndex = 0
    private var position = 0L
    private var index = 0
    private var playing = true
    private var lang = "en"
    private var resize = 0
    private var locked = false
    private var subShiftMs = 0L
    private val extraSubs = HashMap<Int, MutableList<Sub>>() // subtitles found online, by item

    private class Sub(val url: String, val lang: String, val label: String, val mime: String)
    private class Item(val path: String, val name: String, val url: String, val subs: List<Sub>, val pos: Long)

    companion object {
        /** True while the player is on screen (the main page waits for it before closing itself). */
        @Volatile var showing = false
        private val RESIZES = intArrayOf(
            AspectRatioFrameLayout.RESIZE_MODE_FIT,
            AspectRatioFrameLayout.RESIZE_MODE_ZOOM,
            AspectRatioFrameLayout.RESIZE_MODE_FILL,
        )
        private val RESIZE_NAMES = arrayOf("Fit", "Zoom to fill", "Stretch")
        private val SIZES = floatArrayOf(0.04f, 0.0533f, 0.07f, 0.09f)
        private val SIZE_NAMES = arrayOf("Small", "Medium", "Large", "Extra large")
        private val COLORS = intArrayOf(Color.WHITE, Color.YELLOW, Color.rgb(120, 255, 160), Color.CYAN)
        private val COLOR_NAMES = arrayOf("White", "Yellow", "Green", "Cyan")
        private val BACKS = intArrayOf(Color.TRANSPARENT, Color.argb(140, 0, 0, 0), Color.BLACK)
        private val BACK_NAMES = arrayOf("None", "Shaded", "Solid")
    }

    private fun prefs() = getSharedPreferences("player", Context.MODE_PRIVATE)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val plist = intent.getStringExtra("plist")
        val link = intent.getStringExtra("url")
        // Only links to tgvault's own server on this phone.
        val ok = listOfNotNull(plist, link).all { Uri.parse(it).host == "127.0.0.1" }
        if ((plist == null && link == null) || !ok) {
            finish()
            return
        }
        single = link
        listUrl = plist
        position = savedInstanceState?.getLong("position") ?: 0L
        index = savedInstanceState?.getInt("index") ?: -1
        lang = intent.getStringExtra("lang") ?: "en"
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        // Films are watched sideways; the rotate button turns it.
        if (plist != null) requestedOrientation = ActivityInfo.SCREEN_ORIENTATION_SENSOR_LANDSCAPE

        view = PlayerView(this).apply {
            setBackgroundColor(Color.BLACK)
            setShowSubtitleButton(true)
            setShowNextButton(true)
            setShowPreviousButton(true)
            controllerShowTimeoutMs = 3500
            resizeMode = RESIZES[0]
        }
        applySubtitleStyle()
        title = TextView(this).apply {
            setTextColor(Color.WHITE)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 15f)
            maxLines = 1
            ellipsize = android.text.TextUtils.TruncateAt.END
            layoutParams = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f)
        }
        overlay = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setBackgroundColor(Color.argb(110, 0, 0, 0))
            setPadding(dp(14), dp(6), dp(8), dp(6))
            addView(title)
            if (plist != null) {
                addView(chip("Aspect") { cycleAspect() })
                addView(chip("Rotate") { rotate() })
                addView(chip("Lock") { toggleLock() })
                addView(chip("CC+") { findSubtitles() })
                addView(chip("Style") { subtitleStyleDialog() })
            }
            if (pipOk) addView(chip("PiP") { enterPip() })
            addView(chip("Close") { finish() })
        }
        hud = TextView(this).apply {
            setTextColor(Color.WHITE)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 22f)
            setBackgroundColor(Color.argb(150, 0, 0, 0))
            setPadding(dp(18), dp(10), dp(18), dp(10))
            gravity = Gravity.CENTER
            visibility = View.GONE
        }
        val root = FrameLayout(this)
        root.addView(view, FrameLayout.LayoutParams(-1, -1))
        root.addView(overlay, FrameLayout.LayoutParams(-1, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.TOP))
        root.addView(hud, FrameLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.CENTER))
        setContentView(root)
        view.setControllerVisibilityListener(PlayerView.ControllerVisibilityListener { vis -> overlay.visibility = vis })
        installGestures()
    }

    private fun dp(n: Int) = (n * resources.displayMetrics.density).toInt()

    private fun chip(text: String, click: () -> Unit) = TextView(this).apply {
        this.text = text
        setTextColor(Color.WHITE)
        setTextSize(TypedValue.COMPLEX_UNIT_SP, 13f)
        typeface = Typeface.DEFAULT_BOLD
        setPadding(dp(10), dp(8), dp(10), dp(8))
        setOnClickListener { click() }
    }

    // ---------- loading ----------

    override fun onStart() {
        super.onStart()
        showing = true
        if (items.isNotEmpty() || single != null) {
            startPlayer()
            return
        }
        val u = listUrl ?: return
        thread(name = "player-list") {
            try {
                val j = JSONObject(httpGet(u))
                val arr = j.getJSONArray("items")
                val list = ArrayList<Item>()
                for (i in 0 until arr.length()) {
                    val o = arr.getJSONObject(i)
                    list.add(Item(o.getString("path"), o.getString("name"), o.getString("url"), parseSubs(o.optJSONArray("subs")), o.optLong("pos")))
                }
                val l = j.optString("lang")
                runOnUiThread {
                    items = list
                    if (l.isNotEmpty()) lang = l
                    startIndex = j.optInt("start")
                    if (index < 0) index = startIndex
                    startPlayer()
                }
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(this, "Couldn't open that: ${e.message}", Toast.LENGTH_LONG).show()
                    finish()
                }
            }
        }
    }

    private fun parseSubs(a: JSONArray?): List<Sub> {
        val out = ArrayList<Sub>()
        if (a == null) return out
        for (i in 0 until a.length()) {
            val s = a.getJSONObject(i)
            out.add(Sub(s.getString("url"), s.optString("lang"), s.optString("label", "Subtitles"), s.optString("mime", MimeTypes.TEXT_VTT)))
        }
        return out
    }

    private fun mediaItemOf(i: Int): MediaItem {
        val it = items[i]
        val subs = ArrayList<MediaItem.SubtitleConfiguration>()
        val all = it.subs + (extraSubs[i] ?: emptyList<Sub>())
        // The first one in the chosen language is on from the start.
        val firstPick = all.indexOfFirst { s -> s.lang == lang }.let { p -> if (p >= 0) p else 0 }
        all.forEachIndexed { n, s ->
            val shifted = if (subShiftMs != 0L) s.url + (if ('?' in s.url) "&" else "?") + "shift=$subShiftMs" else s.url
            subs.add(
                MediaItem.SubtitleConfiguration.Builder(Uri.parse(shifted))
                    .setMimeType(s.mime)
                    .setLanguage(s.lang.ifEmpty { null })
                    .setLabel(s.label)
                    .setSelectionFlags(if (n == firstPick) C.SELECTION_FLAG_DEFAULT else 0)
                    .build(),
            )
        }
        return MediaItem.Builder()
            .setUri(it.url)
            .setMediaMetadata(MediaMetadata.Builder().setTitle(it.name).build())
            .setSubtitleConfigurations(subs)
            .build()
    }

    private var software = false // software (FFmpeg) decoding first, after a phone decoder failed

    /** Starts the player again where it was, with software decoding first. */
    private fun restartPlayer() {
        player?.let {
            position = it.currentPosition
            index = it.currentMediaItemIndex
            playing = true
            it.release()
        }
        player = null
        view.player = null
        startPlayer()
    }

    /** Offers to open the current video in VLC (or any other player) when the built-in one can't decode it. */
    private fun offerOtherPlayer(positionMs: Long) {
        val url = items.getOrNull(index)?.url ?: single ?: return
        val name = items.getOrNull(index)?.name ?: title.text.toString()
        player?.pause()
        AlertDialog.Builder(this)
            .setTitle("This phone can't play it here")
            .setMessage("Neither your phone's decoder nor the built-in software decoder could play this video. VLC may manage it, and it still plays offline from the downloaded file.")
            .setPositiveButton("Open in VLC") { _, _ ->
                val view = Intent(Intent.ACTION_VIEW).setDataAndType(Uri.parse(url), "video/*")
                    .putExtra("title", name).putExtra("position", positionMs)
                try {
                    startActivity(Intent(view).setPackage("org.videolan.vlc"))
                } catch (e: Exception) {
                    try {
                        startActivity(Intent.createChooser(view, "Open with"))
                    } catch (e2: Exception) {
                        Toast.makeText(this, "Install VLC from the Play Store to play this video.", Toast.LENGTH_LONG).show()
                    }
                }
            }
            .setNegativeButton("Close", null)
            .show()
    }

    private fun startPlayer() {
        // The phone's own decoders come first (they are fast and easy on the battery). FFmpeg in
        // software covers what the phone has no decoder for; if the phone's decoder accepts a
        // video and then fails on it, software decoding goes first for the rest of this viewing.
        val factory = NextRenderersFactory(this)
            .setExtensionRendererMode(
                if (software) DefaultRenderersFactory.EXTENSION_RENDERER_MODE_PREFER else DefaultRenderersFactory.EXTENSION_RENDERER_MODE_ON,
            )
            .setEnableDecoderFallback(true)
        // A video that is still downloading is sent as fast as it arrives, so a request can wait a
        // while for data: wait longer than the usual few seconds, and try again more times.
        val http = DefaultHttpDataSource.Factory().setConnectTimeoutMs(10_000).setReadTimeoutMs(90_000)
        val sources = DefaultMediaSourceFactory(DefaultDataSource.Factory(this, http))
            .setLoadErrorHandlingPolicy(DefaultLoadErrorHandlingPolicy(12))
        val p = ExoPlayer.Builder(this, factory)
            .setMediaSourceFactory(sources)
            .setSeekBackIncrementMs(10_000)
            .setSeekForwardIncrementMs(10_000)
            .build()
        p.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                // A video this phone's decoders can't handle (10-bit HEVC is the usual one on
                // budget phones) can still be played by VLC, which decodes in software.
                if (error.errorCode == PlaybackException.ERROR_CODE_DECODING_FAILED ||
                    error.errorCode == PlaybackException.ERROR_CODE_DECODER_INIT_FAILED ||
                    error.errorCode == PlaybackException.ERROR_CODE_DECODING_FORMAT_EXCEEDS_CAPABILITIES ||
                    error.errorCode == PlaybackException.ERROR_CODE_DECODING_FORMAT_UNSUPPORTED
                ) {
                    if (!software) {
                        software = true
                        Toast.makeText(this@PlayerActivity, "Switching to software decoding for this video…", Toast.LENGTH_SHORT).show()
                        ui.post { restartPlayer() }
                    } else {
                        offerOtherPlayer(p.currentPosition)
                    }
                    return
                }
                Toast.makeText(
                    this@PlayerActivity,
                    "This can't be played here (${error.errorCodeName}).",
                    Toast.LENGTH_LONG,
                ).show()
                // In a playlist, carry on with the next one rather than stopping.
                if (p.hasNextMediaItem()) {
                    p.seekToNextMediaItem()
                    p.prepare()
                    p.play()
                }
            }

            override fun onMediaItemTransition(mediaItem: MediaItem?, reason: Int) {
                index = p.currentMediaItemIndex
                title.text = mediaItem?.mediaMetadata?.title ?: ""
            }
        })
        val single = single
        if (single != null) {
            p.setMediaItem(MediaItem.fromUri(single))
            p.seekTo(position)
        } else {
            // Decide where to start before loading: loading the list reports "item 0" to the listener.
            val at = index.coerceIn(0, items.size - 1)
            val resumeAt = if (position > 0) position else items[at].pos
            p.setMediaItems(items.indices.map { mediaItemOf(it) })
            p.seekTo(at, resumeAt)
            index = at
            title.text = items[at].name
        }
        // Subtitles: the one chosen language, shown from the start when there is one.
        p.trackSelectionParameters = p.trackSelectionParameters.buildUpon()
            .setPreferredTextLanguage(lang)
            .setPreferredAudioLanguage(null)
            .setSelectUndeterminedTextLanguage(true)
            .setIgnoredTextSelectionFlags(0)
            .build()
        p.playWhenReady = playing
        p.prepare()
        view.player = p
        player = p
        ui.post(saver)
    }

    // ---------- where I left off ----------

    private val saver = object : Runnable {
        override fun run() {
            saveProgress()
            ui.postDelayed(this, 10_000)
        }
    }

    private fun saveProgress() {
        val p = player ?: return
        val base = listUrl ?: return
        val i = p.currentMediaItemIndex
        val dur = p.duration
        if (dur <= 0 || dur == C.TIME_UNSET || i < 0 || i >= items.size) return
        val pos = p.currentPosition
        thread(name = "player-progress") {
            try {
                httpPost("$base/progress?i=$i&pos=$pos&dur=$dur", "")
            } catch (_: Exception) {
            }
        }
    }

    override fun onStop() {
        super.onStop()
        showing = false
        ui.removeCallbacks(saver)
        saveProgress()
        player?.let {
            position = it.currentPosition
            index = it.currentMediaItemIndex
            playing = it.playWhenReady
            it.release()
        }
        player = null
        view.player = null
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putLong("position", player?.currentPosition ?: position)
        outState.putInt("index", player?.currentMediaItemIndex ?: index)
    }

    // ---------- controls ----------

    private fun say(text: String) {
        hud.text = text
        hud.visibility = View.VISIBLE
        ui.removeCallbacks(hideHud)
        ui.postDelayed(hideHud, 900)
    }

    private val hideHud = Runnable { hud.visibility = View.GONE }

    private fun cycleAspect() {
        resize = (resize + 1) % RESIZES.size
        view.resizeMode = RESIZES[resize]
        say(RESIZE_NAMES[resize])
    }

    private fun rotate() {
        if (locked) toggleLock()
        val landscape = resources.configuration.orientation == android.content.res.Configuration.ORIENTATION_LANDSCAPE
        requestedOrientation = if (landscape) ActivityInfo.SCREEN_ORIENTATION_SENSOR_PORTRAIT else ActivityInfo.SCREEN_ORIENTATION_SENSOR_LANDSCAPE
    }

    private fun toggleLock() {
        locked = !locked
        requestedOrientation = if (locked) ActivityInfo.SCREEN_ORIENTATION_LOCKED else ActivityInfo.SCREEN_ORIENTATION_SENSOR_LANDSCAPE
        say(if (locked) "Rotation locked" else "Rotation unlocked")
    }

    /** Some phones don't offer picture-in-picture at all; then there's no button for it. */
    private val pipOk by lazy { packageManager.hasSystemFeature(PackageManager.FEATURE_PICTURE_IN_PICTURE) }

    private fun enterPip() {
        try {
            enterPictureInPictureMode(PictureInPictureParams.Builder().setAspectRatio(Rational(16, 9)).build())
        } catch (e: Exception) {
            Toast.makeText(this, "Picture-in-picture isn't available here", Toast.LENGTH_SHORT).show()
        }
    }

    override fun onUserLeaveHint() {
        super.onUserLeaveHint()
        if (pipOk && player?.isPlaying == true) enterPip()
    }

    override fun onPictureInPictureModeChanged(isInPictureInPictureMode: Boolean, newConfig: android.content.res.Configuration) {
        super.onPictureInPictureModeChanged(isInPictureInPictureMode, newConfig)
        view.useController = !isInPictureInPictureMode
        overlay.visibility = if (isInPictureInPictureMode) View.GONE else overlay.visibility
    }

    // Gestures: double-tap either side to skip 10 seconds; slide up and down on
    // the left for brightness and on the right for volume; one tap shows or
    // hides the controls.
    private fun installGestures() {
        val audio = getSystemService(Context.AUDIO_SERVICE) as AudioManager
        val detector = GestureDetector(this, object : GestureDetector.SimpleOnGestureListener() {
            override fun onDown(e: MotionEvent) = true

            override fun onSingleTapConfirmed(e: MotionEvent): Boolean {
                if (view.isControllerFullyVisible) view.hideController() else view.showController()
                return true
            }

            override fun onDoubleTap(e: MotionEvent): Boolean {
                val p = player ?: return true
                if (e.x < view.width / 2f) {
                    p.seekTo((p.currentPosition - 10_000).coerceAtLeast(0))
                    say("−10 s")
                } else {
                    p.seekTo(p.currentPosition + 10_000)
                    say("+10 s")
                }
                return true
            }

            override fun onScroll(e1: MotionEvent?, e2: MotionEvent, dx: Float, dy: Float): Boolean {
                if (e1 == null || abs(dy) < abs(dx)) return false
                val step = dy / view.height.toFloat() // up is positive
                if (e1.x < view.width / 2f) {
                    val lp = window.attributes
                    val cur = if (lp.screenBrightness < 0) 0.5f else lp.screenBrightness
                    lp.screenBrightness = (cur + step).coerceIn(0.02f, 1f)
                    window.attributes = lp
                    say("Brightness ${(lp.screenBrightness * 100).toInt()}%")
                } else {
                    val max = audio.getStreamMaxVolume(AudioManager.STREAM_MUSIC)
                    val cur = audio.getStreamVolume(AudioManager.STREAM_MUSIC)
                    val next = (cur + step * max * 1.6f).toInt().coerceIn(0, max)
                    if (next != cur) audio.setStreamVolume(AudioManager.STREAM_MUSIC, next, 0)
                    say("Volume ${next * 100 / max}%")
                }
                return true
            }
        })
        view.setOnTouchListener { _, ev -> detector.onTouchEvent(ev) }
    }

    // ---------- subtitles ----------

    private fun applySubtitleStyle() {
        val p = prefs()
        val v = view.subtitleView ?: return
        v.setApplyEmbeddedStyles(false)
        v.setApplyEmbeddedFontSizes(false)
        v.setFractionalTextSize(SIZES[p.getInt("size", 1).coerceIn(0, SIZES.size - 1)])
        v.setStyle(
            CaptionStyleCompat(
                COLORS[p.getInt("color", 0).coerceIn(0, COLORS.size - 1)],
                BACKS[p.getInt("back", 1).coerceIn(0, BACKS.size - 1)],
                Color.TRANSPARENT,
                CaptionStyleCompat.EDGE_TYPE_OUTLINE,
                Color.BLACK,
                Typeface.DEFAULT,
            ),
        )
    }

    private fun subtitleStyleDialog() {
        val p = prefs()
        fun pick(label: String, names: Array<String>, key: String, def: Int) {
            AlertDialog.Builder(this)
                .setTitle(label)
                .setSingleChoiceItems(names, p.getInt(key, def)) { d, which ->
                    p.edit().putInt(key, which).apply()
                    applySubtitleStyle()
                    d.dismiss()
                }
                .show()
        }
        val labels = arrayOf("Size", "Colour", "Background", "Timing: later (+0.5 s)", "Timing: earlier (−0.5 s)", "Timing: reset (now ${"%+.1f".format(subShiftMs / 1000.0)} s)")
        AlertDialog.Builder(this)
            .setTitle("Subtitle style")
            .setItems(labels) { _, which ->
                when (which) {
                    0 -> pick("Size", SIZE_NAMES, "size", 1)
                    1 -> pick("Colour", COLOR_NAMES, "color", 0)
                    2 -> pick("Background", BACK_NAMES, "back", 1)
                    3 -> shiftSubs(500)
                    4 -> shiftSubs(-500)
                    5 -> shiftSubs(-subShiftMs)
                }
            }
            .show()
    }

    /** Moves every subtitle in time: the files are served already shifted. */
    private fun shiftSubs(by: Long) {
        subShiftMs += by
        reloadCurrent()
        say("Subtitles ${"%+.1f".format(subShiftMs / 1000.0)} s")
    }

    private fun reloadCurrent() {
        val p = player ?: return
        if (items.isEmpty()) return
        val i = p.currentMediaItemIndex
        val pos = p.currentPosition
        val was = p.playWhenReady
        p.replaceMediaItem(i, mediaItemOf(i))
        p.seekTo(i, pos)
        p.playWhenReady = was
    }

    /** Searches the internet for subtitles for what's playing. */
    private fun findSubtitles() {
        val base = listUrl ?: return
        val i = player?.currentMediaItemIndex ?: return
        Toast.makeText(this, "Looking for subtitles…", Toast.LENGTH_SHORT).show()
        thread(name = "player-subs") {
            try {
                val j = JSONObject(httpGet("$base/subs?i=$i&lang=$lang"))
                val res = j.getJSONArray("results")
                runOnUiThread {
                    if (res.length() == 0) {
                        Toast.makeText(this, "No subtitles found for this", Toast.LENGTH_LONG).show()
                        return@runOnUiThread
                    }
                    val names = Array(res.length()) { n ->
                        val r = res.getJSONObject(n)
                        val hi = if (r.optBoolean("hearing")) " (hearing impaired)" else ""
                        r.optString("name").ifEmpty { "Subtitle ${n + 1}" } + hi
                    }
                    AlertDialog.Builder(this)
                        .setTitle("Choose subtitles")
                        .setItems(names) { _, which -> fetchSubtitle(base, i, res.getJSONObject(which)) }
                        .setNegativeButton("Cancel", null)
                        .show()
                }
            } catch (e: Exception) {
                runOnUiThread { Toast.makeText(this, e.message ?: "Couldn't search for subtitles", Toast.LENGTH_LONG).show() }
            }
        }
    }

    private fun fetchSubtitle(base: String, i: Int, r: JSONObject) {
        Toast.makeText(this, "Getting the subtitles…", Toast.LENGTH_SHORT).show()
        thread(name = "player-subget") {
            try {
                val body = JSONObject().put("download", r.getString("download")).put("lang", r.optString("lang", lang)).toString()
                val got = JSONObject(httpPost("$base/subget?i=$i", body))
                val sub = Sub(got.getString("url"), got.optString("lang"), got.optString("label", "Subtitles"), got.optString("mime", MimeTypes.TEXT_VTT))
                runOnUiThread {
                    extraSubs.getOrPut(i) { ArrayList() }.add(0, sub)
                    reloadCurrent()
                    say("Subtitles added")
                }
            } catch (e: Exception) {
                runOnUiThread { Toast.makeText(this, e.message ?: "Couldn't get that subtitle", Toast.LENGTH_LONG).show() }
            }
        }
    }

    // ---------- plumbing ----------

    private fun httpGet(u: String): String {
        val c = URL(u).openConnection() as HttpURLConnection
        c.connectTimeout = 15_000
        c.readTimeout = 30_000
        val text = (if (c.responseCode in 200..299) c.inputStream else c.errorStream)?.bufferedReader()?.use { it.readText() } ?: ""
        if (c.responseCode !in 200..299) throw Exception(JSONObject(text.ifEmpty { "{}" }).optString("error", "error ${c.responseCode}"))
        return text
    }

    private fun httpPost(u: String, body: String): String {
        val c = URL(u).openConnection() as HttpURLConnection
        c.requestMethod = "POST"
        c.connectTimeout = 15_000
        c.readTimeout = 60_000
        c.doOutput = true
        c.setRequestProperty("Content-Type", "application/json")
        c.outputStream.use { it.write(body.toByteArray()) }
        val text = (if (c.responseCode in 200..299) c.inputStream else c.errorStream)?.bufferedReader()?.use { it.readText() } ?: ""
        if (c.responseCode !in 200..299) throw Exception(JSONObject(text.ifEmpty { "{}" }).optString("error", "error ${c.responseCode}"))
        return text
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
