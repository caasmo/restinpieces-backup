package main

import (
	"log/slog"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/restinpieces-backup-client/rsync"
)

// BackupDaemon runs the rsync backup on a fixed interval. The first
// backup runs immediately at startup; subsequent backups follow the
// interval.
//
// The tick body is synchronous: Run's goroutine performs the transfer
// and the verification inline, so only one backup executes at a time.
// Ticks that fire while a backup is executing are dropped — the ticker
// buffers a single tick and discards the rest — and the next backup
// starts only when the current one finishes.
//
// Consequences of the synchronous body:
//   - At most one backup per interval is guaranteed: two transfers can
//     never race on the destination directory.
//   - At least one backup per interval is NOT guaranteed. When a backup
//     takes longer than the interval, e.g. a 12-minute transfer with a
//     5-minute interval, backups run back-to-back at transfer speed
//     (one every 12 minutes) and the intended cadence is lost.
//
// The administrator should set the interval so that a single backup
// finishes comfortably within it, guaranteeing at least one backup
// per interval.
type BackupDaemon struct {
	daemon.Base
	client   rsync.Client
	destDir  string
	interval time.Duration
}

// NewBackupDaemon creates the daemon around an already constructed
// rsync client. main builds the client (from -l/--local and the
// environment) and passes it in; the daemon reads no environment
// itself. A nil logger falls back to slog.Default().
func NewBackupDaemon(client rsync.Client, destDir string, interval time.Duration, logger *slog.Logger) *BackupDaemon {
	d := &BackupDaemon{
		Base:     daemon.NewBase("BackupDaemon", logger),
		client:   client,
		destDir:  destDir,
		interval: interval,
	}
	// Every daemon log line carries the daemon's identity: reuse the
	// daemon_name attribute the runner attaches to lifecycle logs. The
	// one exception is the shared rsync package's transfer-stat line
	// ("rsync transfer completed"), which logs via the package logger
	// and so lacks daemon_name — accepted trade-off: the daemon is the
	// process's only daemon, making the correlation unambiguous.
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run spawns the daemon's goroutine. One backup runs immediately at
// startup, then one per tick; Ctx cancellation (from Stop) aborts an
// in-flight transfer and unblocks the select, letting the goroutine
// exit cleanly.
func (d *BackupDaemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		// Run once immediately at startup, then on the interval. The
		// ticker starts after this first backup, so the first tick is
		// one full interval after it completes.
		d.backup()

		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()

		for {
			select {
			case <-d.Ctx.Done():
				return
			case <-ticker.C:
				d.backup()
			}
		}
	}()
	return nil
}

// backup runs one backup cycle: the transfer, then the verification.
// The transfer is abortable via d.Ctx (Stop cancels it and the rsync
// client aborts). The verification is deliberately non-cancellable
// (rsync.VerifyBackup): it is a local read-only scan, so a Stop
// landing during it lets the scan run to completion, bounded by the
// runner's shutdown deadline. Verification is the caller's job (the
// rsync package never calls it); the daemon performs it right after
// the transfer, as cmd/rsync does.
func (d *BackupDaemon) backup() {
	err := d.client.Run(d.Ctx)
	if err != nil {
		d.Logger.Error("backup failed", "error", err)
		return
	}
	err = rsync.VerifyBackup(d.destDir)
	if err != nil {
		d.Logger.Error("backup verification failed", "error", err)
		return
	}
	d.Logger.Info("backup completed")
}
