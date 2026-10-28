# tgvault

A lightweight backup app that keeps your folders safe in your own Telegram account. Written in Go. One small `.exe`, managed from a page in your browser.

> **Status:** early development (0.2). Windows and Android.

## How it works

- **One-way backup.** Files only go from your PC to Telegram. tgvault never deletes your local files on its own, and deleting a local file never deletes its Telegram copy. That's what lets you free up disk space safely.
- **Folders are kept.** Every upload carries its full path (for example `Documents/Taxes/2025/return.pdf`) in its Telegram caption, plus hashtags for each folder. tgvault shows your real folder tree and restores whole folders with every subfolder in place. Because the paths live in Telegram, a fresh install can rebuild the list from Telegram alone.
- **Code projects are one ZIP.** Folders with a `.git`, `package.json`, `composer.json`, `go.mod` and similar are offered as *projects*. Once you confirm, each project is backed up a few minutes after you stop editing: now and then as a full ZIP, and in between as small change ZIPs holding only what changed (see [Projects](#projects)). Rebuildable folders (`node_modules`, and `vendor`, `target`, `build`, virtualenvs when the project type makes that certain) are left out. `.git` and `.env` are kept.
- **No stuck syncs.** Each file is its own job. A file that fails retries on its own schedule with the reason shown next to it, and everything else keeps going. Telegram "slow down" requests only pause what they affect.
- **Versions.** Edited files are uploaded as new versions; the last 3 are kept. Every backup of a project is a version you can restore.
- **Free up space.** Deletes local copies only after re-checking that the Telegram copy exists and matches. The suggestions can be narrowed by type, size and the date a file was last modified (**Dates…**).
- **Restore never overwrites.** If a different file is already in the way, the restored copy gets "(restored)" in its name.

Files go straight between your PC and Telegram. There is no tgvault server, no ads and no telemetry.

## Requirements

- Your own Telegram API ID and API hash from [my.telegram.org](https://my.telegram.org) → *API development tools*
- Go 1.27+ to build from source

## Build and run

```
go build -o bin/tgvault.exe -ldflags "-H windowsgui -s -w" ./cmd/tgvault
bin\tgvault.exe
```

Your browser opens the tgvault page. Running `tgvault.exe` again while it's running just reopens the page. Options:

- `--background` start without opening the browser (used by "start with Windows")
- `--data DIR` where settings, login and logs live (default `%APPDATA%\tgvault`)

Run the tests with `go test ./...`.

## Android

The Android app backs up the phone into the same Telegram backup. It runs the same Go code as the PC, shown full-screen in the app.

- **Install:** open the [android-test release](https://github.com/Sammy-77A/tgvault/releases/tag/android-test) on the phone and tap `tgvault-test.apk`. Android asks to allow installing from your browser the first time. The app checks for updates once a day.
- **Log in from the PC.** A phone can't scan its own login QR code, and Telegram's login codes don't always arrive. So while the phone is logged out it shows an address and a 6-digit code; on the PC open tgvault, *Settings → Log in a phone*, and enter them (same Wi-Fi). The PC sends its API keys and approves the login.
- **Permissions, and why:**
  - *All files access* to read the folders you choose and to restore into them.
  - *Run in background* so Android doesn't keep postponing backups.
  - *Notifications* for the few things that need you.
- **What's backed up:** you pick the folders; tgvault suggests Camera, Screenshots, Documents, Download, Recordings and WhatsApp media. They go under `Phone/` in Telegram (`Phone/Camera/IMG_2031.jpg`), separate from the PC's folders. Telegram's own download folders aren't suggested, because those files are already in Telegram.
- **Skipped on the phone:** thumbnails, Android's trash, files still being written (such as a video that's recording), `Android/data` and `Android/obb` (Android doesn't allow reading them), WhatsApp statuses, WhatsApp's hidden caches (such as `.Links`, the pictures of link previews), and `Download/tgvault` (where the app saves files from your backup).
- **When it runs:** continuously while the app is open; when it's closed, every 15–30 minutes and soon after new photos or videos appear, with a "Backing up" notification while uploading. **Wi-Fi only** is on by default; **only while charging** is optional. Both are in Settings.
- **Across devices:** each device sees the other's files in *Cloud files*, labelled "From your PC" or "From your phone", and can restore them into a folder you choose. *Free up space* only ever deletes files from the device it runs on.

### Building the APK

GitHub Actions builds it (`.github/workflows/android.yml`) for 64-bit ARM phones (nearly all current phones; x86 Android emulators block a system call the SQLite library uses): `gomobile bind` turns the `mobile` package into an Android library, then Gradle builds the app in `android/`. Each push to `main` updates the android-test pre-release. Release signing uses the repository secrets `ANDROID_KEYSTORE_B64`, `ANDROID_KEYSTORE_PASSWORD` and `ANDROID_KEY_ALIAS`; keep an offline copy of that key, or updates won't install over the existing app.

## Files over 2 GB

Telegram stores at most about 2 GB per message, so bigger files are stored as several messages, each holding the next 1.99 GB of the file: `movie.mkv.part1of3`, `movie.mkv.part2of3`, … tgvault joins them back together when you restore or download, and checks every part and the whole file against their SHA-256 fingerprints. A file only counts as backed up once every part is in Telegram, and an interrupted upload continues where it stopped.

The parts are a plain split, so you can also rebuild a file without tgvault: download every part from the channel and join them in order, e.g. `copy /b movie.mkv.part1of3 + movie.mkv.part2of3 + movie.mkv.part3of3 movie.mkv` on Windows or `cat movie.mkv.part* > movie.mkv` elsewhere. Restoring needs as much free space as the file itself; tgvault checks first.

## Previews

**View** on a file in Cloud files (or double-click it, or press Enter) shows it without restoring it: pictures, videos and music with a seek bar, the first 512 KB of text and code files, and PDFs in the browser's own PDF viewer (on the phone, in a PDF app through **Open with…**). ← and → (or a swipe on the phone) move through the files in the list.

- A file that's still on this device unchanged is shown from the disk, with no Telegram traffic. Otherwise only the part being looked at is fetched from Telegram, 1 MB at a time: jumping to the middle of a 2 GB film fetches a few MB, and files over 2 GB work the same way.
- Nothing is saved to disk for a preview. Downloads and restores still check every file's SHA-256; a preview checks it when it reads a whole file (pictures, text).
- Previews are served so they can't run anything inside tgvault: web pages, SVG scripts and code show as plain text or are blocked from running.
- Whether a video or song plays depends on the formats the browser or phone supports; some `.mkv` videos (x265/HEVC) play sound only. Download or Open with… plays them in another app.

### Thumbnails

Pictures, phone videos (`.mp4`) and GIFs show a small picture in Cloud files and Free up space, and **Grid** shows a folder as tiles (remembered per device; right-click or long-press a tile for View).

- They are the thumbnails Telegram makes by itself for each stored file (5 to 20 KB each), fetched only for the rows on screen, a batch at a time. Nothing extra is uploaded.
- A picture Telegram made no thumbnail for is shrunk on this device when the original is still here.
- They are kept on disk (up to 200 MB on the PC, 50 MB on the phone; the least recently shown go first) and can be cleared in Settings.
- Telegram makes no thumbnails for `.mkv` videos or music, so those keep their icon.

## Projects

A project is stored as a **chain**: a full ZIP of the project, then a change ZIP for each later backup. A change ZIP holds only the files added or modified since the backup before it, plus `.tgvault-deleted.txt` listing the files that were deleted (and `.tgvault-change.txt`, which says in words which full backup and change it follows). Editing one file in a 500 MB project uploads that file, not 500 MB again.

- A new full ZIP starts a new chain once the changes since the last one add up to more than a quarter of its size, after 7 days, or after 200 changes, so restoring never has a long chain to replay.
- The current chain and the one before it are kept; older chains are deleted from Telegram.
- Restoring or downloading any version rebuilds it from its chain and checks the result against a fingerprint of the project's file list at that moment; nothing is written if it doesn't match.
- If a piece of a chain is deleted inside Telegram, the versions before it still work, and the project's next backup is a full ZIP.

To rebuild a version by hand without tgvault: unzip the full ZIP (`project.zip`), then unzip each change ZIP over it in order (`project.change1.zip`, `project.change2.zip`, … up to the version you want; the number is also in each message's caption), and after each one delete the files listed in its `.tgvault-deleted.txt`.

## Limits (for now)

- Empty files aren't stored.
- A file that changes constantly (like an open log) won't be uploaded until it has been still for 20 seconds.

## Important

- tgvault is an independent project. It is not affiliated with or endorsed by Telegram.
- Your files are subject to Telegram's terms. Don't use tgvault as the only copy of anything important.
- `%APPDATA%\tgvault` holds your Telegram login session, which gives full access to your account. Never share that folder. On Android the session is in the app's private storage, and the log from *Share log* doesn't contain it.

## License

[MIT](LICENSE)
