package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces/config"
	toml "github.com/pelletier/go-toml/v2"
)

// defaultConfigPath is the TOML configuration file read at startup.
const defaultConfigPath = "/etc/restinpieces-backup/local-copy.toml"

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the backup TOML configuration")
	flag.Parse()

	cfg, err := readConfig(*configPath)
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	localCopyDaemon := New(cfg, nil)

	r, err := run.NewRunner()
	if err != nil {
		slog.Error("failed to create runner", "error", err)
		os.Exit(1)
	}
	r.Add(localCopyDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGTERM, then shuts the daemon
	// down gracefully within the 15s default deadline. Run never calls
	// os.Exit — main maps the result to an exit code.
	err = r.Run()
	if err != nil {
		slog.Error("runner exited with errors", "error", err)
		os.Exit(1)
	}
}

func readConfig(path string) (*config.Backup, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	cfg := &config.Backup{}
	err = toml.Unmarshal(data, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	err = config.ValidateBackup(cfg)
	if err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return cfg, nil
}
