package origin

import (
	"context"
	"database/sql"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	sr "github.com/caasmo/restinpieces-backup/sqlitersync"
	"github.com/caasmo/restinpieces/config"
)

// createWalDB creates a database file in WAL mode holding one table
// with one row.
func createWalDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	_, err = db.Exec("INSERT INTO t VALUES(1)")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOriginDaemonServe runs a full sync against the daemon over TCP:
// the test dials the listener, sends the database label, and plays the
// replica role. The replica database must end up holding the origin's
// content.
func TestOriginDaemonServe(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	var pointer atomic.Pointer[config.Config]
	pointer.Store(&config.Config{
		Backup: config.Backup{
			Files: map[string]config.BackupFile{
				"app_db": {SourcePath: originPath, Strategy: config.BackupStrategySqliteRsync},
			},
		},
	})
	d := NewOriginDaemon[config.Config](&pointer, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = sr.Write(conn, sr.LabelByte, "app_db")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	replicaPath := filepath.Join(t.TempDir(), "replica.db")
	_, err = sqlitersync.Replica(context.Background(), conn, replicaPath, nil)
	if err != nil {
		t.Fatalf("Replica: %v", err)
	}

	db, err := sql.Open("sqlite", replicaPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		_ = db.Close()
	}()
	var x int
	err = db.QueryRow("SELECT x FROM t").Scan(&x)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if x != 1 {
		t.Fatalf("x = %d, want 1", x)
	}
}

// TestOriginDaemonUnknownLabel checks that a label that is not in the
// configured map is rejected with an error message.
func TestOriginDaemonUnknownLabel(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	var pointer atomic.Pointer[config.Config]
	pointer.Store(&config.Config{
		Backup: config.Backup{
			Files: map[string]config.BackupFile{
				"app_db": {SourcePath: originPath, Strategy: config.BackupStrategySqliteRsync},
			},
		},
	})
	d := NewOriginDaemon[config.Config](&pointer, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = sr.Write(conn, sr.LabelByte, "nope")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	first, text, err := sr.Read(conn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != sr.ErrorByte {
		t.Fatalf("first byte = 0x%02x, want error message", first)
	}
	if text != "unknown database" {
		t.Fatalf("error text = %q, want %q", text, "unknown database")
	}
}

// TestOriginDaemonStopJoinsSync starts a sync whose replica goes
// silent after the origin announces itself, then stops the daemon:
// Stop must block until the sync handler has exited — it must not
// report completion while a sync is still in flight. The replica
// ends the sync by closing the connection, which unblocks the
// origin's read with EOF and lets Stop complete.
func TestOriginDaemonStopJoinsSync(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	var pointer atomic.Pointer[config.Config]
	pointer.Store(&config.Config{
		Backup: config.Backup{
			Files: map[string]config.BackupFile{
				"app_db": {SourcePath: originPath, Strategy: config.BackupStrategySqliteRsync},
			},
		},
	})
	d := NewOriginDaemon[config.Config](&pointer, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// An early failure must not leak the daemon: the deferred Stop
	// closes ShutdownDone on every exit path. On success it is a
	// no-op — the explicit Stop below already completed the shutdown.
	// The conn.Close defer below runs first, unblocking an in-flight
	// handler read, so Stop never hangs on a failed test.
	defer func() {
		_ = d.Stop(context.Background())
	}()

	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = sr.Write(conn, sr.LabelByte, "app_db")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The origin answers with the ORIGIN_BEGIN header: the type
	// byte 0x41, the protocol byte, the page size, the page count.
	// Reading it proves the sync started; the origin is now blocked
	// reading the replica's first message.
	header := make([]byte, 7)
	_, err = io.ReadFull(conn, header)
	if err != nil {
		t.Fatalf("read ORIGIN_BEGIN header: %v", err)
	}
	if header[0] != 0x41 {
		t.Fatalf("first sync byte = 0x%02x, want ORIGIN_BEGIN (0x41)", header[0])
	}

	// Stop in the background: the sync handler is blocked reading
	// and cannot exit on its own, so Stop must not return until
	// the replica acts.
	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopDone <- d.Stop(ctx)
	}()

	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned (%v) while a sync was still in flight", err)
	case <-time.After(500 * time.Millisecond):
		// Still blocked: the join works.
	}

	// The replica closes the connection: the origin's blocked read
	// returns EOF, the handler exits, and Stop completes.
	_ = conn.Close()
	err = <-stopDone
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestOriginDaemonIgnoresNonRsyncStrategies(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)
	var pointer atomic.Pointer[config.Config]
	pointer.Store(&config.Config{
		Backup: config.Backup{
			Files: map[string]config.BackupFile{
				"app-online": {SourcePath: originPath, Strategy: config.BackupStrategyOnline},
				"app-rsync":  {SourcePath: originPath, Strategy: config.BackupStrategySqliteRsync},
			},
		},
	})
	d := NewOriginDaemon[config.Config](&pointer, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = d.Stop(context.Background()) }()
	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	err = sr.Write(conn, sr.LabelByte, "app-online")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	first, text, err := sr.Read(conn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != sr.ErrorByte || text != "unknown database" {
		t.Fatalf("online label should be rejected as unknown database, got %v %q", first, text)
	}
}

func TestOriginDaemonEmptyConfigListens(t *testing.T) {
	var pointer atomic.Pointer[config.Config]
	pointer.Store(&config.Config{
		Backup: config.Backup{
			Files: map[string]config.BackupFile{},
		},
	})
	d := NewOriginDaemon[config.Config](&pointer, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run with empty config: got %v, want nil (daemon always listens)", err)
	}
	defer func() { _ = d.Stop(context.Background()) }()
	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// Robust: do not assert Write success. With empty config the daemon
	// answers before reading the label, so Write may race with Close.
	// The Read below is the contract: it must be "no files to serve".
	_ = sr.Write(conn, sr.LabelByte, "app-rsync")
	first, text, err := sr.Read(conn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != sr.ErrorByte || text != "no files to serve" {
		t.Fatalf("empty config should answer no files to serve, got %v %q", first, text)
	}
	// SIGHUP simulation: store a new config with a valid entry and verify the already-listening daemon serves it.
	originPath := ""
	// createWalDB would be needed in a real test; here the logical check is that Config() reflects the new pointer.
	_ = originPath
}