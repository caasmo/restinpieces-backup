# rsync backup daemon (`cmd/rsync-daemon`)

The rsync backup daemon performs the same transfer as the one-shot [`cmd/rsync`](../rsync) command — pulling the `latest-*.db` hard links and verifying every received database with `PRAGMA integrity_check` — but on a fixed interval instead of once. The first backup runs immediately at startup, then one per tick. See the repository [README](../../README.md) for usage.

This document records the daemon's transfer engine and, in particular, its security architecture.

# Content

- [Transfer engine: gokrazy rsync](#transfer-engine-gokrazy-rsync)
- [Security model](#security-model)
  - [The threat](#the-threat)
  - [Landlock](#landlock)
  - [Gokrazy's use of landlock](#gokrazys-use-of-landlock)
- [Systemd service](#systemd-service)
- [Security choices of this package](#security-choices-of-this-package)
  - [SSH mode](#ssh-mode)
  - [Local mode](#local-mode)

## Transfer engine: gokrazy rsync

The transfer is built on the [gokrazy rsync](https://github.com/gokrazy/rsync) implementation (`github.com/gokrazy/rsync/rsyncclient`). An rsync transfer has two roles:

- **sender** — reads the source files and streams them.
  - local mode: the system `rsync` binary, spawned as a subprocess.
  - SSH mode: the remote `rsync` binary, started over an SSH session.
- **receiver** — receives the stream and writes the files to the destination directory. The receiver is the gokrazy rsync client running **in-process** in the daemon.

## Security model

### The threat

The rsync wire protocol carries a **file list** sent by the peer: file names, paths, modes, and sizes. A malicious peer can craft a file list designed to trick the receiver into accessing or writing arbitrary paths on the client machine. The protocol is network-facing in SSH mode, and gokrazy mitigates this threat with a filesystem sandbox.

### Landlock

[Landlock](https://landlock.io) is a Linux security feature (kernel 5.13+), implemented as a Linux Security Module — the same mechanism as AppArmor.

- **Default-deny allowlist**: a process declares the paths it may access; the kernel denies every path not in the allowlist.
- **One-way**: once the ruleset is applied, it can never be loosened. Restrictions are monotonic — the process may only ever add more restrictions. The kernel assumes the process may be compromised after setup, so loosening must be impossible at the kernel level.
- **Linux primitives**: it belongs to the same family of low-level Linux sandboxing primitives as namespaces and seccomp — the same primitives systemd hardening and AppArmor build on — and was inspired by OpenBSD's `unveil`.
- **Go package**: `github.com/landlock-lsm/go-landlock`.

### Gokrazy's use of landlock

The transfer runs through gokrazy's `rsyncclient`, a Go rsync implementation that runs **in-process**, inside the daemon process. The API is explicitly built for reuse: `New`'s doc comment says *"you can call `Client.Run` one or more times with the same `Client`"*. The daemon relies on that contract: `receiver` (in `rsync/rsync.go`) caches a single `*rsyncclient.Client` and runs every daemon tick through it.

That reused client is what applies the sandbox: every `Run` calls `restrict.MaybeFileSystem`, a function from gokrazy's `internal/restrict` package (in the `github.com/gokrazy/rsync` module), which installs a landlock ruleset around the process. Because the client is reused, the ruleset is re-installed on every tick — and landlock rulesets stack: the kernel caps them at 16 per process (`LANDLOCK_MAX_NUM_LAYERS`), so the 17th transfer would fail with E2BIG. The one-shot `cmd/rsync` never hits this either: it installs the cage once at startup, before its single `Run`.

The allowlist gokrazy applies is:

- the destination directory — read/write;
- `/etc` — read-only (user/group lookup, DNS configuration).

`/etc` is granted whole, not file-by-file: Go's resolver re-reads `resolv.conf`, `hosts`, `services`, and `nsswitch.conf` on every DNS lookup, and `resolv.conf` may be recreated by DHCP or Tailscale — the individual files alone would break resolution later.

When gokrazy's sandbox is applied (before `DontRestrict` deactivated it), gokrazy's own `log.Printf` shows the ACL and its verification:

```
2026/08/09 21:53:07 setting up landlock ACL (paths ro: [], paths rw: ["."])
2026/08/09 21:53:07 landlock verified: readdir(/sys) = open /sys: permission denied
```

The first line is the allowlist declaration; the second proves the cage is live, the kernel denying `/sys` because it is not on the allowlist.

This allowlist is **hardcoded in gokrazy's internals and cannot be changed by the caller**. The `rsyncclient` API exposes exactly one control: `DontRestrict()`, which switches the sandbox on or off — there is no option to supply a custom allowlist. Because the sandbox is applied in-process, it is irreversible for the lifetime of the daemon process.

In short, gokrazy's landlock is unfit for a daemon: the client is reusable, but the sandbox re-applies on every `Run`, and recreating the client doesn't help — the stacking is per-process, not per `Client`.

## Systemd service

An optional systemd unit applies filesystem confinement equivalent to landlock's, along with additional hardening, at process start — before the daemon runs.

## Security choices of this package

**The package deactivates gokrazy's landlock in both modes** (`rsyncclient.DontRestrict()` in `newReceiver`).

### SSH mode

- **The package uses the `landlock` package.** `landlock.Restrict` applies the allowlist once at startup — after the SSH keys are in memory, before the first transfer (see "Gokrazy's use of landlock").
- **The package keeps the SSH keys in memory.** Loaded once at startup, reused on every dial — the standard `ssh-agent` pattern, the same best practice other packages follow.

### Local mode

- **The package runs without landlock.** Local mode is unsandboxed for both commands.

The systemd unit is optional: [rsync-daemon.service](rsync-daemon.service) applies equivalent confinement plus hardening at process start — defense in depth.
