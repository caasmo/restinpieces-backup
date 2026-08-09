# rsync backup daemon (`cmd/rsync-daemon`)

The rsync backup daemon performs the same transfer as the one-shot [`cmd/rsync`](../rsync) command — pulling the `latest-*.db` hard links and verifying every received database with `PRAGMA integrity_check` — but on a fixed interval instead of once. The first backup runs immediately at startup, then one per tick. See the repository [README](../../README.md) for usage.

This document records the daemon's transfer engine and, in particular, its security architecture.

# Content

- [Transfer engine: gokrazy rsync](#transfer-engine-gokrazy-rsync)
- [Security model](#security-model)
  - [The threat](#the-threat)
  - [Landlock](#landlock)
  - [Gokrazy's use of landlock](#gokrazys-use-of-landlock)
- [Security choices](#security-choices)
  - [SSH mode](#ssh-mode)
  - [Local mode](#local-mode)
- [Systemd service](#systemd-service)

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

The gokrazy receiver runs in-process on the **client machine**, inside the daemon process, and applies landlock to it via `restrict.MaybeFileSystem` (`internal/restrict`). The allowlist it grants is:

- the destination directory — read/write;
- `/etc` — read-only (user/group lookup, DNS configuration);
- `/tmp` — read-only, on gokrazy platform builds.

When the sandbox is applied, the daemon's log shows the ACL and its verification:

```
2026/08/09 21:53:07 setting up landlock ACL (paths ro: [], paths rw: ["."])
2026/08/09 21:53:07 landlock verified: readdir(/sys) = open /sys: permission denied
```

The first line is the allowlist declaration; the second proves the cage is live, the kernel denying `/sys` because it is not on the allowlist.

This allowlist is **hardcoded in gokrazy's internals and cannot be changed by the caller**. The `rsyncclient` API exposes exactly one control: `DontRestrict()`, which switches the sandbox on or off — there is no option to supply a custom allowlist. Because the sandbox is applied in-process, it is irreversible for the lifetime of the daemon process.

## Security choices

### SSH mode

- **Landlock is kept.** The receiver stays sandboxed; the SSH transfer happens under the cage.
- **In-memory SSH keys.** The cage blocks file reads outside the destination directory and `/etc`, so per-dial reads of the private and host key files would fail after the first transfer. Instead the keys are loaded once at startup and kept in memory, reused on every dial — the standard `ssh-agent` pattern. The key material is in process memory during any dial regardless, so caching adds no new exposure.

### Local mode

- **Landlock is deactivated** (`rsyncclient.DontRestrict()`).
- Reasoning:
  1. The threat is moot. The threat model is a malicious peer sending crafted file lists over the network. Local mode has no network peer: the sender is the daemon's own `rsync` subprocess and the data is the daemon's own backups on the same machine.
  2. The cage is incompatible with a long-lived daemon. The sandbox is installed mid-run and is irreversible, yet the daemon must re-read the source directory on every tick to discover new `latest-*.db` files. A process that must keep reading the source cannot live inside a cage that permanently denies it.
- The one-shot `cmd/rsync` keeps the sandbox: it runs a single transfer and exits, so the cage never has a second access to deny.

## Systemd service

The package provides a systemd service unit that applies filesystem confinement equivalent to landlock's, along with additional hardening, at process start — before the daemon runs. Applied up front, the confinement cannot break a long-lived daemon the way gokrazy's mid-run landlock does. This is defense in depth: the systemd hardening layers on top of the daemon's own security choices.
