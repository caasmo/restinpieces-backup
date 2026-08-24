package replica

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	replicaconfig "github.com/caasmo/restinpieces-backup/config/sqlitersync/replica"
	replicaclient "github.com/caasmo/restinpieces-backup/internal/sqlitersync/replica"
	sr "github.com/caasmo/restinpieces-backup/sqlitersync"
)

// recordingClient records the names it is asked to sync. The
// scheduling tests use it to observe which entries the daemon
// considers due without touching a real transport.
type recordingClient struct {
	synced []string
}

func (c *recordingClient) Run(ctx context.Context, name, replicaPath string) (sqlitersync.Stats, error) {
	c.synced = append(c.synced, name)
	return sqlitersync.Stats{}, nil
}

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

// startOrigin runs a minimal origin server in-process: it listens on
// loopback and serves every accepted connection — read the label
// preamble, run the origin side of the sync for the mapped database —
// one goroutine per connection, like the real daemon, so a sync round
// over several due entries never deadlocks on a pending accept. The
// real origin is not importable, so the test plays its role. It
// returns the listener address.
func startOrigin(t *testing.T, files map[string]string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed by the cleanup
			}
			go serveOriginConn(conn, files)
		}
	}()
	return listener.Addr().String()
}

// serveOriginConn runs one accepted connection to completion: the
// preamble exchange under a per-connection deadline, then the sync
// protocol.
func serveOriginConn(conn net.Conn, files map[string]string) {
	defer func() {
		_ = conn.Close()
	}()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	first, text, err := sr.Read(conn)
	if err != nil {
		return
	}
	if first != sr.LabelByte {
		return
	}
	path, ok := files[text]
	if !ok {
		_ = sr.Write(conn, sr.ErrorByte, "unknown database")
		return
	}
	// Accept the preamble by echoing the label, then start the sync
	// protocol, exactly like the real origin daemon.
	err = sr.Write(conn, sr.LabelByte, text)
	if err != nil {
		return
	}
	_, _ = sqlitersync.Origin(context.Background(), conn, path, nil)
}

// TestLocalClientUnknownLabel checks that an entry name the origin
// does not serve fails the sync: the client must not treat the
// rejection as a completed backup.
func TestLocalClientUnknownLabel(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	originAddr := startOrigin(t, map[string]string{"app_db": originPath})

	replicaPath := filepath.Join(t.TempDir(), "replica.db")
	client := &replicaclient.LocalClient{OriginAddr: originAddr}
	_, err := client.Run(context.Background(), "nope", replicaPath)
	if err == nil {
		t.Fatal("sync succeeded for an unknown label")
	}
}

// TestReplicaDaemonSync runs a full sync through the daemon's cycle:
// an in-process origin serves the database, and the sync brings the
// replica database up to the origin's content.
func TestReplicaDaemonSync(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	originAddr := startOrigin(t, map[string]string{"app_db": originPath})

	replicaPath := filepath.Join(t.TempDir(), "replica.db")
	d := New(&replicaclient.LocalClient{OriginAddr: originAddr},
		replicaconfig.Config{
			SyncTimeout: replicaconfig.Duration{Duration: 15 * time.Minute},
			Entries: map[string]replicaconfig.Entry{
				"app_db": {Path: replicaPath, Frequency: replicaconfig.Duration{Duration: time.Minute}},
			}}, nil)
	d.sync(time.Now())

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

// TestIsSyncDue checks the due rule: a missing replica file is always
// due, a freshly written one is not, one whose mtime is older than
// the frequency is due again, and a disabled entry (zero frequency)
// is never due.
func TestIsSyncDue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replica.db")
	now := time.Now()
	if !isSyncDue(path, time.Minute, now) {
		t.Fatal("missing replica file is not due")
	}
	err := os.WriteFile(path, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if isSyncDue(path, time.Minute, now) {
		t.Fatal("fresh replica file is due")
	}
	past := now.Add(-2 * time.Minute)
	err = os.Chtimes(path, past, past)
	if err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if !isSyncDue(path, time.Minute, now) {
		t.Fatal("stale replica file is not due")
	}
	if isSyncDue(path, 0, now) {
		t.Fatal("disabled entry is due")
	}
}

// TestReplicaDaemonSyncsOnlyDueEntries checks the per-entry schedule:
// a fresh replica file is skipped, a missing one is pulled, and a
// disabled entry (zero frequency) is never pulled.
func TestReplicaDaemonSyncsOnlyDueEntries(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	now := time.Now()
	err := os.WriteFile(fresh, []byte("x"), 0644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")

	client := &recordingClient{}
	d := New(client, replicaconfig.Config{
		SyncTimeout: replicaconfig.Duration{Duration: 15 * time.Minute},
		Entries: map[string]replicaconfig.Entry{
			"app":  {Path: fresh, Frequency: replicaconfig.Duration{Duration: time.Hour}},
			"logs": {Path: missing, Frequency: replicaconfig.Duration{Duration: time.Hour}},
			"cold": {Path: filepath.Join(t.TempDir(), "cold.db")},
		}}, nil)
	d.sync(now)

	want := []string{"logs"}
	if !slices.Equal(client.synced, want) {
		t.Fatalf("synced = %v, want %v", client.synced, want)
	}
}

// TestReplicaDaemonInterval checks the tick cadence: the smallest
// active frequency, ignoring disabled entries; zero when nothing is
// active.
func TestReplicaDaemonInterval(t *testing.T) {
	d := New(nil, replicaconfig.Config{
		SyncTimeout: replicaconfig.Duration{Duration: 15 * time.Minute},
		Entries: map[string]replicaconfig.Entry{
			"a":   {Path: "/tmp/a.db", Frequency: replicaconfig.Duration{Duration: 15 * time.Minute}},
			"b":   {Path: "/tmp/b.db", Frequency: replicaconfig.Duration{Duration: 30 * time.Second}},
			"off": {Path: "/tmp/off.db"},
		}}, nil)
	got := d.interval()
	if got != 30*time.Second {
		t.Fatalf("interval = %v, want %v", got, 30*time.Second)
	}
	empty := New(nil, replicaconfig.Config{}, nil)
	if empty.interval() != 0 {
		t.Fatal("no active entries produced a nonzero interval")
	}
}

// TestReplicaDaemonRunStop runs the daemon against an in-process
// origin, waits for the immediate startup sync to complete, then stops
// it: Stop must return once the sync goroutine has exited. The
// frequency is a long one on purpose: the test validates Stop, not
// tick behavior, and a short frequency could fire a second tick whose
// dial the one-connection fixture would never serve, making the test
// nondeterministic.
func TestReplicaDaemonRunStop(t *testing.T) {
	originPath := filepath.Join(t.TempDir(), "origin.db")
	createWalDB(t, originPath)

	originAddr := startOrigin(t, map[string]string{"app_db": originPath})

	replicaPath := filepath.Join(t.TempDir(), "replica.db")
	d := New(&replicaclient.LocalClient{OriginAddr: originAddr},
		replicaconfig.Config{
			SyncTimeout: replicaconfig.Duration{Duration: 15 * time.Minute},
			Entries: map[string]replicaconfig.Entry{
				"app_db": {Path: replicaPath, Frequency: replicaconfig.Duration{Duration: time.Hour}},
			}}, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The first round runs immediately at startup: wait for the
	// replica file to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(replicaPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica was not created by the startup sync")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = d.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// startStalledOrigin runs an origin that accepts one connection and
// then never responds: it reads nothing and holds the connection open
// until the client closes it. A sync against it blocks reading the
// origin's first message, which never comes. It returns the listener
// address and a channel that closes when the connection is accepted,
// so the test knows the sync is genuinely in flight.
func startStalledOrigin(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		close(accepted)
		defer func() {
			_ = conn.Close()
		}()
		// Never read or write: the sync on the other end stays blocked
		// until the client closes the connection.
		time.Sleep(30 * time.Second)
	}()
	return listener.Addr().String(), accepted
}

// TestReplicaDaemonStopAbortsInFlightSync starts a sync against a
// stalled origin, waits until the connection is accepted (so the sync
// is blocked reading the origin's first message), then stops the
// daemon: Stop must return promptly, because runSync's AfterFunc
// closes the connection and the blocked read fails. It must not wait
// out the stalled origin's hold.
func TestReplicaDaemonStopAbortsInFlightSync(t *testing.T) {
	originAddr, accepted := startStalledOrigin(t)

	replicaPath := filepath.Join(t.TempDir(), "replica.db")
	d := New(&replicaclient.LocalClient{OriginAddr: originAddr},
		replicaconfig.Config{
			SyncTimeout: replicaconfig.Duration{Duration: 15 * time.Minute},
			Entries: map[string]replicaconfig.Entry{
				"app_db": {Path: replicaPath, Frequency: replicaconfig.Duration{Duration: time.Hour}},
			}}, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	// The first round runs immediately at startup: wait for it to
	// reach the stalled origin, so Stop below is called mid-sync.
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("sync never reached the origin")
	}

	// Stop with a tight deadline: it must return well before the
	// stalled origin's 30s hold expires, proving the AfterFunc close
	// aborted the blocked sync instead of waiting it out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = d.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Compile-time check: ReplicaDaemon satisfies the runner's Daemon
// contract through daemon.Base.
var _ daemon.Daemon = (*ReplicaDaemon)(nil)
