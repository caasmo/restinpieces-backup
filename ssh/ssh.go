// Package ssh provides the SSH connection helpers shared by the backup
// client commands: environment configuration, the host-key-pinned
// dial, and remote command sessions.
package ssh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

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

// Dial authenticates with the configured private key and returns an
// SSH client. The host key is pinned: HostKeyPath must point to the
// server's public host key (provisioned out-of-band), and a dial
// against any other host key fails.
func Dial(cfg Config) (*cryptossh.Client, error) {
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

	return client, nil
}

// Session is an SSH session running a remote command, with its stdin
// and stdout pipes wired.
type Session struct {
	session *cryptossh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
}

// NewSession opens a new session on client, wires its stdin, stdout
// and stderr pipes (stderr routed to the caller's stderr), and starts
// the given remote command. Release the session with Close.
func NewSession(client *cryptossh.Client, cmd string) (*Session, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH session: %w", err)
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

	err = session.Start(cmd)
	if err != nil {
		closeErr := session.Close()
		return nil, errors.Join(fmt.Errorf("failed to start remote command: %w", err), closeErr)
	}

	return &Session{session: session, stdin: stdin, stdout: stdout}, nil
}

// Pipe returns the bidirectional pipe of the remote command: Read
// reads its stdout, Write writes to its stdin.
func (s *Session) Pipe() io.ReadWriter {
	return &struct {
		io.Reader
		io.Writer
	}{Reader: s.stdout, Writer: s.stdin}
}

// Wait closes stdin to signal the remote command that the input is
// done, then blocks until the command exits.
func (s *Session) Wait() error {
	var errs []error
	if s.stdin != nil {
		closeErr := s.stdin.Close()
		errs = append(errs, closeErr)
		s.stdin = nil
	}
	if s.session != nil {
		waitErr := s.session.Wait()
		errs = append(errs, waitErr)
		s.session = nil
	}
	return errors.Join(errs...)
}

// Close releases the session and its pipes. Idempotent.
func (s *Session) Close() error {
	var errs []error
	if s.stdin != nil {
		closeErr := s.stdin.Close()
		errs = append(errs, closeErr)
		s.stdin = nil
	}
	if s.session != nil {
		closeErr := s.session.Close()
		errs = append(errs, closeErr)
		s.session = nil
	}
	return errors.Join(errs...)
}
