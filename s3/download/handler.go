// Package download provides the S3 download job. Each configured entry
// lists its bucket under an object key prefix and downloads the newest
// backup for the label: the uploader puts the inverted modification time
// in the key, so the bucket returns the newest backup for the label. An exact
// object key selects just that object. The job writes the object into the
// entry's dest_dir as s3download-<label>-<pad>-<name> and skips the
// download when that file already exists. It never decrypts.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caasmo/restinpieces-backup/s3"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	s3client "github.com/caasmo/restinpieces/s3"
)

const (
	// JobTypeS3Download is the job type this handler registers under.
	JobTypeS3Download = "s3_download"

	// DestFileNamePrefix is the constant start of every dest download
	// name: s3download-.
	DestFileNamePrefix = "s3download-"

	// destFileNameFmt renders the dest file name of a downloaded backup:
	// s3download-<label>-<pad>-<name>.
	destFileNameFmt = DestFileNamePrefix + "%s-%s-%s"
)

// Handler downloads one object per configured entry from S3. It is a job
// handler: the scheduler calls Handle on the job's interval.
type Handler struct {
	cfgPointer *atomic.Pointer[config.Config]
	logger     *slog.Logger
	s3Client   *s3client.S3
}

// New creates the handler with the configuration it reads. A nil logger
// falls back to slog.Default().
func New(pointer *atomic.Pointer[config.Config], logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{cfgPointer: pointer, logger: logger}
}

// Handle runs one download pass. The job payload is not used.
func (h *Handler) Handle(ctx context.Context, job db.Job) error {
	cfg := h.cfgPointer.Load()
	return h.handle(ctx, cfg)
}

// handle runs one pass over every entry. It logs when the handler has
// nothing to do and builds the client from the s3 section for this pass.
// One failing entry does not stop the others; all errors are returned
// together.
func (h *Handler) handle(ctx context.Context, cfg *config.Config) error {
	entries := cfg.Backup.S3Download
	if len(entries) == 0 {
		h.logger.Info("No backup.s3-download entries; nothing to do.")
		return nil
	}

	client, err := s3.NewClient(cfg.S3)
	if err != nil {
		h.logger.Info("s3.endpoint is empty; nothing to do.")
		return nil
	}
	h.s3Client = client

	var errs []error
	for _, label := range slices.Sorted(maps.Keys(entries)) {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		entry := entries[label]
		downloadErr := h.downloadOne(ctx, label, entry)
		if downloadErr != nil {
			errs = append(errs, fmt.Errorf("%q: %w", label, downloadErr))
		}
	}
	return errors.Join(errs...)
}

// downloadOne lists the entry's key prefix and downloads the newest backup
// for the label, when its local file does not exist yet. It checks
// min_interval against the newest local download before it calls S3, so a
// skipped entry makes no S3 call.
func (h *Handler) downloadOne(ctx context.Context, label string, entry config.BackupS3DownloadEntry) error {
	if entry.ObjectKeyPrefix == "" {
		return nil // deactivated
	}

	// Checks how long ago this label last downloaded something, using the
	// files already in dest_dir.
	destPrefix := buildDestFilePrefix(label)
	lastTime, ok := lastDestFileDownloadTime(entry.DestDir, destPrefix)
	if ok {
		elapsed := time.Since(lastTime)
		if elapsed < entry.MinInterval.Duration {
			h.logger.Info("Skipping; not due yet", "s3_download", label, "next_download_in", entry.MinInterval.Duration-elapsed)
			return nil
		}
	}

	// The bucket returns the newest backup for the label; an exact object
	// key is its own prefix, so it is the key returned.
	objectKey, found, err := h.ListObjectForPrefix(ctx, entry.Bucket, entry.ObjectKeyPrefix)
	if err != nil {
		return err
	}
	if !found {
		h.logger.Info("Skipping; no object found", "s3_download", label, "object_key_prefix", entry.ObjectKeyPrefix)
		return nil
	}

	destPath, err := buildDestFilePath(entry.DestDir, label, objectKey)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(destPath)
	if statErr == nil && info.Mode().IsRegular() {
		h.logger.Info("Skipping; object already downloaded", "s3_download", label, "path", destPath)
		return nil
	}

	err = h.downloadObject(ctx, entry.Bucket, objectKey, destPath)
	if err != nil {
		return err
	}

	h.logger.Info("Downloaded object", "s3_download", label, "key", objectKey, "path", destPath)
	return nil
}

// ListObjectForPrefix returns the key the bucket lists for prefix. found is
// false when the prefix matches no object.
func (h *Handler) ListObjectForPrefix(ctx context.Context, bucket, prefix string) (string, bool, error) {
	result, err := h.s3Client.ListObjects(ctx, bucket, s3client.ListParams{Prefix: prefix, MaxKeys: 1})
	if err != nil {
		return "", false, fmt.Errorf("failed to list %q: %w", prefix, err)
	}
	if len(result.Contents) == 0 {
		return "", false, nil
	}
	return result.Contents[0].Key, true, nil
}

// lastDestFileDownloadTime returns the modification time of the newest file in dir
// whose name starts with prefix, the entry's last completed download. ok is
// false when no file matches.
func lastDestFileDownloadTime(dir, prefix string) (time.Time, bool) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}

	var latestTime time.Time
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			continue
		}
		if !strings.HasPrefix(dirEntry.Name(), prefix) {
			continue
		}
		info, infoErr := dirEntry.Info()
		if infoErr != nil {
			continue
		}
		if latestTime.IsZero() || info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
		}
	}
	if latestTime.IsZero() {
		return time.Time{}, false
	}
	return latestTime, true
}

// buildDestFilePrefix returns the start of label's dest file names:
// s3download-<label>-.
func buildDestFilePrefix(label string) string {
	return DestFileNamePrefix + label + "-"
}

// buildDestFilePath returns the dest path of the downloaded backup:
// <destDir>/s3download-<label>-<pad>-<name>, where pad and name come from
// the object key through the shared key layout. The s3download- start is
// local to the download; the pad and the name are the uploader's.
//
// Example: destDir "/data/dl", label "app-dl" and key
// "backup/app-s3/251611468335/app.db":
//
//	/data/dl/s3download-app-dl-251611468335-app.db
func buildDestFilePath(destDir, label, objectKey string) (string, error) {
	pad, name, ok := s3.PadAndName(objectKey)
	if !ok {
		return "", fmt.Errorf("object key %q has no pad and name", objectKey)
	}
	return filepath.Join(destDir, fmt.Sprintf(destFileNameFmt, label, pad, name)), nil
}

// downloadObject writes one object from the bucket to destPath. The bytes
// are written to a hidden temp file in the same directory, and a rename
// gives them the final name, so a partial download never looks complete.
func (h *Handler) downloadObject(ctx context.Context, bucket, key, destPath string) (err error) {
	response, err := h.s3Client.GetObject(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("failed to get %q: %w", key, err)
	}
	defer func() {
		err = errors.Join(err, response.Body.Close())
	}()

	tempPath := filepath.Join(filepath.Dir(destPath), ".tmp-"+filepath.Base(destPath))
	file, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	_, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		removeErr := os.Remove(tempPath)
		return errors.Join(fmt.Errorf("failed to copy object %q: %w", key, copyErr), closeErr, removeErr)
	}
	if closeErr != nil {
		removeErr := os.Remove(tempPath)
		return errors.Join(fmt.Errorf("failed to close temp file: %w", closeErr), removeErr)
	}

	renameErr := os.Rename(tempPath, destPath)
	if renameErr != nil {
		removeErr := os.Remove(tempPath)
		return errors.Join(fmt.Errorf("failed to rename temp file: %w", renameErr), removeErr)
	}
	return nil
}
