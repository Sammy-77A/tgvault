# The library app

A copy of the Android app for someone you trust, who can **only look at and
download one folder of your backup**: no backing up, no deleting, no settings.
It needs no server and works while your PC is off, because it talks to Telegram
itself.

## How it works

- It is the normal app built with `-PlibraryFolder=<folder>`, but it has its own
  pipeline and its own release, apart from the normal app's: the `library`
  workflow (`.github/workflows/library.yml`) builds it **only when you start it**
  (Actions → library → Run workflow, or by pushing a tag like `library-1`), and
  publishes `tgvault-library.apk` on the
  [android-library release](https://github.com/Sammy-77A/tgvault/releases/tag/android-library).
  Pushes to `main` build only the normal app, so the two never disturb each other:
  the normal app's update notice only follows its own `android-test` release.
  The install link is
  `https://github.com/Sammy-77A/tgvault/releases/download/android-library/tgvault-library.apk`.
- The folder is the repository variable `LIBRARY_FOLDER` (Settings → Secrets and
  variables → Actions → Variables), or the `folder` you type when starting the
  workflow; it is `Movies` if neither is set. It is the folder's
  name in the backup, such as the label of a PC folder, or a path like `Movies/Action`.
- Its version number counts from 1000 on its own, so it is always above the
  builds that used to come out of the normal pipeline, and Android installs it as an update.
- It installs next to the normal app (package `io.github.sammy77a.tgvault.library`)
  and is named after the folder.
- The app's local server refuses everything except signing in, browsing, searching,
  previewing, playing and downloading files **inside that folder**, so the limit
  doesn't depend on which buttons the page shows. Its Telegram connection is also
  read-only: uploads, deletes and caption edits all fail.
- It does no backups or file watching. It does keep downloads going while the
  app is closed (a foreground service, brought back by an alarm if Android ends the app
  for memory), and it checks the `android-library` release about once a day when opened:
  a notice says an update is ready, and tapping it downloads the APK and opens Android's
  installer (one confirm tap; the first time Android also asks to allow installs from the
  app). Updates install over the old app, so downloads and the sign-in are kept.

## Signing in

The app signs in **as you**: Telegram has no per-folder access, so the phone holds
a full session of your account. Only give it to someone you trust. To sign in, the
person enters your phone number and you read them the code Telegram sends to your
own Telegram (or they show the QR code and you scan it with Telegram → Settings →
Devices → Link Desktop Device). If you use a cloud password, they need it too.
You can end the session at any time in Telegram → Settings → Devices.

## Sharing it

Send the install link above. The person installs it, opens it and signs in as described
above. A normal tgvault already signed in on their phone does not carry over: the two apps
keep separate data, so the movie app signs in once more (a second device in Telegram →
Settings → Devices). Requests are texts in your Saved Messages, so you and anyone signed in
as you see the same list of requests; watching progress and downloads are per phone.

Downloads can be kept on the phone or an SD card (Settings → Storage and space, which also
frees space). Files on an SD card are in `Android/data/<package>/files/tgmoviebox`, which
Android hides from file managers.
