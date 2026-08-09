# restinpieces-backup-client

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/restinpieces-backup-client)](https://pkg.go.dev/github.com/caasmo/restinpieces-backup-client)
[![golangci-lint](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/golangci-lint.yml)
[![sloc](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup-client/master/.github/badges/sloc.json)](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/sloc.yml)
[![deps](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup-client/master/.github/badges/deps.json)](https://github.com/caasmo/restinpieces-backup-client/actions/workflows/dependencies.yml)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/restinpieces-backup-client?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

This repository contains three clients for the backup **local simple** strategy of [restinpieces](https://github.com/caasmo/restinpieces):

- [`cmd/rsync`](#rsync-command-cmdrsync) — pulls the `latest-*.db` hard links via the rsync protocol (over SSH, or locally), then verifies every received database with `PRAGMA integrity_check`.
- [`cmd/rsync-daemon`](#rsync-daemon-cmdrsync-daemon) — the same rsync pull, repeated on a fixed interval by an always-on daemon.
- [`cmd/sftp`](#sftp-command-cmdsftp) — pulls the compressed snapshot archives (`.bck.gz`) over SFTP, decompresses, and verifies with `PRAGMA integrity_check`.

If you want point-in-time recovery, use the litestream package — see [restinpieces-litestream](https://github.com/caasmo/restinpieces-litestream).

The backup system follows a two-step push-pull design: the **server side** — creating server local snapshots via a background job — is built into restinpieces itself (see [doc/backup.md](https://github.com/caasmo/restinpieces/blob/master/doc/backup.md)). This repository provides the **client side**: one-shot binaries that run in a client machine and pull the backups and verify their integrity.

# Content

- [rsync command (`cmd/rsync`)](#rsync-command-cmdrsync)
  - [Build](#build)
  - [Environment variables](#environment-variables)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local)
- [rsync daemon (`cmd/rsync-daemon`)](#rsync-daemon-cmdrsync-daemon)
  - [Build](#build-1)
  - [Environment variables](#environment-variables-1)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-1)
  - [Backup cadence](#backup-cadence)
  - [Signals](#signals)
  - [Security](#security)
- [sftp command (`cmd/sftp`)](#sftp-command-cmdsftp)
  - [Build](#build-2)
- [Running on a schedule](#running-on-a-schedule)
  - [Cron](#cron)
  - [Systemd timer](#systemd-timer)

## rsync command (`cmd/rsync`)

The rsync client runs as the receiver: it starts the `rsync` binary in server (sender) mode — over SSH, or locally on the same machine with `-l` — and pulls every `latest-*.db` file (the `latest-<backupID>.db` hard links the server maintains) into a local destination directory. Files are written atomically (temp file + rename), and every received database must pass `PRAGMA integrity_check`.

### Build

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

### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the full transfer pipeline — protocol, delta transfer, atomic rename, integrity check — on the server machine itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/caasmo/backups RIP_BCK_DEST_DIR=./backups ./backup-client -l
```

In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

## rsync daemon (`cmd/rsync-daemon`)

The rsync daemon performs the same transfer as the [rsync command](#rsync-command-cmdrsync) — the receiver protocol over SSH (or locally with `-l`), pulling the `latest-*.db` hard links, atomic writes, `PRAGMA integrity_check` — but on a fixed interval instead of once. It is the always-on alternative to scheduling the one-shot command: it keeps running, takes a backup immediately at startup, and repeats it every interval.

### Build

```bash
go build -o backup-daemon ./cmd/rsync-daemon
```

The machine that runs the rsync server side (the remote host in SSH mode, the local machine in local mode) must have an rsync-compatible binary in PATH.

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_SOURCE_DIR` | yes | Backup directory on the server containing the `latest-*.db` hard links |
| `RIP_BCK_DEST_DIR` | yes | Local directory the files are pulled into (created if missing) |
| `RIP_BCK_INTERVAL` | yes | How often to run a backup, as a Go duration string (e.g. `5m`, `1h`); must be positive |
| `RIP_BCK_SSH_USER` | yes* | SSH user |
| `RIP_BCK_SSH_HOST` | yes* | SSH host |
| `RIP_BCK_SSH_PORT` | no | SSH port (default `22`) |
| `RIP_BCK_SSH_PRIVATE_KEY_PATH` | yes* | Path to the SSH private key used for authentication |
| `RIP_BCK_SSH_HOST_KEY_PATH` | yes* | Path to the server's public host key; the host key is pinned, so a dial against any other key fails |

\* Only required in SSH mode (the default). Local mode needs only `RIP_BCK_SOURCE_DIR`, `RIP_BCK_DEST_DIR`, and `RIP_BCK_INTERVAL`.

### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the full transfer pipeline — protocol, delta transfer, atomic rename, integrity check — on the server machine itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/caasmo/backups RIP_BCK_DEST_DIR=./backups RIP_BCK_INTERVAL=5m ./backup-daemon -l
```

The first backup runs immediately at startup, then one per interval; Ctrl-C stops the daemon gracefully. In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

### Backup cadence

- The first backup runs immediately at startup; the next one starts one full interval after the previous backup completes.
- Backups run one at a time: a tick that fires while a backup is still running is dropped, so at most one backup per interval is guaranteed.
- At least one backup per interval is *not* guaranteed: when a transfer takes longer than the interval (e.g. a 12-minute transfer with a 5-minute interval), backups run back-to-back and the intended cadence is lost. Set the interval so a single backup finishes comfortably within it.
- A failing backup (transfer or verification) is logged and the next tick retries — the daemon never exits on a failure.
- The destination directory is created at startup and re-created by the receiver on every run, so a mid-run removal self-heals on the next tick.

### Signals

SIGINT, SIGQUIT, and SIGTERM trigger a graceful shutdown: the runner cancels the in-flight transfer and waits up to 15 seconds for the daemon to stop. A stop that lands during the (non-cancellable) verification scan lets the scan run to completion within that deadline.

### Security

The daemon's security architecture is documented in [cmd/rsync-daemon/README.md](cmd/rsync-daemon/README.md): the gokrazy landlock sandbox and the threat it addresses, the daemon's security choices (in-memory SSH keys, landlock applied once at startup in SSH mode, deactivated in local mode), and the optional systemd hardening.

## sftp command (`cmd/sftp`)

The SFTP client connects to the server with a pinned host key, opens an SFTP session, lists the remote backup directory, picks the most recent snapshot by filename (names are timestamp-based, so lexical sorting yields the latest), downloads it, decompresses the `.bck.gz` archive, and verifies the resulting database with `PRAGMA integrity_check`.

### Build

```bash
go build -o sftp-client ./cmd/sftp
```

The connection parameters and directories are hardcoded in the `Config` struct at the top of `main()` (`SSHUser`, `SSHHost`, `SSHPort`, `SSHPrivateKeyPath`, `SSHHostKeyPath`, `RemoteBackupDir`, `LocalBackupDir`) — edit them, rebuild, and run.

## Running on a schedule

Both clients are one-shot runs: exit code `0` means the transfer and the integrity verification succeeded, `1` means any step failed (e.g. the glob matched nothing, a file failed verification, or the server process errored). Run them from a cron job or a systemd timer.

### Cron

rsync client example:

```cron
*/5 * * * * RIP_BCK_SOURCE_DIR=/var/caasmo/backups RIP_BCK_DEST_DIR=/home/user/backups RIP_BCK_SSH_USER=backup RIP_BCK_SSH_HOST=server.example.com RIP_BCK_SSH_PORT=22 RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519 RIP_BCK_SSH_HOST_KEY_PATH=/etc/caasmo/ssh_host_ed25519_key.pub /usr/local/bin/backup-client 2>>/var/log/backup-client.log
```

### Systemd timer

Environment in a separate file:

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
