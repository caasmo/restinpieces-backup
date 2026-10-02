// Package upload provides the S3 upload daemon. Each configured entry
// selects one file: a fixed path, or the newest file matching a path
// prefix. The daemon checks the entry's min_interval against the file's
// modification time and puts the file to an S3-compatible bucket under
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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/restinpieces/config"
	s3client "github.com/caasmo/restinpieces/s3"
)

// tickInterval is how often the daemon checks every entry.
const tickInterval = time.Minute

// backupS3KeyPrefix is the first key segment of every uploaded object:
//
//	backup/<label>/<pad>/<filename>[.age]
//
// For example, label "app-s3", pad 8209066599 and file app.db:
//
//	backup/app-s3/8209066599/app.db
const backupS3KeyPrefix = "backup"

// maxUnixTimestamp is the newest time the inverted pad encodes:
// 9999-12-31T23:59:59Z. The pad is maxUnixTimestamp minus the file's
// modification time, so a bucket listing returns the newest object
// first.
const maxUnixTimestamp = 253402300799

// Daemon uploads one file per configured entry to S3. It ticks every
// minute from startup; an entry is skipped until its min_interval has
// elapsed since the file's modification time.
type Daemon struct {
	daemon.Base
	cfgPointer *atomic.Pointer[config.Config]
	s3Client   *s3client.S3
}

// New creates the daemon with the configuration it reads. A nil logger
// falls back to slog.Default().
func New(pointer *atomic.Pointer[config.Config], logger *slog.Logger) *Daemon {
	d := &Daemon{
		Base:       daemon.NewBase("S3Daemon", logger),
		cfgPointer: pointer,
	}
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run starts the daemon's goroutine. The first pass runs immediately at
// startup (skipped if Stop already fired), then one pass every
// tickInterval. Stop cancels the context, which ends the loop; a request
// in flight aborts when its current request finishes.
func (d *Daemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		for {
			if err := d.Ctx.Err(); err != nil {
				return // stopped before the next pass
			}

			cfg := d.cfgPointer.Load()

			err := d.handle(d.Ctx, cfg)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					d.Logger.Info("upload aborted by shutdown")
					return
				}
				d.Logger.Error("upload pass failed", "error", err)
			}

			select {
			case <-d.Ctx.Done():
				return
			case <-time.After(tickInterval):
			}
		}
	}()
	return nil
}

// Start calls Run so the daemon fits the restinpieces server.Daemon
// interface: the server calls Start() after the HTTP server starts and
// Stop() during shutdown. Register it with srv.AddDaemon while
// restinpieces manages daemons itself.
//
// TODO: remove once restinpieces is on go-daemon-runner; the runner
// calls Run directly.
func (d *Daemon) Start() error {
	return d.Run()
}

// buildS3Client builds the daemon's client from the s3 section. It returns
// an error when the endpoint is empty, in which case the daemon is
// deactivated and the tick is skipped. The section is read on every tick,
// so a reload of the endpoint or the credentials takes effect before the
// next run.
func (d *Daemon) buildS3Client(s3Config config.S3) error {
	if s3Config.Endpoint == "" {
		return errors.New("s3.endpoint is not configured")
	}

	d.s3Client = &s3client.S3{
		Endpoint:             s3Config.Endpoint,
		Region:               s3Config.Region,
		AccessKey:            s3Config.AccessKey,
		SecretKey:            s3Config.SecretKey,
		UsePathStyle:         s3Config.UsePathStyle,
		RequireContentLength: s3Config.RequireContentLength,
	}
	return nil
}

// handle runs one pass over every entry. It logs when the daemon has
// nothing to do and builds the client from the s3 section for this pass.
// One failing entry does not stop the others; all errors are returned
// together.
func (d *Daemon) handle(ctx context.Context, cfg *config.Config) error {
	entries := cfg.Backup.S3Upload
	if len(entries) == 0 {
		d.Logger.Info("No backup.s3-upload entries; nothing to do.")
		return nil
	}

	err := d.buildS3Client(cfg.S3)
	if err != nil {
		d.Logger.Info("s3.endpoint is empty; nothing to do.")
		return nil
	}

	var errs []error
	for _, label := range slices.Sorted(maps.Keys(entries)) {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		entry := entries[label]
		uploadErr := d.uploadOne(ctx, label, entry)
		if uploadErr != nil {
			errs = append(errs, fmt.Errorf("%q: %w", label, uploadErr))
		}
	}
	return errors.Join(errs...)
}

// uploadOne selects the entry's file, checks that the entry's min_interval
// has elapsed since the file's modification time, and puts the file
// under the inverted timestamp key.
func (d *Daemon) uploadOne(ctx context.Context, label string, entry config.BackupS3UploadEntry) error {
	if entry.Path == "" && entry.PathPrefix == "" {
		return nil // deactivated
	}

	filePath, modTime, ok := fileToUpload(entry)
	if !ok {
		d.Logger.Info("Skipping; no file to upload", "s3_upload", label)
		return nil
	}

	elapsed := time.Since(modTime)
	if elapsed < entry.MinInterval.Duration {
		d.Logger.Info("Skipping; not due yet", "s3_upload", label, "next_upload_in", entry.MinInterval.Duration-elapsed)
		return nil
	}

	key := s3ObjectKey(label, filePath, modTime, entry.AgeRecipient)

	exists, err := d.objectExists(ctx, entry.Bucket, key)
	if err != nil {
		return err
	}
	if exists {
		d.Logger.Info("Skipping; backup already in bucket", "s3_upload", label, "key", key)
		return nil
	}

	if entry.AgeRecipient != "" {
		err = d.uploadObjectEncrypted(ctx, entry.Bucket, key, filePath, entry.AgeRecipient)
	} else {
		err = d.uploadObject(ctx, entry.Bucket, key, filePath)
	}
	if err != nil {
		return err
	}

	d.Logger.Info("Stored backup", "s3_upload", label, "key", key)
	return nil
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

// s3ObjectKey returns the bucket key for the upload file: the backup
// prefix, the entry label, the inverted timestamp pad, and the original
// filename. ".age" is appended when the file is encrypted.
// Example:
//
//	backup/app-s3/8209066599/app.db
func s3ObjectKey(label, sourcePath string, modTime time.Time, ageRecipient string) string {
	pad := fmt.Sprintf("%012d", maxUnixTimestamp-modTime.Unix())
	name := filepath.Base(sourcePath)
	if ageRecipient != "" {
		name += ".age"
	}
	return path.Join(backupS3KeyPrefix, label, pad, name)
}

// objectExists reports whether the backup is already in the bucket. A
// 404 response means it is not there; any other error is returned.
func (d *Daemon) objectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := d.s3Client.HeadObject(ctx, bucket, key)
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
func (d *Daemon) uploadObject(ctx context.Context, bucket, key, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open backup: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("failed to stat backup: %w", err), closeErr)
	}

	putErr := d.s3Client.PutObject(ctx, bucket, key, file, info.Size())
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
func (d *Daemon) uploadObjectEncrypted(ctx context.Context, bucket, key, path, recipient string) error {
	recipientID, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("failed to parse age recipient: %w", err)
	}

	// a content length of -1 means the S3 provider accepts an unknown
	// length; the body is then sent in chunks
	contentLength := int64(-1)
	if d.s3Client.RequireContentLength {
		d.Logger.Info("Measuring encrypted content length; the S3 provider requires it", "key", key)
		contentLength, err = encryptedContentLength(path, recipientID)
		if err != nil {
			return fmt.Errorf("failed to measure encrypted backup: %w", err)
		}
		d.Logger.Info("Measured encrypted content length", "key", key, "content_length", contentLength)
	}

	pipeReader, pipeWriter := io.Pipe()
	producerErrCh := make(chan error, 1)

	go func() {
		producerErr := encryptFile(path, recipientID, pipeWriter)
		_ = pipeWriter.CloseWithError(producerErr)
		producerErrCh <- producerErr
	}()

	putErr := d.s3Client.PutObject(ctx, bucket, key, pipeReader, contentLength)
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
