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
// interval, which is the smallest configured frequency capped by
// MaxTickInterval.
//
// The tick body is synchronous: Run's goroutine performs the copy
// inline, so only one copy executes at a time. The due check in the
// engine decides whether a tick makes a backup, so a large frequency
// (for example 24h) still checks every MaxTickInterval and lands
// within minutes of its due time.
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
// startup (skipped if Stop already fired), then one after each
// interval; Ctx cancellation (from Stop) exits the select and an
// in-flight copy aborts at the next boundary. The interval is
// re-read after every run, so a frequency change on reload takes
// effect before the next wait.
func (d *Daemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)
		defer d.ClosePools()

		// Stop may fire before this goroutine runs; do not run a
		// doomed first copy.
		if err := d.Ctx.Err(); err != nil {
			return // stopped before the first copy
		}
		d.copy()

		for {
			select {
			case <-d.Ctx.Done():
				return
			case <-time.After(d.interval()):
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

// MaxTickInterval caps the tick interval. A frequency larger than
// this still checks every MaxTickInterval; the due check skips ticks
// that are not yet due.
const MaxTickInterval = 10 * time.Minute

// interval returns the tick cadence: the smallest frequency among the
// active entries, capped by MaxTickInterval. Every entry's frequency
// is at least this large when it is smaller than the cap, so each
// such entry is checked at least once per its own frequency; entries
// that are not yet due are skipped by the due check. It returns
// MaxTickInterval when no entry is active, so a later reload that
// adds one takes effect at the next tick.
func (d *Daemon) interval() time.Duration {
	min := MaxTickInterval
	for _, entry := range d.strategy.Entries() {
		if entry.SourcePath == "" || entry.DestPath == "" {
			continue
		}
		if entry.Frequency <= 0 {
			continue
		}
		if entry.Frequency < min {
			min = entry.Frequency
		}
	}
	return min
}
