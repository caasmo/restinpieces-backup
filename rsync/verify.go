package rsync

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/caasmo/restinpieces-backup-client/sqlitedb"
	ripbackup "github.com/caasmo/restinpieces/backup"
)

// VerifyBackup sanity-checks the received files: at least one latest-*
// file must be present in the destination directory, none may be
// empty, and every file must pass the SQLite integrity check. The
// zero-match failure itself is detected earlier (the Go glob in
// LocalClient.Run, or the SSH sender failing on the literal pattern,
// with the transfer stats size check as final guard); this glob check
// is the sanity check that the files actually landed in the
// destination directory. Verification is deliberately
// non-cancellable: it is a local read-only scan of files already on
// disk, and process-level interruption (SIGINT) applies.
func VerifyBackup(destDir string) error {
	localLatestGlob := filepath.Join(destDir, ripbackup.LatestGlob)
	latestFiles, err := filepath.Glob(localLatestGlob)
	if err != nil {
		return fmt.Errorf("failed to glob destination directory: %w", err)
	}
	if len(latestFiles) == 0 {
		return fmt.Errorf("no backup files found in destination directory: %s", localLatestGlob)
	}

	// Every failure is collected and reported together: verification is
	// a local read-only scan, so one pass reports every problem instead
	// of failing on the first one.
	var verifyErrs []error
	for _, localPath := range latestFiles {
		fi, statErr := os.Stat(localPath)
		if statErr != nil {
			verifyErrs = append(verifyErrs, fmt.Errorf("failed to stat local file post rsync: %w", statErr))
			continue
		}

		if fi.Size() == 0 {
			removeErr := os.Remove(localPath)
			verifyErrs = append(verifyErrs, errors.Join(fmt.Errorf("local backup file is empty post rsync: %s", localPath), removeErr))
			continue
		}

		d, openErr := sqlitedb.New(localPath)
		if openErr != nil {
			verifyErrs = append(verifyErrs, fmt.Errorf("backup verification failed post rsync: %w", openErr))
			continue
		}
		integrityErr := d.Integrity()
		closeErr := d.Close()
		if integrityErr != nil {
			verifyErrs = append(verifyErrs, fmt.Errorf("backup verification failed post rsync: %w", errors.Join(integrityErr, closeErr)))
			continue
		}
		if closeErr != nil {
			verifyErrs = append(verifyErrs, fmt.Errorf("backup verification failed post rsync: %w", closeErr))
			continue
		}
		slog.Info("Backup verification successful. Local SQLite database is valid.", "path", localPath)
	}

	if len(verifyErrs) > 0 {
		return errors.Join(verifyErrs...)
	}

	return nil
}
