package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/caasmo/restinpieces-backup-client/rsync"
	"github.com/caasmo/restinpieces-backup-client/ssh"
	"github.com/caasmo/restinpieces-backup-client/verification"
)

func main() {
	useLocal := flag.Bool("l", false, "run rsync on the same machine instead of over SSH")
	flag.BoolVar(useLocal, "local", false, "run rsync on the same machine instead of over SSH")
	flag.Parse()

	sourceDir := os.Getenv("RIP_BCK_SOURCE_DIR")
	destDir := os.Getenv("RIP_BCK_DEST_DIR")

	if sourceDir == "" {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_SOURCE_DIR is required"))
		os.Exit(1)
	}
	if destDir == "" {
		slog.Error("Backup failed", "error", fmt.Errorf("RIP_BCK_DEST_DIR is required"))
		os.Exit(1)
	}

	err := os.MkdirAll(destDir, 0755)
	if err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("failed to create destination directory: %w", err))
		os.Exit(1)
	}

	slog.Info("Starting rsync backup client")

	// Connection selection: the -l/--local flag runs rsync on the same
	// machine; the default connects over SSH.
	var client rsync.Client
	if *useLocal {
		client, err = rsync.NewLocalClient(sourceDir, destDir)
	} else {
		sshCfg, cfgErr := ssh.ConfigFromEnv()
		if cfgErr != nil {
			slog.Error("Backup failed", "error", fmt.Errorf("failed to read SSH config: %w", cfgErr))
			os.Exit(1)
		}
		client, err = rsync.NewSSHClient(sshCfg, sourceDir, destDir)
	}
	if err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}

	if err := client.Run(context.Background()); err != nil {
		slog.Error("Backup failed", "error", err)
		os.Exit(1)
	}

	if err := verification.VerifyBackup(destDir); err != nil {
		slog.Error("Backup failed", "error", fmt.Errorf("backup verification failed: %w", err))
		os.Exit(1)
	}
}
