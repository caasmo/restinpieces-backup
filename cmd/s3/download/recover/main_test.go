package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/caasmo/restinpieces-backup/s3/download"
	"github.com/caasmo/restinpieces/config"
)

// writeTOML writes content to a file in a temporary directory and
// returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// writeEncrypted writes content age-encrypted to path.
func writeEncrypted(t *testing.T, path string, recipient age.Recipient, content []byte) {
	t.Helper()

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	writer, err := age.Encrypt(file, recipient)
	if err != nil {
		t.Fatal(err)
	}

	_, err = writer.Write(content)
	if err != nil {
		t.Fatal(err)
	}

	err = writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = file.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig_ReadsS3AndUploads(t *testing.T) {
	path := writeTOML(t, `
[s3]
endpoint = "https://s3.example.com"

[backup.s3-upload.app-s3]
bucket = "my-backups"
path = "data/backups/app.db-20260101T000000Z.db"
age_recipient = "age1example"
min_interval = "1h"
`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.S3.Endpoint != "https://s3.example.com" {
		t.Fatalf("endpoint = %q, want %q", cfg.S3.Endpoint, "https://s3.example.com")
	}
	entry := cfg.Backup.S3Upload["app-s3"]
	if entry.Bucket != "my-backups" || entry.Path != "data/backups/app.db-20260101T000000Z.db" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestUploads_SkipsDeactivated(t *testing.T) {
	cfg := &config.Config{
		Backup: config.Backup{
			S3Upload: config.BackupS3Upload{
				"app-s3": {Path: "data/backups/app.db"},
				"off":    {},
			},
		},
	}

	got := uploads(cfg)
	if len(got) != 1 {
		t.Fatalf("uploads = %d, want 1", len(got))
	}
	if _, ok := got["app-s3"]; !ok {
		t.Fatalf("app-s3 missing from %v", got)
	}
}

func TestValidateConfig_Valid(t *testing.T) {
	cfg := &config.Config{
		S3: config.S3{Endpoint: "https://s3.example.com"},
		Backup: config.Backup{
			S3Upload: config.BackupS3Upload{
				"app-s3": {Path: "data/backups/app.db", Bucket: "my-backups"},
			},
		},
	}

	err := validateConfig(cfg)
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
}

func TestValidateConfig_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{
			name:    "empty endpoint",
			cfg:     &config.Config{},
			wantErr: "s3.endpoint is empty",
		},
		{
			name:    "no uploads",
			cfg:     &config.Config{S3: config.S3{Endpoint: "https://s3.example.com"}},
			wantErr: "no [backup.s3-upload] entries",
		},
		{
			name: "empty bucket",
			cfg: &config.Config{
				S3: config.S3{Endpoint: "https://s3.example.com"},
				Backup: config.Backup{
					S3Upload: config.BackupS3Upload{"app-s3": {Path: "data/backups/app.db"}},
				},
			},
			wantErr: "bucket is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateConfig = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestDestDir_PathAndPrefix(t *testing.T) {
	got := destDir(config.BackupS3UploadEntry{Path: filepath.Join("data", "app.db")})
	if got != "data" {
		t.Fatalf("destDir path = %q, want %q", got, "data")
	}

	got = destDir(config.BackupS3UploadEntry{PathPrefix: filepath.Join("data", "logs", "log-")})
	if got != filepath.Join("data", "logs") {
		t.Fatalf("destDir prefix = %q, want %q", got, filepath.Join("data", "logs"))
	}
}

func TestCreateDestDirs_CreatesMissing(t *testing.T) {
	base := t.TempDir()
	uploads := config.BackupS3Upload{
		"app-s3":     {Path: filepath.Join(base, "data", "app.db")},
		"app-s3-dup": {Path: filepath.Join(base, "data", "other.db")},
		"log-s3":     {PathPrefix: filepath.Join(base, "data", "logs", "log-")},
	}

	dirs, err := createDestDirs(uploads)
	if err != nil {
		t.Fatalf("createDestDirs: %v", err)
	}

	want := []string{filepath.Join(base, "data"), filepath.Join(base, "data", "logs")}
	if !slices.Equal(dirs, want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}

	for _, dir := range want {
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("dir %q = %v, %v", dir, info, statErr)
		}
	}
}

func TestObjectKeyPrefix_Label(t *testing.T) {
	got := objectKeyPrefix("app-s3")
	want := "backup/app-s3/"
	if got != want {
		t.Fatalf("objectKeyPrefix = %q, want %q", got, want)
	}
}

func TestFindAgeEncryptedFiles_ListsOnlyAge(t *testing.T) {
	base := t.TempDir()
	dir1 := filepath.Join(base, "data")
	dir2 := filepath.Join(base, "data", "logs")
	for _, dir := range []string{dir1, dir2} {
		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}

	names := map[string]string{
		filepath.Join(dir1, "s3download-app-s3-1-app.db.age"): "x",
		filepath.Join(dir2, "s3download-log-s3-1-log.db.age"): "x",
		filepath.Join(dir1, "s3download-app-s3-1-app.db"):     "x",
		filepath.Join(dir1, "notes.txt"):                      "x",
	}
	for name, content := range names {
		err := os.WriteFile(name, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := findAgeEncryptedFiles([]string{dir1, dir2})
	if err != nil {
		t.Fatalf("findAgeEncryptedFiles: %v", err)
	}

	want := []string{
		filepath.Join(dir2, "s3download-log-s3-1-log.db.age"),
		filepath.Join(dir1, "s3download-app-s3-1-app.db.age"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("age files = %v, want %v", got, want)
	}
}

func TestDecrypt_DecryptsAgeFiles(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	agePath1 := filepath.Join(dir, "s3download-app-s3-1-app.db.age")
	agePath2 := filepath.Join(dir, "s3download-app-s3-2-app.db.age")
	writeEncrypted(t, agePath1, identity.Recipient(), []byte("first"))
	writeEncrypted(t, agePath2, identity.Recipient(), []byte("second"))

	plainPaths, err := decrypt([]string{agePath1, agePath2}, []age.Identity{identity})
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	want := []string{
		filepath.Join(dir, "s3download-app-s3-1-app.db"),
		filepath.Join(dir, "s3download-app-s3-2-app.db"),
	}
	if !slices.Equal(plainPaths, want) {
		t.Fatalf("decrypted = %v, want %v", plainPaths, want)
	}

	for i, plainPath := range want {
		content, readErr := os.ReadFile(plainPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		wantContent := []string{"first", "second"}[i]
		if string(content) != wantContent {
			t.Fatalf("plain %q = %q, want %q", plainPath, content, wantContent)
		}
	}

	for _, agePath := range []string{agePath1, agePath2} {
		_, statErr := os.Stat(agePath)
		if statErr != nil {
			t.Fatalf("encrypted original removed %q: %v", agePath, statErr)
		}
	}
}

func TestFindDbPaths_ByLabel(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	logsDir := filepath.Join(base, "data", "logs")
	for _, dir := range []string{dataDir, logsDir} {
		err := os.MkdirAll(dir, 0o755)
		if err != nil {
			t.Fatal(err)
		}
	}

	uploads := config.BackupS3Upload{
		"app-s3":  {Path: filepath.Join(dataDir, "app.db"), Bucket: "b"},
		"log-s3":  {PathPrefix: filepath.Join(logsDir, "log-"), Bucket: "b"},
		"missing": {Path: filepath.Join(base, "empty", "app.db"), Bucket: "b"},
	}
	err := os.MkdirAll(filepath.Join(base, "empty"), 0o755)
	if err != nil {
		t.Fatal(err)
	}

	appFile := filepath.Join(dataDir, download.DestFileNamePrefix+"app-s3-1-app.db")
	logFile := filepath.Join(logsDir, download.DestFileNamePrefix+"log-s3-1-log.db")
	decoys := []string{
		appFile,
		logFile,
		filepath.Join(dataDir, download.DestFileNamePrefix+"log-s3-1-log.db"),
		filepath.Join(dataDir, download.DestFileNamePrefix+"app-s3-1-app.db.age"),
		filepath.Join(dataDir, "notes.txt"),
	}
	for _, name := range decoys {
		err := os.WriteFile(name, []byte("x"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := findDbPaths(uploads)
	if err != nil {
		t.Fatalf("findDbPaths: %v", err)
	}

	if got["app-s3"] != appFile {
		t.Fatalf("app-s3 = %q, want %q", got["app-s3"], appFile)
	}
	if got["log-s3"] != logFile {
		t.Fatalf("log-s3 = %q, want %q", got["log-s3"], logFile)
	}
	if got["missing"] != "" {
		t.Fatalf("missing = %q, want empty", got["missing"])
	}
}

func TestFindAppDB_NoneReadable(t *testing.T) {
	_, err := findAppDB(map[string]string{"bad": filepath.Join(t.TempDir(), "bad.db")})
	if err == nil {
		t.Fatal("findAppDB: want an error")
	}
}

func TestMoveAppDB_MovesFile(t *testing.T) {
	source := filepath.Join(t.TempDir(), "s3download-app-s3-1-app.db")
	err := os.WriteFile(source, []byte("database bytes"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "app.db")
	err = moveAppDB(source, target)
	if err != nil {
		t.Fatalf("moveAppDB: %v", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "database bytes" {
		t.Fatalf("target content = %q, want %q", content, "database bytes")
	}

	_, statErr := os.Stat(source)
	if !os.IsNotExist(statErr) {
		t.Fatalf("source still present: %v", statErr)
	}
}
