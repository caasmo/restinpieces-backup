package replica

import (
	"context"
	"errors"
	"fmt"

	"github.com/caasmo/go-sqlite-rsync/sqlitersync"
	"github.com/caasmo/restinpieces-backup/ssh"
)

// SSHClient syncs against an origin server on another machine: it
// dials the machine's system sshd, opens a direct-tcpip channel to the
// origin's loopback listener, and runs the replica side over that
// channel.
type SSHClient struct {
	// Creds are the in-memory credentials LoadCredentials built once
	// at startup; every dial reuses the parsed keys.
	Creds ssh.Credentials
	// OriginAddr is the origin listener's address, reachable from the
	// SSH server's host.
	OriginAddr string
}

// Compile-time check: SSHClient satisfies Client.
var _ Client = (*SSHClient)(nil)

// Run dials the SSH server, opens the direct-tcpip channel to the
// origin listener, then delegates the sync to runSync.
func (c *SSHClient) Run(ctx context.Context, label, replicaPath string) (stats sqlitersync.Stats, err error) {
	// Dial the machine's system sshd with the in-memory credentials.
	// The host key is pinned, so a dial against any other server fails.
	// DialContext bounds the whole connection attempt — TCP dial and
	// SSH handshake — by ctx (the daemon's sync_timeout), so Stop
	// aborts an in-flight dial the same way it aborts the channel
	// open and the sync itself.
	client, err := ssh.DialContext(ctx, c.Creds)
	if err != nil {
		return sqlitersync.Stats{}, err
	}
	// Closing the SSH client unblocks the channel open when the
	// context is cancelled — the same pattern runSync applies to the
	// connection once it exists.
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	defer func() {
		closeErr := client.Close()
		err = errors.Join(err, closeErr)
	}()

	// cryptossh.Client.Dial opens a direct-tcpip channel: the channel
	// is a net.Conn that reaches the origin's loopback listener through
	// the SSH server.
	conn, err := client.Dial("tcp", c.OriginAddr)
	if err != nil {
		return sqlitersync.Stats{}, fmt.Errorf("failed to open channel to %s: %w", c.OriginAddr, err)
	}
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	return runSync(ctx, conn, label, replicaPath)
}
