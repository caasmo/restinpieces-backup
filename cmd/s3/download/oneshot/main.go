// Command oneshot runs one S3 download pass and exits. It reads the [s3]
// and [backup.s3-download] tables from a TOML file, so it needs neither
// the application database nor the age key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/caasmo/restinpieces-backup/s3/download"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	"github.com/pelletier/go-toml/v2"
)

// readConfig loads the application config document from a TOML file: the
// [s3] and [backup.s3-download] tables the handler reads. The file starts
// from the framework defaults, so the sections the command does not need
// can be left out.
func readConfig(path string) (*config.Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	cfg := config.NewDefaultConfig()
	if err := toml.Unmarshal(content, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config file: %w", err)
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "Path to the TOML config file (required)")
	flag.Parse()

	if *configPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := readConfig(*configPath)
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	var pointer atomic.Pointer[config.Config]
	pointer.Store(cfg)
	handler := download.New(&pointer, nil)

	err = handler.Handle(context.Background(), db.Job{})
	if err != nil {
		slog.Error("S3 download failed", "error", err)
		os.Exit(1)
	}
}
