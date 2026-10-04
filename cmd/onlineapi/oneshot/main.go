// Command oneshot runs one onlineapi pass and exits. It reads a TOML file
// whose [backup] section has the application configuration shape — the
// same document ripc scaffolds for app mode — and produces an Online
// Backup API snapshot of every due database. A cron job or a systemd
// timer runs it again for the next pass.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/caasmo/restinpieces-backup/onlineapi"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	"github.com/pelletier/go-toml/v2"
)

// defaultConfigPath is the TOML configuration file read at startup.
const defaultConfigPath = "/etc/restinpieces-backup/onlineapi.toml"

// readConfig loads the application configuration from a TOML file. The
// document has the shape the restinpieces application stores, so the
// [backup.online] section configures the databases.
func readConfig(path string) (config.Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg config.Config
	if err := toml.Unmarshal(content, &cfg); err != nil {
		return config.Config{}, fmt.Errorf("failed to parse config file: %w", err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the TOML config file")
	flag.Parse()

	cfg, err := readConfig(*configPath)
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	var pointer atomic.Pointer[config.Config]
	pointer.Store(&cfg)
	handler := onlineapi.New(&pointer, nil)

	// One pass over every configured database; entries whose frequency
	// has not elapsed are skipped by the due check.
	err = handler.Handle(context.Background(), db.Job{})
	if err != nil {
		slog.Error("onlineapi pass failed", "error", err)
		os.Exit(1)
	}
}
