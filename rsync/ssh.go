package rsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/caasmo/restinpieces-backup-client/ssh"
)

// SSHClient pulls the latest-*.db files from a remote server over SSH.
type SSHClient struct {
	*receiver
	creds ssh.Credentials
}

// Compile-time check: SSHClient satisfies Client.
var _ Client = (*SSHClient)(nil)

// NewSSHClient creates the rsync receiver for the source glob and
// stores it with the in-memory SSH credentials.
func NewSSHClient(creds ssh.Credentials, sourceDir, destDir string) (*SSHClient, error) {
	r, err := newReceiver(sourceDir, destDir)
	if err != nil {
		return nil, err
	}
	return &SSHClient{receiver: r, creds: creds}, nil
}

// remoteRsyncArgs returns the argv for the remote rsync server
// process, with the source glob argument made shell-safe.
//
// gokrazy's ServerCommandOptions returns the fixed server-mode flags
// (e.g. --server --sender -vlogDtpr .) followed by the source path as
// its last element. The source path is the only user-controlled part:
// it is built from RIP_BCK_SOURCE_DIR, which arrives from the
// environment with no guarantees about its characters.
//
// The directory portion of the source path is shell-quoted so shell
// metacharacters in RIP_BCK_SOURCE_DIR cannot be interpreted as
// commands on the remote host. The latest-*.db glob is left unquoted
// so the remote login shell still expands it into the concrete files
// the sender transfers.
//
// Example:
//
//	c.remoteRsyncArgs()
//	// sourceDir = "/var/backups", sourceFileGlob = "latest-*.db"
//	// => [--server --sender -vlogDtpr . '/var/backups'/latest-*.db]
func (c *SSHClient) remoteRsyncArgs() []string {
	// Quote the source directory so shell metacharacters in
	// RIP_BCK_SOURCE_DIR are inert; leave the file glob unquoted so
	// the remote login shell expands it.
	source := ssh.Quote(c.sourceDir) + "/" + c.sourceFileGlob
	return c.rsyncClient.ServerCommandOptions(source)
}

// Run dials the SSH server, starts the remote rsync binary in server
// mode over a session, and delegates to transfer.
func (c *SSHClient) Run(ctx context.Context) (err error) {
	client, err := ssh.Dial(c.creds)
	if err != nil {
		return fmt.Errorf("failed to dial ssh: %w", err)
	}
	defer func() {
		closeErr := client.Close()
		err = errors.Join(err, closeErr)
	}()

	// Build the remote argv with the source glob shell-quoted, then
	// join it into the command string the remote login shell runs.
	serverArgs := c.remoteRsyncArgs()
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

	sizeMB := result.Stats.Size / (1024 * 1024)
	slog.Info(fmt.Sprintf("sender reports: read %d bytes from connection, sent %d bytes, total size of all files on source is %d MB",
		result.Stats.Read, result.Stats.Written, sizeMB))

	return nil
}
