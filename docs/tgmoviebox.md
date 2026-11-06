# tgmoviebox

The library app grown into an offline movie app: browse movies and shows, ask
for what you want, and watch it after it has been downloaded to the phone.
Nothing streams.

## How it fits together

- **The library** is one folder of the backup, `Downloads/Telegram Desktop/tgmoviebox`
  (the repository variable `LIBRARY_FOLDER`). Inside it the PC organizer keeps
  `Movies/Title (Year)/Title (Year).mkv` and `Shows/Title/Season 01/Title - S01E03.mkv`,
  with subtitle files beside their video.
- **The PC** (the normal tgvault): put videos loose in the library folder and
  open the **Movies & shows** tab. *Sort now* reads the file names (`internal/media`),
  checks them against TMDB (`internal/tmdb`) and moves the Telegram copy and the file on
  disk together. Anything unsure waits in *Needs your decision*; one answer is
  remembered for every file named that way.
- **Requests** are short texts in the owner's Saved Messages (a private chat):
  `#tgmb-request {json}` from the phone, `#tgmb-fulfilled` / `#tgmb-declined` from the PC.
  The PC checks every 20 seconds, shows a notification, and the Requests card has
  *Mark as filed* / *Decline*. No server is needed.
- **Keys** (TMDB, and SubDL for subtitle search) are entered on the PC. The PC shares
  them with the phone as `#tgmb-config` in Saved Messages, so the phone app is built
  without any key. Only the PC may post configuration; the read-only engine of the
  phone can post requests and nothing else.
- **The phone app** is the library app (`io.github.sammy77a.tgvault.library`, label
  *tgmoviebox*): Home (TMDB rows, search, details with cast, trailers, seasons, request),
  Downloads (requests with their status, movies and shows to download or watch) and a native
  player (`PlayerActivity.kt`).
- **Downloads** go to the app's private folder (`files/tgvault/mb/...`), not to the gallery.
  *Remove from phone* frees the space; the library in Telegram is untouched.

## The player

Playlists with next/previous and auto-play next, resume where you left off (saved every
10 s and on leaving), speed and audio track (the gear), subtitle tracks (the CC button:
embedded and sidecar, the first in the chosen language is on by default), subtitle search
on SubDL (CC+), size / colour / background / timing offset (Style), aspect modes, rotate and
rotation lock, double-tap to skip 10 s, slide on the left for brightness and on the right
for volume, picture-in-picture.

## Releasing

The phone app has its own release, never `android-test` (that one drives the normal
app's "update ready" notice):

    gh workflow run library.yml --ref main

It publishes `tgvault-library.apk` on the `android-library` pre-release (version code
1000 + run number) and the app checks that release once a day.

## What you need to do once

1. Get a free TMDB key (themoviedb.org, Settings, API) and enter it in the PC's
   Movies & shows tab. The PC shares it with the phone.
2. For subtitle search, get a free SubDL key (subdl.com, account, API) and enter it
   in the same tab (about 2000 searches a day on the free plan, per SubDL).

TMDB: this product uses the TMDB API but is not endorsed or certified by TMDB. The free
key is for non-commercial use.
