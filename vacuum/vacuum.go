// Package vacuum provides the VACUUM INTO snapshot daemon. It backs
// up the databases configured in the vacuum section of the backup
// configuration, producing a clean, defragmented copy of each.
package vacuum

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/caasmo/restinpieces-backup/internal/localcopy"
	"github.com/caasmo/restinpieces/config"
)

// VacuumConfig is the box payload contract: any type exposing the
// vacuum entries satisfies it. config.Config and the standalone
// vacuumCfg both implement it.
type VacuumConfig interface {
	BackupVacuum() config.BackupVacuum
}

// VacuumStrategy reads the vacuum map from the config box on every
// call and copies databases with VACUUM INTO.
type VacuumStrategy[T VacuumConfig] struct {
	box *atomic.Pointer[T]
}

// Entries returns the configured vacuum entries in the common shape.
func (s *VacuumStrategy[T]) Entries() []localcopy.Entry {
	var out []localcopy.Entry
	for key, f := range (*s.box.Load()).BackupVacuum() {
		out = append(out, localcopy.Entry{
			Label:       key,
			SourcePath:  f.SourcePath,
			DestPath:    f.DestPath,
			Frequency:   f.Frequency.Duration,
			Compression: f.Compression,
		})
	}
	return out
}

// Copy performs one VACUUM INTO copy of the source database.
func (s *VacuumStrategy[T]) Copy(ctx context.Context, srcConn *sql.Conn, destPath string, entry localcopy.Entry) error {
	destPath = strings.ReplaceAll(destPath, "'", "''")
	_, err := srcConn.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", destPath))
	if err != nil {
		return fmt.Errorf("failed to execute vacuum statement: %w", err)
	}
	return nil
}

// New creates the vacuum daemon around the config box. The daemon
// reads the box on every tick, so a configuration reload is visible
// at the next tick. A nil logger falls back to slog.Default().
func New[T VacuumConfig](box *atomic.Pointer[T], logger *slog.Logger) *localcopy.Daemon {
	return localcopy.New("VacuumDaemon", &VacuumStrategy[T]{box: box}, logger)
}
