# Projects: upload only what changed

Status: proposed 2026-10-27, waiting for the owner's OK.

This applies to every project tgvault backs up, on the PC and the phone.
Nothing in it is specific to one project; the numbers below just use a real
one as an example.

## The problem

A project is backed up as one ZIP of the whole folder. Every time it has been
quiet for 5 minutes, the whole folder is zipped and uploaded again, however
little changed. A 480 MB project where 1 MB changed costs 480 MB of upload
(about 27 minutes on a 300 KB/s link), and if a file changes while zipping, the
work is thrown away and started again. Telegram ends up holding 3 full copies.

## The idea

- Now and then, upload a **full ZIP** of the project, as today.
- In between, each backup uploads a **change ZIP**: only the files added or
  modified since the previous backup, plus a list of deleted files.
- A version of the project is a full ZIP followed by the change ZIPs after it,
  up to that point. That run is a **chain**.

For a 1 MB edit, the upload is about 1 MB.

## What is uploaded and when

### Deciding what changed

tgvault keeps, for each project, the list of files as they were at the last
backup: path, size, modified time and SHA-256 (new table `project_files`).
When the project is due:

1. List the project's files (same rules as today: rebuildable folders left
   out, `.git` and `.env` kept).
2. A file whose size and modified time are unchanged is taken as unchanged
   (the same rule single files use today). Every other file is hashed; if its
   hash is the same, it's unchanged too (for example after `git checkout`
   touched it).
3. Changed = new files + files with a new hash. Deleted = files in the old
   list that are gone.
4. Nothing changed: nothing is uploaded.

### Full or change ZIP

A **full ZIP** is made when any of these is true, otherwise a change ZIP:

- there is no chain yet (first backup, or the first after this update);
- the change ZIPs since the last full one add up to more than 25% of the full
  ZIP's size (so restoring never means replaying a huge pile of changes);
- the chain is more than 7 days old, or has 200 change ZIPs;
- the previous chain is broken (a piece missing in Telegram, see below).

### Inside a change ZIP

- the changed files at their normal paths;
- `.tgvault-deleted.txt`: one deleted path per line (absent when nothing was
  deleted);
- `.tgvault-change.txt`: plain words saying which full ZIP and previous change
  ZIP this follows, so a person can put the chain in order without tgvault.

A rename is a delete plus an add. Empty folders aren't kept (as today).

### Captions

Full ZIPs keep today's caption, so older tgvault builds still see them as
normal project versions. Change ZIPs use a new kind (`"k": "d"`) with:

| field | meaning |
|---|---|
| `b` | message ID of the chain's full ZIP |
| `prev` | message ID of the previous piece (the full ZIP or the previous change) |
| `n` | position in the chain (1, 2, 3, …) |
| `t` | SHA-256 of the whole project's file list after this change (path + file hash for every file), used to check a restore |
| `fc`, `fd` | how many files changed and were deleted |

Older builds accept only kinds `f` and `p`, so they ignore change ZIPs
completely; they just see the last full ZIP. Full ZIPs also get `t`.

The human-readable caption says e.g.
`📦 Documents/myproject: 12 files changed, 1 deleted (change 4 after the full backup of 27 Oct)`.

## Restoring and downloading

Applies to Restore, Copy to…, Download (PC ZIP and phone), Share, and the
Versions dialog.

1. Pick the version (any full or change ZIP). Download its full ZIP and every
   change ZIP up to it, each checked against its own SHA-256 (as now).
2. Build the result: start from the full ZIP's files, then apply each change
   ZIP in order (replace or add its files, delete the paths in its list).
3. Check the result: the file list's SHA-256 must equal that version's `t`.
   If it doesn't, nothing is written and the error says so.
4. Write it out exactly as a project restores today (into a folder, or as a
   ZIP when downloading).

Free disk space needed: the full ZIP plus the change ZIPs plus the result,
checked before starting, as for big files.

**By hand, without tgvault:** unzip the full ZIP; unzip each change ZIP over
it in order (`n` = 1, 2, 3…, also written in each message's caption); after
each, delete the files listed in its `.tgvault-deleted.txt`. The README will
describe this.

## Versions and clean-up

- **Every backup is a restorable version.** The Versions dialog lists them
  with "full backup" or "12 files changed".
- **Kept:** the current chain and the one before it, so there is always at
  least one complete older chain. Older chains are deleted from Telegram,
  every piece of them. This replaces "keep 3 versions" for projects.
- A chain is never deleted while it is the newest complete one.

## When something goes wrong

- **A file changes while making a change ZIP:** that change ZIP is dropped and
  made again shortly after; it takes seconds, so little is lost.
- **A file changes while making a full ZIP:** as today (try again later).
- **A piece of a chain is deleted inside Telegram:** versions after the gap
  can't be restored, so they're shown as missing, versions before the gap
  still work, and the next backup is a full ZIP.
- **Upload interrupted:** a change ZIP is small and simply uploaded again; a
  full ZIP resumes as uploads do today. The file list in `project_files` is
  only updated once the upload is in Telegram, so nothing is skipped.
- **Two devices backing up the same project path** (unusual): each device's
  chain is kept separate by device, as versions are today.

## Rebuilding from Telegram (full check, other devices)

The full check and the minute sync read change ZIPs too: pieces are linked by
`b` and `prev`, and a chain is recorded up to its first gap. Another device
can list and restore every version without having seen the project itself.

## Other places that touch versions

| action | with chains |
|---|---|
| Remove from backup | deletes every piece of every chain |
| Move in backup | edits every piece's caption; rolled back on failure (as for big files) |
| Copy Telegram links | one line per piece, in order, with a note on joining |
| Cloud files | size shown is the project's size at the latest version, with "· 23 changes since full backup" |
| Full ZIP over 2 GB | split into parts as today; a change ZIP over 2 GB is split the same way |

## Turning it on

- No change for single files or folders that aren't projects.
- Each project's first backup after the update is a full ZIP (there is no
  file list to compare with yet). After that, change ZIPs.
- Existing full-ZIP versions stay restorable; they're cleaned up by the new
  rule once two newer chains exist.

## Code changes

| where | change |
|---|---|
| `internal/meta` | kind `d`, fields `b`, `prev`, `n`, `t`, `fc`, `fd`; caption tests |
| `internal/store` | `project_files` table; chain columns on versions (`base`, `prev`, `seq`, `tree`) |
| `internal/backup/projectdiff.go` (new) | file list, diff, change ZIP writer, full/change decision, tree hash |
| `internal/backup/engine.go` | `uploadProject` makes full or change ZIPs; prune by chain |
| `internal/backup/projectrestore.go` (new) | rebuild a version from its chain, check `t` |
| `internal/backup/cloud.go`, `browse.go`, `split.go` | reconcile/sync chains; delete, move, links, versions |
| `internal/web/ui/app.js` | versions dialog and Cloud files wording |
| README | how project backups work now, restoring by hand |

## Testing

- **Unit tests** with the fake Telegram: full → change → change, restoring
  each version gives exactly the files of that moment (including deleted and
  renamed files); unchanged project uploads nothing; a touched but identical
  file isn't uploaded; the 25% / 7 days / 200 rules start a new full ZIP; a
  missing piece makes later versions missing and forces a full ZIP; pruning
  keeps two chains; reconcile rebuilds chains from captions; a wrong tree hash
  stops the restore; move rolls back; older-build parser ignores kind `d`.
- **Live:** a real project on the PC — edit a few files, check that a change
  ZIP of the right size is uploaded, restore the latest and an older version
  into a scratch folder and compare every file with the originals, then
  delete the restored copies.

## Not included

- Byte-level deduplication inside files (a 50 MB file with one changed byte
  is uploaded whole in the change ZIP). Change ZIPs are compressed, which
  helps for text.
- Honouring `.gitignore` for project ZIPs. It would shrink many projects, but
  it would also drop `.env` and other ignored files people want kept, so it
  needs its own decision.
