package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces-backup/config"
	"github.com/caasmo/restinpieces-backup/landlock"
	"github.com/caasmo/restinpieces-backup/rsync"
	"github.com/caasmo/restinpieces-backup/ssh"
)

func main() {
	useLocal := flag.Bool("l", false, "run rsync on the same machine instead of over SSH")
	flag.BoolVar(useLocal, "local", false, "run rsync on the same machine instead of over SSH")
	flag.Parse()

	cfg, err := config.New()
	if err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}
	// The daemon requires a positive interval: config.New rejects a set
	// non-positive value, and ValidateRsyncDaemon rejects a missing one,
	// so time.NewTicker never sees a zero interval.
	err = cfg.ValidateRsyncDaemon()
	if err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}

	// Create the destination directory once at startup. It is safe to
	// skip re-creating it per tick: the rsync receiver recreates it on
	// every run (gokrazy maincmd.ClientRun calls os.MkdirAll on the
	// destination), so a removal mid-run self-heals on the next tick.
	err = os.MkdirAll(cfg.DestDir, 0755)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("failed to create destination directory: %w", err))
		os.Exit(1)
	}

	slog.Info("Starting rsync backup daemon", "interval", cfg.RsyncDaemon.Interval)

	// Connection selection: the -l/--local flag runs rsync on the same
	// machine; the default connects over SSH. Each branch handles its
	// own error immediately, as cmd/sftp/oneshot does.
	var client rsync.Client
	if *useLocal {
		client, err = rsync.NewLocalClient(cfg.SourceDir, cfg.DestDir)
		if err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
	} else {
		// Load the SSH keys once at startup and keep them in memory,
		// reused on every dial — the standard ssh-agent pattern
		// (README "SSH mode").
		cfgErr := cfg.ValidateSSH()
		if cfgErr != nil {
			slog.Error("Backup failed", "error", fmt.Errorf("failed to read SSH config: %w", cfgErr))
			os.Exit(1)
		}
		creds, loadErr := ssh.LoadCredentials(cfg.SSH)
		if loadErr != nil {
			slog.Error("Backup failed", "error", fmt.Errorf("failed to load SSH credentials: %w", loadErr))
			os.Exit(1)
		}

		// Confine the process for the rest of its life now that the SSH
		// keys are in memory (the allowlist and the rationale live in
		// the landlock package).
		err = landlock.Restrict(cfg.DestDir)
		if err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
		landlock.Verify()

		client, err = rsync.NewSSHClient(creds, cfg.SourceDir, cfg.DestDir)
		if err != nil {
			slog.Error("Backup failed", "error", err)
			os.Exit(1)
		}
	}

	backupDaemon := NewBackupDaemon(client, cfg.DestDir, cfg.RsyncDaemon.Interval, nil)

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
