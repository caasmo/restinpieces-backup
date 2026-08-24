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

It implements the three standard SQLite backup methods: the [Online Backup API](https://www.sqlite.org/backup.html) ([`cmd/onlineapi`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/onlineapi)) and [`VACUUM INTO`](https://www.sqlite.org/lang_vacuum.html) ([`cmd/vacuum`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/vacuum)) for local backups on the same machine, and the [sqlite3_rsync](https://github.com/caasmo/go-sqlite-rsync) protocol ([`cmd/sqlite-rsync`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/sqlite-rsync)) for remote backups to another machine.

For point-in-time restores and syncing to S3 and other object stores, see [restinpieces-litestream](https://github.com/caasmo/restinpieces-litestream).

# Content

- [sqlite3-rsync (`cmd/sqlite-rsync`)](#sqlite3-rsync-cmdsqlite-rsync)
  - [restinpieces integration (`cmd/sqlite-rsync/origin/restinpieces`)](#restinpieces-integration-cmdsqlite-rsyncoriginrestinpieces)
  - [origin daemon (`cmd/sqlite-rsync/origin/daemon`)](#origin-daemon-cmdsqlite-rsyncorigindaemon)
    - [Build](#build)
    - [Configuration file](#configuration-file)
  - [replica daemon (`cmd/sqlite-rsync/replica/daemon`)](#replica-daemon-cmdsqlite-rsyncreplicadaemon)
    - [Build](#build-1)
    - [Configuration](#configuration)
    - [SSH mode (the default)](#ssh-mode-the-default)
    - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local)
    - [Signals](#signals)
    - [Security](#security)
- [online API (`cmd/onlineapi`)](#online-api-cmdonlineapi)
  - [restinpieces integration (`cmd/onlineapi/restinpieces`)](#restinpieces-integration-cmdonlineapirestinpieces)
  - [standalone daemon (`cmd/onlineapi/daemon`)](#standalone-daemon-cmdonlineapidaemon)
    - [Build](#build-2)
    - [Configuration](#configuration-1)
- [VACUUM (`cmd/vacuum`)](#vacuum-cmdvacuum)
  - [restinpieces integration (`cmd/vacuum/restinpieces`)](#restinpieces-integration-cmdvacuumrestinpieces)
  - [standalone daemon (`cmd/vacuum/daemon`)](#standalone-daemon-cmdvacuumdaemon)
    - [Build](#build-3)
    - [Configuration](#configuration-2)
- [rsync (`cmd/rsync`)](#rsync-cmdrsync)
  - [rsync one-shot (`cmd/rsync/oneshot`)](#rsync-one-shot-cmdrsynconeshot)
    - [Build](#build-4)
    - [Environment variables](#environment-variables)
    - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-1)
  - [rsync daemon (`cmd/rsync/daemon`)](#rsync-daemon-cmdrsyncdaemon)
    - [Build](#build-5)
    - [Environment variables](#environment-variables-1)
    - [Local mode for testing (`-l` / `--local`)](#local-mode-for-testing--l----local-2)
    - [Backup cadence](#backup-cadence)
    - [Signals](#signals-1)
    - [Security](#security-1)
  - [sftp one-shot (`cmd/sftp/oneshot`)](#sftp-one-shot-cmdsftponeshot)
    - [Build](#build-6)
  - [Running on a schedule](#running-on-a-schedule)
    - [Cron](#cron)
    - [Systemd timer](#systemd-timer)

## sqlite3-rsync (`cmd/sqlite-rsync`)

The [sqlite3_rsync](https://github.com/caasmo/go-sqlite-rsync) protocol syncs a origin database and a replica by sending only the parts that changed, so it always matches the live database without copying the whole file.

This repository ships the protocol in two forms: a [restinpieces framework implementation](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/sqlite-rsync/origin/restinpieces) that embeds the origin role inside a restinpieces app, and a pair of mostly standalone [origin](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/sqlite-rsync/origin/daemon) and [replica](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/sqlite-rsync/replica/daemon) daemons that run without the framework.

### restinpieces integration (`cmd/sqlite-rsync/origin/restinpieces`)

It embeds the origin role inside a restinpieces application as a [daemon](https://pkg.go.dev/github.com/caasmo/restinpieces-backup/sqlitersync/origin): the daemon creates a loopback listener and waits for the replica to reqeust updates of its configured databases. It reads the `[backup.sqlite-rsync]` section from the application's config.

The complete, runnable example is in [`main.go`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/sqlite-rsync/origin/restinpieces/main.go): it builds the application, creates the origin daemon from the app's config pointer, registers it with `srv.AddDaemon`, then runs the server. The daemon comes from the `github.com/caasmo/restinpieces-backup/sqlitersync/origin` package.

After that configure which databases to sync with the `ripc` tool:

```bash
ripc scaffold backup-sqlite-rsync app-rsync
ripc set backup.sqlite-rsync.entries.app-rsync.source_path /path/to/app.db
```

After that reload the application configuration. 

### origin daemon (`cmd/sqlite-rsync/origin/daemon`)

A Go daemon implementing the origin role of the sqlite3-rsync protocol, for showcasing the protocol and for use outside restinpieces.

It runs on the machine that holds the live database and listens on one TCP address. For every connection it sends only the parts that changed, so the client receives just the changes instead of the whole file.

#### Build

```bash
go build -o sqlite-rsync-origin ./cmd/sqlite-rsync/origin/daemon
```

#### Configuration file

It reads a TOML config file with `-config <path>`. It uses the same shape `ripc` scaffolds:

```toml
[backup.sqlite-rsync]
listen_addr = "127.0.0.1:54321"

[backup.sqlite-rsync.entries.db]
source_path = "/path/to/db"
sync_timeout = "15m"
```

### replica daemon (`cmd/sqlite-rsync/replica/daemon`)

A Go daemon implementing the replica role of the sqlite3-rsync protocol, for showcasing the protocol and for use outside restinpieces. It is an always-on client that pulls the origin's databases to local replica files on a fixed interval.

Every `[entries.<name>]` entry names a database the origin serves, the local file the replica writes, and how often to pull it. Two ways to connect: over SSH (the default), or directly on the same machine with `-l`/`--local`.

#### Build

```bash
go build -o sqlite-rsync-client ./cmd/sqlite-rsync/replica/daemon
```

#### Configuration

The daemon reads one TOML document given by `-config`; the document root is the replica configuration. `origin_addr` is the dial target of the origin listener. `sync_timeout` caps every single pull and is required. The optional `[ssh]` block selects the SSH transport; leaving it out requires `-l`/`--local`, and when present its `port` is required. Each `[entries.<name>]` entry pulls the named database into `path` every `frequency`; a zero frequency disables the entry. The parent directory of every configured path is created at startup if missing.

```toml
origin_addr = "127.0.0.1:54321"
sync_timeout = "15m"

[ssh]
user = "backup"
host = "127.0.0.1"
port = "22"
private_key_path = "/etc/restinpieces-backup/backup_ed25519"
host_key_path = "/etc/restinpieces-backup/host_key"

[entries.logs]
frequency = "30s"
path = "/var/backups/logs.db"

[entries.app]
frequency = "15m"
path = "/var/backups/app.db"
```

#### SSH mode (the default)

The default transport reaches the origin over SSH. The origin listens on `127.0.0.1:54321`. Each sync connects to the origin machine's sshd, authenticates, and asks sshd to open `127.0.0.1:54321` on its side. The sync runs over that connection, as in local mode but with an SSH hop. No extra port is opened. The host key is pinned. The credentials come from the `[ssh]` block; the keys load into memory once at startup and are reused on every dial.

#### Local mode for testing (`-l` / `--local`)

`-l` runs the sync without SSH: the client connects to the origin's listener directly. This is how to test both programs on one machine. Start the server in one terminal, then run the client in a second:

```bash
# Terminal 1 — origin server
./sqlite-rsync-origin -config /path/to/config.toml
```

```bash
# Terminal 2 — client in local mode
./sqlite-rsync-client -config /path/to/replica.toml -l
```

#### Signals

SIGINT, SIGQUIT, and SIGTERM stop the daemon gracefully: the in-flight sync is cancelled, the connection is closed to unblock a sync stuck reading or writing, and the process exits within 15 seconds.

#### Security

In SSH mode the client loads the SSH keys into memory once at startup. It runs without filesystem confinement by design: the sqlite-rsync protocol is label-addressed — no filesystem path crosses the wire — and each sync applies the received stream to exactly one pre-configured replica file, so a bug or a malicious origin cannot steer reads or writes anywhere else. The full rationale lives in [AGENTS.md](AGENTS.md). Local mode is the trusted same-machine transport.

## online API (`cmd/onlineapi`)

The [Online Backup API](https://www.sqlite.org/backup.html) is SQLite's built-in way to copy a live database: the copy runs while the database keeps being written, so a backup never blocks the application.

This repository ships the Online Backup API in two forms: a [restinpieces framework implementation](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/onlineapi/restinpieces) that registers the daemon inside a restinpieces app, and a standalone [daemon](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/onlineapi/daemon) that runs outside restinpieces.

### restinpieces integration (`cmd/onlineapi/restinpieces`)

It embeds the onlineapi daemon inside a restinpieces application: the daemon produces snapshots of the databases configured in the backup section. The complete, runnable example is in [`main.go`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/onlineapi/restinpieces/main.go): it builds the application, creates the onlineapi daemon from the app's config pointer, registers it with `srv.AddDaemon`, then runs the server.

Configure which databases to back up with the `ripc` tool:

```bash
ripc scaffold backup-online app-online
ripc set backup.online.app-online.source_path /path/to/db
```

A SIGHUP reload of the application configuration is visible at the next daemon tick.

### standalone daemon (`cmd/onlineapi/daemon`)

A Go daemon running the Online Backup API outside restinpieces, on any machine that holds live databases. It copies each database into a local backup directory while the database keeps being written, producing a snapshot at a fixed interval and updating a hard link to the last snapshot.

The rsync and sftp commands use that link as their sync target.

#### Build

```bash
go build -o onlineapi ./cmd/onlineapi/daemon
```

#### Configuration

The daemon reads a TOML file (default `/etc/restinpieces-backup/onlineapi.toml`, override with `-config <path>`). It uses the same `[backup]` shape the restinpieces application uses: each database is one `[backup.online.<key>]` section; `<key>` is a label you choose, for example `app-online`:

```toml
[backup.online.app-online]
source_path = "/data/app.db"
dest_path = "/data/backups"
frequency = "24h"
pages_per_step = 100
sleep_interval = "10ms"
```

## VACUUM (`cmd/vacuum`)

The [`VACUUM INTO`](https://www.sqlite.org/lang_vacuum.html) command writes a clean, defragmented copy of a database to a new file, so the snapshot stays compact and consistent while the database keeps being written.

This repository ships VACUUM INTO in two forms: a [restinpieces framework implementation](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/vacuum/restinpieces) that registers the daemon inside a restinpieces app, and a standalone [daemon](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/vacuum/daemon) that runs outside restinpieces.

### restinpieces integration (`cmd/vacuum/restinpieces`)

It embeds the vacuum daemon inside a restinpieces application: the daemon produces VACUUM INTO snapshots of the databases configured in the backup section. The complete, runnable example is in [`main.go`](https://github.com/caasmo/restinpieces-backup/tree/master/cmd/vacuum/restinpieces/main.go): it builds the application, creates the vacuum daemon from the app's config pointer, registers it with `srv.AddDaemon`, then runs the server.

Configure which databases to back up with the `ripc` tool:

```bash
ripc scaffold backup-vacuum app-vacuum
ripc set backup.vacuum.app-vacuum.source_path /path/to/db
```

A SIGHUP reload of the application configuration is visible at the next daemon tick.

### standalone daemon (`cmd/vacuum/daemon`)

A Go daemon running `VACUUM INTO` outside restinpieces, on any machine that holds live databases. It produces a clean, defragmented snapshot of each database at a fixed interval and updates a hard link to the last snapshot.

The rsync and sftp commands use that link as their sync target.

#### Build

```bash
go build -o vacuum ./cmd/vacuum/daemon
```

#### Configuration

The daemon reads a TOML file (default `/etc/restinpieces-backup/vacuum.toml`, override with `-config <path>`). It uses the same `[backup]` shape the restinpieces application uses: each database is one `[backup.vacuum.<key>]` section:

```toml
[backup.vacuum.app-vacuum]
source_path = "/data/other.db"
dest_path = "/data/backups"
frequency = "24h"
```

## rsync (`cmd/rsync`)

### rsync one-shot (`cmd/rsync/oneshot`)

The rsync client runs as the receiver: it starts the `rsync` binary in server (sender) mode — over SSH, or locally on the same machine with `-l` — and pulls every `latest-*.db` file (the hard links the local-copy daemon keeps) into a local destination directory. Files are written atomically (temp file + rename), and every received database must pass `PRAGMA integrity_check`.

#### Build

```bash
go build -o backup-client ./cmd/rsync/oneshot
```

The machine that runs the rsync server side (the remote host in SSH mode, the local machine in local mode) must have an rsync-compatible binary in PATH.

#### Environment variables

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

#### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the whole pipeline without a remote machine: run it on the server itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups ./backup-client -l
```

In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

### rsync daemon (`cmd/rsync/daemon`)

The rsync daemon performs the same transfer as the [rsync one-shot](#rsync-one-shot-cmdrsynconeshot) — the receiver protocol over SSH (or locally with `-l`), pulling the `latest-*.db` hard links, atomic writes, `PRAGMA integrity_check` — but on a fixed interval instead of once. It is the always-on alternative to scheduling the one-shot command.

#### Build

```bash
go build -o backup-daemon ./cmd/rsync/daemon
```

The machine that runs the rsync server side (the remote host in SSH mode, the local machine in local mode) must have an rsync-compatible binary in PATH.

#### Environment variables

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

#### Local mode for testing (`-l` / `--local`)

`-l` runs the whole pipeline without SSH: the client starts the local `rsync` binary in server mode on the same machine and pulls from a local `RIP_BCK_SOURCE_DIR`. This is how to test the whole pipeline without a remote machine: run it on the server itself (where the backup directory with the `latest-*.db` hard links already lives) or on any machine that has a copy of the source directory and an `rsync` binary in PATH:

```bash
RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=./backups RIP_BCK_INTERVAL=5m ./backup-daemon -l
```

Ctrl-C stops the daemon gracefully. In local mode the source glob is expanded by the client itself (there is no shell in between), so zero matches fail before the transfer starts with `no backup files received: server glob matched nothing`.

#### Backup cadence

- The first backup runs immediately at startup; the next one starts one full interval after the previous backup completes.
- Backups run one at a time: a tick that fires while a backup is still running is dropped, so at most one backup per interval is guaranteed.
- At least one backup per interval is *not* guaranteed: when a transfer takes longer than the interval (e.g. a 12-minute transfer with a 5-minute interval), backups run back-to-back and the intended cadence is lost. Set the interval so a single backup finishes comfortably within it.
- A failing backup (transfer or verification) is logged and the next tick retries — the daemon never exits on a failure.
- The destination directory is created at startup and re-created by the receiver on every run, so a mid-run removal self-heals on the next tick.

#### Signals

SIGINT, SIGQUIT, and SIGTERM stop the daemon gracefully: the in-flight transfer is cancelled and the process has up to 15 seconds to exit; a verification scan that is already running is allowed to finish within that time.

#### Security

The daemon's security is documented in [cmd/rsync/daemon/README.md](cmd/rsync/daemon/README.md): the landlock sandbox and the threat it addresses, the in-memory SSH keys, and the optional systemd hardening.

### sftp one-shot (`cmd/sftp/oneshot`)

The SFTP client connects to the server with a pinned host key, opens an SFTP session, lists the remote backup directory, picks the most recent snapshot by filename (names carry a timestamp, so sorting the names finds the latest), downloads it, decompresses the `.bck.gz` archive, and verifies the resulting database with `PRAGMA integrity_check`.

#### Build

```bash
go build -o sftp-client ./cmd/sftp/oneshot
```

The connection parameters and directories are hardcoded in the `Config` struct at the top of `main()` (`SSHUser`, `SSHHost`, `SSHPort`, `SSHPrivateKeyPath`, `SSHHostKeyPath`, `RemoteBackupDir`, `LocalBackupDir`) — edit them, rebuild, and run.

### Running on a schedule

The one-shot commands (`cmd/rsync/oneshot` and `cmd/sftp/oneshot`) are one-shot runs: exit code `0` means the transfer and the integrity verification succeeded, `1` means any step failed (e.g. the glob matched nothing, a file failed verification, or the server process errored). Run them from a cron job or a systemd timer. The daemons (`cmd/rsync/daemon`, `cmd/sqlite-rsync/replica/daemon`, `cmd/sqlite-rsync/origin/daemon`) are always-on and need no scheduling.

#### Cron

rsync client example:

```cron
*/5 * * * * RIP_BCK_SOURCE_DIR=/var/backups RIP_BCK_DEST_DIR=/home/user/backups RIP_BCK_SSH_USER=backup RIP_BCK_SSH_HOST=server.example.com RIP_BCK_SSH_PORT=22 RIP_BCK_SSH_PRIVATE_KEY_PATH=/home/user/.ssh/id_ed25519 RIP_BCK_SSH_HOST_KEY_PATH=/etc/ssh_host_ed25519_key.pub /usr/local/bin/backup-client 2>>/var/log/backup-client.log
```

#### Systemd timer

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
