package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces-backup-client/rsync"
	"github.com/caasmo/restinpieces-backup-client/ssh"
)

func main() {
	useLocal := flag.Bool("l", false, "run rsync on the same machine instead of over SSH")
	flag.BoolVar(useLocal, "local", false, "run rsync on the same machine instead of over SSH")
	flag.Parse()

	sourceDir := os.Getenv("RIP_BCK_SOURCE_DIR")
	destDir := os.Getenv("RIP_BCK_DEST_DIR")
	intervalStr := os.Getenv("RIP_BCK_INTERVAL")

	if sourceDir == "" {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_SOURCE_DIR is required"))
		os.Exit(1)
	}
	if destDir == "" {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_DEST_DIR is required"))
		os.Exit(1)
	}
	if intervalStr == "" {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_INTERVAL is required"))
		os.Exit(1)
	}
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("failed to parse RIP_BCK_INTERVAL: %w", err))
		os.Exit(1)
	}
	// time.NewTicker panics on a non-positive duration, and
	// time.ParseDuration accepts "0s" and negatives — reject them here
	// so the daemon fails fast at startup instead of crashing the
	// goroutine on its first tick.
	if interval <= 0 {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_INTERVAL must be positive"))
		os.Exit(1)
	}

	// Create the destination directory once at startup. It is safe to
	// skip re-creating it per tick: the rsync receiver recreates it on
	// every run (gokrazy maincmd.ClientRun calls os.MkdirAll on the
	// destination), so a removal mid-run self-heals on the next tick.
	err = os.MkdirAll(destDir, 0755)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("failed to create destination directory: %w", err))
		os.Exit(1)
	}

	slog.Info("Starting rsync backup daemon", "interval", interval)

	// Connection selection: the -l/--local flag runs rsync on the same
	// machine; the default connects over SSH. Each branch handles its
	// own error immediately, as cmd/sftp does.
	var client rsync.Client
	if *useLocal {
		client, err = rsync.NewLocalClient(sourceDir, destDir)
		if err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
	} else {
		sshCfg, cfgErr := ssh.ConfigFromEnv()
		if cfgErr != nil {
			slog.Error("Backup failed", "error", fmt.Errorf("failed to read SSH config: %w", cfgErr))
			os.Exit(1)
		}
		client, err = rsync.NewSSHClient(sshCfg, sourceDir, destDir)
		if err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
	}

	backupDaemon := NewBackupDaemon(client, destDir, interval, nil)

	r, err := run.NewRunner()
	if err != nil {
		slog.Error("failed to create runner", "error", err)
		os.Exit(1)
	}
	r.Add(backupDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGTERM, then shuts the daemon
	// down gracefully within the 15s default deadline. Run never calls
	// os.Exit — main maps the result to an exit code.
	err = r.Run()
	if err != nil {
		slog.Error("runner exited with errors", "error", err)
		os.Exit(1)
	}
}
