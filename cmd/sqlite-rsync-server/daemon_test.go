package main

import (
	"context"
	"database/sql"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	"github.com/caasmo/restinpieces-backup/backup"
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

// freePort returns a free TCP address on the loopback interface. The
// port can be taken between the probe and the daemon's bind; if that
// happens the daemon reports the bind error and the test fails clearly.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	err = l.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// TestOriginDaemonServe runs a full sync against the daemon over TCP:
// the test dials the listener, sends the database label, and plays the
// replica role. The replica database must end up holding the origin's
// content.
func TestOriginDaemonServe(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	addr := freePort(t)
	d := NewOriginDaemon(Config{ListenAddr: addr, Files: map[string]string{"app_db": originPath}}, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = backup.Write(conn, backup.LabelByte, "app_db")
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

	addr := freePort(t)
	d := NewOriginDaemon(Config{ListenAddr: addr, Files: map[string]string{"app_db": originPath}}, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = backup.Write(conn, backup.LabelByte, "nope")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	first, text, err := backup.Read(conn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != backup.ErrorByte {
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

	addr := freePort(t)
	d := NewOriginDaemon(Config{ListenAddr: addr, Files: map[string]string{"app_db": originPath}}, nil)
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

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	err = backup.Write(conn, backup.LabelByte, "app_db")
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
