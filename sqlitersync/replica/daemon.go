// Package replica runs the pulling side of the sqlite3_rsync backup
// flow. The replica daemon asks the origin server for the database
// behind each configured entry name and applies the received pages to
// that entry's local replica file. It owns the schedule: the origin is
// reactive and decides nothing.
//
// How connections reach the origin is an implementation detail owned
// by internal/sqlitersync/replica; the daemon only schedules Client
// runs. An embedder needing another transport implements the Client
// interface and passes it to New.
package replica

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"runtime/debug"
	"slices"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	replicaconfig "github.com/caasmo/restinpieces-backup/config/sqlitersync/replica"
	replicaclient "github.com/caasmo/restinpieces-backup/internal/sqlitersync/replica"
)

// ReplicaDaemon pulls every configured entry from the origin server
// on its own cadence. The first round runs immediately at startup;
// afterwards the daemon ticks at the smallest configured frequency
// and syncs the entries that are due — the ones whose replica file's
// mtime is at least one frequency old, plus the ones never synced
// (missing file). This is the same division as internal/localcopy:
// cadence is per entry, the ticker falls out of the smallest one.
//
// Entries are visited in name order every round, so the sync cycle is
// deterministic regardless of map iteration order.
//
// The tick body is synchronous: Run's goroutine performs the syncs
// inline, so only one sync executes at a time. Ticks that fire while
// a sync is executing are dropped — the ticker buffers a single tick
// and discards the rest — and the next round starts only when the
// current one finishes.
//
// The daemon satisfies the daemon.Daemon contract: Run spawns the sync
// goroutine, Stop cancels the daemon context (the goroutine aborts an
// in-flight sync and exits — the sync, its preamble write, and the
// dial all respect the cancelled context), then signals completion. It
// is constructed from plain values and reads no environment itself.
type ReplicaDaemon struct {
	daemon.Base
	client replicaclient.Client
	config replicaconfig.Config
}

// New creates the daemon around the already constructed client and
// the parsed configuration. main builds the client
// (from -l/--local and the configuration) and passes the parsed
// document in; the daemon reads no environment itself. A nil logger
// falls back to slog.Default().
func New(client replicaclient.Client, config replicaconfig.Config, logger *slog.Logger) *ReplicaDaemon {
	d := &ReplicaDaemon{
		Base:   daemon.NewBase("ReplicaDaemon", logger),
		client: client,
		config: config,
	}
	// Every daemon log line carries the daemon's identity: reuse the
	// daemon_name attribute the runner attaches to lifecycle logs.
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run spawns the daemon's goroutine. One round runs immediately at
// startup, then one per tick; Ctx cancellation (from Stop) aborts an
// in-flight sync — including a stuck dial or preamble write — and
// unblocks the select, letting the goroutine exit cleanly.
func (d *ReplicaDaemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		// Stop may fire before this goroutine runs; do not run a
		// doomed first round. Otherwise run once immediately at
		// startup, then on the interval — the ticker starts after
		// this first round, so the first tick is one full interval
		// after it completes.
		ctxErr := d.Ctx.Err()
		if ctxErr != nil {
			return // stopped before the first round
		}
		d.logLabels()
		d.sync(time.Now())

		interval := d.interval()
		if interval <= 0 {
			d.Logger.Info("no active entries: all frequencies are zero/disabled; daemon idle")
			return
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-d.Ctx.Done():
				return
			case <-ticker.C:
				d.sync(time.Now())
			}
		}
	}()
	return nil
}

// logLabels logs the label of every active entry the daemon pulls in
// one line, so startup shows which databases the replica syncs from
// the origin. Disabled entries (zero frequency) are omitted.
func (d *ReplicaDaemon) logLabels() {
	labels := make([]string, 0, len(d.config.Entries))
	for _, name := range slices.Sorted(maps.Keys(d.config.Entries)) {
		if d.config.Entries[name].Frequency.Duration > 0 {
			labels = append(labels, name)
		}
	}
	d.Logger.Info("pulling", "labels", labels)
}

// sync runs one sync round over every configured entry in turn,
// syncing only the entries that are due. A failed sync is logged and
// the next tick tries again.
func (d *ReplicaDaemon) sync(now time.Time) {
	for _, name := range slices.Sorted(maps.Keys(d.config.Entries)) {
		entry := d.config.Entries[name]
		if entry.Frequency.Duration <= 0 {
			d.Logger.Debug("entry disabled, skipping", "label", name)
			continue
		}
		if !isSyncDue(entry.Path, entry.Frequency.Duration, now) {
			continue
		}
		d.syncOne(name, entry.Path)
	}
}

// interval returns the tick cadence: the smallest frequency among the
// active entries. Every entry's frequency is at least this large, so
// each entry is checked at least once per its own frequency; entries
// that are not yet due are skipped by the due check. It returns zero
// when no entry is active, in which case Run never starts the ticker.
func (d *ReplicaDaemon) interval() time.Duration {
	var min time.Duration
	for _, entry := range d.config.Entries {
		if entry.Frequency.Duration <= 0 {
			continue
		}
		if min == 0 || entry.Frequency.Duration < min {
			min = entry.Frequency.Duration
		}
	}
	return min
}

// isSyncDue reports whether an entry is due for a sync: frequency has
// elapsed since the last sync, or the replica file does not exist yet
// (never synced). The replica file's mtime records the last completed
// sync; a disabled entry (zero frequency) is never due. A stat
// failure other than a missing file leaves the entry unscheduled for
// this round; the next tick tries again.
func isSyncDue(path string, frequency time.Duration, now time.Time) bool {
	if frequency <= 0 {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	return now.Sub(info.ModTime()) >= frequency
}

// syncOne runs one database's sync under the configured sync_timeout,
// recovering from a panic so a bug in the library parsing the origin's
// input cannot take down the daemon. It is the per-database unit of
// the sync round.
func (d *ReplicaDaemon) syncOne(name, path string) {
	log := d.Logger.With("label", name, "replica_path", path)
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic during sync", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	log.Info("requesting sync from origin")
	start := time.Now()
	ctx, cancel := context.WithTimeout(d.Ctx, d.config.SyncTimeout.Duration)
	defer cancel()
	stats, err := d.client.Run(ctx, name, path)
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
