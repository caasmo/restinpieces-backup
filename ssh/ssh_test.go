package ssh

import (
	"context"
	"net"
	"testing"
	"time"
)

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
	creds := Credentials{Host: "127.0.0.1", Port: port}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = DialContext(ctx, creds)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("DialContext succeeded against stalled handshake")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("DialContext did not respect ctx: elapsed %v > 5s, err %v", elapsed, err)
	}
}
