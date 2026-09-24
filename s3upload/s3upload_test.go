package s3upload

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
	"github.com/caasmo/restinpieces/config"
)

// fakeS3 is an in-memory bucket for the tests. It answers the HEAD and
// PUT requests the daemon sends and records the stored objects.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
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

// newTestDaemon builds a daemon over the config.
func newTestDaemon(cfg config.Config) *Daemon {
	var pointer atomic.Pointer[config.Config]
	pointer.Store(&cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&pointer, logger)
}

// prepareClient rebuilds the daemon's S3 client from the test config,
// failing the test when the config has no endpoint.
func prepareClient(t *testing.T, daemon *Daemon) {
	t.Helper()
	if !daemon.buildS3Client() {
		t.Fatal("buildS3Client: no client built")
	}
}

// testConfigFor builds a config with one online entry and one upload entry
// whose settings point at the fake bucket.
func testConfigFor(t *testing.T, serverURL, destDir string, entry config.BackupS3Entry) config.Config {
	t.Helper()
	return config.Config{
		Backup: config.Backup{
			OnlineAPI: config.BackupOnlineAPI{
				"app-online": {SourcePath: "/data/app.db", DestPath: destDir, Frequency: config.Duration{Duration: time.Hour}, PagesPerStep: 100},
			},
			S3: config.BackupS3{"app-s3": entry},
		},
		S3: config.S3{Endpoint: serverURL, Region: "test", Bucket: "test-bucket", AccessKey: "ak", SecretKey: "sk", UsePathStyle: true},
	}
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

func TestDaemon_UploadNewestBackup(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	writeBackup(t, destDir, "app-online-app.db-20250801T103000Z.db", []byte("older"))
	newest := writeBackup(t, destDir, "app-online-app.db-20250802T103000Z.db", []byte("newest"))

	cfg := testConfigFor(t, server.URL, destDir, config.BackupS3Entry{
		BackupLabel: "app-online",
		Frequency:   config.Duration{Duration: time.Hour},
	})
	daemon := newTestDaemon(cfg)

	prepareClient(t, daemon)

	if err := daemon.handle(context.Background(), daemon.activeEntries()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := bucket.object(filepath.Base(newest)); string(got) != "newest" {
		t.Fatalf("object = %q, want %q", got, "newest")
	}
	if got := bucket.object("app-online-app.db-20250801T103000Z.db"); got != nil {
		t.Fatalf("older backup should not be uploaded, got %q", got)
	}
}

func TestDaemon_SkipsExistingObject(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	writeBackup(t, destDir, "app-online-app.db-20250802T103000Z.db", []byte("newest"))
	bucket.put("app-online-app.db-20250802T103000Z.db", []byte("already there"))

	cfg := testConfigFor(t, server.URL, destDir, config.BackupS3Entry{
		BackupLabel: "app-online",
		Frequency:   config.Duration{Duration: time.Hour},
	})
	daemon := newTestDaemon(cfg)

	prepareClient(t, daemon)

	if err := daemon.handle(context.Background(), daemon.activeEntries()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := bucket.putCount(); got != 0 {
		t.Fatalf("put count = %d, want 0", got)
	}
}

func TestDaemon_EncryptsWithRecipient(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	writeBackup(t, destDir, "app-online-app.db-20250802T103000Z.db", []byte("plaintext"))
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}

	cfg := testConfigFor(t, server.URL, destDir, config.BackupS3Entry{
		BackupLabel:  "app-online",
		Frequency:    config.Duration{Duration: time.Hour},
		AgeRecipient: identity.Recipient().String(),
	})
	daemon := newTestDaemon(cfg)

	prepareClient(t, daemon)

	if err := daemon.handle(context.Background(), daemon.activeEntries()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	stored := bucket.object("app-online-app.db-20250802T103000Z.db.age")
	if stored == nil {
		t.Fatal("encrypted object not uploaded")
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

func TestDaemon_NoBackupYet(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()

	cfg := testConfigFor(t, server.URL, destDir, config.BackupS3Entry{
		BackupLabel: "app-online",
		Frequency:   config.Duration{Duration: time.Hour},
	})
	daemon := newTestDaemon(cfg)

	prepareClient(t, daemon)

	if err := daemon.handle(context.Background(), daemon.activeEntries()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := bucket.putCount(); got != 0 {
		t.Fatalf("put count = %d, want 0", got)
	}
}

func TestDaemon_NoEndpoint(t *testing.T) {
	daemon := newTestDaemon(config.Config{})

	if daemon.buildS3Client() {
		t.Fatal("buildS3Client: expected false when s3.endpoint is empty")
	}
}

func TestDaemon_UnknownBackupLabel(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()

	cfg := testConfigFor(t, server.URL, destDir, config.BackupS3Entry{
		BackupLabel: "missing",
		Frequency:   config.Duration{Duration: time.Hour},
	})
	daemon := newTestDaemon(cfg)

	// A backup_label that names no entry is dropped by activeEntries, so
	// the tick has nothing to upload and no error.
	prepareClient(t, daemon)

	if err := daemon.handle(context.Background(), daemon.activeEntries()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := bucket.putCount(); got != 0 {
		t.Fatalf("put count = %d, want 0", got)
	}
}
