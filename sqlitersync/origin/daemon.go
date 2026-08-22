package origin

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	sr "github.com/caasmo/restinpieces-backup/sqlitersync"
	"github.com/caasmo/restinpieces/config"
	"golang.org/x/sync/errgroup"
)

// labelTimeout bounds reading the label message: a peer that connects
// and sends nothing must not hold a connection open.
const labelTimeout = 10 * time.Second

// defaultSyncTimeout is the longest a sync runs when the file
// entry's SyncTimeout is left at zero.
const defaultSyncTimeout = 15 * time.Minute

const defaultListenAddr = "127.0.0.1:54321"

type OriginConfig interface {
	BackupSqliteRsync() config.BackupSqliteRsync
}

// OriginDaemon serves the configured databases over the sqlite3_rsync
// protocol. It listens on one TCP address; every connection names the
// database it wants to sync with a database label, and the daemon runs
// the origin side of the protocol for that database. The daemon is
// reactive: it knows no schedule, the client decides when to sync.
//
// The daemon reads its configuration from the current-config box it
// receives in the constructor, at every decision point. The box is
// owned by whoever assembles the daemon: the restinpieces app passes
// its own box (app.ConfigPointer()), a standalone main publishes its
// own config struct into a box it creates. The daemon never publishes.
//
// The daemon satisfies the daemon.Daemon contract (Run/Stop) and the
// restinpieces server.Daemon contract (Start/Stop): Run/Start creates
// the listener and spawns the accept goroutine, Stop cancels the
// daemon context (the goroutine closes the listener, waits for
// in-flight syncs to finish or abort at their next message boundary,
// then signals completion).
type OriginDaemon[T OriginConfig] struct {
	daemon.Base
	cfgPointer *atomic.Pointer[T]
}

func New[T OriginConfig](pointer *atomic.Pointer[T], logger *slog.Logger) *OriginDaemon[T] {
	d := &OriginDaemon[T]{
		Base:       daemon.NewBase("OriginDaemon", logger),
		cfgPointer: pointer,
	}
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

func (d *OriginDaemon[T]) Config() config.BackupSqliteRsync {
	return (*d.cfgPointer.Load()).BackupSqliteRsync()
}

func (d *OriginDaemon[T]) entries() map[string]config.BackupSqliteRsyncEntry {
	cfg := d.Config()
	filtered := make(map[string]config.BackupSqliteRsyncEntry, len(cfg.Entries))
	for k, e := range cfg.Entries {
		if e.SourcePath == "" {
			continue
		}
		filtered[k] = e
	}
	return filtered
}

func (d *OriginDaemon[T]) listenAddr() string {
	addr := d.Config().ListenAddr
	if addr == "" {
		return defaultListenAddr
	}
	return addr
}

// hasFilesToServe reports whether the daemon has at least one file to serve.
func (d *OriginDaemon[T]) hasFilesToServe() bool {
	return len(d.entries()) != 0
}

// Start starts the daemon under the restinpieces server.Daemon
// contract.
// TODO: remove once restinpieces is on go-daemon-runner; the runner calls Run directly.
func (d *OriginDaemon[T]) Start() error {
	return d.Run()
}

// Run creates the listener and spawns the accept goroutine. A bind
// error is a startup failure: it is returned, and the runner rolls
// back the already-started daemons. Stop cancels the daemon context;
// the goroutine closes the listener to unblock Accept, waits for
// every in-flight sync to finish or abort, then closes ShutdownDone.
func (d *OriginDaemon[T]) Run() error {
	listener, err := net.Listen("tcp", d.listenAddr())
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", d.listenAddr(), err)
	}
	d.Logger.Info("listening", "listen_addr", d.listenAddr())

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
func (d *OriginDaemon[T]) handleConn(conn net.Conn) (err error) {
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	if !d.hasFilesToServe() {
		return sr.Write(conn, sr.ErrorByte, "no files to serve")
	}

	// The label must arrive promptly: a silent peer is rejected once
	// the deadline passes. The deadline bounds the whole preamble —
	// the label read and the rejection write alike — so no I/O on
	// this connection is ever unbounded.
	err = conn.SetDeadline(time.Now().Add(labelTimeout))
	if err != nil {
		return err
	}
	first, text, err := sr.Read(conn)
	if err != nil {
		return err
	}
	if first != sr.LabelByte {
		return fmt.Errorf("%w: first message must name a database", sr.ErrInvalid)
	}
	label := text
	fileCfg, ok := d.entries()[label]
	if !ok {
		return sr.Write(conn, sr.ErrorByte, "unknown database")
	}
	// Accept the preamble: echo the label back. The peer reads this
	// message and starts the sync protocol only after it; the first
	// sync byte (ORIGIN_BEGIN) follows this echo.
	err = sr.Write(conn, sr.LabelByte, label)
	if err != nil {
		return err
	}
	// The label arrived within the preamble deadline; the sync now runs
	// under the sync deadline. The origin exits by one of two paths:
	// the context, checked at every message boundary while the peer
	// talks; or this connection deadline, when the peer stops talking.
	syncTimeout := fileCfg.SyncTimeout.Duration
	if syncTimeout == 0 {
		syncTimeout = defaultSyncTimeout
	}
	err = conn.SetDeadline(time.Now().Add(syncTimeout))
	if err != nil {
		return err
	}

	// Stop cancels the daemon context, aborting the sync at its next
	// message boundary.
	log := d.Logger.With("label", label)
	log.Info("starting sync", "origin", fileCfg.SourcePath)
	start := time.Now()
	stats, err := sqlitersync.Origin(d.Ctx, conn, fileCfg.SourcePath, nil)
	if err != nil {
		return fmt.Errorf("origin %s: %w", fileCfg.SourcePath, err)
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
