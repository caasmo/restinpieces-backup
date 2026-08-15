package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/caasmo/go-daemon-runner/run"
	"github.com/caasmo/restinpieces-backup-client/config"
	"github.com/caasmo/restinpieces-backup-client/landlock"
	"github.com/caasmo/restinpieces-backup-client/ssh"
)

// Hardcoded configuration for the initial client: one database, one
// label, one fixed interval. Only the label and the replica directory
// come from the environment.
const (
	// syncInterval is the time between syncs. The value coincidentally
	// matches defaultSyncInterval (daemon.go), the fallback used when
	// the interval is zero; this one is the actual cadence main always
	// passes.
	syncInterval = 15 * time.Minute
	// originAddr is the origin server's loopback listener, reachable
	// directly in local mode and through an SSH channel in SSH mode.
	originAddr = "127.0.0.1:9909"
)

// readConfig loads the minimal configuration from the environment: the
// database label from RIP_BCK_REPLICA_LABEL, the replica directory from
// RIP_BCK_REPLICA_DIR. The label must match the label the origin server
// serves. The replica database lives at <dir>/<label>.db.
func readConfig() (label, replicaDir string, err error) {
	label = os.Getenv("RIP_BCK_REPLICA_LABEL")
	if label == "" {
		return "", "", fmt.Errorf("RIP_BCK_REPLICA_LABEL is required")
	}
	replicaDir = os.Getenv("RIP_BCK_REPLICA_DIR")
	if replicaDir == "" {
		return "", "", fmt.Errorf("RIP_BCK_REPLICA_DIR is required")
	}
	return label, replicaDir, nil
}

func main() {
	useLocal := flag.Bool("l", false, "run the sync on the same machine instead of over SSH")
	flag.BoolVar(useLocal, "local", false, "run the sync on the same machine instead of over SSH")
	flag.Parse()

	label, replicaDir, err := readConfig()
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}
	// Create the replica directory once at startup so the replica
	// database file can be created on the first sync.
	err = os.MkdirAll(replicaDir, 0755)
	if err != nil {
		slog.Error("failed to create replica directory", "error", err)
		os.Exit(1)
	}
	replicaPath := filepath.Join(replicaDir, label+".db")

	// Connection selection: the -l/--local flag runs the sync on the
	// same machine; the default connects over SSH. Each branch handles
	// its own error immediately.
	var client Client
	if *useLocal {
		client = &LocalClient{originAddr: originAddr}
	} else {
		// The SSH credentials are hardcoded for now; LoadCredentials
		// keeps the keys in memory, reused on every dial. The host here
		// is a placeholder: it points at the local machine so SSH mode
		// can be exercised in a dev setup, but a real deployment sets
		// it to the machine running the origin server.
		creds, loadErr := ssh.LoadCredentials(config.SSHConfig{
			User:           "backup",
			Host:           "127.0.0.1",
			Port:           "22",
			PrivateKeyPath: "/etc/restinpieces-backup-client/backup_ed25519",
			HostKeyPath:    "/etc/restinpieces-backup-client/host_key",
		})
		if loadErr != nil {
			slog.Error("failed to load SSH credentials", "error", loadErr)
			os.Exit(1)
		}

		// Confine the process for the rest of its life. The client is
		// the receiver of the sync: it parses the origin's stream and
		// writes it to disk, and it holds the SSH keys in memory — so
		// a bug or a malicious origin cannot read or write anything
		// beyond the replica directory and /etc (the allowlist and the
		// full rationale live in the landlock package). The keys are
		// already in memory, and the replica directory exists, so no
		// legitimate access is denied. The cage applies in SSH mode
		// only: local mode is the trusted same-machine dev/test
		// transport.
		err = landlock.Restrict(replicaDir)
		if err != nil {
			slog.Error("failed to apply landlock", "error", err)
			os.Exit(1)
		}
		landlock.Verify()

		client = &SSHClient{creds: creds, originAddr: originAddr}
	}

	replicaDaemon := NewReplicaDaemon(client, map[string]string{label: replicaPath}, syncInterval, nil)

	r, err := run.NewRunner()
	if err != nil {
		slog.Error("failed to create runner", "error", err)
		os.Exit(1)
	}
	r.Add(replicaDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGTERM, then shuts the daemon
	// down gracefully within the 15s default deadline. Run never calls
	// os.Exit — main maps the result to an exit code.
	err = r.Run()
	if err != nil {
		slog.Error("runner exited with errors", "error", err)
		os.Exit(1)
	}
}
