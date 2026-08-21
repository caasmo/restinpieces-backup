package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces-backup/sqlitersync/origin"
	"github.com/caasmo/restinpieces/config"
	"github.com/pelletier/go-toml/v2"
)

// originCfg is the standalone origin configuration: the [backup]
// section of the application config document. It satisfies
// OriginConfig; the daemon reads the backup files through
// BackupFiles.
//
// The section shape lives in restinpieces (config.Backup and
// config.BackupFile) and the strategy constants
// (config.BackupStrategyOnline, config.BackupStrategyVacuum,
// config.BackupStrategySqliteRsync). Users who do not want to import
// restinpieces can copy those two structs and the three constants here
// and use them locally (or hard-code "sqlite-rsync" in the strategy
// filter).
//
// There is no validation in the standalone path: the daemon only
// checks that at least one file with strategy "sqlite-rsync" is
// configured. Users who want validation can copy
// config.ValidateBackup from restinpieces here and call it after
// unmarshal.
type originCfg struct {
	Backup config.Backup
}

// BackupFiles returns the configured backup files, keyed by database
// label.
func (c originCfg) BackupFiles() map[string]config.BackupFile {
	return c.Backup.Files
}

// readConfig loads the configuration from a TOML file whose [backup]
// section has the application configuration shape — the same document
// ripc scaffolds for app mode. Paths and WAL mode are validated by the
// library at sync time, not at boot.
func readConfig(path string) (originCfg, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return originCfg{}, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg originCfg
	if err := toml.Unmarshal(content, &cfg); err != nil {
		return originCfg{}, fmt.Errorf("failed to parse config file: %w", err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "path to the TOML config file")
	flag.Parse()

	cfg, err := readConfig(*configPath)
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	var pointer atomic.Pointer[originCfg]
	pointer.Store(&cfg)
	originDaemon := origin.NewOriginDaemon[originCfg](&pointer, nil)

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
