// Package onlineapi provides the Online Backup API snapshot daemon.
// It backs up the databases configured in the online section of the
// backup configuration using SQLite's online backup API, so a live
// database can be copied while it is being written.
package onlineapi

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/caasmo/restinpieces-backup/internal/localcopy"
	"github.com/caasmo/restinpieces/config"
	"modernc.org/sqlite"
)

// OnlineApiConfig is the box payload contract: any type exposing the
// online entries satisfies it. config.Config and the standalone
// onlineapiCfg both implement it.
type OnlineApiConfig interface {
	BackupOnlineAPI() config.BackupOnlineAPI
}

// OnlineApiStrategy reads the online map from the config box on every
// call and copies databases with the Online Backup API. The tuning
// fields (pages_per_step, sleep_interval) are read per entry from the
// box at copy time — they never enter the shared Entry shape.
type OnlineApiStrategy[T OnlineApiConfig] struct {
	box    *atomic.Pointer[T]
	logger *slog.Logger
}

// Entries returns the configured online entries in the common shape.
func (s *OnlineApiStrategy[T]) Entries() []localcopy.Entry {
	var out []localcopy.Entry
	for key, f := range (*s.box.Load()).BackupOnlineAPI() {
		out = append(out, localcopy.Entry{
			Label:       key,
			SourcePath:  f.SourcePath,
			DestPath:    f.DestPath,
			Frequency:   f.Frequency.Duration,
			Compression: f.Compression,
		})
	}
	return out
}

// Copy performs one online backup copy of the source database using
// the entry's pages_per_step and sleep_interval.
func (s *OnlineApiStrategy[T]) Copy(ctx context.Context, srcConn *sql.Conn, destPath string, entry localcopy.Entry) error {
	f := (*s.box.Load()).BackupOnlineAPI()[entry.Label] // full config, per entry
	pagesPerStep := f.PagesPerStep
	sleepInterval := f.SleepInterval.Duration // 0 is valid: no throttling

	backup, err := newBackup(srcConn, destPath)
	if err != nil {
		return fmt.Errorf("failed to initialize backup: %w", err)
	}
	defer func() {
		finishErr := backup.Finish()
		if finishErr != nil {
			s.logger.Error("error closing backup resource", "error", finishErr)
		}
	}()

	// Initialize the progress logger
	logger, err := newModuloLogger(s.logger, backup)
	if err != nil {
		return fmt.Errorf("failed to initialize progress logger: %w", err)
	}
	if logger == nil { // This happens if the database is empty
		s.logger.Info("Source database is empty. Backup completed immediately.")
		return nil
	}

	s.logger.Info("Starting online backup copy", "pages_per_step", pagesPerStep, "sleep_interval", sleepInterval, "total_pages", logger.totalPages)

	for {
		// --- abort on shutdown before each step ---
		if err := ctx.Err(); err != nil {
			return err
		}

		more, err := backup.Step(int32(pagesPerStep))
		if err != nil {
			return fmt.Errorf("backup step failed: %w", err)
		}

		if !more {
			logger.logFinal(backup)
			s.logger.Info("Online backup copy completed successfully.")
			return nil
		}

		logger.log(backup)

		// The throttle is a select, not a sleep: shutdown cancels the
		// context and aborts the copy at the next step boundary.
		if sleepInterval > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleepInterval):
			}
		}
	}
}

// New creates the onlineapi daemon around the config box. The daemon
// reads the box on every tick, so a configuration reload is visible
// at the next tick. A nil logger falls back to slog.Default().
func New[T OnlineApiConfig](box *atomic.Pointer[T], logger *slog.Logger) *localcopy.Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	return localcopy.New("OnlineApiDaemon", &OnlineApiStrategy[T]{box: box, logger: logger}, logger)
}

// destURI builds the file: URI for a backup destination path. The
// path is percent-escaped (url.PathEscape): a raw '?' or '#' in
// destPath would be misread by SQLite's URI parser as query or
// fragment and silently misdirect the backup. Mirrors sourceDSN on
// the source side.
func destURI(destPath string) string {
	return "file:" + url.PathEscape(destPath)
}

// newBackup creates the online backup of the source connection into
// destPath through the modernc driver's Backup API. The destination is
// opened by the driver as a file: URI (NewBackup opens it via the
// driver's DSN-parsing path), escaped by destURI.
func newBackup(srcConn *sql.Conn, destPath string) (*sqlite.Backup, error) {
	var backup *sqlite.Backup
	err := srcConn.Raw(func(driverConn any) error {
		backupAPI, ok := driverConn.(interface {
			NewBackup(dstUri string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("driver does not support the online backup API")
		}
		var err error
		backup, err = backupAPI.NewBackup(destURI(destPath))
		return err
	})
	if err != nil {
		return nil, err
	}
	return backup, nil
}

// --- Modulo Logger ---

// moduloLogger encapsulates the logic for logging backup progress.
type moduloLogger struct {
	logger          *slog.Logger
	totalPages      int
	logPageInterval int
	nextLogTarget   int
}

// newModuloLogger creates and initializes a progress logger.
func newModuloLogger(logger *slog.Logger, backup *sqlite.Backup) (*moduloLogger, error) {
	if _, err := backup.Step(0); err != nil {
		return nil, fmt.Errorf("backup step(0) failed: %w", err)
	}
	totalPages := backup.PageCount()
	if totalPages == 0 {
		return nil, nil
	}

	const numLogPoints = 10
	logPageInterval := totalPages / numLogPoints
	if logPageInterval == 0 {
		logPageInterval = 1
	}

	return &moduloLogger{
		logger:          logger,
		totalPages:      totalPages,
		logPageInterval: logPageInterval,
		nextLogTarget:   logPageInterval,
	}, nil
}

// log checks if the backup has progressed enough to warrant a log message.
func (m *moduloLogger) log(backup *sqlite.Backup) {
	copiedPages := m.totalPages - backup.Remaining()
	if copiedPages >= m.nextLogTarget {
		m.logProgress(backup)
		m.nextLogTarget += m.logPageInterval
	}
}

// logFinal logs the final progress message.
func (m *moduloLogger) logFinal(backup *sqlite.Backup) {
	m.logProgress(backup)
}

// logProgress is a private helper to format and write the progress log message.
func (m *moduloLogger) logProgress(backup *sqlite.Backup) {
	copiedPages := m.totalPages - backup.Remaining()
	m.logger.Info("Online backup in progress",
		"pages_copied", copiedPages,
		"total_pages", m.totalPages,
	)
}
