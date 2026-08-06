# restinpieces-backup-client

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/restinpieces-backup-client)](https://pkg.go.dev/github.com/caasmo/restinpieces-backup-client)
[![golangci-lint](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/golangci-lint.yml)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/restinpieces-backup-client?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

Client side of the simple default [restinpieces](https://github.com/caasmo/restinpieces) backup system. If you want point-in-time recovery, use the litestream package — see [restinpieces-litestream](https://github.com/caasmo/restinpieces-litestream).

The backup system follows a two-step push-pull design: the **server side** — creating server local snapshots via a background job — is built into restinpieces itself (see [doc/backup.md](https://github.com/caasmo/restinpieces/blob/master/doc/backup.md)). This repository provides the **client side**: one-shot binaries that run in a client machine and pull the backups and verify their integrity.

Two commands are provided:

- [`cmd/rsync`](#rsync-command-cmdrsync) — pulls the `latest-*.db` hard links via the rsync protocol (over SSH, or locally), then verifies every received database with `PRAGMA integrity_check`.
- [`cmd/sftp`](#sftp-command-cmdsftp) — pulls the compressed snapshot archives (`.bck.gz`) over SFTP, decompresses, and verifies with `PRAGMA integrity_check`.

## rsync command (`cmd/rsync`)

The rsync client runs as the receiver: it starts the `rsync` binary in server (sender) mode — over SSH, or locally on the same machine with `-l` — and pulls every `latest-*.db` file (the `latest-<backupID>.db` hard links the server maintains) into a local destination directory. Files are written atomically (temp file + rename), and every received database must pass `PRAGMA integrity_check`.

Build:

```bash
go build -o backup-client ./cmd/rsync
```

The machine that runs the rsync server side (the remote host in SSH mode, the local machine in local mode) must have an rsync-compatible binary in PATH.

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_SOURCE_DIR` | yes | Backup directory on the server containing the `latest-*.db` hard links |
| `RIP_BCK_DEST_DIR` | yes | Local directory the files are pulled into (created if missing) |
| `RIP_BCK_SSH_USER` | yes* | SSH user |
| `RIP_BCK_SSH_HOST` | yes* | SSH host |
| `RIP_BCK_SSH_PORT` | no | SSH port (default `22`) |
| `RIP_BCK_SSH_PRIVATE_KEY_PATH` | yes* | Path to the SSH private key used for authentication |
| `RIP_BCK_SSH_HOST_KEY_PATH` | yes* | Path to the server's public host key; the host key is pinned, so a dial against any other key fails |

\* Only required in SSH mode (the default). Local mode needs only `RIP_BCK_SOURCE_DIR` and `RIP_BCK_DEST_DIR`.

### Running on a schedule

The binary is a one-shot run: exit code `0` means the transfer and the integrity verification succeeded, `1` means any step failed (e.g. the glob matched nothing, a file failed verification, or the server process errored). Run it from a cron job or a systemd timer.

Cron:

```cron
*/5 * * * * RIP_BCK_SOURCE_DIR=/var/caasmo/backups RIP_BCK_DEST_DIR=/home/user/backups RIP_BCK_SSH_USER=backup RIP_BCK_SSH_HOST=server.example.com RIP_BCK_SSH_PORT=22 RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519 RIP_BCK_SSH_HOST_KEY_PATH=/etc/caasmo/ssh_host_ed25519_key.pub /usr/local/bin/backup-client 2>>/var/log/backup-client.log
```

Systemd timer (environment in a separate file):

```ini
# /etc/caasmo/backup-client.env
RIP_BCK_SOURCE_DIR=/var/caasmo/backups
RIP_BCK_DEST_DIR=/home/user/backups
RIP_BCK_SSH_USER=backup
RIP_BCK_SSH_HOST=server.example.com
RIP_BCK_SSH_PORT=22
RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519
RIP_BCK_SSH_HOST_KEY_PATH=/etc/caasmo/ssh_host_ed25519_key.pub
```

```ini
# /etc/systemd/system/backup-client.service
[Unit]
Description=restinpieces backup client (rsync pull)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/caasmo/backup-client.env
ExecStart=/usr/local/bin/backup-client
```

```ini
# /etc/systemd/system/backup-client.timer
[Unit]
Description=Run the restinpieces backup client every 5 minutes

[Timer]
OnCalendar=*:0/5

[Install]
WantedBy=timers.target
```

```bash
systemctl enable --now backup-client.timer
```

### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the full transfer pipeline — protocol, delta transfer, atomic rename, integrity check — on the server machine itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/caasmo/backups RIP_BCK_DEST_DIR=./backups ./backup-client -l
```

In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

## sftp command (`cmd/sftp`)

The SFTP client connects to the server with a pinned host key, opens an SFTP session, lists the remote backup directory, picks the most recent snapshot by filename (names are timestamp-based, so lexical sorting yields the latest), downloads it, decompresses the `.bck.gz` archive, and verifies the resulting database with `PRAGMA integrity_check`.

Build:

```bash
go build -o sftp-client ./cmd/sftp
```

The connection parameters and directories are hardcoded in the `Config` struct at the top of `main()` (`SSHUser`, `SSHHost`, `SSHPort`, `SSHPrivateKeyPath`, `SSHHostKeyPath`, `RemoteBackupDir`, `LocalBackupDir`) — edit them, rebuild, and run.

## License

MIT — see [LICENSE](LICENSE).
