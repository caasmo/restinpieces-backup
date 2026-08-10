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
)

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
	matches, err := filepath.Glob(path.Join(c.sourceDir, c.sourceFileGlob))
	if err != nil {
		return fmt.Errorf("failed to glob source directory: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no backup files received: server glob matched nothing: %s",
			path.Join(c.sourceDir, c.sourceFileGlob))
	}

	// ServerCommandOptions(path string, paths ...string) requires the
	// first source as a separate non-variadic argument, so the first
	// match goes in path and the rest are spread into paths.
	serverArgs := c.rsyncClient.ServerCommandOptions(matches[0], matches[1:]...)

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

	sizeMB := result.Stats.Size / (1024 * 1024)
	slog.Info(fmt.Sprintf("sender reports: read %d bytes from connection, sent %d bytes, total size of all files on source is %d MB",
		result.Stats.Read, result.Stats.Written, sizeMB))

	return nil
}
