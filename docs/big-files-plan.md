# Backing up files over 2 GB

Status: approved 2026-10-26 and implemented.

## The limit

A Telegram message can carry a file of at most 2,097,152,000 bytes (4000
pieces of 512 KiB), or twice that with Telegram Premium. tgvault skips
anything bigger today ("over 2 GB; Telegram can't store files this big yet").

## The idea

Store a big file as several messages ("parts"), each holding one slice of
the file's bytes, and join the slices back together when the file is
restored. Nothing is compressed or changed: part 1 is bytes 0 to 1.99 GB,
part 2 the next 1.99 GB, and so on.

### Part size

3800 × 512 KiB = 1,992,294,400 bytes (about 1.99 GB) per part. It stays
safely under Telegram's limit and lines up with the 512 KiB pieces uploads
already use, so the existing resume code works inside each part. A 5 GB
video becomes 3 parts. The same size is used with or without Premium, so
every device reads every backup the same way.

### No extra disk space

- **Upload:** parts are read straight from the original file at their offset.
  No split copies are written, which matters on the phone.
- **Restore:** parts are downloaded in order into one temporary file next to
  the destination, then renamed into place. The space needed is the file's
  own size. tgvault checks free space first and says how much is missing
  instead of failing halfway.

## What goes into Telegram

Each part is its own message, with a file name showing what it is:
`movie.mkv.part1of3`, `movie.mkv.part2of3`, … and a caption such as
`📄 Videos/movie.mkv (part 2 of 3)`.

The machine-readable line uses a new caption version (`v: 2`) with these
fields:

| field | meaning |
|---|---|
| `p`, `s`, `m`, `h`, `d` | as today: path, whole-file size, modified time, whole-file SHA-256, device |
| `g` | random ID shared by all parts of one upload |
| `pi`, `pn` | this part's number (1-based) and how many there are |
| `po`, `ps`, `ph` | this part's byte offset, size and SHA-256 |

Why a new version: older tgvault builds accept only `v: 1` captions, so they
ignore parts completely. They can't mistake part 1 for the whole file. Every
`v: 1` file stays exactly as it is.

Because the parts are a plain byte split, the file can be recovered even
without tgvault: download every part from the channel and join them in order
(`copy /b a.part1of3 + a.part2of3 + a.part3of3 a` on Windows, or
`cat a.part* > a`). The README will say so.

## When a split file counts as backed up

A split version exists for tgvault only once **every** part is in Telegram
and agrees with the others (same `g`, `p`, `s`, `h`, `pn`, and offsets
that fit together). Until then:

- it doesn't appear in Cloud files;
- the file isn't marked as backed up;
- Free up space never offers it.

## Database

- **versions:** unchanged. A split version uses its first part's message ID
  as its ID, so everything keyed by version keeps working.
- **New table `parts`:** (version message ID, part number, message ID,
  offset, size, SHA-256). Single-message files have no rows here.
- **New table `part_uploads`:** parts already posted for an upload that isn't
  finished yet (path, group ID, whole-file hash, part number, message ID).
  This lets a 10 GB upload continue after a restart, a lost connection or the
  phone's 9-minute background window, instead of starting over.

## Uploading

1. Read the file once to compute the whole-file SHA-256 and each part's
   SHA-256 together.
2. If the newest backup already has that hash, stop: nothing changed.
3. Look in `part_uploads` for an unfinished upload of the same content (same
   hash). If there is one, keep its group ID and skip the parts already
   posted.
4. Upload the remaining parts in order. Each part reuses today's upload code,
   reading only its byte range. After each part, record its message ID and
   check the file's size and modified time haven't changed.
5. Once the last part is posted, record the version and its parts, and clear
   `part_uploads`.

If the file changes during the upload, every part posted for it is deleted
and the new content is uploaded later, as single files are handled today. If
the file is deleted after a complete upload, the upload is kept, the same rule
as today.

Progress shows "uploading part 2 of 3 · 41%" in the overview, Activity and
the phone notification.

Project ZIPs over 2 GB are split the same way.

## Restoring and downloading

Applies to Restore, Copy to…, phone Download, phone Share, and PC ZIP
download.

- Check free space at the destination first.
- Download part 1, 2, 3… into one temp file, checking each part's SHA-256 as
  it arrives. A part that fails its check or its connection is fetched again
  from its own start, up to 3 tries, without redoing earlier parts.
- After the last part, check the whole-file SHA-256, then rename into place.
  Anything that fails leaves no half-written file behind.
- PC single-file download in the browser streams parts back to back. If a
  part fails its check, the browser download is aborted, so it shows as
  failed rather than saving a corrupt file.
- ZIP downloads with files over 4 GB use ZIP64, which Go writes
  automatically and Windows 10/11 can open.

## Every other action

| action | split files |
|---|---|
| Remove from backup, keeping 3 versions | deletes every part's message |
| Move in backup | edits every part's caption. If any edit fails, the ones already edited are changed back, so parts never disagree. |
| Free up space / Delete from this device | confirms every part still exists in Telegram with the right size before deleting the local copy |
| Copy Telegram links | one line per part ("part 1 of 3: link…"), with a note that they must be joined |
| Full check against Telegram (every 6 h) and minute sync | group parts by `g`; record complete groups; a part deleted inside Telegram makes the whole version missing, so the file uploads again |
| Cloud files / Activity | size shows the whole file, with "· 3 parts" |

## Leftover parts

A part posted by an upload that never finished is invisible but takes space.

- **This device's own:** deleted when the file changes or is removed, or
  when an upload is abandoned (file gone).
- **Another device's:** never deleted while it might still be uploading. Only
  groups older than 7 days with no new part are removed, during the full
  check.

## Turning it on

- Items currently skipped as "over 2 GB" go back into the queue once, after
  the update.
- On the phone, big files obey Wi-Fi only and charging only like everything
  else. Expect hours per file; uploads continue across background runs.
- The project-ZIP limit message and the Activity help text are updated.

## Testing

- **Unit tests:** a fake Telegram in memory, so split upload, resume after an
  interruption, merge, corruption of one part, a missing part, move with a
  failed edit, and the full check's grouping can all be tested offline with
  tiny part sizes (for example 10 KB).
- **Caption tests:** a `v: 2` caption round trips, and a `v: 1` reader
  ignores it.
- **Live test**, only with the owner's OK: an environment variable (`TGVAULT_TEST_PART_SIZE`) makes parts
  50 MB. Back up a 120 MB test file (3 parts) to the real channel, restore it
  on the PC and the phone, compare hashes, then delete those test messages.
  Then one real file over 2 GB end to end.

## Not included

- Using Premium's 4 GB limit: one part size for everyone is simpler, and the
  only cost is more messages.
- Uploading several parts at the same time: Telegram's posting limits make
  it barely faster, and it complicates resuming.
