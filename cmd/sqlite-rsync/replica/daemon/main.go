// Command daemon is the replica side of the sqlite3_rsync backup
// flow: it periodically asks the origin server for the database behind
// each configured entry and applies the received pages to that entry's
// local file. The -config flag names the TOML configuration file; the
// -l/--local flag selects the transport.
//
// The daemon runs without landlock confinement, and that is a
// considered choice, not an omission. The threat a filesystem cage
// answers is a compromised or buggy origin steering the daemon to
// arbitrary reads or writes. The protocol denies that attack: it is
// label-addressed — the replica sends an entry name, never a path —
// and the receiver grammar has no operation expressible as "write
// somewhere else"; each sync applies the incoming page stream to
// exactly one already-opened SQLite file, at the path this process's
// own configuration named. A hostile stream can corrupt the one
// replica file at worst. The asset landlock could never protect anyway
// is the actual secret, the SSH private key, which sits in memory from
// startup on. What remains is a memory-safety bug in the stream
// parser, which filesystem confinement does not contain either.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/caasmo/go-daemon-runner/run"
	replicaconfig "github.com/caasmo/restinpieces-backup/config/sqlitersync/replica"
	replicaclient "github.com/caasmo/restinpieces-backup/internal/sqlitersync/replica"
	"github.com/caasmo/restinpieces-backup/sqlitersync/replica"
	"github.com/caasmo/restinpieces-backup/ssh"
	"github.com/pelletier/go-toml/v2"
)

// readConfig loads the replica configuration from the TOML file at
// path, straight into the shared document shapes. Paths and WAL mode
// are validated by the library at sync time, not at boot;
// origin_addr, sync_timeout, every entry's path, and — in remote
// mode — every [ssh] field are validated here: the daemon must not
// guess what to pull, where to write it, how long a pull may run, or
// how to reach the sshd.
func readConfig(path string, useLocal bool) (replicaconfig.Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return replicaconfig.Config{}, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg replicaconfig.Config
	err = toml.Unmarshal(content, &cfg)
	if err != nil {
		return replicaconfig.Config{}, fmt.Errorf("failed to parse config file: %w", err)
	}
	if cfg.OriginAddr == "" {
		return replicaconfig.Config{}, fmt.Errorf("origin_addr is required")
	}
	if cfg.SyncTimeout.Duration <= 0 {
		return replicaconfig.Config{}, fmt.Errorf("sync_timeout is required and must be positive")
	}
	if !useLocal {
		if cfg.SSH == nil {
			return replicaconfig.Config{}, fmt.Errorf("remote mode requires an [ssh] section")
		}
		if cfg.SSH.User == "" {
			return replicaconfig.Config{}, fmt.Errorf("ssh.user is required")
		}
		if cfg.SSH.Host == "" {
			return replicaconfig.Config{}, fmt.Errorf("ssh.host is required")
		}
		if cfg.SSH.Port == "" {
			return replicaconfig.Config{}, fmt.Errorf("ssh.port is required")
		}
		if cfg.SSH.PrivateKeyPath == "" {
			return replicaconfig.Config{}, fmt.Errorf("ssh.private_key_path is required")
		}
		if cfg.SSH.HostKeyPath == "" {
			return replicaconfig.Config{}, fmt.Errorf("ssh.host_key_path is required")
		}
	}
	seen := make(map[string]string, len(cfg.Entries))
	active := 0
	for name, entry := range cfg.Entries {
		if name == "" {
			return replicaconfig.Config{}, fmt.Errorf("entries: an entry name is empty")
		}
		if entry.Path == "" {
			return replicaconfig.Config{}, fmt.Errorf("entries.%s.path is required", name)
		}
		cleaned := filepath.Clean(entry.Path)
		if prev, ok := seen[cleaned]; ok {
			return replicaconfig.Config{}, fmt.Errorf("entries %q and %q share path %q", prev, name, entry.Path)
		}
		seen[cleaned] = name
		if entry.Frequency.Duration > 0 {
			active++
		}
	}
	if len(cfg.Entries) > 0 && active == 0 {
		return replicaconfig.Config{}, fmt.Errorf("no active entries: all frequencies are zero/disabled")
	}
	return cfg, nil
}

// preparePaths creates the parent directory of every configured entry
// path once at startup, deduplicated, so the first sync can create
// the replica file wherever the configuration puts it.
func preparePaths(cfg replicaconfig.Config) error {
	dirs := make(map[string]struct{}, len(cfg.Entries))
	for _, entry := range cfg.Entries {
		dirs[filepath.Dir(entry.Path)] = struct{}{}
	}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return fmt.Errorf("failed to create replica directory %s: %w", dir, err)
		}
	}
	return nil
}

// selectClient builds the transport selected by -l/--local. Every
// rule was checked by readConfig; the only failure left here is
// loading the SSH key files from disk.
func selectClient(cfg replicaconfig.Config, useLocal bool) (replicaclient.Client, error) {
	if useLocal {
		return &replicaclient.LocalClient{OriginAddr: cfg.OriginAddr}, nil
	}
	creds, err := ssh.LoadCredentials(*cfg.SSH)
	if err != nil {
		return nil, fmt.Errorf("failed to load SSH credentials: %w", err)
	}
	return &replicaclient.SSHClient{Creds: creds, OriginAddr: cfg.OriginAddr}, nil
}

func main() {
	configPath := flag.String("config", "", "path to the TOML config file")
	useLocal := flag.Bool("l", false, "run the sync on the same machine instead of over SSH")
	flag.BoolVar(useLocal, "local", false, "run the sync on the same machine instead of over SSH")
	flag.Parse()

	// Validate the flags before reading anything: without a config
	// file path there is nothing to load.
	if *configPath == "" {
		slog.Error("-config is required")
		os.Exit(1)
	}

	cfg, err := readConfig(*configPath, *useLocal)
	if err != nil {
		slog.Error("failed to read config", "error", err)
		os.Exit(1)
	}

	err = preparePaths(cfg)
	if err != nil {
		slog.Error("failed to prepare replica directories", "error", err)
		os.Exit(1)
	}

	client, err := selectClient(cfg, *useLocal)
	if err != nil {
		slog.Error("failed to build client", "error", err)
		os.Exit(1)
	}

	replicaDaemon := replica.New(client, cfg, nil)

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
