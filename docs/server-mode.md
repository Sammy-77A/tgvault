# Server mode: a file host for a few friends

`tgvault --server` runs tgvault on an always-on computer (a small Linux VPS, or any PC that stays on) and lets a few friends store and fetch files through an HTTP API. Everything is stored in **your** Telegram backup channel, under `Friends/<name>/…`, so it also shows up in your own Cloud files.

- Each friend has an **API key**, their **own folder**, and a **storage limit**. Nobody can see or change anyone else's files.
- An upload waits in a local folder (`<data>/spool/<name>`) until it is safely in Telegram, then the local copy is removed. Files over 2 GB are split into parts automatically, same as your own backups.
- Downloads stream straight from Telegram with HTTP range requests, so they resume and videos seek. Only the bytes asked for are fetched.
- Share links let anyone without a key download one file, until the link expires or is revoked.

It is meant for you and a handful of people you know. Telegram can restrict an account that serves files to the public, and every friend's files live in your account.

## Set up

1. **Build** (on your PC, for a Linux server):
   ```
   GOOS=linux GOARCH=amd64 go build -o tgvault ./cmd/tgvault
   ```
   Copy `tgvault` to the server. For the `friends` commands on Windows, build without `-H windowsgui` so their output shows.
2. **Run it** (a systemd unit is below). Use one data folder, e.g. `/var/lib/tgvault`:
   ```
   tgvault --server --data /var/lib/tgvault --api 127.0.0.1:8800 \
           --public-url https://files.example.com --behind-proxy --spool-max 20GB
   ```
3. **Log in to Telegram once.** The management page only listens on the server itself (`127.0.0.1:8790`). Reach it through an SSH tunnel and log in the usual way (API ID and hash, then QR or phone code):
   ```
   ssh -L 8790:127.0.0.1:8790 you@server
   ```
   then open the address `tgvault` printed (`http://127.0.0.1:8790/?t=…`; it is also in `<data>/ui.json`). The server must be the **only** place logged in with this session, and your PC and phone log in separately as before.
4. **Put HTTPS in front** of the API port. With Caddy, `/etc/caddy/Caddyfile`:
   ```
   files.example.com {
       reverse_proxy 127.0.0.1:8800
   }
   ```
   Caddy gets the certificate by itself. Keep `--behind-proxy` so a wrong-key lockout applies to the real client address. Don't expose port 8800 directly; keys would travel unencrypted.
5. **Add your friends:**
   ```
   tgvault friends --data /var/lib/tgvault add alice --quota 50GB
   ```
   It prints the key once. Send it to them privately. Other commands: `list`, `quota NAME SIZE`, `key NAME` (new key), `disable NAME`, `enable NAME`, `remove NAME` (their files stay in Telegram).

systemd unit, `/etc/systemd/system/tgvault.service`:
```
[Unit]
Description=tgvault file server
After=network-online.target

[Service]
ExecStart=/usr/local/bin/tgvault --server --data /var/lib/tgvault --api 127.0.0.1:8800 --public-url https://files.example.com --behind-proxy
Restart=always
User=tgvault

[Install]
WantedBy=multi-user.target
```

## The API

Send the key as `Authorization: Bearer tgv_…` (or `X-Api-Key`). Paths are relative to the friend's own folder. Errors come back as `{"error": "…"}`.

| Call | What it does |
|---|---|
| `PUT /v1/files/<path>` | Upload the request body as `<path>`. Replaces an existing file (the last 3 versions are kept in Telegram). Answers `202` once received; it reaches Telegram shortly after. |
| `GET /v1/files/<path>` | Download. Supports `Range`. Add `?inline=1` to show images, video, audio and PDFs in the browser. |
| `GET /v1/status/<path>` | `queued`, `uploading`, `retrying`, `stored` or `rejected`. Poll it after an upload. |
| `GET /v1/list?path=<folder>` | Files and folders in a folder, plus `pending` uploads not yet in Telegram. |
| `GET /v1/zip?path=<folder>` | A folder as a ZIP. |
| `DELETE /v1/files/<path>` | Remove a file or folder, from Telegram too. `409` while it is still uploading. |
| `GET /v1/me` | Name, space used, limit. |
| `POST /v1/shares` `{"path": "...", "hours": 168}` | Make a share link (`hours` 0 = 7 days, -1 = never expires). Returns `url`. |
| `GET /v1/shares`, `DELETE /v1/shares/<id>` | List or revoke links. |
| `GET /s/<token>` | The share link itself. No key needed. |

```
curl -H "Authorization: Bearer $KEY" -T movie.mkv https://files.example.com/v1/files/films/movie.mkv
curl -H "Authorization: Bearer $KEY" https://files.example.com/v1/status/films/movie.mkv
curl -H "Authorization: Bearer $KEY" -o movie.mkv https://files.example.com/v1/files/films/movie.mkv
```

Folders are made by uploading a path (`a/b/c.txt`); upload a whole folder by sending each file.

## Limits and behaviour

- **Storage limit** counts the latest version of each file plus uploads still waiting. Over it, uploads get `507`.
- **Room for waiting uploads** (`--spool-max`, default 20 GB): while the server is still sending earlier uploads to Telegram, new ones wait (`503`, `Retry-After`). Three uploads are received at once.
- **Not stored:** empty files, temporary and system files, and anything inside folders like `node_modules` (the same rules as your own backups). The API answers `422` instead of accepting them and losing them.
- **Wrong keys:** ten wrong keys from one address block it for ten minutes.
- **Keys and links** are stored only as hashes; a lost key can't be looked up, only replaced (`friends key NAME`).
- Uploads are not encrypted at rest in Telegram, as with your own backups.
