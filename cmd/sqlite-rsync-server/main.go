package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caasmo/go-daemon-runner/run"
)

// Config holds the sqlite-rsync server configuration, loaded from the
// environment by readConfig.
type Config struct {
	// ListenAddr is the TCP address the server listens on.
	ListenAddr string
	// Files maps a database label to the origin database path.
	Files map[string]string
	// SyncTimeout is the longest one sync may run. A sync that takes
	// longer is aborted, releasing its read transaction on the origin
	// database, which blocks WAL checkpointing while open. Zero uses
	// the default of 15 minutes.
	SyncTimeout time.Duration
}

// readConfig loads the configuration from the environment: the listen
// address from RIP_BCK_ORIGIN_LISTEN_ADDR, the database to serve from
// RIP_BCK_ORIGIN_FILE. One database is configured, served under the
// fixed label "db". Paths and WAL mode are validated by the library at
// sync time, not at boot.
func readConfig() (Config, error) {
	listenAddr := os.Getenv("RIP_BCK_ORIGIN_LISTEN_ADDR")
	if listenAddr == "" {
		return Config{}, fmt.Errorf("RIP_BCK_ORIGIN_LISTEN_ADDR is required")
	}
	file := os.Getenv("RIP_BCK_ORIGIN_FILE")
	if file == "" {
		return Config{}, fmt.Errorf("RIP_BCK_ORIGIN_FILE is required")
	}
	return Config{
		ListenAddr: listenAddr,
		Files:      map[string]string{"db": file},
	}, nil
}

func main() {
	cfg, err := readConfig()
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	originDaemon := NewOriginDaemon(cfg, nil)

	r, err := run.NewRunner()
	if err != nil {
		slog.Error("failed to create runner", "error", err)
		os.Exit(1)
	}
	r.Add(originDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGTERM, then shuts the daemon
	// down gracefully within the 15s default deadline. Run never calls
	// os.Exit — main maps the result to an exit code.
	err = r.Run()
	if err != nil {
		slog.Error("runner exited with errors", "error", err)
		os.Exit(1)
	}
}
