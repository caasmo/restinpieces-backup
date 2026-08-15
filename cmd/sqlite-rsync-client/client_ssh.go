package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/caasmo/restinpieces-backup-client/ssh"
)

// SSHClient syncs against an origin server on another machine: it
// dials the machine's system sshd, opens a direct-tcpip channel to the
// origin's loopback listener, and runs the replica side over that
// channel.
type SSHClient struct {
	creds      ssh.Credentials
	originAddr string
}

// Compile-time check: SSHClient satisfies Client.
var _ Client = (*SSHClient)(nil)

// Run dials the SSH server, opens the direct-tcpip channel to the
// origin listener, then delegates the sync to runSync.
func (c *SSHClient) Run(ctx context.Context, label, replicaPath string) (err error) {
	// Dial the machine's system sshd with the in-memory credentials.
	// The host key is pinned, so a dial against any other server fails.
	// The ssh package's own 15s timeout bounds the dial itself; the
	// channel open below is bounded by the AfterFunc close.
	client, err := ssh.Dial(c.creds)
	if err != nil {
		return err
	}
	// Closing the SSH client unblocks the channel open when the
	// context is cancelled — the same pattern runSync applies to the
	// channel once it exists.
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	defer func() {
		closeErr := client.Close()
		err = errors.Join(err, closeErr)
	}()

	// cryptossh.Client.Dial opens a direct-tcpip channel: the channel
	// is a net.Conn that reaches the origin's loopback listener through
	// the SSH server.
	conn, err := client.Dial("tcp", c.originAddr)
	if err != nil {
		return fmt.Errorf("failed to open channel to %s: %w", c.originAddr, err)
	}
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	return runSync(ctx, conn, label, replicaPath)
}
