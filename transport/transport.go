package transport

import (
	"context"
	"io"
)

// Transport connects to an rsync server (remote or local) and returns
// the bidirectional pipe the rsyncclient protocol runs over.
//
// A Transport is single-use: Connect may be called at most once, and
// the instance is released by Close. Repeated Connect is a caller bug
// and leaks the first connection.
type Transport interface {
	// Connect starts the rsync server process with the given server args
	// (e.g. --server --sender -vlogDtpr . /path/to/latest-*.db) and
	// returns the io.ReadWriter the client protocol runs over.
	// SSH: session.Start("rsync " + args) over an SSH session.
	// Local: exec.Command("rsync", args...) on the same machine.
	Connect(ctx context.Context, serverArgs []string) (io.ReadWriter, error)

	// Wait signals the server that the transfer is done (close stdin)
	// and blocks until the server process exits. Called after
	// rsyncClient.Run completes.
	Wait() error

	// Close releases all resources (SSH client + session, exec.Cmd
	// process, etc). Must be idempotent: safe to call after Wait(),
	// after a failed Connect(), or if Connect() was never called.
	Close() error
}
