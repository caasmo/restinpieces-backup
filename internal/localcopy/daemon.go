package localcopy

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
)

// Daemon runs the shared backup engine on a fixed interval. The first
// copy runs immediately at startup; subsequent copies follow the
// interval, which is the smallest configured frequency.
//
// The tick body is synchronous: Run's goroutine performs the copy
// inline, so only one copy executes at a time. Ticks that fire while
// a copy is running are dropped — the ticker buffers a single tick
// and discards the rest — and the next copy starts only when the
// current one finishes.
//
// The daemon satisfies the daemon.Daemon contract: Run spawns the
// copy goroutine, Stop cancels the daemon context; the copy aborts
// at the next entry or step boundary (Shutdown semantics), the
// goroutine exits and signals completion via ShutdownDone. Start wraps
// Run for the restinpieces server.Daemon contract. The strategy reads
// the config box on every tick, so a configuration reload is visible
// at the next tick.
type Daemon struct {
	daemon.Base
	*Engine
}

// New creates the daemon around the strategy. The Engine is embedded:
// its methods are promoted, so the copy runs d.handle directly. A nil
// logger falls back to slog.Default().
func New(name string, strategy Strategy, logger *slog.Logger) *Daemon {
	d := &Daemon{
		Base: daemon.NewBase(name, logger),
	}
	d.Logger = d.Logger.With("daemon_name", d.Name())
	d.Engine = NewEngine(strategy, d.Logger)
	return d
}

// Run spawns the daemon's goroutine. One copy runs immediately at
// startup (skipped if Stop already fired), then one per tick; Ctx
// cancellation (from Stop) exits the select and an in-flight copy
// aborts at the next boundary.
func (d *Daemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)
		defer d.Engine.ClosePools()

		// Stop may fire before this goroutine runs; do not run a
		// doomed first copy. Otherwise run once immediately at
		// startup, then on the interval — the ticker starts after this
		// first copy, so the first tick is one full interval after it
		// completes.
		if err := d.Ctx.Err(); err != nil {
			return // stopped before the first copy
		}
		d.copy()

		interval := d.interval()
		if interval <= 0 {
			return // no active entries; nothing to do
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-d.Ctx.Done():
				return
			case <-ticker.C:
				d.copy()
			}
		}
	}()
	return nil
}

// Start wraps Run for the restinpieces server.Daemon lifecycle
// (server.go: Start/Stop instead of Run/Stop), so this daemon can be
// registered via srv.AddDaemon while restinpieces still manages
// daemons itself.
//
// TODO: remove once restinpieces is on go-daemon-runner; the runner
// calls Run directly.
func (d *Daemon) Start() error {
	return d.Run()
}

// copy runs one copy over every configured database in turn. A
// failed copy is logged and the next tick tries again. On shutdown
// the copy aborts at the next entry or step boundary; an aborted
// copy is not an error (Shutdown semantics), it is logged as info.
func (d *Daemon) copy() {
	err := d.handle(d.Ctx, time.Now())
	if err != nil {
		if errors.Is(err, context.Canceled) {
			d.Logger.Info("copy aborted by shutdown")
			return
		}
		d.Logger.Error("copy failed", "error", err)
	}
}

// interval returns the tick cadence: the smallest frequency among the
// active entries. Every entry's frequency is at least this large, so
// each entry is checked at least once per its own frequency; entries
// that are not yet due are skipped by the due check. It returns zero
// when no entry is active, in which case Run never starts the ticker.
func (d *Daemon) interval() time.Duration {
	var min time.Duration
	for _, entry := range d.strategy.Entries() {
		if entry.SourcePath == "" || entry.DestPath == "" {
			continue
		}
		if min == 0 || entry.Frequency < min {
			min = entry.Frequency
		}
	}
	return min
}
