package upload

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/caasmo/restinpieces-backup/s3"
	"github.com/caasmo/restinpieces/config"
)

// fakeS3 is an in-memory bucket for the tests. It answers the HEAD and
// PUT requests the daemon sends and records the stored objects.
type fakeS3 struct {
	mu                   sync.Mutex
	objects              map[string][]byte
	puts                 int
	lastContentLength    int64
	lastTransferEncoding []string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: make(map[string][]byte)}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		f.lastContentLength = r.ContentLength
		f.lastTransferEncoding = r.TransferEncoding
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[key] = data
		f.puts++
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) object(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func (f *fakeS3) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// lastPutRequest returns the content length and transfer encoding of the
// most recent PUT request.
func (f *fakeS3) lastPutRequest() (int64, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastContentLength, f.lastTransferEncoding
}

// newTestHandler builds a handler over the config.
func newTestHandler(cfg config.Config) *Handler {
	var pointer atomic.Pointer[config.Config]
	pointer.Store(&cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&pointer, logger)
}

// testConfigFor builds a config with one S3 upload entry whose settings
// point at the fake bucket.
func testConfigFor(serverURL string, entry config.BackupS3UploadEntry) config.Config {
	return config.Config{
		Backup: config.Backup{
			S3Upload: config.BackupS3Upload{"app-s3": entry},
		},
		S3: config.S3{Endpoint: serverURL, Region: "test", AccessKey: "ak", SecretKey: "sk", UsePathStyle: true},
	}
}

// objectKey builds the bucket key the daemon is expected to write for the
// file, through the shared key layout.
func objectKey(label, filePath string, modTime time.Time, ageRecipient string) string {
	name := filepath.Base(filePath)
	if ageRecipient != "" {
		name += ".age"
	}
	return s3.ObjectKey(label, s3.Pad(modTime), name)
}

func startFakeS3(t *testing.T) (*httptest.Server, *fakeS3) {
	t.Helper()
	bucket := newFakeS3()
	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)
	return server, bucket
}

func writeBackup(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// handleOnce runs one handler pass over the config's entries.
func handleOnce(t *testing.T, handler *Handler, cfg config.Config) {
	t.Helper()
	err := handler.handle(context.Background(), &cfg)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
}

// backdate sets a file's modification time, so the newest match under a
// path prefix is unambiguous.
func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	err := os.Chtimes(path, old, old)
	if err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func TestHandler_UploadsFixedPath(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()
	file := writeBackup(t, dir, "app.db", []byte("data"))

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket: "test-bucket",
		Path:   file,
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	key := objectKey("app-s3", file, info.ModTime(), "")
	if got := bucket.object(key); string(got) != "data" {
		t.Fatalf("object = %q, want %q", got, "data")
	}
}

func TestHandler_UploadsLatestUnderPrefix(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()
	older := writeBackup(t, dir, "app.db-20260101T000000Z.db", []byte("older"))
	newest := writeBackup(t, dir, "app.db-20260102T000000Z.db", []byte("newest"))
	backdate(t, older, 3*time.Hour)
	backdate(t, newest, 2*time.Hour)

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket:             "test-bucket",
		PathPrefix:         filepath.Join(dir, "app.db-"),
		PathPrefixSelector: "latest",
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	info, err := os.Stat(newest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	key := objectKey("app-s3", newest, info.ModTime(), "")
	if got := bucket.object(key); string(got) != "newest" {
		t.Fatalf("object = %q, want %q", got, "newest")
	}
}

func TestHandler_SkipsExistingObject(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()
	file := writeBackup(t, dir, "app.db", []byte("newest"))

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	bucket.put(objectKey("app-s3", file, info.ModTime(), ""), []byte("already there"))

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket: "test-bucket",
		Path:   file,
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)
	if got := bucket.putCount(); got != 0 {
		t.Fatalf("put count = %d, want 0", got)
	}
}

func TestHandler_EncryptsWithRecipient(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()
	file := writeBackup(t, dir, "app.db", []byte("plaintext"))
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket:       "test-bucket",
		Path:         file,
		AgeRecipient: identity.Recipient().String(),
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	stored := bucket.object(objectKey("app-s3", file, info.ModTime(), identity.Recipient().String()))
	if stored == nil {
		t.Fatal("encrypted object not put")
	}
	contentLength, transferEncoding := bucket.lastPutRequest()
	if contentLength != -1 {
		t.Errorf("content length = %d, want -1", contentLength)
	}
	if len(transferEncoding) != 1 || transferEncoding[0] != "chunked" {
		t.Errorf("transfer encoding = %v, want [chunked]", transferEncoding)
	}
	reader, err := age.Decrypt(bytes.NewReader(stored), identity)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(plaintext) != "plaintext" {
		t.Fatalf("decrypted = %q, want %q", plaintext, "plaintext")
	}
}

func TestHandler_EncryptsWithRequiredContentLength(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()
	file := writeBackup(t, dir, "app.db", []byte("plaintext"))
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket:       "test-bucket",
		Path:         file,
		AgeRecipient: identity.Recipient().String(),
	})
	cfg.S3.RequireContentLength = true
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	stored := bucket.object(objectKey("app-s3", file, info.ModTime(), identity.Recipient().String()))
	if stored == nil {
		t.Fatal("encrypted object not put")
	}

	contentLength, transferEncoding := bucket.lastPutRequest()
	if contentLength != int64(len(stored)) {
		t.Errorf("content length = %d, want %d", contentLength, len(stored))
	}
	if len(transferEncoding) != 0 {
		t.Errorf("transfer encoding = %v, want none", transferEncoding)
	}

	reader, err := age.Decrypt(bytes.NewReader(stored), identity)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	plaintext, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(plaintext) != "plaintext" {
		t.Fatalf("decrypted = %q, want %q", plaintext, "plaintext")
	}
}

func TestHandler_NoFileYet(t *testing.T) {
	server, bucket := startFakeS3(t)
	dir := t.TempDir()

	cfg := testConfigFor(server.URL, config.BackupS3UploadEntry{
		Bucket:             "test-bucket",
		PathPrefix:         filepath.Join(dir, "app.db-"),
		PathPrefixSelector: "latest",
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)
	if got := bucket.putCount(); got != 0 {
		t.Fatalf("put count = %d, want 0", got)
	}
}
