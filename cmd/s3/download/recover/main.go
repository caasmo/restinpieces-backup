// Command recover restores the databases backed up to S3 onto a machine
// that has no data yet. It reads the application configuration from a
// TOML file, downloads the newest object of every
// [backup.s3-upload] entry through the s3 download handler, decrypts the
// downloads that arrived encrypted with age, and moves the first
// downloaded database that ripc can read into data/app.db.
//
// The command takes no flags: the deployment runs it from the project
// home, where the config file, the age key, and the application database
// already sit. The ripc binary is expected next to this command.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"filippo.io/age"
	"github.com/caasmo/restinpieces-backup/s3"
	"github.com/caasmo/restinpieces-backup/s3/download"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	"github.com/pelletier/go-toml/v2"
)

// The deployment owns the canonical layout, and the systemd service
// unit enforces it: ExecStart starts the app with -dbpath data/app.db
// -agekey age.key. ripdep defines the same names as AGE_KEY_FILENAME,
// APP_DB_FILENAME, APP_DATA_DIRNAME, and APP_BIN_DIRNAME. Keep the
// values in sync with the deployment.
const (
	// configFileName is the application configuration file the
	// deployment copies into the project home.
	configFileName = "config.toml"

	// ageKeyFileName is the age identity file the service unit passes to
	// the app, ripdep's AGE_KEY_FILENAME.
	ageKeyFileName = "age.key"

	// ripcPath is the CLI binary the recovery runs, in the deployment's
	// bin directory, ripdep's APP_BIN_DIRNAME.
	ripcPath = "bin/ripc"

	// dataDir is the application data directory the service unit runs
	// against, ripdep's APP_DATA_DIRNAME. The downloads land here.
	dataDir = "data"

	// appDBPath is the application database the service unit passes to
	// the app, ripdep's APP_DATA_DIRNAME joined with APP_DB_FILENAME.
	appDBPath = dataDir + "/app.db"

	// downloadNamePattern is the start of every file the download
	// handler writes: s3download-<label>-<pad>-<name>.
	downloadNamePattern = download.DestFileNamePrefix + "*"
)

// loadConfig reads the application configuration from path on top of the
// framework defaults. It does not run the full config validation: the
// document describes the old machine, so sections the recovery does not
// need (backup sources, certificate paths) may not exist yet.
func loadConfig(path string) (*config.Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := config.NewDefaultConfig()
	err = toml.Unmarshal(content, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return cfg, nil
}

// uploads returns the [backup.s3-upload] entries that uploaded
// something: an entry with both path and path_prefix empty is
// deactivated.
func uploads(cfg *config.Config) config.BackupS3Upload {
	uploads := make(config.BackupS3Upload)
	for label, entry := range cfg.Backup.S3Upload {
		if entry.Path == "" && entry.PathPrefix == "" {
			continue
		}
		uploads[label] = entry
	}
	return uploads
}

// validateConfig checks the two sections the recovery reads. It fails
// before any network call when the endpoint is missing, no upload entry
// is configured, or an entry lacks its bucket.
func validateConfig(cfg *config.Config) error {
	if cfg.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is empty")
	}

	entries := uploads(cfg)
	if len(entries) == 0 {
		return fmt.Errorf("no [backup.s3-upload] entries")
	}

	for _, label := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[label]
		if entry.Bucket == "" {
			return fmt.Errorf("s3-upload.%s.bucket is required", label)
		}
	}
	return nil
}

// destDir returns the directory the upload came from: the directory of
// path, or of path_prefix when path is empty.
func destDir(entry config.BackupS3UploadEntry) string {
	source := entry.Path
	if source == "" {
		source = entry.PathPrefix
	}

	return filepath.Dir(source)
}

// createDestDirs creates the directories the uploads came from, so the
// downloads have somewhere to land, and returns them. A fresh machine
// has none.
func createDestDirs(uploads config.BackupS3Upload) ([]string, error) {
	seen := make(map[string]struct{})
	var dirs []string

	for _, entry := range uploads {
		dir := destDir(entry)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}

		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			return nil, fmt.Errorf("failed to create %q: %w", dir, err)
		}
		dirs = append(dirs, dir)
	}

	slices.Sort(dirs)
	return dirs, nil
}

// objectKeyPrefix returns the bucket prefix that holds the objects
// uploaded for label: "backup/<label>/", where "backup" is the current
// value of s3.KeyPrefix, the leading segment every uploaded object
// shares. The uploader writes each object as
// backup/<label>/<pad>/<filename>, where the pad counts time down, so a
// listing of this prefix returns the newest object first.
//
// For example, label "app-s3" gives "backup/app-s3/", the prefix of
// "backup/app-s3/251611468335/app.db".
func objectKeyPrefix(label string) string {
	return path.Join(s3.KeyPrefix, label) + "/"
}

// downloadFromS3 runs one download pass through the s3 download handler
// over the upload entries: each upload label becomes a download entry
// whose key prefix holds the objects the uploader wrote for it, so the
// handler fetches the newest object of every label back into the
// directory it came from.
func downloadFromS3(cfg *config.Config, uploads config.BackupS3Upload) error {
	downloads := make(config.BackupS3Download)
	for label, entry := range uploads {
		slog.Info("Downloading backup", "label", label, "bucket", entry.Bucket, "object_key_prefix", objectKeyPrefix(label))
		downloads[label] = config.BackupS3DownloadEntry{
			Bucket:          entry.Bucket,
			ObjectKeyPrefix: objectKeyPrefix(label),
			DestDir:         destDir(entry),
			MinInterval:     config.Duration{}, // zero: never skip
		}
	}
	cfg.Backup.S3Download = downloads

	var pointer atomic.Pointer[config.Config]
	pointer.Store(cfg)

	handler := download.New(&pointer, nil)
	return handler.Handle(context.Background(), db.Job{})
}

// loadAgeKey reads the age key file and returns the identities the
// decrypt step uses. The raw key material is zeroed after parsing.
func loadAgeKey(path string) ([]age.Identity, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read age key file %q: %w", path, err)
	}

	identities, err := age.ParseIdentities(bytes.NewReader(content))
	for i := range content {
		content[i] = 0
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse age identities from %q: %w", path, err)
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("no age identities found in %q", path)
	}
	return identities, nil
}

// findAgeEncryptedFiles lists the age-encrypted downloads in dirs — the
// files matching s3download-*.age — sorted.
func findAgeEncryptedFiles(dirs []string) ([]string, error) {
	var ageFilePaths []string

	for _, dir := range dirs {
		paths, globErr := filepath.Glob(filepath.Join(dir, downloadNamePattern+".age"))
		if globErr != nil {
			return nil, fmt.Errorf("failed to list %q: %w", dir, globErr)
		}
		ageFilePaths = append(ageFilePaths, paths...)
	}

	slices.Sort(ageFilePaths)
	slog.Info("Found encrypted downloads", "files", ageFilePaths)
	return ageFilePaths, nil
}

// findCompressedFiles lists the gzip-compressed downloads in dirs — the
// files matching s3download-*.bck.gz — sorted.
func findCompressedFiles(dirs []string) ([]string, error) {
	var gzPaths []string

	for _, dir := range dirs {
		paths, globErr := filepath.Glob(filepath.Join(dir, downloadNamePattern+".bck.gz"))
		if globErr != nil {
			return nil, fmt.Errorf("failed to list %q: %w", dir, globErr)
		}
		gzPaths = append(gzPaths, paths...)
	}

	slices.Sort(gzPaths)
	slog.Info("Found compressed downloads", "files", gzPaths)
	return gzPaths, nil
}

// decrypt decrypts the given age-encrypted files in place next to their
// encrypted originals. The encrypted originals are kept, and one
// failing file does not stop the others.
func decrypt(ageFilePaths []string, identities []age.Identity) error {
	var plainPaths []string
	var errs []error
	for _, ageFilePath := range ageFilePaths {
		plainPath := strings.TrimSuffix(ageFilePath, ".age")

		decryptErr := decryptOne(ageFilePath, plainPath, identities)
		if decryptErr != nil {
			errs = append(errs, decryptErr)
			continue
		}
		plainPaths = append(plainPaths, plainPath)
	}
	slog.Info("Decrypted downloads", "files", plainPaths)
	return errors.Join(errs...)
}

// decryptOne decrypts one age file to plainPath, the file next to the
// encrypted original. A failed copy leaves nothing behind.
func decryptOne(agePath, plainPath string, identities []age.Identity) (err error) {
	encrypted, openErr := os.Open(agePath)
	if openErr != nil {
		return fmt.Errorf("failed to open %q: %w", agePath, openErr)
	}
	defer func() {
		err = errors.Join(err, encrypted.Close())
	}()

	reader, decryptErr := age.Decrypt(encrypted, identities...)
	if decryptErr != nil {
		return fmt.Errorf("failed to decrypt %q: %w", agePath, decryptErr)
	}

	file, createErr := os.Create(plainPath)
	if createErr != nil {
		return fmt.Errorf("failed to create %q: %w", plainPath, createErr)
	}

	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		removeErr := os.Remove(plainPath)
		return errors.Join(fmt.Errorf("failed to decrypt %q", agePath), copyErr, closeErr, removeErr)
	}
	return nil
}

// decompress gunzips the given .bck.gz files in place next to their
// compressed originals, under the plain .db name the compression
// replaced. The compressed originals are kept, and one failing file does
// not stop the others.
func decompress(gzPaths []string) error {
	var plainPaths []string
	var errs []error
	for _, gzPath := range gzPaths {
		plainPath := strings.TrimSuffix(gzPath, ".bck.gz") + ".db"

		decompressErr := decompressOne(gzPath, plainPath)
		if decompressErr != nil {
			errs = append(errs, decompressErr)
			continue
		}
		plainPaths = append(plainPaths, plainPath)
	}
	slog.Info("Decompressed downloads", "files", plainPaths)
	return errors.Join(errs...)
}

// decompressOne gunzips one .bck.gz file to plainPath, the file next to
// the compressed original. A failed copy leaves nothing behind.
func decompressOne(gzPath, plainPath string) (err error) {
	source, openErr := os.Open(gzPath)
	if openErr != nil {
		return fmt.Errorf("failed to open %q: %w", gzPath, openErr)
	}
	defer func() {
		err = errors.Join(err, source.Close())
	}()

	reader, gzipErr := gzip.NewReader(source)
	if gzipErr != nil {
		return fmt.Errorf("failed to gunzip %q: %w", gzPath, gzipErr)
	}
	defer func() {
		err = errors.Join(err, reader.Close())
	}()

	file, createErr := os.Create(plainPath)
	if createErr != nil {
		return fmt.Errorf("failed to create %q: %w", plainPath, createErr)
	}

	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		removeErr := os.Remove(plainPath)
		return errors.Join(fmt.Errorf("failed to decompress %q", gzPath), copyErr, closeErr, removeErr)
	}
	return nil
}

// findDbPaths returns the downloaded database for each upload, keyed by
// label: the file matching s3download-<label>-*.db in the dir the upload
// came from. Empty string when a label has none. The handler downloads
// one file per label.
func findDbPaths(uploads config.BackupS3Upload) (map[string]string, error) {
	dbs := make(map[string]string)

	for label, entry := range uploads {
		dir := destDir(entry)
		pattern := download.DestFileNamePrefix + label + "-*.db"

		paths, globErr := filepath.Glob(filepath.Join(dir, pattern))
		if globErr != nil {
			return nil, fmt.Errorf("failed to list %q: %w", dir, globErr)
		}
		if len(paths) > 0 {
			dbs[label] = paths[0]
		} else {
			dbs[label] = ""
		}
	}

	slog.Info("Found database files", "dbs", dbs)
	return dbs, nil
}

// isAppDB reports whether ripc reads path as an application database:
// it runs "ripc -agekey <key> -dbpath <path> paths" and reports a zero
// exit.
func isAppDB(path string) bool {
	cmd := exec.Command(ripcPath, "-agekey", ageKeyFileName, "-dbpath", path, "paths")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	err := cmd.Run()
	return err == nil
}

// findAppDB returns the db path ripc reads as an application database.
// It fails when none reads.
func findAppDB(dbs map[string]string) (string, error) {
	for _, dbPath := range dbs {
		if dbPath == "" {
			continue
		}
		if isAppDB(dbPath) {
			slog.Info("Found app database", "db", dbPath)
			return dbPath, nil
		}
		slog.Info("Skipping; ripc cannot read the database", "db", dbPath)
	}
	return "", fmt.Errorf("no readable database among %v", dbs)
}

// moveAppDB moves a recovered database into place as the application
// database. It refuses when the target already reads as one.
func moveAppDB(source, target string) error {
	if isAppDB(target) {
		return fmt.Errorf("target %q already exists", target)
	}

	err := os.Rename(source, target)
	if err != nil {
		return fmt.Errorf("failed to move %q to %q: %w", source, target, err)
	}
	slog.Info("Moved app database", "source", source, "target", target)
	return nil
}

// printSummary logs one line per upload with the db file found for it,
// plus the s3download file picked as the app database.
func printSummary(dbs map[string]string, appDB string) {
	for label, db := range dbs {
		slog.Info("recovered", "label", label, "db", db)
	}
	slog.Info("recovered app database", "db", appDB)
}

func main() {
	cfg, err := loadConfig(configFileName)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	slog.Info("Loaded config", "path", configFileName)

	uploads := uploads(cfg)

	err = validateConfig(cfg)
	if err != nil {
		slog.Error("invalid config", "error", err)
		os.Exit(1)
	}
	slog.Info("Config validated", "endpoint", cfg.S3.Endpoint, "uploads", len(uploads))

	identities, err := loadAgeKey(ageKeyFileName)
	if err != nil {
		slog.Error("failed to load the age key", "error", err)
		os.Exit(1)
	}
	slog.Info("Loaded age key", "path", ageKeyFileName, "identities", len(identities))

	dirs, err := createDestDirs(uploads)
	if err != nil {
		slog.Error("failed to create the dest directories", "error", err)
		os.Exit(1)
	}
	slog.Info("Created destination directories", "dirs", dirs)

	downloadErr := downloadFromS3(cfg, uploads)
	if downloadErr != nil {
		slog.Error("some entries failed to download", "error", downloadErr)
	}

	ageFilePaths, err := findAgeEncryptedFiles(dirs)
	if err != nil {
		slog.Error("failed to list encrypted downloads", "error", err)
		os.Exit(1)
	}

	err = decrypt(ageFilePaths, identities)
	if err != nil {
		slog.Error("some downloads failed to decrypt", "error", err)
	}

	compressedPaths, err := findCompressedFiles(dirs)
	if err != nil {
		slog.Error("failed to list compressed downloads", "error", err)
		os.Exit(1)
	}

	err = decompress(compressedPaths)
	if err != nil {
		slog.Error("some downloads failed to decompress", "error", err)
	}

	dbs, err := findDbPaths(uploads)
	if err != nil {
		slog.Error("failed to list downloaded databases", "error", err)
		os.Exit(1)
	}

	if isAppDB(appDBPath) {
		slog.Info("App database already exists", "path", appDBPath)
		return
	}

	appDB, err := findAppDB(dbs)
	if err != nil {
		slog.Error("no downloaded database is readable", "error", err)
		os.Exit(1)
	}

	err = moveAppDB(appDB, appDBPath)
	if err != nil {
		slog.Error("failed to move the app database into place", "error", err)
		os.Exit(1)
	}

	printSummary(dbs, appDB)
}
