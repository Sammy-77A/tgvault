# Previews, part 2: thumbnails, looking inside ZIPs, videos that don't play

Status: part 1 (thumbnails) implemented 2026-10-27; part 2 (ZIPs and projects) implemented 2026-10-29; part 3 not started.

Part 1 turned out simpler than planned. The first step found that Telegram
already makes thumbnails for every stored picture (JPEG, PNG, WebP, GIF) and
`.mp4` video, even when sent as files, and none for `.mkv` or music. So
nothing is made or uploaded during backups, and old backups have thumbnails
on every device. Pictures Telegram skipped are shrunk locally when the
original is still on the device. Whether each file has a Telegram thumbnail
is learned when it's first shown (remembered for a few hours when it has
none), so there is no new database column. Video thumbnails for `.mkv` wait
for ffmpeg (part 3).

These are the three items the first previews plan left out. Each part can be
built and shipped on its own. The suggested order is at the end.

---

## 1. Thumbnails in the file list

### Goal

Pictures and videos in Cloud files show a small picture instead of a generic
icon, on the PC and the phone, so you can tell photos apart without opening
each one. A **Grid** view shows a folder of photos as tiles.

### Where thumbnails come from

| file | how its thumbnail is made |
|---|---|
| A new picture backed up from now on | the device that backs it up shrinks it (longest side 320 px, JPEG, under 200 KB, which is Telegram's limit) and sends it with the file as the message's thumbnail. Telegram keeps it next to the file. |
| A new video, phone | Android takes a frame from it (`MediaMetadataRetriever`, built into the phone) and sends it the same way. |
| A new video, PC | a frame taken with ffmpeg once the video converter (part 3) is installed. Until then, PC videos get no thumbnail. |
| Pictures backed up before this | 1. If Telegram made its own thumbnail for the file, that one is used. 2. If not, and the file is still on this device, it's made from the local copy. 3. If not, and the picture is under 5 MB, the picture is fetched once and shrunk. 4. Otherwise the plain icon stays. |
| Old videos | made from the local copy where there is one (phone; PC with ffmpeg); otherwise the plain icon. |

Telegram's thumbnails can't be added to files that are already in the
channel without uploading those files again, so for old backups, cases 2 and
3 are kept on that device only. New backups carry their thumbnail in
Telegram, so every device sees it.

Pictures are shrunk with Go's own image code (JPEG, PNG, GIF, plus WebP and
BMP from `golang.org/x/image`). Phone photos are turned upright using their
EXIF orientation, so they don't show sideways. HEIC photos get a thumbnail
on the phone (Android decodes them) and, once ffmpeg is installed, on the
PC (see part 3).

### Fetching and keeping them

- Telegram hands out a message's thumbnail as a tiny file of its own
  (`upload.getFile` on the document's thumbnail, usually 5 to 20 KB), so
  showing a folder of photos costs a few hundred KB, not the photos.
- Whether a file has a Telegram thumbnail is read from the message list the
  app already fetches. That is stored in a new `thumb` column on `versions`,
  so no extra calls are needed to find out.
- Only rows that are on screen load their thumbnail, at most 4 at a time.
  Scrolling past a folder quickly doesn't queue hundreds of downloads.
- Thumbnails are cached on disk by file fingerprint (SHA-256), so the same
  photo in two places or versions is fetched once. The cache is capped
  (PC 200 MB, phone 50 MB) and the oldest entries are dropped first. It can
  be cleared in Settings.
- Making thumbnails while backing up adds a little work per picture (well
  under a second for a phone photo). It is skipped for pictures over 50 MB.
  If it fails, the file is still backed up, just without a thumbnail.

### Screen

- List view: a 40 px thumbnail at the start of each row for pictures and
  videos. Other files keep their icon.
- Grid view: a toggle next to the sort menu, remembered per device. Tiles
  keep the same selection rules as rows: tap or click to select, Shift or
  Ctrl for ranges, long-press on the phone, View or double-click to open.
- In the preview viewer, the thumbnail shows at once while the full picture
  loads.

### Code changes

| where | change |
|---|---|
| `internal/thumb` (new) | shrink a picture to a JPEG thumbnail; EXIF orientation |
| `internal/tgclient/transfer.go` | upload the thumbnail with the file (`InputMediaUploadedDocument.Thumb`); `ReadThumb(msgID)`; report whether a message has a thumbnail |
| `internal/backup` | make thumbnails during backup; `Thumbnail(version)` following the order in the table; disk cache |
| `internal/store` | `thumb` column on `versions` |
| `internal/web` | `GET /api/thumb?msg=` (always `image/jpeg`, the same safety headers as previews) |
| `mobile`, `Core.kt` | `VideoFrame(path)` and HEIC decoding through the phone bridge |
| `app.js`, `style.css` | thumbnails in rows, grid view, lazy loading |

### First step: check what Telegram already has

Files are sent with "send as file" set. It isn't certain that Telegram makes
its own thumbnails for pictures sent that way. The first step reads a few
existing photo messages from inside the running app to find out. If it
does, old photos get thumbnails for free on every device; if not, cases 2 to
4 above cover them.

### Testing

- Unit tests: shrinking each image type, a sideways JPEG turned upright, a
  broken image doesn't stop the backup, cache cap and clearing.
- Live: a folder of phone photos on the PC and the phone, in list and grid;
  a new photo backed up from the phone shows its thumbnail on the PC; a new
  video from the phone; range selection in grid view (sel2.js extended); the
  number of Telegram requests while scrolling a large folder.

---

## 2. Looking inside ZIPs and code projects

### Goal

Open a ZIP in Cloud files like a folder: see what's in it, preview a file
inside, and download only the files you need. The same works for code
projects (which tgvault stores as ZIPs), including **restoring a single file
or folder from a project** instead of the whole project.

### How it works

A ZIP keeps its table of contents at the end of the file. The preview reader
from part 1 can already read any part of a stored file, so:

- **Listing** a ZIP reads only its table of contents: usually one or two
  1 MB blocks, a few MB for a ZIP with tens of thousands of files. A 4 GB ZIP
  stored in parts works the same.
- **Opening one file inside** reads only that file's bytes from the ZIP and
  unpacks them on the fly.
- **Projects** are a full ZIP plus change ZIPs. The listing reads the table
  of contents of each ZIP in the chain and applies them in order (changed
  files replace older ones, deleted files are removed), giving the project as
  it was at that version. Each file is then read from whichever ZIP in the
  chain holds its latest copy, so nothing has to be rebuilt.
- Listings are kept in memory while you browse, so going in and out of
  folders inside a ZIP doesn't fetch anything again.
- ZIPs on this device's disk are read from disk, as previews are.

### Screen

- A ZIP or project row gets an **Open** button. Clicking it, or
  double-clicking the row, goes inside. The path bar shows where you are, e.g.
  `Documents › backups.zip › photos 2019`, and Back or the path bar leads out
  again.
- Inside, files have **View** (the same viewer: ←/→, swipe, back button) and
  **Download**. Several selected files or a folder download as one ZIP made
  from the original compressed data, without packing them again.
- Projects also get **Restore**: puts the chosen files or folders back into
  the project on its device, or elsewhere as today, leaving the rest of the
  project untouched.
- Old versions: the Versions dialog gets an Open button, to look inside an
  older version of a ZIP or project.

### Limits

- Files stored without compression inside the ZIP (common for photos and
  videos in phone exports) can be seeked freely. Compressed files have to be
  unpacked from their start, so jumping far ahead in a compressed video
  inside a ZIP is slow; the viewer says so.
- Password-protected ZIPs: the list shows, files inside show a lock and can't
  be opened.
- Only ZIP for now. 7z, RAR and .tar.gz don't keep a table of contents in a
  form that can be read in pieces (or need extra libraries); those stay
  download-only.
- ZIPs inside ZIPs aren't opened.

### Safety

- Names inside a ZIP are cleaned before they are used: no `..`, no drive
  letters or absolute paths, so a restore can't write outside its folder.
- Files from inside a ZIP are served with the same content-type allow-list,
  `nosniff` and sandbox rules as other previews. An HTML file inside a ZIP is
  still plain text.
- A file can't unpack to more than its stated size, and text previews stop
  at 512 KB, so a "ZIP bomb" can't fill memory or disk.

### Code changes

| where | change |
|---|---|
| `internal/backup/preview.go` | `ReadAt` on the block reader (Go's `archive/zip` needs it) |
| `internal/backup/zipview.go` (new) | `ZipList(version, dir)`, `ZipOpen(version, name)`; project chains merged from each ZIP's table of contents and deleted list; listing cache |
| `internal/backup/chain.go` | restore of chosen paths from a chain |
| `internal/web` | `GET /api/zip?msg=&dir=`; previews and downloads take `&entry=`; bulk actions for files inside a ZIP |
| `app.js`, `style.css` | going inside ZIPs, path bar, Open in Versions |

No database changes and nothing new in Telegram.

### Testing

- Unit tests with the fake Telegram: listing reads only the end of the file;
  stored and compressed entries; a ZIP split across parts; ZIP64 (over 4 GB
  or 65,535 files); bad names cleaned; a ZIP bomb stopped; a project chain
  with changes and deletions lists and restores the right files.
- Live: a real ZIP, a big split one, the Kilimalll project at its latest
  version and an older one (one file restored to a temporary folder and
  compared), on the PC and the phone.

---

## 3. Videos and photos the browser can't show

### What's going on

Some files don't play or show in the browser or the phone's WebView, for
example the Smallville `.mkv` episodes (x265/HEVC video; sound plays, no
picture), and HEIC photos on the PC. The reason differs from file to file:

- the **container** isn't supported (`.mkv`, `.avi`), even when the video
  inside it is fine;
- the **video codec** isn't decoded (HEVC/x265 depends on the browser and
  the graphics chip; old DivX/Xvid never plays);
- the **audio codec** isn't decoded (AC3, DTS, common in films).

Your PC's graphics chip (Intel UHD, i5-1035G1) can decode HEVC in hardware,
and Windows has the HEVC and HEIF extensions installed. Whether Brave uses
the chip for HEVC couldn't be checked from my test browser, which has no
graphics chip. The app checks this in your Brave at run time (see below).

### The plan, from cheapest to most work

**A. Play in VLC (PC). Small.** VLC is installed and plays all of these. A
**Play in VLC** button in the viewer starts VLC on a link to the preview, so
it streams from disk or Telegram and seeks like the browser does, with no
download first. VLC can't send the app's login cookie, so the link carries a
one-time key that works only for that file, only from this PC, and expires
after 6 hours. If VLC isn't installed, the button becomes **Open with…**,
which downloads to a temporary folder and opens the file in the Windows
default app (the same as the phone's Open with…).

**B. Built-in player for the phone. Medium.** The phone app gets a native
video player (Google's Media3 ExoPlayer, Apache-licensed, adds about 2 MB to
the app). It plays MKV, HEVC (phones decode it in hardware), subtitles in
the file, and seeking, straight from the same preview stream. The viewer
uses it automatically when the WebView can't play a video. Audio codecs the
phone doesn't have (DTS on most phones) still won't play; the player says
so and offers Open with…. HEIC photos on the phone are converted to JPEG by
Android for the viewer.

**C. Converting on the fly in the PC browser. Large, optional.** For playing
in the browser without VLC, using ffmpeg:

- **Getting ffmpeg:** not shipped in the repo or the app. Settings gets
  "Video converter: Install (about 100 MB download)". It downloads a pinned
  ffmpeg build for Windows from its official build site, checks its SHA-256,
  and keeps it in `%LOCALAPPDATA%\tgvault\ffmpeg`. An ffmpeg already on PATH
  is used if present. ffmpeg runs as a separate program, so its licence is no
  issue for tgvault.
- **Looking first:** `ffprobe` reads the start of the file (through the
  preview stream, so a few MB) and says what's inside. The browser is asked
  whether it can decode that video codec (`MediaCapabilities`), which also
  settles the HEVC question above for your Brave.
- **Repacking when possible:** if the browser can decode the video and only
  the container or audio is the problem, the video is copied into a streaming
  MP4 unchanged and only the audio is converted to AAC. This is cheap: barely
  any CPU, starts in a second or two.
- **Converting when needed:** otherwise the video is converted to H.264 using
  the Intel chip's video encoder (Quick Sync), falling back to Windows' own
  encoder. It is limited to 1080p and one conversion at a time, and stops as
  soon as the viewer closes. On your laptop, hardware conversion of a 1080p
  episode should keep up with playback; this gets measured during the build.
- **Seeking:** the viewer gets its own seek bar for converted videos.
  Jumping restarts ffmpeg at that point (`-ss`), reading only that part of
  the file from disk or Telegram.
- **Subtitles** inside MKVs are converted to WebVTT and can be turned on in
  the viewer.
- **HEIC photos on the PC** are converted to JPEG for the viewer. Once ffmpeg
  is installed, PC thumbnails for videos (part 1) come from it too.

Nothing is written to disk while converting; the output is streamed to the
browser.

### Code changes

| where | change |
|---|---|
| `internal/web/preview.go` | one-time file keys for VLC and ffmpeg; `GET /api/convert?msg=&t=` streaming MP4; `GET /api/subs` |
| `internal/convert` (new) | find or install ffmpeg (pinned URL and hash), probe, choose repack or convert, start, stop |
| `internal/web/platform_windows.go` | find VLC; open a file with the default app |
| `android` | Media3 player activity; HEIC to JPEG in `Core.kt` |
| `app.js`, `style.css` | Play in VLC, Open with… on the PC, converted-video seek bar, subtitles toggle |

### Testing

- Unit tests: picking repack or convert from probe results, one-time keys
  (wrong file, expired, used from elsewhere), ffmpeg stopped when the viewer
  closes.
- Live: a Smallville episode in VLC, in the phone player, and repacked or
  converted in Brave with seeking; a file with AC3 audio; an MKV with
  subtitles; a HEIC photo on both devices; CPU use during conversion.

---

## Suggested order

1. **Thumbnails (part 1).** It changes what new backups send to Telegram
   (each new picture carries its thumbnail), so it's worth doing first: a
   photo backed up before it lands may only get a thumbnail on the device
   that has it.
2. **ZIPs and projects (part 2).** No new dependencies, nothing changes in
   Telegram, and it brings single-file restore from projects, which saves
   rebuilding the whole Kilimalll project to get one file back.
3. **Videos (part 3)** in its own order: A (VLC) and B (phone player) first,
   since they cover almost every file; C (conversion in the browser) only if
   you still want it after using A and B.

## Not included

- Converting files for keeping (this only converts for watching).
- Thumbnails for documents (first page of a PDF or Word file).
- Archive formats other than ZIP.
