package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	"github.com/caasmo/restinpieces-backup-client/backup"
	"golang.org/x/sync/errgroup"
)

// labelTimeout bounds reading the label message: a peer that connects
// and sends nothing must not hold a connection open.
const labelTimeout = 10 * time.Second

// defaultSyncTimeout is the longest a sync runs when the configuration
// leaves Config.SyncTimeout at zero.
const defaultSyncTimeout = 15 * time.Minute

// OriginDaemon serves the configured databases over the sqlite3_rsync
// protocol. It listens on one TCP address; every connection names the
// database it wants to sync with a database label, and the daemon runs
// the origin side of the protocol for that database. The daemon is
// reactive: it knows no schedule, the client decides when to sync.
//
// The daemon satisfies the daemon.Daemon contract: Run creates the
// listener and spawns the accept goroutine, Stop cancels the daemon
// context (the goroutine closes the listener, waits for in-flight
// syncs to finish or abort at their next message boundary, then
// signals completion).
type OriginDaemon struct {
	daemon.Base
	cfg Config
}

// NewOriginDaemon creates the daemon around the backup configuration.
// main loads the configuration from the environment and passes it in;
// the daemon reads no environment itself. A nil logger falls back to
// slog.Default().
func NewOriginDaemon(cfg Config, logger *slog.Logger) *OriginDaemon {
	if cfg.SyncTimeout == 0 {
		cfg.SyncTimeout = defaultSyncTimeout
	}
	d := &OriginDaemon{
		Base: daemon.NewBase("OriginDaemon", logger),
		cfg:  cfg,
	}
	// Every daemon log line carries the daemon's identity: reuse the
	// daemon_name attribute the runner attaches to lifecycle logs.
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run creates the listener and spawns the accept goroutine. A bind
// error is a startup failure: it is returned, and the runner rolls
// back the already-started daemons. Stop cancels the daemon context;
// the goroutine closes the listener to unblock Accept, waits for
// every in-flight sync to finish or abort, then closes ShutdownDone.
func (d *OriginDaemon) Run() error {
	listener, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", d.cfg.ListenAddr, err)
	}

	// The errgroup owns one goroutine per accepted connection. No
	// concurrency cap: the peer is the trusted loopback backup
	// client, and the OS fd limit bounds concurrent connections.
	// The accept loop runs inline in the daemon goroutine; when Stop
	// closes the listener and the loop exits, Wait joins every
	// in-flight sync before ShutdownDone closes. A plain group,
	// like the runner's: it joins goroutines and collects errors,
	// it does not propagate cancellation.
	var group errgroup.Group

	go func() {
		defer close(d.ShutdownDone)
		defer func() {
			// The Stop goroutine may have closed the listener
			// already; a double close reports net.ErrClosed,
			// which is the expected shutdown path, not a
			// failure.
			_ = listener.Close()
		}()

		// Stop cancels Ctx; closing the listener unblocks Accept.
		go func() {
			<-d.Ctx.Done()
			_ = listener.Close()
		}()

		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					break
				}
				// No backoff on transient accept errors, unlike the
				// net/http server's tempDelay pattern: fd exhaustion
				// needs hundreds of concurrent connections, which a
				// loopback origin serving one or two syncs cannot
				// reach. A spin, should it ever happen, clears with
				// the condition and never touches the shutdown path.
				d.Logger.Error("accept failed", "error", err)
				continue
			}
			group.Go(func() error {
				defer func() {
					if r := recover(); r != nil {
						// A panic in one connection must not take
						// down the daemon: the peer's input is
						// parsed by library code, and a bug there
						// would otherwise kill every in-flight
						// sync. Log it with the stack and let
						// handleConn's defers close the
						// connection.
						d.Logger.Error("panic serving connection",
							"panic", r, "stack", string(debug.Stack()))
					}
				}()
				err := d.handleConn(conn)
				if err != nil {
					d.Logger.Error("connection failed", "error", err)
				}
				return nil
			})
		}

		// The listener is closed: no more accepts. Join every
		// in-flight sync before signaling completion — ShutdownDone
		// closes only after the last handler has exited. Handlers
		// log their own errors and return nil, so Wait reports
		// nothing on the shutdown path.
		_ = group.Wait()
	}()
	return nil
}

// handleConn runs one connection to completion: read the label message,
// resolve it to the origin database, then run the origin side of the
// sync. The connection is closed on every return path.
func (d *OriginDaemon) handleConn(conn net.Conn) (err error) {
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	// The label must arrive promptly: a silent peer is rejected once
	// the deadline passes. The deadline bounds the whole preamble —
	// the label read and the rejection write alike — so no I/O on
	// this connection is ever unbounded.
	err = conn.SetDeadline(time.Now().Add(labelTimeout))
	if err != nil {
		return err
	}
	first, text, err := backup.Read(conn)
	if err != nil {
		return err
	}
	if first != backup.LabelByte {
		return fmt.Errorf("%w: first message must name a database", backup.ErrInvalid)
	}
	label := text
	path, ok := d.cfg.Files[label]
	if !ok {
		return backup.Write(conn, backup.ErrorByte, "unknown database")
	}
	// The label arrived within the preamble deadline; the sync now runs
	// under the sync deadline. The origin exits by one of two paths:
	// the context, checked at every message boundary while the peer
	// talks; or this connection deadline, when the peer stops talking.
	err = conn.SetDeadline(time.Now().Add(d.cfg.SyncTimeout))
	if err != nil {
		return err
	}

	// Stop cancels the daemon context, aborting the sync at its next
	// message boundary.
	log := d.Logger.With("label", label, "role", "origin")
	log.Info("starting sync", "origin", path)
	start := time.Now()
	stats, err := sqlitersync.Origin(d.Ctx, conn, path, nil)
	if err != nil {
		return fmt.Errorf("origin %s: %w", path, err)
	}
	logSyncSummary(log, stats, time.Since(start))
	return nil
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
	bytesPerSec := float64(0)
	if elapsed > 0 {
		bytesPerSec = float64(wireBytes) / elapsed.Seconds()
	}
	speedup := float64(0)
	if wireBytes > 0 && wireBytes <= dbSize {
		speedup = float64(dbSize) / float64(wireBytes)
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
		"elapsed", elapsed,
	)
}
