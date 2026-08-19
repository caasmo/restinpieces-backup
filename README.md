# restinpieces-backup

[![Go Reference](https://pkg.go.dev/badge/github.com/caasmo/restinpieces-backup)](https://pkg.go.dev/github.com/caasmo/restinpieces-backup)
[![Test](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml)
[![golangci-lint](https://github.com/caasmo/restinpieces-backup/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/caasmo/restinpieces-backup/actions/workflows/golangci-lint.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/coverage.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/test.yml)
[![sloc](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/sloc.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/sloc.yml)
[![deps](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/caasmo/restinpieces-backup/master/.github/badges/deps.json)](https://github.com/caasmo/restinpieces-backup/actions/workflows/dependencies.yml)
[![GitHub Release](https://img.shields.io/github/v/release/caasmo/restinpieces-backup?style=flat)]()
[![Built Go](https://img.shields.io/badge/built_with-Go-00ADD8.svg?style=flat)]()

This repository holds the backup tools of a [restinpieces](https://github.com/caasmo/restinpieces) deployment. Two mechanisms live here, for two different needs:

**Snapshot pull — the backup local simple strategy.** The server side creates local snapshots of each database as `latest-*.db` hard links; these commands pull the snapshots to the backup machine and verify them:

- [`cmd/rsync`](#rsync-command-cmdrsync) — pulls the `latest-*.db` hard links via the rsync protocol (over SSH, or locally), then verifies every received database with `PRAGMA integrity_check`.
- [`cmd/rsync-daemon`](#rsync-daemon-cmdrsync-daemon) — the same rsync pull, repeated on a fixed interval by an always-on daemon.
- [`cmd/sftp`](#sftp-command-cmdsftp) — pulls the compressed snapshot archives (`.bck.gz`) over SFTP, decompresses, and verifies with `PRAGMA integrity_check`.

**Continuous replica sync — sqlite3-rsync.** A live database is kept in sync with a replica by two always-on commands that speak the [sqlite3_rsync](https://github.com/caasmo/go-sqlite-rsync) protocol:

- [`cmd/sqlite-rsync-server`](#sqlite3-rsync-origin-cmdsqlite-rsync-server) — the origin: serves databases over TCP, the client decides when to sync.
- [`cmd/sqlite-rsync-client`](#sqlite3-rsync-client-cmdsqlite-rsync-client) — the replica: connects on a fixed interval and brings the replica database up to the origin's content.

If you want point-in-time recovery, use the litestream package — see [restinpieces-litestream](https://github.com/caasmo/restinpieces-litestream).

The snapshot backup system follows a two-step push-pull design: the **server side** — creating server local snapshots via a background job — is built into restinpieces itself (see [doc/backup.md](https://github.com/caasmo/restinpieces/blob/master/doc/backup.md)). This repository provides the **client side**: one-shot binaries that run in a client machine and pull the backups and verify their integrity. The sqlite3_rsync pair turns this around: its origin server lives in this repository and runs on the machine with the live database, while the client runs on the backup machine and syncs on its own schedule.

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
- [sqlite3-rsync origin (`cmd/sqlite-rsync-server`)](#sqlite3-rsync-origin-cmdsqlite-rsync-server)
  - [Build](#build-3)
  - [Environment variables](#environment-variables-2)
- [sqlite3-rsync client (`cmd/sqlite-rsync-client`)](#sqlite3-rsync-client-cmdsqlite-rsync-client)
  - [Build](#build-4)
  - [Environment variables](#environment-variables-3)
  - [SSH mode (the default)](#ssh-mode-the-default)
  - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-2)
  - [Sync cadence](#sync-cadence)
  - [Signals](#signals-1)
  - [Security](#security-1)
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
| `RIP_BCK_SOURCE_DIR` | yes | Backup directory on the server containing the `latest-*.db` hard links. In SSH mode the directory is shell-quoted on the remote command; only the `latest-*.db` glob is expanded by the remote shell |
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
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups ./backup-client -l
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
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups RIP_BCK_INTERVAL=5m ./backup-daemon -l
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

## sqlite3-rsync origin (`cmd/sqlite-rsync-server`)

The origin server is the database side of the sqlite3_rsync pair. It listens on one TCP address; every connection names the database it wants to sync with a database label, and the server runs the origin side of the protocol for that database, sending only the pages that differ. The server is reactive: it knows no schedule, the client decides when to sync. The listener is meant for loopback: the client reaches it directly in local mode, or through this machine's SSH server in the default SSH mode (see the client's [SSH mode (the default)](#ssh-mode-the-default)). It serves a single database for now, the file given in `RIP_BCK_ORIGIN_FILE`, under the fixed label `db`.

### Build

```bash
go build -o sqlite-rsync-server ./cmd/sqlite-rsync-server
```

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_ORIGIN_LISTEN_ADDR` | yes | TCP address the server listens on, e.g. `127.0.0.1:9909` |
| `RIP_BCK_ORIGIN_FILE` | yes | Path to the origin database file, served under the fixed label `db`; the database must be in WAL mode |

A sync that runs longer than 15 minutes is aborted, releasing its read transaction on the origin database.

## sqlite3-rsync client (`cmd/sqlite-rsync-client`)

The client is the replica side of the sqlite3_rsync pair, an always-on daemon. On each interval it connects to the origin server, sends the database label, runs the replica side of the sync, and brings the replica database up to the origin's content. The first sync runs immediately at startup, then one per interval. Two transports produce the connection: the default connects over SSH and reaches the origin's loopback listener through a direct-tcpip channel; `-l`/`--local` dials the listener directly on the same machine. In SSH mode the process confines itself before the first sync.

### Build

```bash
go build -o sqlite-rsync-client ./cmd/sqlite-rsync-client
```

### Environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `RIP_BCK_REPLICA_LABEL` | yes | The database label; it must match the label the origin server serves. The replica database lives at `<dir>/<label>.db` |
| `RIP_BCK_REPLICA_DIR` | yes | Local directory the replica database is written into (created if missing) |

Everything else is hardcoded for now: the origin address (`127.0.0.1:9909`), the sync interval (15 minutes), and the SSH credentials — see [SSH mode (the default)](#ssh-mode-the-default). Edit them in `main.go`, rebuild, and run.

### SSH mode (the default)

The default transport reaches the origin over SSH. The origin server runs on the machine that also runs the system SSH server and listens on loopback (`127.0.0.1:9909`). Each sync dials that machine's sshd, authenticates with the private key, and opens a direct-tcpip channel — the SSH client asks the SSH server to connect to `127.0.0.1:9909` on its side — then runs the sync over the channel, exactly as in local mode but with the SSH hop in front. Only the system SSH server and the origin process need to run on that machine; no extra port is opened on the origin. The host key is pinned: a dial against a server with any other key fails. The credentials are hardcoded in `main.go` for now: user `backup`, host `127.0.0.1`, port `22`, private key at `/etc/restinpieces-backup/backup_ed25519`, host key at `/etc/restinpieces-backup/host_key`; the host is a placeholder that points at the local machine so SSH mode can be exercised in a development setup.

### Local mode for testing (`-l` / `--local`)

`-l` runs the sync without SSH: the client dials the origin's loopback listener directly. This is how to test the whole pair on one machine. Create a WAL-mode origin database, start the server in one terminal, then run the client in a second:

```bash
sqlite3 /tmp/origin.db "PRAGMA journal_mode=WAL; CREATE TABLE t(x); INSERT INTO t VALUES(1);"
```

```bash
# Terminal 1 — origin server
RIP_BCK_ORIGIN_LISTEN_ADDR=127.0.0.1:9909 RIP_BCK_ORIGIN_FILE=/tmp/origin.db ./sqlite-rsync-server
```

```bash
# Terminal 2 — client in local mode; the server serves the fixed label "db"
RIP_BCK_REPLICA_LABEL=db RIP_BCK_REPLICA_DIR=/tmp/replica ./sqlite-rsync-client -l
```

The client logs `starting sync` and `sync completed` immediately at startup, then stays up until the next interval. Check that the replica holds the origin's content, then Ctrl-C both processes:

```bash
sqlite3 /tmp/replica/db.db "SELECT * FROM t;"
```

### Sync cadence

- The first sync runs immediately at startup; the next one starts one full interval after the previous sync completes.
- Syncs run one at a time: a tick that fires while a sync is still running is dropped, so at most one sync per interval is guaranteed.
- A single sync is bounded by a 15-minute timeout: one that takes longer is aborted, releasing the connection.
- A failing sync (unreachable origin, rejected label, transfer error) is logged and the next tick retries — the daemon never exits on a failure.

### Signals

SIGINT, SIGQUIT, and SIGTERM trigger a graceful shutdown: the runner cancels the in-flight sync and waits up to 15 seconds for the daemon to stop. The daemon closes its connection to unblock a sync stuck reading or writing.

### Security

In SSH mode the client loads the SSH keys into memory once at startup, then confines itself with the landlock sandbox before the first sync: the process may access only the replica directory (read/write) and `/etc` (read-only), so a bug or a malicious origin cannot read or write anything beyond that. The sandbox is described in [cmd/rsync-daemon/README.md](cmd/rsync-daemon/README.md). Local mode is the trusted same-machine transport and runs unsandboxed.

## Running on a schedule

The one-shot commands (`cmd/rsync` and `cmd/sftp`) are one-shot runs: exit code `0` means the transfer and the integrity verification succeeded, `1` means any step failed (e.g. the glob matched nothing, a file failed verification, or the server process errored). Run them from a cron job or a systemd timer. The daemons (`cmd/rsync-daemon`, `cmd/sqlite-rsync-client`, `cmd/sqlite-rsync-server`) are always-on and need no scheduling.

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
