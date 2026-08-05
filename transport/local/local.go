package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/caasmo/restinpieces-backup-client/transport"
)

// Transport implements transport.Transport by running the rsync binary
// on the local machine. The serverArgs from rsyncclient.ServerCommandOptions
// are passed directly — they are identical to what SSH runs remotely.
type Transport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// Verify interface compliance.
var _ transport.Transport = (*Transport)(nil)

// New returns a local transport. No configuration is needed — the
// rsync binary is discovered via PATH.
func New() *Transport {
	return &Transport{}
}

// Connect starts the local rsync binary in server mode with the given
// server args and returns the io.ReadWriter the client protocol runs
// over. On partial failure, Connect cleans up before returning.
func (t *Transport) Connect(ctx context.Context, serverArgs []string) (io.ReadWriter, error) {
	t.cmd = exec.CommandContext(ctx, "rsync", serverArgs...)
	t.cmd.Stderr = os.Stderr

	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("failed to get stdout pipe: %w", err), stdin.Close())
	}

	err = t.cmd.Start()
	if err != nil {
		// os/exec already closed the parent-side pipes on failure
		// (Cmd.Start closes parentIOPipes when the process did not start).
		return nil, fmt.Errorf("failed to start local rsync: %w", err)
	}

	t.stdin = stdin

	rw := &struct {
		io.Reader
		io.Writer
	}{Reader: stdout, Writer: stdin}

	return rw, nil
}

// Wait closes stdin to signal the local rsync process that the
// transfer is done, then blocks until the process exits.
func (t *Transport) Wait() error {
	var errs []error
	if t.stdin != nil {
		closeErr := t.stdin.Close()
		errs = append(errs, closeErr)
		t.stdin = nil
	}
	if t.cmd != nil {
		waitErr := t.cmd.Wait()
		errs = append(errs, waitErr)
		t.cmd = nil
	}
	return errors.Join(errs...)
}

// Close kills the local rsync process if still running and reaps it.
// Idempotent.
func (t *Transport) Close() error {
	var errs []error
	if t.stdin != nil {
		closeErr := t.stdin.Close()
		errs = append(errs, closeErr)
		t.stdin = nil
	}
	if t.cmd != nil && t.cmd.Process != nil {
		killErr := t.cmd.Process.Kill()
		reapErr := t.cmd.Wait() // reap the killed process, prevent zombie
		errs = append(errs, killErr, reapErr)
		t.cmd = nil
	}
	return errors.Join(errs...)
}
