// Command restinpieces is an example of embedding the onlineapi daemon
// in a restinpieces application: the app serves its HTTP API and, in
// the background, produces online backup snapshots of the databases
// configured in the backup section.
//
// The daemon reads the [backup] section of the application
// configuration from the app's current-config box, so it needs no
// configuration of its own. The databases are configured where the
// rest of the application configuration lives, with the shape ripc
// scaffolds for app mode:
//
//	[backup.online.app_db]
//	source_path = "/path/to/db"
//	dest_path = "/path/to/backups"
//	frequency = "24h"
//	pages_per_step = 100
//	sleep_interval = "10ms"
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
	"github.com/caasmo/restinpieces-backup/onlineapi"
	"github.com/caasmo/restinpieces/config"
)

func main() {
	// Define flags directly in main
	dbPath := flag.String("dbpath", "", "Path to the SQLite database file (required)")
	ageKeyPath := flag.String("age-key", "", "Path to the age identity (private key) file (required)")

	// Set custom usage message for the application
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -dbpath <database-path> -age-key <identity-file-path>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Start a restinpieces application that also produces online backup snapshots of the configured databases.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	// Parse flags
	flag.Parse()

	// Validate required flags
	if *dbPath == "" || *ageKeyPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	dbPool, err := restinpieces.NewZombiezenPerformancePool(*dbPath)
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
		restinpieces.WithZombiezenPool(dbPool),
		restinpieces.WithAgeKeyPath(*ageKeyPath),
	)
	if err != nil {
		slog.Error("failed to initialize application", "error", err)
		os.Exit(1)
	}

	// --- OnlineApi daemon setup ---
	// New loads and validates the application configuration from the
	// config store, so the current-config box is already populated.
	// The onlineapi daemon holds that box (coreApp.ConfigPointer()) and
	// reads the backup.online configuration at every tick.
	onlineapiDaemon := onlineapi.New[config.Config](coreApp.ConfigPointer(), nil)

	// The daemon satisfies the restinpieces server.Daemon contract:
	// the server starts it with Start() after the HTTP server, and
	// stops it with Stop() during graceful shutdown.
	srv.AddDaemon(onlineapiDaemon)

	// Run blocks until SIGINT/SIGQUIT/SIGHUP. SIGINT/SIGQUIT shut the
	// server and the daemons down gracefully within the configured
	// deadline; SIGHUP reloads the application configuration.
	srv.Run()

	slog.Info("Server shut down gracefully.")
}
