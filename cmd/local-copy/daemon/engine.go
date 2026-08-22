package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caasmo/restinpieces/backup"
	"github.com/caasmo/restinpieces/config"
	"modernc.org/sqlite"
)

const (
	// timestampFormat is the UTC timestamp layout used in backup filenames.
	// It is lexicographically sortable (chronological order equals string order).
	// Produces e.g. "20250801T103000Z".
	timestampFormat = "20060102T150405Z"

	// baseFmt is the shared filename template: backupID and UTC timestamp
	// joined with a dash. The extension is appended separately
	// (compressedExt / uncompressedExt).
	baseFmt         = "%s-%s"
	compressedExt   = ".bck.gz"
	uncompressedExt = ".db"

	// compressedFmt is the filename template for gzip-compressed backups,
	// e.g. "app.db-20250801T103000Z.bck.gz".
	compressedFmt = baseFmt + compressedExt

	// uncompressedFmt is the filename template for plain SQLite copies,
	// e.g. "app.db-20250801T103000Z.db".
	uncompressedFmt = baseFmt + uncompressedExt
)

// The regexes are the parse encoding of the same grammar String() renders,
// with the extension written out escaped. TestBackupFileRoundTrip links the
// two encodings and catches drift between them (e.g. a timestampFormat change
// without a regex update).
var (
	compressedRe   = regexp.MustCompile(`^(.+)-(\d{8}T\d{6}Z)\.bck\.gz$`)
	uncompressedRe = regexp.MustCompile(`^(.+)-(\d{8}T\d{6}Z)\.db$`)
)

// backupFile is one backup file: everything needed to render or parse its
// filename. The backup directory is not part of the struct.
type backupFile struct {
	// backupID identifies the configured source file this backup belongs to:
	// the config label (backup.online.<key> or backup.vacuum.<key>) joined with the source
	// file's basename, e.g. label "app" + source_path "data/app.db"
	// → "app-app.db".
	backupID   string
	time       time.Time // UTC timestamp
	compressed bool      // ".bck.gz" vs ".db"
}

// String renders the filename, e.g. "app_db-app.db-20250801T103000Z.bck.gz".
func (f backupFile) String() string {
	format := uncompressedFmt
	if f.compressed {
		format = compressedFmt
	}
	return fmt.Sprintf(format, f.backupID, f.time.UTC().Format(timestampFormat))
}

// errInvalidBackupFile is the sentinel for parseBackupFile: the filename is
// not a valid backup. Always returned wrapped via %w so callers can
// errors.Is(err, errInvalidBackupFile).
var errInvalidBackupFile = errors.New("invalid backup filename")

// parseBackupFile parses a backup filename. The caller has already
// established the name is a backup file (extension gate in latestBackupFiles);
// a name that fails the grammar or has a malformed timestamp returns an error
// wrapping errInvalidBackupFile.
func parseBackupFile(filename string) (backupFile, error) {
	re := uncompressedRe
	compressed := strings.HasSuffix(filename, compressedExt)
	if compressed {
		re = compressedRe
	}
	m := re.FindStringSubmatch(filename)
	if m == nil {
		return backupFile{}, fmt.Errorf("%w: %q", errInvalidBackupFile, filename)
	}
	ts, err := time.Parse(timestampFormat, m[2])
	if err != nil {
		return backupFile{}, fmt.Errorf("%w: %q: invalid timestamp: %v", errInvalidBackupFile, filename, err)
	}
	return backupFile{
		backupID:   m[1],
		time:       ts,
		compressed: compressed,
	}, nil
}

// dumpFunc performs one backup copy of the source database into
// destPath. onlineBackup and vacuumInto are the two implementations.
type dumpFunc func(ctx context.Context, srcConn *sql.Conn, destPath string, entry backupEntry) error

// backupEntry is the engine's view of one configured backup: the
// fields both strategies share, plus the strategy's dump function.
// entries() builds one per configured label.
type backupEntry struct {
	label         string
	sourcePath    string
	destPath      string
	frequency     time.Duration
	compression   bool
	pagesPerStep  int
	sleepInterval time.Duration
	dump          dumpFunc
}

// Engine runs the local copy backup engine. handle is its testable
// core; daemon.go adds the go-daemon-runner lifecycle.
type Engine struct {
	cfg    *config.Backup
	logger *slog.Logger
}

// NewEngine creates the engine around the already validated config
// snapshot. A nil logger falls back to slog.Default(), mirroring the
// daemon constructors (daemon.NewBase, New).
func NewEngine(cfg *config.Backup, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{cfg: cfg, logger: logger}
}

// entries returns the engine's view of every configured backup in
// deterministic order: the online entries by sorted label, then the
// vacuum entries by sorted label.
func (e *Engine) entries() []backupEntry {
	var entries []backupEntry
	for _, key := range slices.Sorted(maps.Keys(e.cfg.Online)) {
		f := e.cfg.Online[key]
		entries = append(entries, backupEntry{
			label:         key,
			sourcePath:    f.SourcePath,
			destPath:      f.DestPath,
			frequency:     f.Frequency.Duration,
			compression:   f.Compression,
			pagesPerStep:  f.PagesPerStep,
			sleepInterval: f.SleepInterval.Duration,
			dump:          e.onlineBackup,
		})
	}
	for _, key := range slices.Sorted(maps.Keys(e.cfg.Vacuum)) {
		f := e.cfg.Vacuum[key]
		entries = append(entries, backupEntry{
			label:       key,
			sourcePath:  f.SourcePath,
			destPath:    f.DestPath,
			frequency:   f.Frequency.Duration,
			compression: f.Compression,
			dump:        e.vacuumInto,
		})
	}
	return entries
}

// handle runs one copy over every configured database in turn,
// online entries first, then vacuum entries. A failed copy is logged
// and the next entry is tried; errors are returned together.
func (e *Engine) handle(ctx context.Context, now time.Time) error {
	if len(e.cfg.Online)+len(e.cfg.Vacuum) == 0 {
		e.logger.Info("No backup files configured; backup deactivated.")
		return nil
	}

	latest := e.latestBackupFiles(e.cfg)

	var errs []error
	for _, entry := range e.entries() {
		// --- step 0: abort on shutdown ---
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		// --- step 1: skip entries with empty source_path (deactivated) ---
		if entry.sourcePath == "" {
			e.logger.Info("Skipping backup; source_path is empty (entry deactivated)", "db", entry.label)
			continue
		}

		// --- step 2: skip entries with empty dest_path (deactivated) ---
		if entry.destPath == "" {
			e.logger.Info("Skipping backup; dest_path is empty (entry deactivated)", "db", entry.label)
			continue
		}

		backupID := e.buildBackupID(entry.label, entry.sourcePath)

		// --- step 3: skip if not yet due ---
		if !e.isBackupDue(latest[backupID].time, entry.frequency, now) {
			e.logger.Info("Skipping backup; not yet due",
				"db", backupID,
				"due_at", latest[backupID].time.Add(entry.frequency).Format(timestampFormat),
			)
			continue
		}

		// --- step 4: dest_path must be an existing directory ---
		dirInfo, statErr := os.Stat(entry.destPath)
		if statErr != nil {
			errs = append(errs, fmt.Errorf("%q: dest_path: %w", backupID, statErr))
			continue
		}
		if !dirInfo.IsDir() {
			errs = append(errs, fmt.Errorf("%q: dest_path is not a directory: %s", backupID, entry.destPath))
			continue
		}

		// --- step 5: source file must exist and be a file ---
		srcInfo, srcErr := os.Stat(entry.sourcePath)
		if srcErr != nil {
			errs = append(errs, fmt.Errorf("%q: source database file not found: %s: %w", backupID, entry.sourcePath, srcErr))
			continue
		}
		if srcInfo.IsDir() {
			errs = append(errs, fmt.Errorf("%q: source path is a directory, not a database file: %s", backupID, entry.sourcePath))
			continue
		}

		// --- step 6: backup copy ---
		copyErr := e.handleDbFile(ctx, entry, backupID, now)
		if copyErr != nil {
			errs = append(errs, copyErr)
		}
	}
	return errors.Join(errs...)
}

// handleDbFile runs one backup copy for one entry. Opens its own
// source pool and connection; the defers close them on every return
// path. The strategy's dump runs via entry.dump.
func (e *Engine) handleDbFile(ctx context.Context, entry backupEntry, backupID string, now time.Time) error {
	backupDir := entry.destPath

	srcDB, srcConn, openErr := openSourceConn(ctx, entry.sourcePath)
	if openErr != nil {
		return fmt.Errorf("%q: open source db: %w", backupID, openErr)
	}
	defer func() {
		connCloseErr := srcConn.Close()
		if connCloseErr != nil {
			e.logger.Error("Error closing source database connection", "error", connCloseErr)
		}
		dbCloseErr := srcDB.Close()
		if dbCloseErr != nil {
			e.logger.Error("Error closing source database", "error", dbCloseErr)
		}
	}()

	f := backupFile{
		backupID:   backupID,
		time:       now,
		compressed: entry.compression,
	}
	finalPath := filepath.Join(backupDir, f.String())

	if entry.compression {
		tempPath := e.buildTempPath(backupID, now)
		tempFinalPath := finalPath + ".tmp" // same directory, os.Rename is atomic

		// --- 5a: dump to temp ---
		err := entry.dump(ctx, srcConn, tempPath, entry)
		if err != nil {
			removeErr := os.Remove(tempPath)
			if removeErr != nil {
				e.logger.Error("Failed to remove temp file after failed backup", "path", tempPath, "error", removeErr)
			}
			return fmt.Errorf("%q: %w", backupID, err)
		}

		// --- 5b: compress temp to .tmp in backupDir ---
		err = e.compressFile(tempPath, tempFinalPath)
		removeErr := os.Remove(tempPath)
		if removeErr != nil {
			e.logger.Error("Failed to remove temp file", "path", tempPath, "error", removeErr)
		}
		if err != nil {
			removeErr = os.Remove(tempFinalPath)
			if removeErr != nil {
				e.logger.Error("Failed to remove partial .tmp file", "path", tempFinalPath, "error", removeErr)
			}
			return fmt.Errorf("%q: %w", backupID, err)
		}

		// --- 5c: atomic promote ---
		err = os.Rename(tempFinalPath, finalPath)
		if err != nil {
			removeErr = os.Remove(tempFinalPath)
			if removeErr != nil {
				e.logger.Error("Failed to remove .tmp file after failed rename", "path", tempFinalPath, "error", removeErr)
			}
			return fmt.Errorf("%q: %w", backupID, err)
		}
		return nil
	}

	tempFinalPath := finalPath + ".tmp" // same directory, os.Rename is atomic

	// --- 5a: dump to .tmp in backupDir ---
	err := entry.dump(ctx, srcConn, tempFinalPath, entry)
	if err != nil {
		removeErr := os.Remove(tempFinalPath)
		if removeErr != nil {
			e.logger.Error("Failed to remove .tmp file after failed backup", "path", tempFinalPath, "error", removeErr)
		}
		return fmt.Errorf("%q: %w", backupID, err)
	}

	// --- 5b: atomic promote ---
	err = os.Rename(tempFinalPath, finalPath)
	if err != nil {
		removeErr := os.Remove(tempFinalPath)
		if removeErr != nil {
			e.logger.Error("Failed to remove .tmp file after failed rename", "path", tempFinalPath, "error", removeErr)
		}
		return fmt.Errorf("%q: %w", backupID, err)
	}

	// --- 5c: update latest link ---
	// Uncompressed only — the rsync pull client needs a stable
	// filename to sync. Compressed .bck.gz is not consumable as-is,
	// so no link is created for it.
	latestPath := e.buildLatestPath(backupDir, backupID)
	return e.linkLatest(finalPath, latestPath)
}

// buildBackupID returns the prefix used in backup filenames and hardlinks.
// Produces <key>-<basename> so same-basename source paths do not collide
// (AGENTS.md: map keys are labels, not identifiers).
//
// Example: buildBackupID("app_db", "data/app.db") → "app_db-app.db"
func (e *Engine) buildBackupID(key, sourcePath string) string {
	return key + "-" + filepath.Base(sourcePath)
}

// buildTempPath returns a unique staging path in os.TempDir for the
// database dump before compression. Produces e.g. "/tmp/backup-app.db-1234567890.db".
func (e *Engine) buildTempPath(dbName string, now time.Time) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("backup-%s-%d.db", dbName, now.UnixNano()))
}

// buildLatestPath constructs the stable hardlink path for a dbName.
// Uses the shared backup.LatestFmt convention so clients can discover it.
func (e *Engine) buildLatestPath(backupDir, dbName string) string {
	return filepath.Join(backupDir, fmt.Sprintf(backup.LatestFmt, dbName))
}

// latestBackupFiles scans each configured destination directory once
// and returns the most recent backup file for each backupID derived
// from the configured source files. Errors are logged internally; an
// empty map is returned when a directory is absent or unreadable (all
// backups in it are treated as due).
func (e *Engine) latestBackupFiles(cfg *config.Backup) map[string]backupFile {
	destDirs := make(map[string][]string)
	for _, key := range slices.Sorted(maps.Keys(cfg.Online)) {
		f := cfg.Online[key]
		if f.SourcePath == "" || f.DestPath == "" {
			continue // deactivated entry, never backed up
		}
		destDirs[f.DestPath] = append(destDirs[f.DestPath], e.buildBackupID(key, f.SourcePath))
	}
	for _, key := range slices.Sorted(maps.Keys(cfg.Vacuum)) {
		f := cfg.Vacuum[key]
		if f.SourcePath == "" || f.DestPath == "" {
			continue // deactivated entry, never backed up
		}
		destDirs[f.DestPath] = append(destDirs[f.DestPath], e.buildBackupID(key, f.SourcePath))
	}
	latest := make(map[string]backupFile)
	for _, dir := range slices.Sorted(maps.Keys(destDirs)) {
		backupIDs := destDirs[dir]
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				e.logger.Warn("Failed to scan backup directory", "error", err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			// Extension gate: only backup filenames reach the parser; everything
			// else (stale .tmp, logs, etc.) is not a backup.
			if !strings.HasSuffix(name, uncompressedExt) && !strings.HasSuffix(name, compressedExt) {
				continue
			}
			parsed, err := parseBackupFile(name)
			if err != nil {
				continue // link, pre-feature backup, or junk — never a backup
			}
			for _, id := range backupIDs {
				if parsed.backupID == id && parsed.time.After(latest[id].time) {
					latest[id] = parsed
					break
				}
			}
		}
	}
	return latest
}

// isBackupDue returns true if frequency has elapsed since latestTime.
// A zero latestTime means no previous backup exists (always due).
func (e *Engine) isBackupDue(latestTime time.Time, frequency time.Duration, now time.Time) bool {
	if latestTime.IsZero() {
		return true
	}
	return now.Sub(latestTime) >= frequency
}

// linkLatest atomically replaces latestPath to point at the backup file.
//
// Uses link(2) + rename(2) per POSIX.1-2024: os.Link fails when the
// target already exists, so the new hardlink is created under a temp
// name first. os.Rename then atomically replaces latestPath — clients
// always observe a valid link, never ENOENT.
//
// The os.Remove handles stale .tmp left by a prior crash between
// link and rename.
func (e *Engine) linkLatest(backupPath, latestPath string) error {
	tmp := latestPath + ".tmp"
	_ = os.Remove(tmp) // crash recovery, ignore "not found"
	if err := os.Link(backupPath, tmp); err != nil {
		return fmt.Errorf("linkLatest: link: %w", err)
	}
	return os.Rename(tmp, latestPath)
}

// sourceDSN builds the SQLite file: URI for the source database. The
// connection is read-only (mode=ro): the engine only reads the source
// (VACUUM INTO never writes it — "VACUUM (but not VACUUM INTO) is a
// write operation" — and the online backup API holds only read locks
// on it), and a read-only open fails with "unable to open database
// file" instead of silently creating an empty database if the source
// vanished between the stat and the open. No journal_mode pragma is
// applied: journal mode is a persistent property of the source file,
// and rewriting it is a side effect a backup tool must not have. The
// path is percent-escaped (url.PathEscape): a raw '?' or '#' in
// dbPath would be misread by SQLite's URI parser as query or fragment
// (this is the sqlitedb convention). For a WAL-mode source, the
// read-only open needs the -shm file readable or the source directory
// writable (wal.html); the daemon runs on the same host as the source,
// so this holds in the live-writer, clean-close, and leftover -wal
// states alike, and any failure is logged and retried next tick.
func sourceDSN(dbPath string) string {
	return "file:" + url.PathEscape(dbPath) + "?mode=ro&_busy_timeout=5000"
}

// openSourceConn opens the source database for backup and returns the
// pooled handle and one checked-out connection. database/sql's
// *sql.DB is itself the connection pool — modernc registers the
// "sqlite" driver, so sql.Open returns the pool, and SetMaxOpenConns(1)
// pins a single connection for the operations that must run on one
// connection (VACUUM INTO and the online backup API). The caller holds
// the checked-out connection for the whole copy and closes both on
// exit.
func openSourceConn(ctx context.Context, dbPath string) (*sql.DB, *sql.Conn, error) {
	db, err := sql.Open("sqlite", sourceDSN(dbPath))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open source database: %w", err)
	}
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		closeErr := db.Close()
		return nil, nil, errors.Join(fmt.Errorf("failed to open source database: %w", err), closeErr)
	}
	return db, conn, nil
}

// vacuumInto creates a clean, defragmented copy of the database.
func (e *Engine) vacuumInto(ctx context.Context, srcConn *sql.Conn, destPath string, entry backupEntry) error {
	destPath = strings.ReplaceAll(destPath, "'", "''")
	_, err := srcConn.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", destPath))
	if err != nil {
		return fmt.Errorf("failed to execute vacuum statement: %w", err)
	}
	return nil
}

// onlineBackup performs a live backup using the SQLite Online Backup
// API. pages_per_step 0 means "use default": validation allows it and
// this is where the 100-page default is applied.
func (e *Engine) onlineBackup(ctx context.Context, srcConn *sql.Conn, destPath string, entry backupEntry) error {
	pagesPerStep := entry.pagesPerStep
	if pagesPerStep == 0 {
		pagesPerStep = config.NewBackupOnlineEntryDefaults().PagesPerStep
	}
	sleepInterval := entry.sleepInterval // 0 is valid: no throttling

	backup, err := newBackup(srcConn, destPath)
	if err != nil {
		return fmt.Errorf("failed to initialize backup: %w", err)
	}
	defer func() {
		finishErr := backup.Finish()
		if finishErr != nil {
			e.logger.Error("error closing backup resource", "error", finishErr)
		}
	}()

	// Initialize the progress logger
	logger, err := newModuloLogger(e.logger, backup)
	if err != nil {
		return fmt.Errorf("failed to initialize progress logger: %w", err)
	}
	if logger == nil { // This happens if the database is empty
		e.logger.Info("Source database is empty. Backup completed immediately.")
		return nil
	}

	e.logger.Info("Starting online backup copy", "pages_per_step", pagesPerStep, "sleep_interval", sleepInterval, "total_pages", logger.totalPages)

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
			e.logger.Info("Online backup copy completed successfully.")
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

// --- Other Helpers ---

// compressFile reads a source file, compresses it with gzip, and writes to a destination file.
func (e *Engine) compressFile(sourcePath, destPath string) error {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to open source file for compression: %w", err)
	}
	defer func() {
		if err := sourceFile.Close(); err != nil {
			e.logger.Error("Error closing source file", "error", err)
		}
	}()

	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file for compression: %w", err)
	}
	defer func() {
		if err := destFile.Close(); err != nil {
			e.logger.Error("Error closing destination file", "error", err)
		}
	}()

	gzipWriter := gzip.NewWriter(destFile)
	defer func() {
		if err := gzipWriter.Close(); err != nil {
			e.logger.Error("Error closing gzip writer", "error", err)
		}
	}()

	if _, err := io.Copy(gzipWriter, sourceFile); err != nil {
		return fmt.Errorf("failed to copy and compress data: %w", err)
	}

	return nil
}
