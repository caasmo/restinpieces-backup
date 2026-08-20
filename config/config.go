// Package config loads the backup client configuration from the
// environment. It is the single source of truth for the RIP_BCK_*
// environment contract shared by cmd/rsync/oneshot and
// cmd/rsync/daemon.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds the configuration shared by the backup client commands,
// loaded from the environment by New.
type Config struct {
	// SourceDir is the source directory whose latest-*.db files are
	// pulled.
	SourceDir string
	// DestDir is the local destination directory the files are written
	// to.
	DestDir string
	// SSH holds the SSH connection configuration.
	SSH SSHConfig
	// RsyncDaemon holds the rsync-daemon-specific settings.
	RsyncDaemon RsyncDaemonConfig
}

// SSHConfig holds the parameters needed to establish an SSH connection.
type SSHConfig struct {
	// User is the SSH login name.
	User string
	// Host is the server's hostname or IP address.
	Host string
	// Port is the server's SSH port; defaults to "22".
	Port string
	// PrivateKeyPath is the path of the client's private key file.
	PrivateKeyPath string
	// HostKeyPath is the path of the server's public host key file.
	HostKeyPath string
}

// RsyncDaemonConfig holds the rsync-daemon-specific settings.
type RsyncDaemonConfig struct {
	// Interval is the time between backups; the daemon requires a
	// positive value.
	Interval time.Duration
}

// New reads the configuration from environment variables.
// RIP_BCK_SSH_PORT defaults to "22". RIP_BCK_SOURCE_DIR and
// RIP_BCK_DEST_DIR are required. The SSH section and the interval are
// optional here — local mode and the one-shot command do not use them;
// the SSH-mode commands validate the SSH section with ValidateSSH, and
// a set RIP_BCK_INTERVAL must be positive (the daemon requires it).
func New() (Config, error) {
	cfg := Config{
		SourceDir: os.Getenv("RIP_BCK_SOURCE_DIR"),
		DestDir:   os.Getenv("RIP_BCK_DEST_DIR"),
		SSH: SSHConfig{
			User:           os.Getenv("RIP_BCK_SSH_USER"),
			Host:           os.Getenv("RIP_BCK_SSH_HOST"),
			Port:           os.Getenv("RIP_BCK_SSH_PORT"),
			PrivateKeyPath: os.Getenv("RIP_BCK_SSH_PRIVATE_KEY_PATH"),
			HostKeyPath:    os.Getenv("RIP_BCK_SSH_HOST_KEY_PATH"),
		},
	}
	if cfg.SSH.Port == "" {
		cfg.SSH.Port = "22"
	}

	intervalStr := os.Getenv("RIP_BCK_INTERVAL")
	if intervalStr != "" {
		interval, err := time.ParseDuration(intervalStr)
		if err != nil {
			return Config{}, fmt.Errorf("failed to parse RIP_BCK_INTERVAL: %w", err)
		}
		// time.NewTicker panics on a non-positive duration: reject zero
		// and negative intervals here so the daemon fails fast at
		// startup instead of crashing the goroutine on its first tick.
		if interval <= 0 {
			return Config{}, fmt.Errorf("RIP_BCK_INTERVAL must be positive")
		}
		cfg.RsyncDaemon.Interval = interval
	}

	if cfg.SourceDir == "" {
		return Config{}, fmt.Errorf("RIP_BCK_SOURCE_DIR is required")
	}
	if cfg.DestDir == "" {
		return Config{}, fmt.Errorf("RIP_BCK_DEST_DIR is required")
	}

	return cfg, nil
}

// ValidateSSH reports whether the SSH section can configure a
// connection. The SSH-mode commands call it before loading the keys;
// local mode never does.
func (c Config) ValidateSSH() error {
	if c.SSH.User == "" {
		return fmt.Errorf("RIP_BCK_SSH_USER is required")
	}
	if c.SSH.Host == "" {
		return fmt.Errorf("RIP_BCK_SSH_HOST is required")
	}
	if c.SSH.PrivateKeyPath == "" {
		return fmt.Errorf("RIP_BCK_SSH_PRIVATE_KEY_PATH is required")
	}
	if c.SSH.HostKeyPath == "" {
		return fmt.Errorf("RIP_BCK_SSH_HOST_KEY_PATH is required")
	}
	return nil
}

// ValidateRsyncDaemon reports whether the daemon-specific settings are
// usable: the daemon requires the interval. config.New rejects a set
// non-positive interval, and this method rejects a missing one, so
// time.NewTicker never sees a zero interval. cmd/rsync/daemon calls it
// right after New; the other commands never do.
func (c Config) ValidateRsyncDaemon() error {
	if c.RsyncDaemon.Interval == 0 {
		return fmt.Errorf("RIP_BCK_INTERVAL is required")
	}
	return nil
}
