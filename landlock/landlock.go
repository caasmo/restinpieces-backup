// Package landlock confines the process to the backup paths, applied
// once at startup. It replaces gokrazy's per-transfer sandbox, which
// stacks a ruleset layer on every Run and would exhaust the kernel's
// 16-layer cap in a daemon.
package landlock

import (
	"fmt"
	"log/slog"
	"os"

	ll "github.com/landlock-lsm/go-landlock/landlock"
)

// Restrict confines the process to the destination directory
// (read/write) and /etc (read-only, for DNS and the host key) — the
// allowlist gokrazy's receiver would otherwise apply per transfer.
// Call once at startup, after the SSH keys are loaded into memory and
// before the first transfer. Best-effort: on kernels without landlock
// this is a no-op.
//
// The allowlist covers exactly the paths the transfer touches. Any
// filesystem operation after the transfer (e.g. copying a file from
// the source directory) must be allowed here first: add the source
// path read-only, plus any other paths the operation needs, or the
// cage denies the access.
//
// The go-landlock version is our own: go.mod pins v0.9.0 directly,
// not gokrazy's older pin of the same library. Go's minimal version
// selection builds everything against our v0.9.0 — safe because the
// API gokrazy's internal/restrict uses there is unchanged and its cage
// is deactivated anyway (rsync.DontRestrict), so this Restrict is the
// only landlock applied.
func Restrict(destDir string) error {
	slog.Info("setting up landlock ACL", "paths_ro", []string{"/etc"}, "paths_rw", []string{destDir})

	err := ll.V3.BestEffort().RestrictPaths(
		ll.RODirs("/etc").IgnoreIfMissing(),
		ll.RWDirs(destDir).WithRefer(),
	)
	if err != nil {
		return fmt.Errorf("failed to apply landlock: %w", err)
	}
	return nil
}

// Verify proves the cage is live by attempting to read a path that is
// never on the allowlist (/sys) and logging the expected denial — the
// same check gokrazy's internal/restrict performs after applying.
// Call after Restrict.
func Verify() {
	// We use /sys because that path should never be required for
	// regular functioning, yet is standard enough to be present on all
	// supported Linux versions (including gokrazy).
	const verifyPath = "/sys"
	_, err := os.ReadDir(verifyPath)
	if err == nil {
		slog.Info("landlock seems ineffective: readdir(/sys) unexpectedly worked!")
	} else {
		slog.Info("landlock verified: readdir(/sys) denied", "error", err)
	}
}
