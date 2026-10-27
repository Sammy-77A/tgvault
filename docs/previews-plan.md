# File previews

Status: approved 2026-10-27 and implemented. Built differently from the plan: text is read through the same `/api/preview` endpoint with a Range header (no separate text endpoint).

## Goal

Look at a backed-up file without restoring or downloading it first: see a
photo, play a song or video (and jump around in it), read a PDF or a text
file. Works in Cloud files on the PC and the phone, for single files and for
files over 2 GB stored in parts.

## What can be previewed

| type | how it's shown | examples |
|---|---|---|
| Images | full picture, fitted to the screen, tap/click to zoom | jpg, png, gif, webp, bmp |
| Video | player with seek bar; playback starts before the whole file is fetched | mp4, webm, mov (most), m4v |
| Audio | player with seek bar | mp3, m4a, aac, ogg, opus, wav, flac |
| PDF | PC: the browser's own PDF viewer in a new tab. Phone: WebView can't show PDFs, so it opens in a PDF app (see below) | pdf |
| Text and code | first 512 KB as plain text, with a note if there is more | txt, md, log, csv, json, xml, html, svg (as text), go, js, py, … |
| Anything else | an info card (name, size, date, versions, where it's from) with Download, and on the phone **Open with…** | docx, xlsx, apk, zip, project ZIPs |

Whether a video or audio file plays depends on the codecs the browser or
phone supports (for example, some .mkv and .avi files won't). When playback
fails, the viewer says so and offers Download / Open with… instead of a black
box.

## How you open it

- A **View** button on each file row, next to Download and Restore.
- PC: double-click a file, or press Enter on it.
- Single click and phone tap keep selecting, exactly as now, so the range
  selection you just got doesn't change.
- From the Versions dialog, each older version gets a View button too.

Inside the viewer:

- ← / → (PC) or swipe (phone) moves to the previous / next file in the
  current list, in the order shown (so sorting and search results work).
- Download, Versions and Close buttons; Esc or the phone's back button closes.
- The file's name, size and date stay visible at the top.

## Where the bytes come from

1. **From this device's disk, when possible.** If the file is still here and
   unchanged (same size and modified time as the backup), the preview reads
   it straight from disk: instant, no Telegram traffic. On your slow link this
   matters most.
2. **Otherwise from Telegram, streamed.** Only the bytes being looked at are
   fetched. Opening a 2 GB film and jumping to the middle fetches a few MB
   around that point, not the whole file.

### Streaming from Telegram

- A new endpoint `GET /api/preview?msg=<version id>` that understands HTTP
  `Range` requests (what video and audio players use to seek). It answers
  `206 Partial Content` with just the requested bytes.
- Telegram hands files out in 1 MB blocks at 1 MB boundaries. A request for
  bytes 5,300,000–5,400,000 fetches block 5 and returns the slice needed.
- A small in-memory cache (about 32 MB of recent blocks) stops the player's
  repeated small requests from fetching the same block twice. Nothing is
  written to disk, so previews never fill up the phone.
- Open-ended requests (`bytes=0-`) are fed block by block only as fast as
  the player reads, and stop as soon as the player stops or the viewer
  closes. Closing the viewer cancels the Telegram download.
- Split files: a byte position is mapped to its part and the offset inside
  that part (parts are 1900 MiB, a whole number of 1 MB blocks, so a block
  never straddles two parts). Seeking across parts just works.
- Telegram's file references expire after a while; the stored location is
  refreshed and the request retried once when that happens.

### Checking the bytes

Restores and downloads keep checking the SHA-256, as now. A preview that
reads only part of a file can't check the whole-file hash, so it doesn't.
When a preview does read a file from start to finish in one go (images, text),
the hash is checked and a mismatch cuts the response off, so a corrupt image
shows as broken rather than wrong.

## Phone specifics

- **Video full screen:** the WebView needs a small addition
  (`WebChromeClient.onShowCustomView`) so the full-screen button works and
  rotates properly.
- **PDFs and other files: Open with…** downloads the file into the app's
  cache (as Share does today), then hands it to Android with `ACTION_VIEW`, so
  your PDF reader, Office app, etc. opens it. The cache copy is deleted the
  next time Open with… or Share is used, as Share does now.
- Previews don't change the background rules: the app is in the foreground
  while you look, so it stays connected; Wi-Fi only / charging only apply to
  backups, not to things you open yourself. Videos stream only when you press
  play, and the viewer shows the file size before you do.

## Safety

The app's own page runs on the same address that serves the previews, and
that page can do everything (delete backups, log out). A backed-up HTML or
SVG file must never be able to run script there. So the preview endpoint:

- serves only an allow-list of content types (images, audio, video, PDF);
  everything else, including HTML, SVG, XML and JavaScript, is sent as
  `text/plain`;
- sends `X-Content-Type-Options: nosniff` and a
  `Content-Security-Policy: sandbox` header, so even opening a preview URL
  directly in a tab can't run script or read the app's cookie;
- PDFs are the one exception to `sandbox` (Chrome/Edge refuse to show PDFs in
  a sandboxed page); their viewer runs PDF scripts in its own isolated
  process, not in the app's page;
- needs the same login cookie as every other API call; only 127.0.0.1 (PC)
  or the app itself (phone) can reach it, as now.

## Code changes

| where | change |
|---|---|
| `internal/tgclient/transfer.go` | `DownloadRange(ctx, msgID, offset, length, w)` using `upload.getFile` with 1 MB aligned blocks; file-location cache with refresh on expiry |
| `internal/backup/preview.go` (new) | `OpenPreview(version)` returns an `io.ReadSeeker`-style reader: local file when unchanged, else Telegram blocks (single or split); block cache |
| `internal/web/server.go` | `GET /api/preview` using `http.ServeContent` (gives Range, 206, If-Range for free), content-type allow-list, safety headers; `GET /api/preview-text` for the first 512 KB |
| `internal/web/ui/app.js`, `style.css` | viewer overlay, View buttons, double-click/Enter, ←/→, swipe, Versions → View |
| `android/.../MainActivity.kt` | full-screen video; Open with… |
| `mobile/mobile.go`, `Core.kt` | `OpenWith(path)` bridge, like Share |
| README | short "Previews" section |

No database changes. No change to what is stored in Telegram, and older
builds aren't affected.

## Testing

- **Unit tests** with the fake Telegram: range reads at the start, middle,
  end and across a block edge; reads across a split-file part edge; local
  file used when unchanged and not used when changed; cache reuse; cancelled
  read stops fetching; HTML/SVG served as text with the safety headers.
- **Live, on the PC and the phone:** a photo, a song (seek), a video (seek to
  the middle and near the end, full screen on the phone), a PDF, a text file,
  a file that is only in Telegram (not on that device), and part of a split
  file. Nothing is changed in Telegram by previewing, so no clean-up is
  needed.

## Order of work

1. Streaming core (Telegram range reads, split mapping, cache) + tests.
2. Preview endpoint + safety headers + tests.
3. PC viewer.
4. Phone: viewer in the WebView, full-screen video, Open with….
5. Live test on both devices, README, commit, phone install.

## Not included (can follow later)

- **Thumbnails in the file list.** Telegram makes small thumbnails for many
  images it stores, so photo thumbnails are possible; videos would need
  frame extraction, which means shipping ffmpeg. Worth a separate small plan
  once previews are in.
- Looking inside ZIPs and project ZIPs (listing and opening single files).
- Converting formats the device can't play (HEIC photos on the PC, some
  .mkv/.avi videos).
- Editing or rotating files.
