// Package rsync provides the rsync transfer flow shared by the
// one-shot backup script (cmd/rsync) and the backup daemon. Two client
// types run the receiver protocol: SSHClient over SSH, LocalClient
// with the local rsync binary. Post-transfer verification is the
// caller's job: VerifyBackup checks the received files.
package rsync

import (
	"context"
	"fmt"
	"io"
	"path"

	ripbackup "github.com/caasmo/restinpieces/backup"
	"github.com/gokrazy/rsync/rsyncclient"
)

// Client runs an rsync backup transfer.
type Client interface {
	// Run performs the transfer and logs the transfer statistics.
	// The concrete implementations use the named return (err error)
	// so their deferred cleanup joins behave as in the original.
	// Transfer statistics are log-only: Run does not return them.
	// Run may be called multiple times on the same client: gokrazy
	// documents "You can call [Client.Run] one or more times with the
	// same [Client]" (rsyncclient.New, v0.3.4). Calls must be
	// sequential — the daemon's synchronous tick body runs at most one
	// transfer at a time.
	Run(ctx context.Context) error
}

// receiver holds the rsync receiver client and the transfer state
// shared by both client types.
//
// rsyncClient is created once and reused across every transfer. The
// gokrazy rsyncclient.New doc comment guarantees this reuse (v0.3.4):
// "You can call [Client.Run] one or more times with the same
// [Client]."
type receiver struct {
	rsyncClient    *rsyncclient.Client
	sourceDir      string // SSH: remote backup dir; local: local dir
	sourceFileGlob string // backup file pattern in sourceDir, e.g. "latest-*.db"
	destDir        string
}

// newReceiver creates the rsync receiver and computes the source glob.
func newReceiver(sourceDir, destDir string) (*receiver, error) {
	// Create the rsync client. We do NOT use WithSender() because we are
	// the receiver (pulling from the server to local disk).
	//
	// rsyncclient writes to a temp file and atomically renames on success.
	// If the transfer or checksum fails, the destination file is never
	// touched.
	//
	// Landlock is deactivated here (DontRestrict): gokrazy re-applies a
	// landlock ruleset on every Run and the rulesets stack, capped at 16
	// per process — a long-lived daemon would exhaust them. The SSH-mode
	// mains apply the equivalent landlock once at startup instead
	// (landlock.Restrict, README "Gokrazy's use of landlock").
	rsyncClient, err := rsyncclient.New([]string{"-av"}, rsyncclient.DontRestrict())
	if err != nil {
		return nil, fmt.Errorf("failed to create rsync client: %w", err)
	}

	return &receiver{
		rsyncClient:    rsyncClient,
		sourceDir:      sourceDir,
		sourceFileGlob: ripbackup.LatestGlob,
		destDir:        destDir,
	}, nil
}

// transfer runs the rsync protocol over rw and waits for the server
// process to exit, returning the transfer statistics.
func (r *receiver) transfer(ctx context.Context, rw io.ReadWriter, wait func() error) (*rsyncclient.Result, error) {
	// Pass the destination DIRECTORY: the receiver treats it as a
	// directory (verified in clientmaincmd.go: os.MkdirAll + os.OpenRoot)
	// and writes each file into it under its source name.
	result, err := r.rsyncClient.Run(ctx, rw, []string{r.destDir})
	if err != nil {
		return nil, fmt.Errorf("rsync transfer failed: %w", err)
	}

	// Wait closes stdin and blocks until the server process exits. It
	// must run before the zero-match check below: a nonzero exit is the
	// only signal of a remote-side failure (e.g. a stale or renamed
	// source directory), and skipping it would force-close the SSH
	// session without collecting the remote exit code.
	waitErr := wait()
	if waitErr != nil {
		return nil, fmt.Errorf("rsync server process exited with error: %w", waitErr)
	}

	// A zero-size result means the sender transferred no files: fail
	// instead of silently accepting an empty backup.
	if result == nil || result.Stats == nil || result.Stats.Size == 0 {
		return nil, fmt.Errorf("no backup files received: server glob matched nothing: %s",
			path.Join(r.sourceDir, r.sourceFileGlob))
	}

	return result, nil
}
