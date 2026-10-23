# tgvault

A lightweight backup app that keeps your folders safe in your own Telegram account. Written in Go. One small `.exe`, managed from a page in your browser.

> **Status:** early development (0.1). Windows is the main target.

## How it works

- **One-way backup.** Files only go from your PC to Telegram. tgvault never deletes your local files on its own, and deleting a local file never deletes its Telegram copy. That's what lets you free up disk space safely.
- **Folders are kept.** Every upload carries its full path (for example `Documents/Taxes/2025/return.pdf`) in its Telegram caption, plus hashtags for each folder. tgvault shows your real folder tree and restores whole folders with every subfolder in place. Because the paths live in Telegram, a fresh install can rebuild the list from Telegram alone.
- **Code projects are one ZIP.** Folders with a `.git`, `package.json`, `composer.json`, `go.mod` and similar are offered as *projects*. Once you confirm, each project is backed up as a single ZIP a few minutes after you stop editing. Rebuildable folders (`node_modules`, and `vendor`, `target`, `build`, virtualenvs when the project type makes that certain) are left out. `.git` and `.env` are kept.
- **No stuck syncs.** Each file is its own job. A file that fails retries on its own schedule with the reason shown next to it, and everything else keeps going. Telegram "slow down" requests only pause what they affect.
- **Versions.** Edited files are uploaded as new versions; the last 3 are kept.
- **Free up space.** Deletes local copies only after re-checking that the Telegram copy exists and matches.
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

## Limits (for now)

- Files over 2 GB are skipped with a warning (Telegram's per-file limit).
- Empty files aren't stored.
- A file that changes constantly (like an open log) won't be uploaded until it has been still for 20 seconds.

## Important

- tgvault is an independent project. It is not affiliated with or endorsed by Telegram.
- Your files are subject to Telegram's terms. Don't use tgvault as the only copy of anything important.
- `%APPDATA%\tgvault` holds your Telegram login session, which gives full access to your account. Never share that folder.

## License

[MIT](LICENSE)
