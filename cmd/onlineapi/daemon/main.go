package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces-backup/onlineapi"
	"github.com/caasmo/restinpieces/config"
	"github.com/pelletier/go-toml/v2"
)

// defaultConfigPath is the TOML configuration file read at startup.
const defaultConfigPath = "/etc/restinpieces-backup/onlineapi.toml"

// onlineapiCfg is the standalone onlineapi configuration: the [backup]
// section of the application config document. It satisfies
// OnlineApiConfig; the daemon reads the backup configuration through
// BackupOnlineAPI.
//
// The section shape lives in restinpieces (config.Backup,
// config.BackupOnlineAPI and the entry types). Users who do not want to
// import restinpieces can copy those structs here and use them
// locally.
type onlineapiCfg struct {
	Backup config.Backup `toml:"backup"`
}

func (c onlineapiCfg) BackupOnlineAPI() config.BackupOnlineAPI {
	return c.Backup.OnlineAPI
}

// readConfig loads the configuration from a TOML file whose [backup]
// section has the application configuration shape — the same document
// ripc scaffolds for app mode. The whole backup section is validated
// like the app validates it.
func readConfig(path string) (onlineapiCfg, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return onlineapiCfg{}, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg onlineapiCfg
	if err := toml.Unmarshal(content, &cfg); err != nil {
		return onlineapiCfg{}, fmt.Errorf("failed to parse config file: %w", err)
	}
	if err := config.ValidateBackup(&cfg.Backup); err != nil {
		return onlineapiCfg{}, fmt.Errorf("config validation failed: %w", err)
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

	var pointer atomic.Pointer[onlineapiCfg]
	pointer.Store(&cfg)
	onlineapiDaemon := onlineapi.New[onlineapiCfg](&pointer, nil)

	r, err := run.NewRunner()
	if err != nil {
		slog.Error("failed to create runner", "error", err)
		os.Exit(1)
	}
	r.Add(onlineapiDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGTERM, then shuts the daemon
	// down gracefully within the 15s default deadline. Run never calls
	// os.Exit — main maps the result to an exit code.
	err = r.Run()
	if err != nil {
		slog.Error("runner exited with errors", "error", err)
		os.Exit(1)
	}
}
