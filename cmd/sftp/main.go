
package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"zombiezen.com/go/sqlite"
)

// Config holds the configuration for the pullfile client.
type Config struct {
	SSHUser           string
	SSHHost           string
	SSHPort           string
	SSHPrivateKeyPath string
	RemoteBackupDir   string
	LocalBackupDir    string
}

func main() {
	// Basic configuration. Replace with your actual data.
	cfg := Config{
		SSHUser:           "user",
		SSHHost:           "machine-b.example.com",
		SSHPort:           "22",
		SSHPrivateKeyPath: "/home/user/.ssh/id_rsa",
		RemoteBackupDir:   "/var/caasmo/backups",
		LocalBackupDir:    "/home/lipo/backups",
	}

	ctx := context.Background()
	slog.Info("Starting pullfile client")

	sftpClient, err := setupSftpClient(cfg)
	if err != nil {
		slog.Error("Failed to set up SFTP client", "error", err)
		os.Exit(1)
	}
	defer func() {
		_ = sftpClient.Close()
	}()

	latestBackupFilename, err := findLatestBackup(sftpClient, cfg.RemoteBackupDir)
	if err != nil {
		slog.Error("Failed to find latest backup", "error", err)
		os.Exit(1)
	}
	slog.Info("Found latest backup file to fetch", "filename", latestBackupFilename)

	localPath, err := downloadBackup(sftpClient, cfg.RemoteBackupDir, latestBackupFilename, cfg.LocalBackupDir)
	if err != nil {
		slog.Error("Failed to download backup", "error", err)
		os.Exit(1)
	}
	slog.Info("Successfully downloaded backup", "path", localPath)

	if err := verifyBackup(ctx, localPath); err != nil {
		slog.Error("Backup verification failed", "error", err)
		os.Exit(1)
	}

	slog.Info("Backup verification successful! The backup is valid.", "path", localPath)
}

func setupSftpClient(cfg Config) (*sftp.Client, error) {
	key, err := os.ReadFile(cfg.SSHPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User: cfg.SSHUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%s", cfg.SSHHost, cfg.SSHPort)
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to dial ssh: %w", err)
	}

	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to create sftp client: %w", err)
	}

	return client, nil
}

// findLatestBackup lists files in the remote directory and returns the name of the most recent one.
func findLatestBackup(client *sftp.Client, remoteDir string) (string, error) {
	files, err := client.ReadDir(remoteDir)
	if err != nil {
		return "", fmt.Errorf("could not list remote directory: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() > files[j].Name()
	})

	if len(files) == 0 {
		return "", fmt.Errorf("no backup files found in remote directory: %s", remoteDir)
	}

	return files[0].Name(), nil
}

func downloadBackup(client *sftp.Client, remoteDir, filename, localDir string) (localPath string, err error) {
	err = os.MkdirAll(localDir, 0755)
	if err != nil {
		return "", fmt.Errorf("could not create local backup directory: %w", err)
	}

	remotePath := filepath.Join(remoteDir, filename)
	localPath = filepath.Join(localDir, filename)

	srcFile, err := client.Open(remotePath)
	if err != nil {
		return "", fmt.Errorf("could not open remote backup file: %w", err)
	}
	defer func() {
		closeErr := srcFile.Close()
		err = errors.Join(err, closeErr)
	}()

	dstFile, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("could not create local backup file: %w", err)
	}
	defer func() {
		closeErr := dstFile.Close()
		err = errors.Join(err, closeErr)
	}()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return "", fmt.Errorf("failed to copy backup file: %w", err)
	}

	return localPath, nil
}

func verifyBackup(ctx context.Context, gzippedBackupPath string) (err error) {
	tempDBPath := filepath.Join(os.TempDir(), fmt.Sprintf("verified-%d.db", time.Now().UnixNano()))
	err = decompressFile(gzippedBackupPath, tempDBPath)
	if err != nil {
		return fmt.Errorf("failed to decompress for verification: %w", err)
	}
	defer func() {
		removeErr := os.Remove(tempDBPath)
		err = errors.Join(err, removeErr)
	}()

	slog.Info("Decompressed backup for verification", "path", tempDBPath)

	conn, err := sqlite.OpenConn(tempDBPath, sqlite.OpenReadOnly)
	if err != nil {
		return fmt.Errorf("failed to open decompressed database: %w", err)
	}
	defer func() {
		closeErr := conn.Close()
		err = errors.Join(err, closeErr)
	}()

	stmt, err := conn.Prepare("PRAGMA integrity_check;")
	if err != nil {
		return fmt.Errorf("failed to prepare integrity_check statement: %w", err)
	}
	defer func() {
		finalizeErr := stmt.Finalize()
		err = errors.Join(err, finalizeErr)
	}()

	row, err := stmt.Step()
	if err != nil {
		return fmt.Errorf("failed to execute integrity_check: %w", err)
	}
	if !row {
		return fmt.Errorf("integrity_check returned no rows")
	}

	result := stmt.ColumnText(0)
	if result != "ok" {
		return fmt.Errorf("integrity_check failed, result was: %s", result)
	}

	return nil
}

func decompressFile(sourcePath, destPath string) (err error) {
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to open source file for decompression: %w", err)
	}
	defer func() {
		closeErr := sourceFile.Close()
		err = errors.Join(err, closeErr)
	}()

	gzipReader, err := gzip.NewReader(sourceFile)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		closeErr := gzipReader.Close()
		err = errors.Join(err, closeErr)
	}()

	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file for decompression: %w", err)
	}
	defer func() {
		closeErr := destFile.Close()
		err = errors.Join(err, closeErr)
	}()

	_, err = io.Copy(destFile, gzipReader)
	if err != nil {
		return fmt.Errorf("failed to copy and decompress data: %w", err)
	}

	return nil
}
