// Package rsync provides the rsync transfer flow shared by the
// one-shot backup script (cmd/rsync) and the backup daemon. Two client
// types run the receiver protocol: SSHClient over SSH, LocalClient
// with the local rsync binary. Post-transfer verification is the
// caller's job (the verification package).
package rsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/caasmo/restinpieces-backup-client/ssh"
	ripbackup "github.com/caasmo/restinpieces/backup"
	"github.com/gokrazy/rsync/rsyncclient"
)

// Client runs an rsync backup transfer.
type Client interface {
	// Run performs the transfer and logs the transfer statistics.
	// The concrete implementations use the named return (err error)
	// so their deferred cleanup joins behave as in the original.
	// Transfer statistics are log-only: Run does not return them.
	Run(ctx context.Context) error
}

// receiver holds the rsync receiver client and the transfer state
// shared by both client types.
type receiver struct {
	rsyncClient *rsyncclient.Client
	globPath    string
	destDir     string
}

// newReceiver creates the rsync receiver and computes the source glob.
func newReceiver(sourceDir, destDir string) (*receiver, error) {
	// Create the rsync client. We do NOT use WithSender() because we are
	// the receiver (pulling from the server to local disk).
	//
	// rsyncclient writes to a temp file and atomically renames on success.
	// If the transfer or checksum fails, the destination file is never
	// touched.
	rsyncClient, err := rsyncclient.New([]string{"-av"})
	if err != nil {
		return nil, fmt.Errorf("failed to create rsync client: %w", err)
	}

	// path.Join ensures / separators regardless of OS and preserves the
	// glob pattern. ripbackup.LatestGlob ("latest-*.db") is the shared
	// contract from the restinpieces backup package: it selects every
	// latest-<backupID> hard link the server maintains, excluding the
	// transient "<name>.db.tmp" link-atomicity artifact and never
	// matching timestamped snapshots.
	globPath := path.Join(sourceDir, ripbackup.LatestGlob)

	return &receiver{
		rsyncClient: rsyncClient,
		globPath:    globPath,
		destDir:     destDir,
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

	// Zero-match glob is a failure, not a success: the sender must
	// transfer at least one file. Both paths detect the empty file list
	// before this check — the local path in LocalClient.Run (Go glob
	// before spawning), the SSH path in the sender, which fails on the
	// literal pattern the shell leaves in place — so this size check is
	// the final guard. The sender's final stats report the total size of
	// files (the sum of the file-list lengths), which is zero iff the
	// file list was empty, even when stale files from a database
	// removed from the server's config linger in the destination
	// directory and would make the local glob match. Stats is always
	// non-nil after a successful Run (verified in source v0.3.4: Run
	// wraps the stats from maincmd.ClientRun into &Result{Stats}).
	if result == nil || result.Stats == nil || result.Stats.Size == 0 {
		return nil, fmt.Errorf("no backup files received: server glob matched nothing: %s", r.globPath)
	}

	// Wait closes stdin and blocks until the server process exits. A
	// nonzero exit after a successful protocol run is a server-side
	// failure (e.g. the sender could not read a matched file), so the
	// run fails instead of silently accepting the transfer.
	waitErr := wait()
	if waitErr != nil {
		return nil, fmt.Errorf("rsync server process exited with error: %w", waitErr)
	}

	return result, nil
}

// SSHClient pulls the latest-*.db files from a remote server over SSH.
type SSHClient struct {
	*receiver
	sshCfg ssh.Config
}

// Compile-time check: SSHClient satisfies Client.
var _ Client = (*SSHClient)(nil)

// NewSSHClient creates the rsync receiver for the source glob and
// stores it with the job config.
func NewSSHClient(sshCfg ssh.Config, sourceDir, destDir string) (*SSHClient, error) {
	r, err := newReceiver(sourceDir, destDir)
	if err != nil {
		return nil, err
	}
	return &SSHClient{receiver: r, sshCfg: sshCfg}, nil
}

// Run dials the SSH server, starts the remote rsync binary in server
// mode over a session, and delegates to transfer.
func (c *SSHClient) Run(ctx context.Context) (err error) {
	client, err := ssh.Dial(c.sshCfg)
	if err != nil {
		return fmt.Errorf("failed to dial ssh: %w", err)
	}
	defer func() {
		closeErr := client.Close()
		err = errors.Join(err, closeErr)
	}()

	// ServerCommandOptions returns the arguments to pass to the rsync
	// binary in server mode, generated by gokrazy from the client's own
	// options (e.g. --server --sender -vlogDtpr . /path/to/latest-*.db).
	serverArgs := c.rsyncClient.ServerCommandOptions(c.globPath)

	// Args are joined unquoted: session.Start runs the command through
	// the remote login shell, which splits it and expands the source
	// glob (latest-*.db) into the concrete files the sender transfers.
	// On zero matches the shell leaves the literal pattern in place,
	// which the sender then fails on. The source directory must be free
	// of shell metacharacters. LocalClient.Run performs the same
	// expansion in Go, because its exec.Command runs the binary without
	// a shell.
	remoteCmd := fmt.Sprintf("rsync %s", strings.Join(serverArgs, " "))
	session, err := ssh.NewSession(client, remoteCmd)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := session.Close()
		err = errors.Join(err, closeErr)
	}()

	result, err := c.transfer(ctx, session.Pipe(), session.Wait)
	if err != nil {
		return err
	}

	slog.Info("rsync transfer completed",
		"bytes_read", result.Stats.Read,
		"bytes_written", result.Stats.Written,
		"total_size", result.Stats.Size,
	)

	return nil
}

// LocalClient pulls the latest-*.db files from the same machine.
type LocalClient struct {
	*receiver
}

// Compile-time check: LocalClient satisfies Client.
var _ Client = (*LocalClient)(nil)

// NewLocalClient creates the rsync receiver for the source glob and
// stores it with the job config.
func NewLocalClient(sourceDir, destDir string) (*LocalClient, error) {
	r, err := newReceiver(sourceDir, destDir)
	if err != nil {
		return nil, err
	}
	return &LocalClient{receiver: r}, nil
}

// Run starts the local rsync binary in server mode and delegates to
// transfer.
func (c *LocalClient) Run(ctx context.Context) (err error) {
	// exec.Command runs the rsync binary without a shell, so the source
	// glob in serverArgs would reach the sender literally and fail. The
	// SSH path gets its expansion from the remote login shell
	// (SSHClient.Run); here the same expansion happens in Go. The glob
	// is the trailing serverArgs element: ServerCommandOptions appends
	// the source path last, after the server options and the "." module
	// marker. Zero matches fail before the process is spawned.
	matches, err := filepath.Glob(c.globPath)
	if err != nil {
		return fmt.Errorf("failed to glob source directory: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no backup files received: server glob matched nothing: %s", c.globPath)
	}

	// ServerCommandOptions returns the arguments to pass to the rsync
	// binary in server mode, generated by gokrazy from the client's own
	// options (e.g. --server --sender -vlogDtpr . /path/to/latest-*.db).
	serverArgs := c.rsyncClient.ServerCommandOptions(c.globPath)
	serverArgs = append(serverArgs[:len(serverArgs)-1], matches...)

	cmd := exec.CommandContext(ctx, "rsync", serverArgs...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Join(fmt.Errorf("failed to get stdout pipe: %w", err), stdin.Close())
	}

	err = cmd.Start()
	if err != nil {
		// os/exec already closed the parent-side pipes on failure
		// (Cmd.Start closes parentIOPipes when the process did not start).
		return fmt.Errorf("failed to start local rsync: %w", err)
	}

	rw := &struct {
		io.Reader
		io.Writer
	}{Reader: stdout, Writer: stdin}

	// wait closes stdin to signal the local rsync process that the
	// transfer is done, then blocks until the process exits.
	wait := func() error {
		var errs []error
		closeErr := stdin.Close()
		errs = append(errs, closeErr)
		waitErr := cmd.Wait()
		errs = append(errs, waitErr)
		cmd = nil
		return errors.Join(errs...)
	}

	// Kill the local rsync process if the transfer failed before wait
	// could reap it. The protocol failed, so stdin EOF cannot be relied
	// on for graceful termination — SIGKILL is the deterministic teardown.
	defer func() {
		if cmd != nil {
			killErr := cmd.Process.Kill()
			reapErr := cmd.Wait() // reap the killed process, prevent zombie
			err = errors.Join(err, killErr, reapErr)
			cmd = nil
		}
	}()

	result, err := c.transfer(ctx, rw, wait)
	if err != nil {
		return err
	}

	slog.Info("rsync transfer completed",
		"bytes_read", result.Stats.Read,
		"bytes_written", result.Stats.Written,
		"total_size", result.Stats.Size,
	)

	return nil
}
