# restinpieces-backup

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/restinpieces-backup)](https://pkg.go.dev/github.com/caasmo/restinpieces-backup)
[![Test](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml)
[![golangci-lint](https://github.com/caasmo/restinpieces-backup/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup/actions/workflows/golangci-lint.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/coverage.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml)
[![sloc](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/sloc.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/sloc.yml)
[![deps](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/deps.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/dependencies.yml)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/restinpieces-backup?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

This repository holds the backup tools for a [restinpieces](https://github.com/caasmo/restinpieces) deployment.

`cmd/sqlite-rsync/origin/server` and `cmd/sqlite-rsync/replica/client` keep a live database and a replica — a second copy of the database — in continuous sync. Over the [sqlite3_rsync](https://github.com/caasmo/go-sqlite-rsync) protocol the replica receives only the parts that changed, so it always matches the live database without copying the whole file.

A snapshot is a full copy of a database, frozen at one moment in time. The snapshot tools come in two steps: making a snapshot, then moving it to another machine.

`cmd/local-copy/daemon` makes the snapshots on the machine that runs the databases: it produces a snapshot of each database at a fixed interval and keeps a hard link — a second name for the same file — to the last snapshot.

`cmd/rsync/oneshot`, `cmd/rsync/daemon`, and `cmd/sftp` move snapshots to a backup machine, so a broken server does not lose its backups. They pull the `latest-*.db` links (or the compressed `.bck.gz` archives) and verify every received database with `PRAGMA integrity_check` — SQLite's built-in check that a database file is not corrupted.

If you need to restore a database to any past moment, not just the latest snapshot, use the litestream package — see [restinpieces-litestream](https://github.com/caasmo/restinpieces-litestream).

# Content

- [sqlite3-rsync origin (`cmd/sqlite-rsync/origin/server`)](#sqlite3-rsync-origin-cmdsqlite-rsyncoriginserver)
  - [Build](#build)
  - [Configuration file](#configuration-file)
- [sqlite3-rsync client (`cmd/sqlite-rsync/replica/client`)](#sqlite3-rsync-client-cmdsqlite-rsyncreplicaclient)
  - [Build](#build-1)
  - [Environment variables](#environment-variables-1)
  - [SSH mode (the default)](#ssh-mode-the-default)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local)
  - [Signals](#signals)
  - [Security](#security)
- [local-copy daemon (`cmd/local-copy/daemon`)](#local-copy-daemon-cmdlocal-copydaemon)
  - [Build](#build-2)
  - [Configuration](#configuration)
  - [Backup cadence](#backup-cadence)
  - [Signals](#signals-1)
- [rsync one-shot (`cmd/rsync/oneshot`)](#rsync-one-shot-cmdrsynconeshot)
  - [Build](#build-3)
  - [Environment variables](#environment-variables-2)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-1)
- [rsync daemon (`cmd/rsync/daemon`)](#rsync-daemon-cmdrsyncdaemon)
  - [Build](#build-4)
  - [Environment variables](#environment-variables-3)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-2)
  - [Backup cadence](#backup-cadence-1)
  - [Signals](#signals-2)
  - [Security](#security-1)
- [sftp command (`cmd/sftp`)](#sftp-command-cmdsftp)
  - [Build](#build-5)
- [Running on a schedule](#running-on-a-schedule)
  - [Cron](#cron)
  - [Systemd timer](#systemd-timer)

## sqlite3-rsync origin (`cmd/sqlite-rsync/origin/server`)

The origin server runs on the machine that holds the live database. It listens on one TCP address, and for every connection it sends only the parts that changed, so the client receives just the changes instead of the whole file. The server never starts a sync on its own: the client decides when. It serves the databases listed in its TOML config file, one per label.

### Build

```bash
go build -o sqlite-rsync-origin ./cmd/sqlite-rsync/origin/server
```

### Configuration file

It reads a TOML config file with `-config <path>`:

```toml
[backup.files.db]
source_path = "/path/to/db"
```

## sqlite3-rsync client (`cmd/sqlite-rsync/replica/client`)

The client is an always-on daemon that copies the origin's database to a replica. On each interval it connects to the origin server and brings the replica database up to the origin's content. Two ways to connect: over SSH (the default), or directly on the same machine with `-l`/`--local`.

### Build

```bash
go build -o sqlite-rsync-client ./cmd/sqlite-rsync/replica/client
```

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_REPLICA_LABEL` | yes | The name both sides use for the database (the origin serves `db` for now); the replica lives at `<dir>/<label>.db` |
| `RIP_BCK_REPLICA_DIR` | yes | Local directory the replica database is written into (created if missing) |

Everything else is hardcoded for now: the origin address (`127.0.0.1:9909`), the sync interval (15 minutes), and the SSH credentials — see [SSH mode (the default)](#ssh-mode-the-default). Edit them in `main.go`, rebuild, and run.

### SSH mode (the default)

The default transport reaches the origin over SSH. The origin server runs on the machine that also runs the system SSH server and listens on `127.0.0.1:9909`. Each sync connects to that machine's sshd, authenticates with the private key, and asks the SSH server to open the connection to `127.0.0.1:9909` on its side — the sync then runs over that connection, exactly as in local mode but with the SSH hop in front. No extra port is opened on the origin. The host key is pinned: a connection to a server with any other key fails. The credentials are hardcoded in `main.go` for now: user `backup`, host `127.0.0.1`, port `22`, private key at `/etc/restinpieces-backup/backup_ed25519`, host key at `/etc/restinpieces-backup/host_key`; the host is a placeholder that points at the local machine so SSH mode can be exercised in a development setup.

### Local mode for testing (`-l` / `--local`)

`-l` runs the sync without SSH: the client connects to the origin's listener directly. This is how to test both programs on one machine. Start the server in one terminal, then run the client in a second:

```bash
# Terminal 1 — origin server
./sqlite-rsync-origin -config /path/to/config.toml
```

```bash
# Terminal 2 — client in local mode
RIP_BCK_REPLICA_LABEL=db RIP_BCK_REPLICA_DIR=/tmp/replica ./sqlite-rsync-client -l
```

### Signals

SIGINT, SIGQUIT, and SIGTERM stop the daemon gracefully: the in-flight sync is cancelled, the connection is closed to unblock a sync stuck reading or writing, and the process exits within 15 seconds.

### Security

In SSH mode the client loads the SSH keys into memory once at startup, then restricts itself before the first sync: the process may access only the replica directory (read/write) and `/etc` (read-only), so a bug or a malicious origin cannot read or write anything beyond that. The restriction uses the landlock sandbox described in [cmd/rsync/daemon/README.md](cmd/rsync/daemon/README.md). Local mode is the trusted same-machine transport and runs without the restriction.

## local-copy daemon (`cmd/local-copy/daemon`)

`cmd/local-copy/daemon` copies the databases on a machine into local backup directories: it produces a snapshot of each database at a fixed interval and updates a hard link to the last snapshot. The rsync and sftp commands use that link as their sync target.

### Build

```bash
go build -o local-copy ./cmd/local-copy/daemon
```

### Configuration

The daemon reads a TOML file (default `/etc/restinpieces-backup/local-copy.toml`, override with `-config <path>`). Each database is one `[files.<key>]` section; `<key>` is a name you choose, for example `app_db`:

```toml
[files.app_db]
source_path = "/data/app.db"
dest_path = "/data/backups"
frequency = "24h"
```

| Field | Description |
| --- | --- |
| `source_path` | The database file to back up (required) |
| `dest_path` | Directory the snapshots go into (required) |
| `frequency` | How often to back up this database, as a Go duration such as `24h` (required) |
| `strategy` | `online` (default) copies the live database in small steps; `vacuum` first compacts a private copy, then copies it |
| `compression` | `true` writes a gzip-compressed `.bck.gz` snapshot; `false` (default) writes a plain `.db` copy |
| `online_api_pages_per_step` | In online mode, how many pages (SQLite's unit of storage) each step copies (default 100) |
| `online_api_sleep_interval` | Pause between online steps, as a Go duration (default `10ms`) |

At startup every entry is validated — the paths must exist and `frequency` must be positive — and a broken config refuses to start. An entry whose `source_path` or `dest_path` is empty is skipped. Snapshot files are named `<key>-<name>-<timestamp>.db` (or `.bck.gz` when compressed); the `latest-` link is kept for plain snapshots only.

### Backup cadence

- The first backup runs immediately at startup; the next one starts one full interval after the previous one completes.
- The interval is the smallest `frequency` among the configured entries.
- Backups run one at a time: a tick that fires while a backup is still running is dropped.
- A failing backup is logged and retried on the next tick — the daemon never exits on a failure.

### Signals

SIGINT, SIGQUIT, and SIGTERM stop the daemon gracefully: the in-flight backup is cancelled and the process exits. A copy aborted mid-way by the shutdown is not an error — the next backup covers it.

## rsync one-shot (`cmd/rsync/oneshot`)

The rsync client runs as the receiver: it starts the `rsync` binary in server (sender) mode — over SSH, or locally on the same machine with `-l` — and pulls every `latest-*.db` file (the hard links the local-copy daemon keeps) into a local destination directory. Files are written atomically (temp file + rename), and every received database must pass `PRAGMA integrity_check`.

### Build

```bash
go build -o backup-client ./cmd/rsync/oneshot
```

The machine that runs the rsync server side (the remote host in SSH mode, the local machine in local mode) must have an rsync-compatible binary in PATH.

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_SOURCE_DIR` | yes | Backup directory on the server containing the `latest-*.db` hard links. In SSH mode the directory is shell-quoted on the remote command; only the `latest-*.db` glob is expanded by the remote shell |
| `RIP_BCK_DEST_DIR` | yes | Local directory the files are pulled into (created if missing) |
| `RIP_BCK_SSH_USER` | yes* | SSH user |
| `RIP_BCK_SSH_HOST` | yes* | SSH host |
| `RIP_BCK_SSH_PORT` | no | SSH port (default `22`) |
| `RIP_BCK_SSH_PRIVATE_KEY_PATH` | yes* | Path to the SSH private key used for authentication |
| `RIP_BCK_SSH_HOST_KEY_PATH` | yes* | Path to the server's public host key; the host key is pinned, so a dial against any other key fails |

\* Only required in SSH mode (the default). Local mode needs only `RIP_BCK_SOURCE_DIR` and `RIP_BCK_DEST_DIR`.

### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the whole pipeline without a remote machine: run it on the server itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups ./backup-client -l
```

In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

## rsync daemon (`cmd/rsync/daemon`)

The rsync daemon performs the same transfer as the [rsync one-shot](#rsync-one-shot-cmdrsynconeshot) — the receiver protocol over SSH (or locally with `-l`), pulling the `latest-*.db` hard links, atomic writes, `PRAGMA integrity_check` — but on a fixed interval instead of once. It is the always-on alternative to scheduling the one-shot command.

### Build

```bash
go build -o backup-daemon ./cmd/rsync/daemon
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

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the whole pipeline without a remote machine: run it on the server itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups RIP_BCK_INTERVAL=5m ./backup-daemon -l
```

Ctrl-C stops the daemon gracefully. In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

### Backup cadence

- The first backup runs immediately at startup; the next one starts one full interval after the previous backup completes.
- Backups run one at a time: a tick that fires while a backup is still running is dropped, so at most one backup per interval is guaranteed.
- At least one backup per interval is *not* guaranteed: when a transfer takes longer than the interval (e.g. a 12-minute transfer with a 5-minute interval), backups run back-to-back and the intended cadence is lost. Set the interval so a single backup finishes comfortably within it.
- A failing backup (transfer or verification) is logged and the next tick retries — the daemon never exits on a failure.
- The destination directory is created at startup and re-created by the receiver on every run, so a mid-run removal self-heals on the next tick.

### Signals

SIGINT, SIGQUIT, and SIGTERM stop the daemon gracefully: the in-flight transfer is cancelled and the process has up to 15 seconds to exit; a verification scan that is already running is allowed to finish within that time.

### Security

The daemon's security is documented in [cmd/rsync/daemon/README.md](cmd/rsync/daemon/README.md): the landlock sandbox and the threat it addresses, the in-memory SSH keys, and the optional systemd hardening.

## sftp command (`cmd/sftp`)

The SFTP client connects to the server with a pinned host key, opens an SFTP session, lists the remote backup directory, picks the most recent snapshot by filename (names carry a timestamp, so sorting the names finds the latest), downloads it, decompresses the `.bck.gz` archive, and verifies the resulting database with `PRAGMA integrity_check`.

### Build

```bash
go build -o sftp-client ./cmd/sftp
```

The connection parameters and directories are hardcoded in the `Config` struct at the top of `main()` (`SSHUser`, `SSHHost`, `SSHPort`, `SSHPrivateKeyPath`, `SSHHostKeyPath`, `RemoteBackupDir`, `LocalBackupDir`) — edit them, rebuild, and run.

## Running on a schedule

The one-shot commands (`cmd/rsync/oneshot` and `cmd/sftp`) are one-shot runs: exit code `0` means the transfer and the integrity verification succeeded, `1` means any step failed (e.g. the glob matched nothing, a file failed verification, or the server process errored). Run them from a cron job or a systemd timer. The daemons (`cmd/rsync/daemon`, `cmd/sqlite-rsync/replica/client`, `cmd/sqlite-rsync/origin/server`) are always-on and need no scheduling.

### Cron

rsync client example:

```cron
*/5 * * * * RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=/home/user/backups RIP_BCK_SSH_USER=backup RIP_BCK_SSH_HOST=server.example.com RIP_BCK_SSH_PORT=22 RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519 RIP_BCK_SSH_HOST_KEY_PATH=/etc/ssh_host_ed25519_key.pub /usr/local/bin/backup-client 2>>/var/log/backup-client.log
```

### Systemd timer

Environment in a separate file:

```ini
# /etc/backup-client.env
RIP_BCK_SOURCE_DIR=/var/backups
RIP_BCK_DEST_DIR=/home/user/backups
RIP_BCK_SSH_USER=backup
RIP_BCK_SSH_HOST=server.example.com
RIP_BCK_SSH_PORT=22
RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519
RIP_BCK_SSH_HOST_KEY_PATH=/etc/ssh_host_ed25519_key.pub
```

```ini
# /etc/systemd/system/backup-client.service
[Unit]
Description=restinpieces backup client (rsync pull)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
EnvironmentFile=/etc/backup-client.env
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
