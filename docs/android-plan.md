# tgvault for Android: implementation plan

Status: draft, waiting for approval. Nothing below has been built yet.

## Goal

Back up the phone's files to the same Telegram backup as the PC, with the same
rules: one-way backup, last 3 versions kept, local deletes never touch Telegram,
rate limits handled quietly, restore and free-up-space available. Each device
can see and restore the other's files.

## Approach in one paragraph

The Android app is a thin Kotlin shell around the existing Go code. The Go
backup engine, Telegram client, database and web page are compiled into an
Android library with `gomobile bind`. The app runs the same local web page and
shows it in a full-screen WebView, so the phone gets the same screens as the PC
without writing a second UI. The Kotlin side only does what Go can't do on
Android: permissions, running in the background, notifications, telling the
gallery about changed files, and app updates. The APK is built by GitHub
Actions, so nothing large is installed on the PC.

## Architecture

```
Android app (Kotlin, small)
 ├─ MainActivity ── WebView ──> http://127.0.0.1:<random port>/?t=<token>
 ├─ BackupWorker (WorkManager, runs in the background with a notification)
 ├─ Host bridge: notifications, media scanner, network/battery state
 └─ tgvault.aar (Go, via gomobile)
     └─ internal/app  ── same wiring as the PC
         ├─ backup engine   (shared)
         ├─ tgclient        (shared)
         ├─ store (SQLite)  (shared)
         ├─ web page + API  (shared, with phone-only settings)
         └─ notify          (shared; sends through the host bridge)
```

## Phase 0: proof that the Go core runs on Android

Risky assumptions are checked first, before the big work.

1. Add a GitHub Actions workflow that installs Go, the Android SDK/NDK (already
   on GitHub's runners) and gomobile, and builds a tiny APK.
2. That APK starts the existing Go web server and shows it in a WebView.
3. Check on a real phone:
   - pure-Go SQLite (`modernc.org/sqlite`) works on android/arm64;
   - gotd connects to Telegram and logs in;
   - the WebView can reach the local page.
4. Check the login options on a phone:
   - Code login: the code arrives in the Telegram app on the same phone.
   - **Login link:** QR login can't be used on the same phone (you can't scan
     your own screen). Test whether a `tg://login?token=…` link opened from
     tgvault makes the Telegram app ask "Log in to tgvault?". If yes, that
     becomes a one-tap login. If not, code login it is.

Checkpoint: you install the test APK and log in. If something fails here, the
plan is adjusted before more is built.

## Phase 1: reshape the Go code to be shared (PC behaviour unchanged)

1. Move the wiring in `cmd/tgvault/main.go` into a new `internal/app` package
   (`app.Start(ctx, Options) (*App, error)`), used by both the PC command and the
   Android library.
2. New `mobile/` package: the small gomobile API Kotlin calls:
   - `Start(dataDir, deviceInfo) → url`, `Stop()`;
   - `RunBackup(deadlineSeconds) → finished?` for background runs;
   - `SetConditions(onWifi, charging)`;
   - `SetHost(host)`, where `host` is a Kotlin interface for notifications,
     media scans and opening links.
3. Engine "run until done" mode: scan, upload until the queue is empty or the
   deadline is reached, record where it stopped, return. Uploads already resume
   across restarts.
4. Platform files for Android (`//go:build android`) next to the Windows ones:
   - no Windows autostart (the background worker handles it);
   - no PowerShell folder picker; instead a folder browser built into the page
     (`/api/browse`), which the PC can also use;
   - no fsnotify watcher (see Phase 3 for how changes are noticed).
5. Pluggable notification sender, so Android notifications go through Kotlin.
6. Web server: random port and token on Android. Other apps on a phone can reach
   127.0.0.1, so the existing token check is what protects it, and it stays.

Checkpoint: PC build and tests unchanged; the PC app is rebuilt and restarted.

## Phase 2: several devices in one backup

Today every path in Telegram starts with a PC folder name (`Documents/…`). If the
phone also had a `Documents` folder, the two would mix. So:

1. **Device name.** Each device gets a name (PC: `PC`, phone: `Phone`, both
   changeable once at setup). New uploads from the phone go under
   `Phone/<folder>/…`, for example `Phone/Camera/IMG_2031.jpg`.
2. **Your existing PC backup stays where it is.** No renaming and no re-uploading
   of the 3,480+ files already backed up. The PC keeps its paths; adding a PC
   backup folder whose name equals a device name is refused, so the two can
   never collide.
3. **Caption field `dev`.** Each upload also records which device made it, so
   tgvault never has to guess from the path.
4. **Cloud screen grouped by device:** "This PC" and "Phone" at the top.
5. **Restoring another device's files** asks where to put them up front. The
   restore code already supports a target folder; it's just made the default
   path in the UI.
6. **Safety rules across devices:**
   - Free up space only ever deletes files from the device it runs on.
   - Pruning to 3 versions works per path, so devices don't trim each other.
   - "Remove from backup" on another device's files is allowed but warns that
     the files came from the other device; that device sees the removal at its
     next Telegram check and backs the file up again if it still has it (the
     existing `RequeueMissing` behaviour).
   - Both devices only *find* the existing "tgvault backup" channel; a new
     channel is created only if none exists, so two devices can't end up with
     two channels.
7. Shared rate limits: both devices use the same account, so each one's pacing
   and cooldown already back off when Telegram asks.

## Phase 3: the Android app

### Setup

1. First start: enter the API ID and hash (same as the PC; a Paste button
   helps), then log in (link or code, decided in Phase 0).
2. Ask for permissions, explaining each one:
   - **All files access** (`MANAGE_EXTERNAL_STORAGE`), to read and restore
     folders. Allowed for apps installed outside the Play Store.
   - **Notifications** (Android 13+).
   - **Ignore battery optimisation**, so Android doesn't keep postponing backups.
3. Folder suggestions, ticked by default:
   - Camera (`DCIM/Camera`), Screenshots, Pictures, Movies, Documents,
     Download, Recordings;
   - WhatsApp media (`Android/media/com.whatsapp/WhatsApp/Media`);
   - memory-card folders if a card is present.
   Not suggested, because their files are already in Telegram:
   `Pictures/Telegram`, `Download/Telegram`, `Telegram/`.

### Phone skip rules (added to the shared rules)

- `.thumbnails` and other hidden cache folders;
- `.trashed-*` (Android's trash) and `.pending-*` (files still being written,
  e.g. a video that's still recording);
- `Android/data`, `Android/obb` (Android doesn't allow reading these anyway);
- WhatsApp `.Statuses` (other people's temporary statuses);
- project-folder detection off on the phone (it rarely has code projects).

### Running in the background

Android stops apps that run constantly, so:

1. **App open:** backs up continuously, like the PC.
2. **App closed:** a WorkManager job runs every 15–30 minutes, and also as soon
   as new photos or videos appear (a gallery-change trigger). While uploading it
   shows a quiet notification, "Backing up 12 files".
3. **Conditions:** "Wi-Fi only" (on by default) and "Only while charging" (off by
   default) are set in Settings and passed to WorkManager.
4. **Time limits:** Android 15 caps this kind of background work at about
   6 hours a day. When Android says time's up, tgvault saves where it stopped and
   continues in the next window. Uploads resume rather than restart.
5. **After a reboot:** WorkManager resumes on its own. No separate autostart.

### Phone-specific behaviour

- **Gallery stays correct.** After restoring or freeing up space, the app tells
  Android's media scanner, so Gallery and Google Photos update straight away.
- **Free up space** deletes files permanently (Android's trash is bypassed for
  apps with file access), so it keeps the same checks as the PC: the Telegram
  copy is verified by hash and confirmed to exist before anything is deleted.
- **Restore** checks there's enough free space first, on both phone and PC.
- **Notifications** use the existing rules (logged out, files failing for a day,
  a folder missing such as a removed SD card) as Android notifications. Tapping
  one opens the app.
- **Downloads** from the Cloud screen save into `Download/tgvault/` instead of a
  browser download.

### Phone-friendly page

- Make the existing page work at phone width: single-column layout, larger tap
  targets, a bottom tab bar.
- Hide PC-only settings (start with Windows, quit) on the phone and show the
  phone's own (Wi-Fi only, charging only, battery status, permissions).
- A banner if a needed permission was turned off later.

## Phase 4: building, signing, updating

1. **GitHub Actions** builds on every push: Go tests, gomobile library, debug APK.
2. **Releases:** tagging a version (e.g. `v0.2.0`) builds a signed APK and the
   Windows exe and attaches both to a GitHub Release.
3. **Signing key:** generated once and stored as GitHub repository secrets. You
   also keep one offline copy: if the key is lost, future updates can't install
   over the old app (you'd have to uninstall and reinstall).
4. **Updates:** the app checks the latest GitHub release once a day and offers
   the new APK. Android then asks to confirm the install.
5. Targets: Android 8.0 and newer, 64-bit phones (arm64), plus x86_64 for the
   emulator used in automated tests.

## Phase 5: testing

1. Go unit tests for: device namespacing and the collision rule, phone skip
   rules, run-until-done mode, cross-device reconcile (one device's uploads seen
   and restorable on the other), free space never touching another device's
   files.
2. CI emulator test: install the APK, start it, check the page loads and the
   background job runs.
3. On your phone, through a written checklist: login, first backup of a small
   folder, take a photo and see it back up, Wi-Fi-only respected, reboot,
   restore a file, free up space and check the Gallery, see phone files on the
   PC and restore one there.
4. **Optional, faster:** if you turn on USB debugging and connect the phone, I
   can install builds, read the app's logs and take screenshots directly
   (needs Android's small `platform-tools`, about 15 MB, on the PC). Otherwise
   you install APKs from GitHub and send screenshots.

## Phase 6: docs

README section for the phone: install, permissions and why, what is and isn't
backed up, the background limits, and how restore across devices works.

## Known limits

- **Files over 2 GB are skipped** (Telegram's limit). Phone videos reach this
  more often than PC files: about 10 minutes of 4K. The file-splitting feature
  you put on hold would fix it; see decisions.
- Other apps' private data (`Android/data`), app settings and chat databases
  can't be backed up. Android doesn't allow it.
- Backups while the app is closed run in windows, not instantly; a new photo may
  take up to ~15 minutes to go up unless the gallery trigger fires sooner.
- iPhone is not covered.

## Order and checkpoints

| Step | You'll see |
|------|-----------|
| 0. Proof on Android | A test APK that logs in |
| 1. Shared Go core | Nothing changes on the PC |
| 2. Several devices | Cloud screen grouped by device on the PC |
| 3. Android app | Working phone backup |
| 4. Build and release | A GitHub Release with the APK and exe |
| 5. Testing | Checklist run on your phone |
| 6. Docs | README updated |

Each phase is committed and pushed separately, and the PC app keeps working
after every step.

## Decisions (2026-10-24)

1. Device names: `PC` and `Phone`.
2. Wi-Fi only: on by default.
3. Files over 2 GB: skipped for now (file splitting stays on hold).
4. The phone app is a light APK that shows tgvault's screens full-screen in the
   app (WebView), not a browser tab or a PWA. A PWA can't run backups in the
   background or read the phone's folders.
5. Testing without a cable: automated tests on an Android emulator in GitHub
   Actions, plus wireless debugging (Android 11+, same Wi-Fi, no USB) if you
   opt in. Otherwise APKs from GitHub Releases plus screenshots, with a
   "Share log" button in the app's Settings.
