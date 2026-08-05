package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/caasmo/restinpieces-backup-client/transport"
	cryptossh "golang.org/x/crypto/ssh"
)

// Config holds the parameters needed to establish an SSH connection.
type Config struct {
	User           string
	Host           string
	Port           string
	PrivateKeyPath string
	HostKeyPath    string
}

// ConfigFromEnv reads SSH configuration from environment variables.
// RIP_BCK_SSH_PORT defaults to "22"; all other variables are required.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		User:           os.Getenv("RIP_BCK_SSH_USER"),
		Host:           os.Getenv("RIP_BCK_SSH_HOST"),
		Port:           os.Getenv("RIP_BCK_SSH_PORT"),
		PrivateKeyPath: os.Getenv("RIP_BCK_SSH_PRIVATE_KEY_PATH"),
		HostKeyPath:    os.Getenv("RIP_BCK_SSH_HOST_KEY_PATH"),
	}
	if cfg.Port == "" {
		cfg.Port = "22"
	}

	switch {
	case cfg.User == "":
		return cfg, fmt.Errorf("RIP_BCK_SSH_USER is required")
	case cfg.Host == "":
		return cfg, fmt.Errorf("RIP_BCK_SSH_HOST is required")
	case cfg.PrivateKeyPath == "":
		return cfg, fmt.Errorf("RIP_BCK_SSH_PRIVATE_KEY_PATH is required")
	case cfg.HostKeyPath == "":
		return cfg, fmt.Errorf("RIP_BCK_SSH_HOST_KEY_PATH is required")
	}

	return cfg, nil
}

// Transport implements transport.Transport over an SSH connection.
type Transport struct {
	client  *cryptossh.Client
	session *cryptossh.Session
	stdin   io.WriteCloser
}

// Verify interface compliance.
var _ transport.Transport = (*Transport)(nil)

// New dials the SSH server and returns a Transport that owns the
// connection. The host key is pinned: HostKeyPath must point to the
// server's public host key (provisioned out-of-band), and a dial
// against any other host key fails.
func New(cfg Config) (*Transport, error) {
	key, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read private key: %w", err)
	}

	signer, err := cryptossh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %w", err)
	}

	hostKey, err := os.ReadFile(cfg.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read host key: %w", err)
	}

	pubKey, _, _, _, err := cryptossh.ParseAuthorizedKey(hostKey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse host key: %w", err)
	}

	sshConfig := &cryptossh.ClientConfig{
		User: cfg.User,
		Auth: []cryptossh.AuthMethod{
			cryptossh.PublicKeys(signer),
		},
		HostKeyCallback: cryptossh.FixedHostKey(pubKey),
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)
	client, err := cryptossh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to dial ssh: %w", err)
	}

	return &Transport{client: client}, nil
}

// Connect opens an SSH session, starts the remote rsync binary in
// server mode with the given server args, and returns the io.ReadWriter
// the client protocol runs over. On partial failure (session created
// but pipe setup fails), Connect closes the session before returning.
// The context is not used here: dialing already happened in New with a
// fixed timeout, and cancellation of the transfer is handled by
// rsyncClient.Run.
func (t *Transport) Connect(_ context.Context, serverArgs []string) (io.ReadWriter, error) {
	session, err := t.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH session for rsync: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		closeErr := session.Close()
		return nil, errors.Join(fmt.Errorf("failed to get stdin pipe: %w", err), closeErr)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		closeErr := session.Close()
		return nil, errors.Join(fmt.Errorf("failed to get stdout pipe: %w", err), closeErr)
	}

	session.Stderr = os.Stderr

	remoteCmd := fmt.Sprintf("rsync %s", strings.Join(serverArgs, " "))
	err = session.Start(remoteCmd)
	if err != nil {
		closeErr := session.Close()
		return nil, errors.Join(fmt.Errorf("failed to start remote rsync: %w", err), closeErr)
	}

	t.session = session
	t.stdin = stdin

	rw := &struct {
		io.Reader
		io.Writer
	}{Reader: stdout, Writer: stdin}

	return rw, nil
}

// Wait closes stdin to signal the remote rsync process that the
// transfer is done, then blocks until the remote command exits.
func (t *Transport) Wait() error {
	var errs []error
	if t.stdin != nil {
		closeErr := t.stdin.Close()
		errs = append(errs, closeErr)
		t.stdin = nil
	}
	if t.session != nil {
		waitErr := t.session.Wait()
		errs = append(errs, waitErr)
		t.session = nil
	}
	return errors.Join(errs...)
}

// Close releases all resources: session and SSH connection. Idempotent.
func (t *Transport) Close() error {
	var errs []error
	if t.stdin != nil {
		closeErr := t.stdin.Close()
		errs = append(errs, closeErr)
		t.stdin = nil
	}
	if t.session != nil {
		closeErr := t.session.Close()
		errs = append(errs, closeErr)
		t.session = nil
	}
	if t.client != nil {
		closeErr := t.client.Close()
		errs = append(errs, closeErr)
		t.client = nil
	}
	return errors.Join(errs...)
}
