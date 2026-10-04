// Package upload provides the S3 upload job. Each configured entry
// selects one file: a fixed path, or the newest file matching a path
// prefix. The job puts the file to an S3-compatible bucket under
// the object key backup/<label>/<pad>/<filename>; a file whose object
// already exists is not uploaded again. When a recipient is configured
// the file is encrypted with age while it is uploaded.
//
// The pad is the file's modification time counted down from year 9999
// and zero-padded, so a bucket listing returns the newest object first.
// The zero-padded width keeps the pad in its own key segment.
//
// Each file is sent in a single PUT request (the s3 client has no
// multipart support), so a file must stay under the 5 GiB S3 limit.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/caasmo/restinpieces-backup/s3"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	s3client "github.com/caasmo/restinpieces/s3"
)

// JobTypeS3Upload is the job type this handler registers under.
const JobTypeS3Upload = "s3_upload"

// Handler uploads one file per configured entry to S3. It is a job
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

// Handle runs one upload pass. The job payload is not used.
func (h *Handler) Handle(ctx context.Context, job db.Job) error {
	cfg := h.cfgPointer.Load()
	return h.handle(ctx, cfg)
}

// uploadOne selects the entry's file and puts it under the inverted
// timestamp key, unless the object is already in the bucket.
func (h *Handler) uploadOne(ctx context.Context, label string, entry config.BackupS3UploadEntry) error {
	if entry.Path == "" && entry.PathPrefix == "" {
		return nil // deactivated
	}

	filePath, modTime, ok := fileToUpload(entry)
	if !ok {
		h.logger.Info("Skipping; no file to upload", "s3_upload", label)
		return nil
	}

	pad := s3.Pad(modTime)
	name := filepath.Base(filePath)
	if entry.AgeRecipient != "" {
		name += ".age"
	}
	key := s3.ObjectKey(label, pad, name)

	exists, err := h.objectExists(ctx, entry.Bucket, key)
	if err != nil {
		return err
	}
	if exists {
		h.logger.Info("Skipping; backup already in bucket", "s3_upload", label, "key", key)
		return nil
	}

	if entry.AgeRecipient != "" {
		err = h.uploadObjectEncrypted(ctx, entry.Bucket, key, filePath, entry.AgeRecipient)
	} else {
		err = h.uploadObject(ctx, entry.Bucket, key, filePath)
	}
	if err != nil {
		return err
	}

	h.logger.Info("Stored backup", "s3_upload", label, "key", key)
	return nil
}

// handle runs one pass over every entry. It logs when the daemon has
// nothing to do and builds the client from the s3 section for this pass.
// One failing entry does not stop the others; all errors are returned
// together.
func (h *Handler) handle(ctx context.Context, cfg *config.Config) error {
	entries := cfg.Backup.S3Upload
	if len(entries) == 0 {
		h.logger.Info("No backup.s3-upload entries; nothing to do.")
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
		uploadErr := h.uploadOne(ctx, label, entry)
		if uploadErr != nil {
			errs = append(errs, fmt.Errorf("%q: %w", label, uploadErr))
		}
	}
	return errors.Join(errs...)
}

// fileToUpload returns the file to upload and its modification time: the
// fixed Path, or the file with the greatest modification time among the
// names in the prefix's directory that start with its base name. ok is
// false when the file does not exist or no name matches.
func fileToUpload(entry config.BackupS3UploadEntry) (path string, modTime time.Time, ok bool) {
	// --- fixed path ---
	if entry.Path != "" {
		info, err := os.Stat(entry.Path)
		if err != nil {
			return "", time.Time{}, false
		}
		return entry.Path, info.ModTime(), true
	}

	// --- prefix ---
	dir := filepath.Dir(entry.PathPrefix)
	base := filepath.Base(entry.PathPrefix)

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	var latestPath string
	var latestTime time.Time
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			continue
		}
		if !strings.HasPrefix(dirEntry.Name(), base) {
			continue
		}
		info, infoErr := dirEntry.Info()
		if infoErr != nil {
			continue
		}
		if latestPath == "" || info.ModTime().After(latestTime) {
			latestPath = filepath.Join(dir, dirEntry.Name())
			latestTime = info.ModTime()
		}
	}

	if latestPath == "" {
		return "", time.Time{}, false
	}
	return latestPath, latestTime, true
}

// objectExists reports whether the backup is already in the bucket. A
// 404 response means it is not there; any other error is returned.
func (h *Handler) objectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := h.s3Client.HeadObject(ctx, bucket, key)
	if err == nil {
		return true, nil
	}
	var responseErr *s3client.ResponseError
	if !errors.As(err, &responseErr) {
		return false, err
	}
	if responseErr.Status != http.StatusNotFound {
		return false, err
	}
	return false, nil
}

// uploadObject puts one backup without encryption. PutObject closes
// the file when the request ends.
func (h *Handler) uploadObject(ctx context.Context, bucket, key, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open backup: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("failed to stat backup: %w", err), closeErr)
	}

	putErr := h.s3Client.PutObject(ctx, bucket, key, file, info.Size())
	if putErr != nil {
		return fmt.Errorf("failed to put %q: %w", key, putErr)
	}
	return nil
}

// uploadObjectEncrypted puts one backup encrypted with age. When the S3 provider
// requires a content length, the encrypted content length is not known in
// advance, so the backup is encrypted once into io.Discard to learn the
// content length and then encrypted again while the request is sent.
// Otherwise the encrypted body is sent in chunks, with no length. age
// encrypts through a writer while the request body needs a reader; the
// standard bridge is an io.Pipe with a goroutine running the producer,
// unbuffered so memory stays flat. Producer errors travel through
// CloseWithError, so a failed read aborts the request.
func (h *Handler) uploadObjectEncrypted(ctx context.Context, bucket, key, path, recipient string) error {
	recipientID, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("failed to parse age recipient: %w", err)
	}

	// a content length of -1 means the S3 provider accepts an unknown
	// length; the body is then sent in chunks
	contentLength := int64(-1)
	if h.s3Client.RequireContentLength {
		h.logger.Info("Measuring encrypted content length; the S3 provider requires it", "key", key)
		contentLength, err = encryptedContentLength(path, recipientID)
		if err != nil {
			return fmt.Errorf("failed to measure encrypted backup: %w", err)
		}
		h.logger.Info("Measured encrypted content length", "key", key, "content_length", contentLength)
	}

	pipeReader, pipeWriter := io.Pipe()
	producerErrCh := make(chan error, 1)

	go func() {
		producerErr := encryptFile(path, recipientID, pipeWriter)
		_ = pipeWriter.CloseWithError(producerErr)
		producerErrCh <- producerErr
	}()

	putErr := h.s3Client.PutObject(ctx, bucket, key, pipeReader, contentLength)
	// PutObject may return before the body is drained (for example when the
	// request fails); closing the reader unblocks a producer still writing.
	_ = pipeReader.Close()
	producerErr := <-producerErrCh
	return errors.Join(putErr, producerErr)
}

// encryptedContentLength encrypts path once into io.Discard to learn the
// content length of the ciphertext without keeping the bytes. The caller
// sends the returned count as the content length of the encrypted upload.
func encryptedContentLength(path string, recipient age.Recipient) (int64, error) {
	discard := &discardWriter{}
	err := encryptFile(path, recipient, discard)
	if err != nil {
		return 0, err
	}
	return discard.written, nil
}

// discardWriter writes every byte to io.Discard and records the total. A
// first encryption pass into it learns the content length without keeping
// the ciphertext.
type discardWriter struct {
	written int64
}

func (w *discardWriter) Write(p []byte) (int, error) {
	n, _ := io.Discard.Write(p)
	w.written += int64(n)
	return n, nil
}

// encryptFile copies path into w through age encryption. It flushes the
// final encrypted chunk and the authentication tag before returning.
func encryptFile(path string, recipient age.Recipient, w io.Writer) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open backup: %w", err)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()

	encryptWriter, err := age.Encrypt(w, recipient)
	if err != nil {
		return fmt.Errorf("failed to create age writer: %w", err)
	}

	_, copyErr := io.Copy(encryptWriter, file)
	closeErr := encryptWriter.Close()
	if copyErr != nil {
		return errors.Join(fmt.Errorf("failed to encrypt backup: %w", copyErr), closeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close age writer: %w", closeErr)
	}
	return nil
}
