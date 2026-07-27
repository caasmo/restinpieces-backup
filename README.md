# restinpieces-backup-client

An SFTP pull client for retrieving backup files created by the [restinpieces](https://github.com/caasmo/restinpieces) framework.

The backup system follows a two-step push-pull design. The **server side** — creating compressed SQLite snapshots via a background job — is built into restinpieces itself (see [doc/backup.md](https://github.com/caasmo/restinpieces/blob/master/doc/backup.md)). This repository provides the **client side**: a standalone binary that connects to the server via SFTP, finds the latest backup, downloads it, and verifies its integrity.

## How it works

The client runs as a one-shot binary and performs the following steps:

1. **Establish SFTP connection** — opens an SSH connection to the remote host using key-based authentication, then creates an SFTP client over that connection.

2. **Find latest backup** — lists all files in the configured remote backup directory, sorts them by name in descending order, and selects the most recent one. Backup filenames are assumed to be timestamp-based so lexical sorting yields the latest.

3. **Download backup** — ensures the local backup directory exists, opens the selected file on the remote server, and copies it to the local filesystem via SFTP.

4. **Verify integrity** — decompresses the downloaded `.bck.gz` archive into a temporary location, opens the resulting SQLite database in read-only mode, and runs `PRAGMA integrity_check`. The verification passes only if the result is `ok`. The temporary decompressed database is cleaned up afterwards.

## Features

- SFTP pull of backup files from a remote server
- Automatically finds the latest backup by filename timestamp
- Decompresses `.bck.gz` archives
- Verifies integrity with `PRAGMA integrity_check`

## Usage

```bash
go run github.com/caasmo/restinpieces-backup-client/cmd/sftp@latest \
  -host myserver.example.com \
  -user backup \
  -remote-dir /data/backups \
  -local-dir ./backups
```

See [cmd/sftp](cmd/sftp/main.go) for configuration options.

## License

MIT — see [LICENSE](LICENSE).
