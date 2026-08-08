// Package verification provides the post-transfer backup quality
// checks: at least one latest-* file present in the destination
// directory, none empty, and every file passing the SQLite integrity
// check.
package verification

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	ripbackup "github.com/caasmo/restinpieces/backup"
	"zombiezen.com/go/sqlite"
)

// VerifyBackup sanity-checks the received files: at least one latest-*
// file must be present in the destination directory, none may be empty,
// and every file must pass the SQLite integrity check. The zero-match
// failure itself is detected earlier (the Go glob in LocalClient.Run,
// or the SSH sender failing on the literal pattern, with the transfer
// stats size check as final guard); this glob check is the sanity
// check that the files actually landed in the destination directory.
// Verification is deliberately non-cancellable: it is a local read-only
// scan of files already on disk, and process-level interruption
// (SIGINT) applies.
func VerifyBackup(destDir string) error {
	localLatestGlob := filepath.Join(destDir, ripbackup.LatestGlob)
	latestFiles, err := filepath.Glob(localLatestGlob)
	if err != nil {
		return fmt.Errorf("failed to glob destination directory: %w", err)
	}
	if len(latestFiles) == 0 {
		return fmt.Errorf("no backup files found in destination directory: %s", localLatestGlob)
	}

	var emptyErrs []error
	for _, localPath := range latestFiles {
		fi, statErr := os.Stat(localPath)
		if statErr != nil {
			return fmt.Errorf("failed to stat local file post rsync: %w", statErr)
		}

		if fi.Size() == 0 {
			removeErr := os.Remove(localPath)
			emptyErr := fmt.Errorf("local backup file is empty post rsync: %s", localPath)
			emptyErrs = append(emptyErrs, errors.Join(emptyErr, removeErr))
			continue
		}

		err = VerifySqliteIntegrity(localPath)
		if err != nil {
			return fmt.Errorf("backup verification failed post rsync: %w", err)
		}
		slog.Info("Backup verification successful. Local SQLite database is valid.", "path", localPath)
	}

	if len(emptyErrs) > 0 {
		return errors.Join(emptyErrs...)
	}

	return nil
}

// VerifySqliteIntegrity verifies a single database file with
// PRAGMA integrity_check. Opening a WAL-mode database read-only
// initializes the WAL infrastructure and leaves two inert artifacts
// next to the database (-shm and -wal); both are ignored by the
// latest-*.db glob and never affect the result. integrity_check
// returns a single "ok" row when the database is healthy, or one row
// per problem otherwise — the first row decides pass/fail, so the
// result set need not be exhausted.
func VerifySqliteIntegrity(dbPath string) (err error) {
	// Opening a WAL-mode database read-only initializes the WAL
	// infrastructure and leaves two artifacts next to the database in
	// the destination directory: the -shm wal-index (32768 bytes) and
	// the -wal file. Both are always inert here: the connection is
	// read-only and integrity_check never writes, so the -wal stays at
	// 0 bytes and the wal-index describes an empty WAL. On later runs
	// the same artifact names are reused — the stale index is validated
	// against the empty -wal on open and discarded, and every page is
	// read from the .db file alone, so the verification result is never
	// affected. The artifacts hold no backup data and are ignored by
	// the latest-*.db glob.
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		return fmt.Errorf("failed to open database for verification: %w", err)
	}
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	stmt, err := conn.Prepare("PRAGMA integrity_check;")
	if err != nil {
		return fmt.Errorf("failed to prepare integrity_check statement: %w", err)
	}
	defer func() {
		finalizeErr := stmt.Finalize()
		err = errors.Join(err, finalizeErr)
	}()

	// integrity_check returns a single "ok" row when the database is
	// healthy, or one row per problem otherwise — the first row decides
	// pass/fail, so the result set need not be exhausted.
	row, err := stmt.Step()
	if err != nil {
		return fmt.Errorf("failed to execute integrity_check: %w", err)
	}
	if !row {
		return fmt.Errorf("integrity_check returned no rows")
	}

	result := stmt.ColumnText(0)
	if result != "ok" {
		return fmt.Errorf("integrity_check failed, result was: %s", result)
	}

	return nil
}
