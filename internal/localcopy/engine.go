// Package localcopy provides the shared snapshot engine used by the
// vacuum and onlineapi daemons. It runs one copy cycle over the
// strategy's configured entries: check due, verify paths, copy the
// database through the strategy, atomically promote the result, and
// refresh the latest hardlink.
//
// The package is strategy-agnostic: all differences between the two
// backup strategies live behind the Strategy interface.
package localcopy

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
	_ "modernc.org/sqlite"
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

// Entry is one configured backup in the common shape shared by every
// strategy. Strategy-specific fields (pages_per_step, sleep_interval)
// are owned by the Strategy implementation, never by Entry.
type Entry struct {
	Label       string
	SourcePath  string
	DestPath    string
	Frequency   time.Duration
	Compression bool
}

// Strategy is one backup strategy: how to enumerate its configured
// entries and how to copy one database. VacuumStrategy and
// OnlineApiStrategy are the two implementations. Entries is called on
// every tick and reads the config box, so a configuration reload is
// visible at the next tick.
type Strategy interface {
	Entries() []Entry
	Copy(ctx context.Context, srcConn *sql.Conn, destPath string, entry Entry) error
}

// Engine runs the shared copy pipeline over a strategy's entries.
// handle is its testable core; daemon.go adds the go-daemon-runner
// lifecycle.
//
// One pool per source file. The pool itself holds no FD; a connection
// from it does. With SetConnMaxIdleTime the pool frees the FD of
// idle connections, so no bookkeeping is needed for stale files.
type Engine struct {
	logger   *slog.Logger
	strategy Strategy
	pools    map[string]*sql.DB
}

// NewEngine creates the engine around the strategy. A nil logger
// falls back to slog.Default(), mirroring the daemon constructors
// (daemon.NewBase, New).
func NewEngine(strategy Strategy, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{strategy: strategy, logger: logger, pools: make(map[string]*sql.DB)}
}

// ClosePools closes all pools. Call on daemon shutdown; each tick
// only returns the connection, the pool lives on.
func (e *Engine) ClosePools() {
	for _, db := range e.pools {
		_ = db.Close()
	}
}

// handle runs one copy over every configured database in turn. A
// failed copy is logged and the next entry is tried; errors are
// returned together.
func (e *Engine) handle(ctx context.Context, now time.Time) error {
	entries := e.strategy.Entries()
	if len(entries) == 0 {
		e.logger.Info("No backup files configured; backup deactivated.")
		return nil
	}

	latest := e.latestBackupFiles(entries)

	var errs []error
	for _, entry := range entries {
		// --- step 0: abort on shutdown ---
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		// --- step 1: skip entries with empty source_path (deactivated) ---
		if entry.SourcePath == "" {
			e.logger.Info("Skipping backup; source_path is empty (entry deactivated)", "db", entry.Label)
			continue
		}

		// --- step 2: skip entries with empty dest_path (deactivated) ---
		if entry.DestPath == "" {
			e.logger.Info("Skipping backup; dest_path is empty (entry deactivated)", "db", entry.Label)
			continue
		}

		backupID := e.buildBackupID(entry.Label, entry.SourcePath)

		// --- step 3: skip if not yet due ---
		if !e.isBackupDue(latest[backupID].time, entry.Frequency, now) {
			e.logger.Info("Skipping backup; not yet due",
				"db", backupID,
				"due_at", latest[backupID].time.Add(entry.Frequency).Format(timestampFormat),
			)
			continue
		}

		// --- step 4: dest_path must be an existing directory ---
		dirInfo, statErr := os.Stat(entry.DestPath)
		if statErr != nil {
			errs = append(errs, fmt.Errorf("%q: dest_path: %w", backupID, statErr))
			continue
		}
		if !dirInfo.IsDir() {
			errs = append(errs, fmt.Errorf("%q: dest_path is not a directory: %s", backupID, entry.DestPath))
			continue
		}

		// --- step 5: source file must exist and be a file ---
		srcInfo, srcErr := os.Stat(entry.SourcePath)
		if srcErr != nil {
			errs = append(errs, fmt.Errorf("%q: source database file not found: %s: %w", backupID, entry.SourcePath, srcErr))
			continue
		}
		if srcInfo.IsDir() {
			errs = append(errs, fmt.Errorf("%q: source path is a directory, not a database file: %s", backupID, entry.SourcePath))
			continue
		}

		// --- step 6: backup copy ---
		copyErr := e.handleFile(ctx, entry, backupID, now)
		if copyErr != nil {
			errs = append(errs, copyErr)
		}
	}
	return errors.Join(errs...)
}

// handleFile runs one backup copy for one entry.
//
// No integrity check is performed here. This is a conscious choice:
// the daemon's job is only to produce a snapshot and atomically
// promote it; the client on the other machine verifies the file
// (even before download) and is the single source of truth for
// validity.
//
// One pool per source file, created lazily via poolConn. The pool
// lives on the engine; the connection holds the FD. With a 10m idle
// deadline the pool frees the FD when idle, so no bookkeeping.
func (e *Engine) handleFile(ctx context.Context, entry Entry, backupID string, now time.Time) error {
	srcConn, openErr := e.poolConn(ctx, entry.SourcePath)
	if openErr != nil {
		return fmt.Errorf("%q: open source db: %w", backupID, openErr)
	}
	defer func() {
		connCloseErr := srcConn.Close()
		if connCloseErr != nil {
			e.logger.Error("Error closing source database connection", "error", connCloseErr)
		}
	}()

	if entry.Compression {
		return e.handleCompressed(ctx, entry, backupID, now, srcConn)
	}
	return e.handleUncompressed(ctx, entry, backupID, now, srcConn)
}

func (e *Engine) handleCompressed(ctx context.Context, entry Entry, backupID string, now time.Time, srcConn *sql.Conn) error {
	f := backupFile{
		backupID:   backupID,
		time:       now,
		compressed: true,
	}
	finalPath := filepath.Join(entry.DestPath, f.String())
	tempPath := e.buildTempPath(backupID, now)
	tempFinalPath := finalPath + ".tmp" // same directory, os.Rename is atomic

	// --- 5a: dump to temp ---
	err := e.strategy.Copy(ctx, srcConn, tempPath, entry)
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

func (e *Engine) handleUncompressed(ctx context.Context, entry Entry, backupID string, now time.Time, srcConn *sql.Conn) error {
	f := backupFile{
		backupID:   backupID,
		time:       now,
		compressed: false,
	}
	finalPath := filepath.Join(entry.DestPath, f.String())
	backupDir := entry.DestPath
	tempFinalPath := finalPath + ".tmp" // same directory, os.Rename is atomic

	// --- 5a: dump to .tmp in backupDir ---
	err := e.strategy.Copy(ctx, srcConn, tempFinalPath, entry)
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
func (e *Engine) latestBackupFiles(entries []Entry) map[string]backupFile {
	destDirs := make(map[string][]string)
	for _, entry := range entries {
		if entry.SourcePath == "" || entry.DestPath == "" {
			continue // deactivated entry, never backed up
		}
		destDirs[entry.DestPath] = append(destDirs[entry.DestPath], e.buildBackupID(entry.Label, entry.SourcePath))
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
// (this is the sqlite convention). For a WAL-mode source, the
// read-only open needs the -shm file readable or the source directory
// writable (wal.html); the daemon runs on the same host as the source,
// so this holds in the live-writer, clean-close, and leftover -wal
// states alike, and any failure is logged and retried next tick.
func sourceDSN(dbPath string) string {
	return "file:" + url.PathEscape(dbPath) + "?mode=ro&_busy_timeout=5000"
}

// poolConn returns a connection from the pool for dbPath.
// The pool is created lazily once per path and kept on the engine.
// The connection holds the FD; the pool itself holds none. With
// SetConnMaxIdleTime the pool frees the FD of idle connections, so
// no bookkeeping is needed for stale files.
func (e *Engine) poolConn(ctx context.Context, dbPath string) (*sql.Conn, error) {
	db := e.pools[dbPath]
	if db == nil {
		var err error
		db, err = sql.Open("sqlite", sourceDSN(dbPath))
		if err != nil {
			return nil, fmt.Errorf("failed to open source database: %w", err)
		}
		db.SetMaxOpenConns(1)
		db.SetConnMaxIdleTime(10 * time.Minute)
		e.pools[dbPath] = db
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source database: %w", err)
	}
	return conn, nil
}

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
