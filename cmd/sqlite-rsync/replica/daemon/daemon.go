package main

import (
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
)

// defaultSyncInterval is the time between syncs when the configuration
// leaves the interval at zero. It is the cadence, not a sync bound —
// the sync bound is defaultSyncTimeout (client.go). The value
// coincidentally matches defaultSyncTimeout and main's syncInterval;
// each has its own meaning and may diverge.
const defaultSyncInterval = 15 * time.Minute

// ReplicaDaemon runs the replica sync on a fixed interval. The first
// sync runs immediately at startup; subsequent syncs follow the
// interval.
//
// The tick body is synchronous: Run's goroutine performs the sync
// inline, so only one sync executes at a time. Ticks that fire while a
// sync is executing are dropped — the ticker buffers a single tick and
// discards the rest — and the next sync starts only when the current
// one finishes.
//
// The daemon satisfies the daemon.Daemon contract: Run spawns the sync
// goroutine, Stop cancels the daemon context (the goroutine aborts an
// in-flight sync and exits — the sync, its preamble write, and the
// dial all respect the cancelled context), then signals completion. It
// is constructed from plain values and reads no environment itself.
type ReplicaDaemon struct {
	daemon.Base
	client   Client
	files    map[string]string
	interval time.Duration
}

// NewReplicaDaemon creates the daemon around the already constructed
// client and the label-to-replica-path mapping. main builds the client
// (from -l/--local and the environment) and passes it in; the daemon
// reads no environment itself. A nil logger falls back to
// slog.Default().
func NewReplicaDaemon(client Client, files map[string]string, interval time.Duration, logger *slog.Logger) *ReplicaDaemon {
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	d := &ReplicaDaemon{
		Base:     daemon.NewBase("ReplicaDaemon", logger),
		client:   client,
		files:    files,
		interval: interval,
	}
	// Every daemon log line carries the daemon's identity: reuse the
	// daemon_name attribute the runner attaches to lifecycle logs.
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run spawns the daemon's goroutine. One sync runs immediately at
// startup, then one per tick; Ctx cancellation (from Stop) aborts an
// in-flight sync — including a stuck dial or preamble write — and
// unblocks the select, letting the goroutine exit cleanly.
func (d *ReplicaDaemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		// Run once immediately at startup, then on the interval. The
		// ticker starts after this first sync, so the first tick is
		// one full interval after it completes.
		d.sync()

		ticker := time.NewTicker(d.interval)
		defer ticker.Stop()

		for {
			select {
			case <-d.Ctx.Done():
				return
			case <-ticker.C:
				d.sync()
			}
		}
	}()
	return nil
}

// sync runs one sync cycle: every configured database in turn. The
// sync is abortable via d.Ctx: Stop cancels it and the client aborts
// the dial, the preamble write, and the sync itself. A failed sync is
// logged and the next interval tries again.
//
// files is a map, so the iteration order is non-deterministic once
// more than one database is configured; with a single database the
// order is irrelevant.
func (d *ReplicaDaemon) sync() {
	for label, path := range d.files {
		d.syncOne(label, path)
	}
}

// syncOne runs one database's sync, recovering from a panic so a bug
// in the library parsing the origin's input cannot take down the
// daemon. It is the per-database unit of the sync cycle.
func (d *ReplicaDaemon) syncOne(label, path string) {
	log := d.Logger.With("label", label, "replica_path", path)
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic during sync", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	log.Info("requesting sync from origin")
	start := time.Now()
	stats, err := d.client.Run(d.Ctx, label, path)
	if err != nil {
		log.Error("sync failed", "error", err)
		return
	}
	logSyncSummary(log, stats, time.Since(start))
}

// logSyncSummary logs the completed line of one sync with the run's
// per-run summary: the raw counters (Stats) plus the derived
// presentation values of the C -v printout (sqlite3_rsync.c
// L2392-2417) — database size, transfer speed and speedup. speedup
// follows the C guard (L2408-2414): it is 0 when the wire traffic was
// larger than the database, where C omits the value.
func logSyncSummary(log *slog.Logger, stats sqlitersync.Stats, elapsed time.Duration) {
	wireBytes := stats.BytesSent + stats.BytesReceived
	dbSize := uint64(stats.PageCount) * uint64(stats.PageSize)
	bytesPerSec := 0
	if elapsed > 0 {
		bytesPerSec = int(float64(wireBytes) / elapsed.Seconds())
	}
	speedup := 0
	if wireBytes > 0 && wireBytes <= dbSize {
		speedup = int(float64(dbSize) / float64(wireBytes))
	}
	log.Info("sync completed",
		"db_size", dbSize,
		"page_count", stats.PageCount,
		"page_size", stats.PageSize,
		"bytes_sent", stats.BytesSent,
		"bytes_received", stats.BytesReceived,
		"page_updates", stats.PageUpdates,
		"hash_messages", stats.HashMessages,
		"hash_rounds", stats.HashRounds,
		"protocol", stats.Protocol,
		"bytes_per_sec", bytesPerSec,
		"speedup", speedup,
		"elapsed", int(elapsed.Seconds()),
	)
}
