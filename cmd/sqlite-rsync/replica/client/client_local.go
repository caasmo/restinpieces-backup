package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
)

// LocalClient syncs against an origin server on the same machine,
// dialing its loopback listener directly with no SSH.
type LocalClient struct {
	originAddr string
}

// Compile-time check: LocalClient satisfies Client.
var _ Client = (*LocalClient)(nil)

// Run dials the origin listener, then delegates the sync to runSync.
func (c *LocalClient) Run(ctx context.Context, label, replicaPath string) (stats sqlitersync.Stats, err error) {
	// Dial the origin's loopback listener. DialContext bounds the dial
	// by dialTimeout and aborts it when ctx is cancelled, so an
	// unreachable server fails the sync quickly and a shutdown does
	// not wait on a dial in flight.
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", c.originAddr)
	if err != nil {
		return sqlitersync.Stats{}, fmt.Errorf("failed to dial origin: %w", err)
	}
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	return runSync(ctx, conn, label, replicaPath)
}
