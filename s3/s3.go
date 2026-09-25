// Package s3 provides the S3 daemon. It copies the newest backup of a
// configured backup label to an S3-compatible bucket, encrypting it with
// age when a recipient is configured. The backups are produced by the
// online and vacuum daemons and found by their filenames; this daemon
// never opens a database.
//
// Each backup is sent in a single PUT request (the s3 client has no
// multipart support), so a backup must stay under the 5 GiB S3 limit.
package s3

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

// Daemon puts backups into S3 on an interval. The first one runs
// immediately at startup, and after that one runs every interval.
// The interval is the smallest frequency among the entries (5m when none
// is active); it is re-read after every run, so a frequency change on
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
		Base:       daemon.NewBase("S3Daemon", logger),
		cfgPointer: pointer,
	}
	d.Logger = d.Logger.With("daemon_name", d.Name())
	return d
}

// Run starts the daemon's goroutine. One run happens immediately at
// startup (skipped if Stop already fired), then one after each interval.
// Stop cancels the context, which ends the loop; a request in flight
// aborts when its current request finishes.
func (d *Daemon) Run() error {
	go func() {
		defer close(d.ShutdownDone)

		for {
			if err := d.Ctx.Err(); err != nil {
				return // stopped before the next run
			}

			cfg := d.cfgPointer.Load()
			entries := d.activeEntries(cfg)

			err := d.handle(d.Ctx, cfg.S3, entries)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					d.Logger.Info("put aborted by shutdown")
					return
				}
				d.Logger.Error("put failed", "error", err)
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

// activeEntry is an ad hoc struct that holds one entry active in
// the current tick: the label, the fields it needs, and the
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

// activeEntries returns the entries that can run, flattened into a
// list and resolved so the caller needs nothing else. An entry is left out
// when its backup_label is empty (deactivated) or when the online or
// vacuum backup it names is deactivated (no source or destination path).
// It runs once per tick on the config snapshot and its list is passed to
// handle and interval, so neither re-checks an entry.
func (d *Daemon) activeEntries(cfg *config.Config) []activeEntry {
	var entries []activeEntry
	for label, entry := range cfg.BackupS3() {
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

// buildS3Client builds the daemon's client from the s3 section. It returns
// an error when the endpoint is empty, in which case the daemon is
// deactivated and the tick is skipped. The section is read on every tick,
// so a reload of the endpoint or the credentials takes effect before the
// next run.
func (d *Daemon) buildS3Client(s3Config config.S3) error {
	if s3Config.Endpoint == "" {
		return errors.New("s3.endpoint is not configured")
	}

	d.s3Client = &s3.S3{
		Endpoint:     s3Config.Endpoint,
		Region:       s3Config.Region,
		Bucket:       s3Config.Bucket,
		AccessKey:    s3Config.AccessKey,
		SecretKey:    s3Config.SecretKey,
		UsePathStyle: s3Config.UsePathStyle,
	}
	return nil
}

// handle runs one pass over every entry in the list. It logs when the
// daemon is deactivated — no entries, or no S3 endpoint configured — and
// builds the client from the s3 section for the current tick. When one
// entry fails, the remaining entries are still tried, and all errors are
// returned together. The entries are already resolved, so they are not
// re-checked.
func (d *Daemon) handle(ctx context.Context, s3Config config.S3, entries []activeEntry) error {
	if len(entries) == 0 {
		d.Logger.Info("No active S3 entries; S3 deactivated.")
		return nil
	}

	err := d.buildS3Client(s3Config)
	if err != nil {
		d.Logger.Info("S3 deactivated; s3.endpoint is not configured.")
		return nil
	}

	var errs []error
	for _, active := range entries {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}

		putErr := d.putOne(ctx, active)
		if putErr != nil {
			errs = append(errs, fmt.Errorf("%q: %w", active.label, putErr))
		}
	}
	return errors.Join(errs...)
}

// defaultInterval is how often the daemon checks for a new backup when
// no entry is active.
const defaultInterval = 5 * time.Minute

// interval returns how often the daemon checks for a new backup:
// the smallest frequency among the entries, or defaultInterval
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
// the bucket root, with ".age" appended when the backup is encrypted.
func s3ObjectKey(backupPath, ageRecipient string) string {
	key := filepath.Base(backupPath)
	if ageRecipient != "" {
		key += ".age"
	}
	return key
}

// putOne puts the newest backup of one entry. It returns nil
// when there is nothing to do: no backup exists yet, or the backup is
// already in the bucket.
func (d *Daemon) putOne(ctx context.Context, active activeEntry) error {
	backupPath, ok := localcopy.LatestBackupPath(active.backupDir, active.backupLabel, active.sourcePath)
	if !ok {
		d.Logger.Info("Skipping; no backup yet", "s3", active.label, "backup_label", active.backupLabel)
		return nil
	}

	key := s3ObjectKey(backupPath, active.ageRecipient)

	stored, err := d.isAlreadyStored(ctx, key)
	if err != nil {
		return err
	}
	if stored {
		d.Logger.Info("Skipping; backup already in bucket", "s3", active.label, "key", key)
		return nil
	}

	if active.ageRecipient != "" {
		err = d.putEncrypted(ctx, key, backupPath, active.ageRecipient)
	} else {
		err = d.putFile(ctx, key, backupPath)
	}
	if err != nil {
		return err
	}

	d.Logger.Info("Stored backup", "s3", active.label, "key", key)
	return nil
}

// isAlreadyStored reports whether the backup is already in the bucket. A
// 404 response means it is not there; any other error is returned.
func (d *Daemon) isAlreadyStored(ctx context.Context, key string) (bool, error) {
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

// putFile puts one backup without encryption. PutObject closes
// the file when the request ends.
func (d *Daemon) putFile(ctx context.Context, key, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open backup: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("failed to stat backup: %w", err), closeErr)
	}

	putErr := d.s3Client.PutObject(ctx, key, file, info.Size())
	if putErr != nil {
		return fmt.Errorf("failed to put %q: %w", key, putErr)
	}
	return nil
}

// putEncrypted puts one backup encrypted with age. The encrypted
// size is not known in advance, so the request is sent in chunks. age
// encrypts through a writer while the request body needs a reader; the
// standard bridge is an io.Pipe with a goroutine running the producer,
// unbuffered so memory stays flat. Producer errors travel through
// CloseWithError, so a failed read aborts the request.
func (d *Daemon) putEncrypted(ctx context.Context, key, path, recipient string) error {
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
	putErr := d.s3Client.PutObject(ctx, key, pipeReader, -1)
	// PutObject may return before the body is drained (for example when the
	// request fails); closing the reader unblocks a producer still writing.
	_ = pipeReader.Close()
	producerErr := <-producerErrCh
	return errors.Join(putErr, producerErr)
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
