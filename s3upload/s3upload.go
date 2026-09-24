// Package s3upload provides the S3 upload daemon. It uploads the newest
// backup of a configured backup label to an S3-compatible bucket,
// encrypting it with age when a recipient is configured. The backups are
// produced by the online and vacuum daemons and found by their filenames;
// this daemon never opens a database.
//
// Each backup is uploaded in a single PUT request (the s3 client has no
// multipart upload), so a backup must stay under the 5 GiB S3 limit.
package s3upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/caasmo/go-daemon-runner/daemon"
	"github.com/caasmo/restinpieces-backup/internal/localcopy"
	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/s3"
)

// Daemon uploads backups to S3 on an interval. The first upload runs
// immediately at startup, and after that one upload runs every interval.
// The interval is the smallest frequency among the entries (5m when none
// is active); it is re-read after every upload, so a frequency change on
// reload takes effect before the next wait.
type Daemon struct {
	daemon.Base
	cfgPointer *atomic.Pointer[config.Config]
	s3Client   *s3.S3
}

// New creates the daemon with the configuration it reads. A nil logger
// falls back to slog.Default().
func New(pointer *atomic.Pointer[config.Config], logger *slog.Logger) *Daemon {
	d := &Daemon{
		Base:       daemon.NewBase("S3UploadDaemon", logger),
		cfgPointer: pointer,
	}
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run starts the daemon's goroutine. One upload runs immediately at
// startup (skipped if Stop already fired), then one after each interval.
// Stop cancels the context, which ends the loop; an upload in flight
// aborts when its current request finishes.
func (d *Daemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		for {
			if err := d.Ctx.Err(); err != nil {
				return // stopped before the next upload
			}

			entries := d.activeEntries()

			if d.buildS3Client() {
				err := d.handle(d.Ctx, entries)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						d.Logger.Info("upload aborted by shutdown")
						return
					}
					d.Logger.Error("upload failed", "error", err)
				}
			}

			select {
			case <-d.Ctx.Done():
				return
			case <-time.After(d.interval(entries)):
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

// activeEntry is an ad hoc struct that holds one upload entry active in
// the current tick: the label, the fields the upload needs, and the
// resolved paths of the backup it points at. It exists so activeEntries
// can gather the current active entries once and pass them around as one
// simple value.
type activeEntry struct {
	label        string
	backupLabel  string
	frequency    time.Duration
	ageRecipient string
	sourcePath   string
	backupDir    string
}

// activeEntries returns the upload entries that can run, flattened into a
// list and resolved so the caller needs nothing else. An entry is left out
// when its backup_label is empty (deactivated) or when the online or
// vacuum backup it names is deactivated (no source or destination path).
// It runs once per tick and its list is passed to handle and interval, so
// neither re-checks an entry.
func (d *Daemon) activeEntries() []activeEntry {
	cfg := d.cfgPointer.Load()
	var entries []activeEntry
	for label, entry := range cfg.BackupS3Upload() {
		if entry.BackupLabel == "" {
			continue
		}

		online, inOnline := cfg.BackupOnlineAPI()[entry.BackupLabel]
		vacuum, inVacuum := cfg.BackupVacuum()[entry.BackupLabel]

		var sourcePath, destPath string
		if inOnline {
			sourcePath, destPath = online.SourcePath, online.DestPath
		} else if inVacuum {
			sourcePath, destPath = vacuum.SourcePath, vacuum.DestPath
		}
		if sourcePath == "" || destPath == "" {
			continue
		}

		entries = append(entries, activeEntry{
			label:        label,
			backupLabel:  entry.BackupLabel,
			frequency:    entry.Frequency.Duration,
			ageRecipient: entry.AgeRecipient,
			sourcePath:   sourcePath,
			backupDir:    destPath,
		})
	}
	return entries
}

// buildS3Client validates the s3 section and rebuilds the daemon's client
// for the current tick. It returns false when the endpoint is empty, in
// which case the tick is skipped. The section is read on every tick, so a
// reload of the endpoint or the credentials takes effect before the next
// upload.
func (d *Daemon) buildS3Client() bool {
	s3Config := d.cfgPointer.Load().S3
	if s3Config.Endpoint == "" {
		return false
	}

	d.s3Client = &s3.S3{
		Endpoint:     s3Config.Endpoint,
		Region:       s3Config.Region,
		Bucket:       s3Config.Bucket,
		AccessKey:    s3Config.AccessKey,
		SecretKey:    s3Config.SecretKey,
		UsePathStyle: s3Config.UsePathStyle,
	}
	return true
}

// handle runs one upload over every entry in the list. The daemon's client
// holds the validated S3 for the current tick, so handle never reads the
// configuration itself and never checks the s3 fields. When one upload
// fails, the remaining entries are still tried, and all errors are
// returned together. The entries are already resolved, so they are not
// re-checked.
func (d *Daemon) handle(ctx context.Context, entries []activeEntry) error {
	if len(entries) == 0 {
		d.Logger.Info("No active S3 uploads; uploads deactivated.")
		return nil
	}

	var errs []error
	for _, active := range entries {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		uploadErr := d.uploadOne(ctx, active)
		if uploadErr != nil {
			errs = append(errs, fmt.Errorf("%q: %w", active.label, uploadErr))
		}
	}
	return errors.Join(errs...)
}

// defaultInterval is how often the daemon checks for a new backup when
// no entry is active.
const defaultInterval = 5 * time.Minute

// interval returns how often the daemon checks for a new backup to
// upload: the smallest frequency among the entries, or defaultInterval
// when the list is empty.
func (d *Daemon) interval(entries []activeEntry) time.Duration {
	min := defaultInterval
	for _, active := range entries {
		if active.frequency <= 0 {
			continue // zero means the default
		}
		if active.frequency < min {
			min = active.frequency
		}
	}
	return min
}

// s3ObjectKey returns the object name for a backup: the backup filename at
// the bucket root, with ".age" appended when it is uploaded encrypted.
func s3ObjectKey(backupPath, ageRecipient string) string {
	key := filepath.Base(backupPath)
	if ageRecipient != "" {
		key += ".age"
	}
	return key
}

// uploadOne uploads the newest backup of one upload entry. It returns nil
// when there is nothing to do: no backup exists yet, or the backup is
// already in the bucket.
func (d *Daemon) uploadOne(ctx context.Context, active activeEntry) error {
	backupPath, ok := localcopy.LatestBackupPath(active.backupDir, active.backupLabel, active.sourcePath)
	if !ok {
		d.Logger.Info("Skipping upload; no backup yet", "s3_upload", active.label, "backup_label", active.backupLabel)
		return nil
	}

	key := s3ObjectKey(backupPath, active.ageRecipient)

	uploaded, err := d.isAlreadyUploaded(ctx, key)
	if err != nil {
		return err
	}
	if uploaded {
		d.Logger.Info("Skipping upload; backup already uploaded", "s3_upload", active.label, "key", key)
		return nil
	}

	if active.ageRecipient != "" {
		err = d.uploadEncrypted(ctx, key, backupPath, active.ageRecipient)
	} else {
		err = d.uploadFile(ctx, key, backupPath)
	}
	if err != nil {
		return err
	}

	d.Logger.Info("Uploaded backup", "s3_upload", active.label, "key", key)
	return nil
}

// isAlreadyUploaded reports whether the backup is already in the bucket. A
// 404 response means it is not there; any other error is returned.
func (d *Daemon) isAlreadyUploaded(ctx context.Context, key string) (bool, error) {
	_, err := d.s3Client.HeadObject(ctx, key)
	if err == nil {
		return true, nil
	}
	var responseErr *s3.ResponseError
	if !errors.As(err, &responseErr) {
		return false, err
	}
	if responseErr.Status != http.StatusNotFound {
		return false, err
	}
	return false, nil
}

// uploadFile uploads one backup without encryption. PutObject closes
// the file when the upload ends.
func (d *Daemon) uploadFile(ctx context.Context, key, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open backup: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("failed to stat backup: %w", err), closeErr)
	}

	uploadErr := d.s3Client.PutObject(ctx, key, file, info.Size())
	if uploadErr != nil {
		return fmt.Errorf("failed to upload %q: %w", key, uploadErr)
	}
	return nil
}

// uploadEncrypted uploads one backup encrypted with age. The encrypted
// size is not known in advance, so the request is sent in chunks. age
// encrypts through a writer while the request body needs a reader; the
// standard bridge is an io.Pipe with a goroutine running the producer,
// unbuffered so memory stays flat. Producer errors travel through
// CloseWithError, so a failed read aborts the upload.
func (d *Daemon) uploadEncrypted(ctx context.Context, key, path, recipient string) error {
	recipientID, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("failed to parse age recipient: %w", err)
	}

	pipeReader, pipeWriter := io.Pipe()
	producerErrCh := make(chan error, 1)

	go func() {
		producerErr := encryptFile(path, recipientID, pipeWriter)
		_ = pipeWriter.CloseWithError(producerErr)
		producerErrCh <- producerErr
	}()

	// size -1: the encrypted size is unknown, the body is sent in chunks
	uploadErr := d.s3Client.PutObject(ctx, key, pipeReader, -1)
	// PutObject may return before the body is drained (for example when the
	// request fails); closing the reader unblocks a producer still writing.
	_ = pipeReader.Close()
	producerErr := <-producerErrCh
	return errors.Join(uploadErr, producerErr)
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
