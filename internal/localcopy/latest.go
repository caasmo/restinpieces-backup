package localcopy

import (
	"os"
	"path/filepath"
	"strings"
)

// buildBackupID returns the prefix used in backup filenames and hardlinks.
// It is <label>-<basename>, so two source paths with the same basename do not
// collide.
//
// Example: buildBackupID("app_db", "data/app.db") → "app_db-app.db"
func buildBackupID(label, sourcePath string) string {
	return label + "-" + filepath.Base(sourcePath)
}

// scanBackupDir reads dir once and returns the most recent backup file
// for each requested backupID. A backupID with no matching backup is
// absent from the map. The error is the directory read error, so the
// caller decides whether a missing directory matters.
func scanBackupDir(dir string, backupIDs []string) (map[string]backupFile, error) {
	wanted := make(map[string]struct{}, len(backupIDs))
	for _, id := range backupIDs {
		wanted[id] = struct{}{}
	}

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	latest := make(map[string]backupFile)
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			continue
		}
		name := dirEntry.Name()
		// Extension gate: only backup filenames reach the parser; everything
		// else (stale .tmp, logs, etc.) is not a backup.
		if !strings.HasSuffix(name, uncompressedExt) && !strings.HasSuffix(name, compressedExt) {
			continue
		}
		parsed, err := parseBackupFile(name)
		if err != nil {
			continue // link, pre-feature backup, or junk — never a backup
		}
		if _, ok := wanted[parsed.backupID]; !ok {
			continue
		}
		if parsed.time.After(latest[parsed.backupID].time) {
			latest[parsed.backupID] = parsed
		}
	}
	return latest, nil
}

// LatestBackupPath returns the path of the newest backup in destPath for the
// backup identified by label and sourcePath. The second return value is
// false when the directory or a matching backup is missing. The local
// copy daemons find their backups the same way, so they and the S3
// daemon always agree on which backup is the newest.
func LatestBackupPath(destPath, label, sourcePath string) (string, bool) {
	backupID := buildBackupID(label, sourcePath)

	found, err := scanBackupDir(destPath, []string{backupID})
	if err != nil {
		return "", false
	}

	latest, ok := found[backupID]
	if !ok {
		return "", false
	}
	return filepath.Join(destPath, latest.String()), true
}
