# restinpieces-sqlite-backup

An SFTP pull client for retrieving backup files created by the [restinpieces](https://github.com/caasmo/restinpieces) framework.

The backup system follows a two-step push-pull design. The **server side** — creating compressed SQLite snapshots via a background job — is built into restinpieces itself (see [doc/backup.md](https://github.com/caasmo/restinpieces/blob/master/doc/backup.md)). This repository provides the **client side**: a standalone binary that connects to the server via SFTP, finds the latest backup, downloads it, and verifies its integrity.

## Features

- SFTP pull of backup files from a remote server
- Automatically finds the latest backup by filename timestamp
- Decompresses `.bck.gz` archives
- Verifies integrity with `PRAGMA integrity_check`

## Usage

```bash
go run github.com/caasmo/restinpieces-sqlite-backup/cmd/client@latest \
  -host myserver.example.com \
  -user backup \
  -remote-dir /data/backups \
  -local-dir ./backups
```

See [cmd/client](cmd/client/main.go) for configuration options.

## License

MIT — see [LICENSE](LICENSE).
