// Command restinpieces is an example of embedding the S3 upload daemon in
// a restinpieces application: the app serves its HTTP API and, in the
// background, uploads the newest backups of the configured backup labels
// to an S3-compatible bucket.
//
// The daemon reads the [backup] and [s3] sections of the application
// configuration from the running app, so it needs no
// configuration of its own. The uploads are configured like the rest of
// the application configuration, with the tables ripc scaffolds for app
// mode:
//
//	[backup.online.app-online]
//	source_path = "/path/to/db"
//	dest_path = "/path/to/backups"
//	frequency = "24h"
//
//	[backup.s3_upload.app-s3]
//	backup_label = "app-online"
//	frequency = "5m"
//	age_recipient = "age1..."
//
//	[s3]
//	endpoint = "https://s3.example.com"
//	region = "auto"
//	bucket = "my-backups"
//	access_key = "..."
//	secret_key = "..."
//	use_path_style = true
//
// A SIGHUP reload of the application configuration is visible at the
// next daemon tick.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/caasmo/restinpieces"
	"github.com/caasmo/restinpieces-backup/s3upload"
)

func main() {
	// Define flags directly in main
	dbPath := flag.String("dbpath", "", "Path to the SQLite database file (required)")
	ageKeyPath := flag.String("age-key", "", "Path to the age identity (private key) file (required)")

	// Set custom usage message for the application
	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "Usage: %s -dbpath <database-path> -age-key <identity-file-path>\n\n", os.Args[0])
		_, _ = fmt.Fprintf(os.Stderr, "Start a restinpieces application that also uploads backup backups to S3.\n\n")
		_, _ = fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	// Parse flags
	flag.Parse()

	// Validate required flags
	if *dbPath == "" || *ageKeyPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	dbPool, err := restinpieces.NewModerncPool(*dbPath)
	if err != nil {
		slog.Error("failed to create database pool", "error", err)
		os.Exit(1)
	}

	defer func() {
		slog.Info("Closing database pool...")
		if err := dbPool.Close(); err != nil {
			slog.Error("Error closing database pool", "error", err)
		}
	}()

	// Standard slog logger to stderr. Providing a logger through
	// WithLogger keeps the framework's internal batch log daemon out
	// of the process: the example needs no log database.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	coreApp, srv, err := restinpieces.New(
		restinpieces.WithLogger(logger),
		restinpieces.WithModerncPool(dbPool),
		restinpieces.WithAgeKeyPath(*ageKeyPath),
	)
	if err != nil {
		slog.Error("failed to initialize application", "error", err)
		os.Exit(1)
	}

	// --- S3 upload daemon setup ---
	// New loads and validates the application configuration from the
	// config store, so the current configuration is already loaded.
	// The daemon holds the pointer (coreApp.ConfigPointer()) and reads the
	// backup and s3 configuration at every tick.
	s3Daemon := s3upload.New(coreApp.ConfigPointer(), nil)

	// The daemon satisfies the restinpieces server.Daemon interface:
	// the server starts it with Start() after the HTTP server, and
	// stops it with Stop() during graceful shutdown.
	srv.AddDaemon(s3Daemon)

	// Run blocks until SIGINT/SIGQUIT/SIGHUP. SIGINT/SIGQUIT shut the
	// server and the daemons down gracefully within the configured
	// deadline; SIGHUP reloads the application configuration.
	srv.Run()

	slog.Info("Server shut down gracefully.")
}
