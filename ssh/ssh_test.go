package ssh

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"
)

const testPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZWQyNTUxOQAAACCrscagRQGiwrqKJLa+9ANa5JRcT7RD/zIWIraIwWOoRwAAAIiveqiSr3qokgAAAAtzc2gtZWQyNTUxOQAAACCrscagRQGiwrqKJLa+9ANa5JRcT7RD/zIWIraIwWOoRwAAAED2Epb69dqWbvp347Zibo65xjqgOQ0fPcq/L8HJtkn+4KuxxqBFAaLCuooktr70A1rklFxPtEP/MhYitojBY6hHAAAAAAECAwQF
-----END OPENSSH PRIVATE KEY-----`

const testHostKeyPub = `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKuxxqBFAaLCuooktr70A1rklFxPtEP/MhYitojBY6hH`

func mustTestCreds(t *testing.T, host, port string) Credentials {
	t.Helper()
	signer, err := cryptossh.ParsePrivateKey([]byte(testPrivateKey))
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	pubKey, _, _, _, err := cryptossh.ParseAuthorizedKey([]byte(testHostKeyPub))
	if err != nil {
		t.Fatalf("parse host key: %v", err)
	}
	return Credentials{
		User:    "test",
		Host:    host,
		Port:    port,
		signer:  signer,
		hostKey: pubKey,
	}
}

// TestQuote verifies Quote wraps its input in single quotes, escaping
// embedded single quotes, so a POSIX shell treats the value literally.
func TestQuote(t *testing.T) {
	testCases := []struct {
		in   string
		want string
	}{
		{"/var/backups", "'/var/backups'"},
		{"a b; rm -rf /", "'a b; rm -rf /'"},
		{"/path/it's", "'/path/it'\\''s'"},
		{"", "''"},
	}
	for _, tc := range testCases {
		got := Quote(tc.in)
		if got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDialContextCancelsHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Never send SSH version string — NewClientConn blocks on
		// version exchange until DialContext's AfterFunc closes conn.
		time.Sleep(30 * time.Second)
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	creds := mustTestCreds(t, "127.0.0.1", port)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = DialContext(ctx, creds)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DialContext succeeded against stalled handshake")
	}
	if strings.Contains(err.Error(), "host key is required") || strings.Contains(err.Error(), "signer is required") {
		t.Fatalf("DialContext failed with validation error, not handshake cancel: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("DialContext did not respect ctx: elapsed %v > 5s, err %v", elapsed, err)
	}
}
