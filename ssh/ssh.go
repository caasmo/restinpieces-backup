// Package ssh provides the SSH connection helpers shared by the backup
// client commands: loading the in-memory credentials, the
// host-key-pinned dial, and remote command sessions.
package ssh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/caasmo/restinpieces-backup-client/config"
	cryptossh "golang.org/x/crypto/ssh"
)

// Credentials holds the SSH identity in memory: the connection
// parameters and the parsed keys. LoadCredentials builds it once at
// startup; every dial reuses the parsed keys instead of re-reading the
// key files.
type Credentials struct {
	// User is the SSH login name.
	User string
	// Host is the server's hostname or IP address.
	Host string
	// Port is the server's SSH port.
	Port string
	// signer is the parsed private key used for client authentication.
	signer cryptossh.Signer
	// hostKey is the server's public host key, pinned: a dial against
	// any other host key fails.
	hostKey cryptossh.PublicKey
}

// LoadCredentials reads the private and host key files once, parses
// them, and returns the in-memory credentials used by Dial.
func LoadCredentials(cfg config.SSHConfig) (Credentials, error) {
	key, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return Credentials{}, fmt.Errorf("unable to read private key: %w", err)
	}

	signer, err := cryptossh.ParsePrivateKey(key)
	if err != nil {
		return Credentials{}, fmt.Errorf("unable to parse private key: %w", err)
	}

	hostKey, err := os.ReadFile(cfg.HostKeyPath)
	if err != nil {
		return Credentials{}, fmt.Errorf("unable to read host key: %w", err)
	}

	pubKey, _, _, _, err := cryptossh.ParseAuthorizedKey(hostKey)
	if err != nil {
		return Credentials{}, fmt.Errorf("unable to parse host key: %w", err)
	}

	return Credentials{
		User:    cfg.User,
		Host:    cfg.Host,
		Port:    cfg.Port,
		signer:  signer,
		hostKey: pubKey,
	}, nil
}

// Dial authenticates with the in-memory credentials and returns an SSH
// client. The host key is pinned: LoadCredentials validated the host
// key file (provisioned out-of-band), and a dial against any other host
// key fails.
func Dial(creds Credentials) (*cryptossh.Client, error) {
	sshConfig := &cryptossh.ClientConfig{
		User: creds.User,
		Auth: []cryptossh.AuthMethod{
			cryptossh.PublicKeys(creds.signer),
		},
		HostKeyCallback: cryptossh.FixedHostKey(creds.hostKey),
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%s", creds.Host, creds.Port)
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

// Quote wraps s in single quotes for a POSIX login shell, escaping any
// embedded single quotes, so the remote shell treats the value as a
// literal word instead of interpreting its metacharacters.
//
// This is the primitive used to make a user-controlled path inert when
// it is interpolated into a remote shell command. Anything inside the
// returned quotes is taken literally: no glob expansion, no parameter
// expansion, no command substitution, no word splitting.
//
// Example:
//
//	Quote("/var/backups")   // "'/var/backups'"
//	Quote("a b; rm -rf /")  // "'a b; rm -rf /'"
//	Quote("/path/it's")     // "'/path/it'\''s'"
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
