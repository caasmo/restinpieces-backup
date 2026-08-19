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

	localCopyDaemon := NewLocalCopyDaemon(cfg, nil)

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

// readConfig loads the backup configuration from the TOML file, fills
// per-file tuning defaults, and validates it. The file holds the
// framework's Backup shape: one [files.<label>] section per database.
// (Temporary stopgap for the go-daemon-runner configProvider — see the
// phase lead-in.)
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

	// Hand-written minimal entries (source/dest/frequency only) get the
	// NewBackupFileDefaults tuning values: strategy defaults to online
	// at runtime, which demands pages_per_step >= 1 (Step(0) would copy
	// nothing and never finish) and tolerates a zero sleep interval (no
	// throttling).
	def := config.NewBackupFileDefaults()
	for key, f := range cfg.Files {
		if f.OnlineAPIPagesPerStep == 0 {
			f.OnlineAPIPagesPerStep = def.OnlineAPIPagesPerStep
		}
		if f.OnlineAPISleepInterval.Duration == 0 {
			f.OnlineAPISleepInterval = def.OnlineAPISleepInterval
		}
		cfg.Files[key] = f
	}

	err = config.ValidateBackup(cfg)
	if err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}