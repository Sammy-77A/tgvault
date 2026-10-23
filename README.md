# tgvault

A lightweight, self-run file drive that stores your files in your own Telegram account. Written in Go.

> **Status:** early development, nothing usable yet.

## Goals

- One small executable, no installer and no bundled runtime
- Low memory use
- Your files go directly between your computer and Telegram, with no third-party server in the middle
- No ads, no telemetry

## Requirements

- Your own Telegram API ID and API hash from [my.telegram.org](https://my.telegram.org) (API development tools)
- Go 1.27 or newer to build from source

## Important

- tgvault is an independent project. It is not affiliated with or endorsed by Telegram.
- Telegram limits each file to 2 GB, and your files are subject to Telegram's terms. Don't use tgvault as the only copy of anything important.
- Your API hash and session files give full access to your Telegram account. Never share or commit them.

## License

[MIT](LICENSE)
