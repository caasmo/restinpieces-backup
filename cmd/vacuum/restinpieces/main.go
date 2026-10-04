// Command restinpieces is an example of embedding the vacuum handler
// in a restinpieces application: the app serves its HTTP API and, in
// the background, produces VACUUM INTO snapshots of the databases
// configured in the backup section.
//
// The handler reads the [backup] section of the application
// configuration from the app's current-config box, so it needs no
// configuration of its own. The databases are configured where the
// rest of the application configuration lives, with the shape ripc
// scaffolds for app mode:
//
//	[backup.vacuum.app_db]
//	source_path = "/path/to/db"
//	dest_path = "/path/to/backups"
//	frequency = "24h"
//
// The scheduler runs the handler on the interval of the [scheduler.jobs]
// entry whose job_type is "vacuum". An entry is backed up only when its
// frequency has elapsed:
//
//	[scheduler.jobs.vacuum]
//	job_type = "vacuum"
//	interval = "1m"
//	activated = true
//
// A SIGHUP reload of the application configuration is visible at the
// next scheduled run.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/caasmo/restinpieces"
	"github.com/caasmo/restinpieces-backup/vacuum"
)

func main() {
	// Define flags directly in main
	dbPath := flag.String("dbpath", "", "Path to the SQLite database file (required)")
	ageKeyPath := flag.String("age-key", "", "Path to the age identity (private key) file (required)")

	// Set custom usage message for the application
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -dbpath <database-path> -age-key <identity-file-path>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Start a restinpieces application that also produces vacuum snapshots of the configured databases.\n\n")
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

	// --- Vacuum job setup ---
	// New loads and validates the application configuration from the
	// config store, so the current-config box is already populated.
	// The handler holds that box (coreApp.ConfigPointer()) and reads the
	// backup.vacuum configuration at every run.
	vacuumHandler := vacuum.New(coreApp.ConfigPointer(), nil)

	// Registering the handler makes the scheduler run it on the interval
	// of the [scheduler.jobs] entry whose job_type is "vacuum".
	err = srv.AddJobHandler(vacuum.JobTypeVacuum, vacuumHandler)
	if err != nil {
		slog.Error("failed to register the vacuum job handler", "error", err)
		os.Exit(1)
	}

	// Run blocks until SIGINT/SIGQUIT/SIGHUP. SIGINT/SIGQUIT shut the
	// server and the daemons down gracefully within the configured
	// deadline; SIGHUP reloads the application configuration.
	srv.Run()

	slog.Info("Server shut down gracefully.")
}
