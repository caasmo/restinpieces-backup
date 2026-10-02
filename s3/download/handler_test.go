package download

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/config"
)

// fakeS3 is an in-memory bucket for the tests. It answers the GET and list
// requests the handler sends and records how many of each it served.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    int
	lists   int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: make(map[string][]byte)}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")

	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Query().Get("list-type") == "2" {
		f.serveList(w, r.URL.Query().Get("prefix"))
		return
	}

	data, ok := f.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.gets++
	_, _ = w.Write(data)
}

// serveList answers a ListObjectsV2 request with every key that has the
// prefix, in ascending order. The handler takes the first entry.
func (f *fakeS3) serveList(w http.ResponseWriter, prefix string) {
	f.lists++

	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	_, _ = io.WriteString(w, "<IsTruncated>false</IsTruncated>")
	for _, key := range keys {
		lastModified := time.Now().UTC().Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key><LastModified>%s</LastModified><Size>%d</Size></Contents>", key, lastModified, len(f.objects[key]))
	}
	_, _ = io.WriteString(w, "</ListBucketResult>")
}

func (f *fakeS3) put(key string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
}

func (f *fakeS3) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func (f *fakeS3) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// newTestHandler builds a handler over the config.
func newTestHandler(cfg config.Config) *Handler {
	var pointer atomic.Pointer[config.Config]
	pointer.Store(&cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&pointer, logger)
}

// testConfigFor builds a config with one S3 download entry whose settings
// point at the fake bucket. The s3 section holds the connection only; the
// entry names the bucket.
func testConfigFor(serverURL string, entry config.BackupS3DownloadEntry) config.Config {
	return config.Config{
		Backup: config.Backup{
			S3Download: config.BackupS3Download{"app-dl": entry},
		},
		S3: config.S3{Endpoint: serverURL, Region: "test", AccessKey: "ak", SecretKey: "sk", UsePathStyle: true},
	}
}

func startFakeS3(t *testing.T) (*httptest.Server, *fakeS3) {
	t.Helper()
	bucket := newFakeS3()
	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)
	return server, bucket
}

// writeDownload writes a local file as the handler would have written it.
func writeDownload(t *testing.T, dir, name string, data []byte) string {
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

// backdate sets a file's modification time so the entry's min_interval
// has elapsed.
func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	err := os.Chtimes(path, old, old)
	if err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func TestHandler_DownloadsExactKey(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	bucket.put("backup/app-s3/251611468335/app.db", []byte("data"))
	bucket.put("backup/app-s3/251611468335/app.db.age", []byte("encrypted sibling"))

	cfg := testConfigFor(server.URL, config.BackupS3DownloadEntry{
		Bucket:          "test-bucket",
		ObjectKeyPrefix: "backup/app-s3/251611468335/app.db",
		DestDir:         destDir,
		MinInterval:     config.Duration{Duration: time.Hour},
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	destPath := filepath.Join(destDir, "s3download-app-dl-251611468335-app.db")
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("downloaded = %q, want %q", got, "data")
	}
}

func TestHandler_DownloadsNewestUnderPrefix(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	bucket.put("backup/app-s3/251611468336/app.db", []byte("older"))
	bucket.put("backup/app-s3/251611468335/app.db", []byte("newer"))

	cfg := testConfigFor(server.URL, config.BackupS3DownloadEntry{
		Bucket:          "test-bucket",
		ObjectKeyPrefix: "backup/app-s3/",
		DestDir:         destDir,
		MinInterval:     config.Duration{Duration: time.Hour},
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	destPath := filepath.Join(destDir, "s3download-app-dl-251611468335-app.db")
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "newer" {
		t.Fatalf("downloaded = %q, want %q", got, "newer")
	}
}

func TestHandler_SkipsNotDue(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	writeDownload(t, destDir, "s3download-app-dl-251611468335-app.db", []byte("data"))
	bucket.put("backup/app-s3/251611468335/app.db", []byte("newer"))

	cfg := testConfigFor(server.URL, config.BackupS3DownloadEntry{
		Bucket:          "test-bucket",
		ObjectKeyPrefix: "backup/app-s3/",
		DestDir:         destDir,
		MinInterval:     config.Duration{Duration: time.Hour},
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)
	if got := bucket.getCount(); got != 0 {
		t.Fatalf("get count = %d, want 0", got)
	}
	if got := bucket.listCount(); got != 0 {
		t.Fatalf("list count = %d, want 0", got)
	}
}

func TestHandler_SkipsExistingDownload(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()
	localPath := writeDownload(t, destDir, "s3download-app-dl-251611468335-app.db", []byte("already here"))
	backdate(t, localPath, 2*time.Hour)
	bucket.put("backup/app-s3/251611468335/app.db", []byte("data"))

	cfg := testConfigFor(server.URL, config.BackupS3DownloadEntry{
		Bucket:          "test-bucket",
		ObjectKeyPrefix: "backup/app-s3/251611468335/app.db",
		DestDir:         destDir,
		MinInterval:     config.Duration{Duration: time.Hour},
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	if got := bucket.getCount(); got != 0 {
		t.Fatalf("get count = %d, want 0", got)
	}
	if got := bucket.listCount(); got != 1 {
		t.Fatalf("list count = %d, want 1", got)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "already here" {
		t.Fatalf("file = %q, want %q", got, "already here")
	}
}

func TestHandler_NoObjectYet(t *testing.T) {
	server, bucket := startFakeS3(t)
	destDir := t.TempDir()

	cfg := testConfigFor(server.URL, config.BackupS3DownloadEntry{
		Bucket:          "test-bucket",
		ObjectKeyPrefix: "backup/app-s3/",
		DestDir:         destDir,
		MinInterval:     config.Duration{Duration: time.Hour},
	})
	handler := newTestHandler(cfg)

	handleOnce(t, handler, cfg)

	if got := bucket.getCount(); got != 0 {
		t.Fatalf("get count = %d, want 0", got)
	}
	if got := bucket.listCount(); got != 1 {
		t.Fatalf("list count = %d, want 1", got)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dest dir has %d files, want 0", len(entries))
	}
}
