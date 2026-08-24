// Package replica provides the transports the replica daemon
// uses to reach the origin server: dialing the origin's loopback
// listener directly when both run on the same machine (LocalClient),
// or opening a direct-tcpip channel through the remote machine's
// system sshd (SSHClient). The transports differ only in how they
// produce the connection; the sync itself is the same for both.
package replica

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	sr "github.com/caasmo/restinpieces-backup/sqlitersync"
)

// dialTimeout bounds the local transport's dial: an unreachable peer
// must not hold the sync loop open.
const dialTimeout = 10 * time.Second

// Client runs one replica sync of one database against the origin
// server: connect, send the database label, run the replica side of
// the protocol over the connection, close.
type Client interface {
	// Run performs one full sync. The caller owns ctx, which cancels
	// the sync at any point: the dial, the preamble write, and the
	// sync itself all respect ctx. It returns the run's per-run
	// summary (Stats) — also when the sync itself fails, holding the
	// partial counts; a failure before it (the dial, the preamble
	// write) returns the zero Stats — and the run's error, if any.
	Run(ctx context.Context, label, replicaPath string) (sqlitersync.Stats, error)
}

// runSync sends the label and runs the replica side of the sync over
// the connection. The caller owns the sync deadline: the daemon
// applies the configured sync_timeout to its context before calling.
// Closing the connection unblocks any blocked read or write in the
// sync — a property of net.Conn itself, not of the library. The stop
// function disarms the callback on the normal path.
func runSync(ctx context.Context, conn net.Conn, label, replicaPath string) (sqlitersync.Stats, error) {
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	// Send the preamble first: the label byte (0x01) plus the database
	// name. This is the message the origin server reads before it
	// accepts the sync; a label the server does not know is rejected.
	err := sr.Write(conn, sr.LabelByte, label)
	if err != nil {
		return sqlitersync.Stats{}, fmt.Errorf("send label: %w", err)
	}

	// The preamble response decides the sync: an echo of the label
	// (0x01 plus the same name) accepts it, any other first byte is a
	// rejection whose text says why. Only an accepted preamble starts
	// the sync protocol.
	first, text, err := sr.Read(conn)
	if err != nil {
		return sqlitersync.Stats{}, fmt.Errorf("read origin response: %w", err)
	}
	if first != sr.LabelByte {
		return sqlitersync.Stats{}, fmt.Errorf("origin rejected sync: %s", text)
	}

	// Now run the replica side of the protocol: it sends the hashes of
	// the replica's pages, receives back only the pages that differ,
	// and blocks until the sync ends.
	stats, err := sqlitersync.Replica(ctx, conn, replicaPath, nil)
	if err != nil {
		return sqlitersync.Stats{}, fmt.Errorf("replica sync: %w", err)
	}
	return stats, nil
}
