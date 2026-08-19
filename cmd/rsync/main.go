package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

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

	err = os.MkdirAll(cfg.DestDir, 0755)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("failed to create destination directory: %w", err))
		os.Exit(1)
	}

	slog.Info("Starting rsync backup client")

	// Connection selection: the -l/--local flag runs rsync on the same
	// machine; the default connects over SSH.
	var client rsync.Client
	if *useLocal {
		client, err = rsync.NewLocalClient(cfg.SourceDir, cfg.DestDir)
	} else {
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
	}
	if err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}

	err = client.Run(context.Background())
	if err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}

	err = rsync.VerifyBackup(cfg.DestDir)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("backup verification failed: %w", err))
		os.Exit(1)
	}
}
