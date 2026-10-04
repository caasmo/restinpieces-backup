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

// JobTypeVacuum is the job type this handler registers under.
const JobTypeVacuum = "vacuum"

// VacuumStrategy reads the vacuum map from the config pointer on every
// call and copies databases with VACUUM INTO.
type VacuumStrategy struct {
	cfgPointer *atomic.Pointer[config.Config]
}

// Entries returns the configured vacuum entries in the common shape.
func (s *VacuumStrategy) Entries() []localcopy.Entry {
	var out []localcopy.Entry
	for key, f := range (*s.cfgPointer.Load()).Backup.Vacuum {
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
func (s *VacuumStrategy) Copy(ctx context.Context, srcConn *sql.Conn, destPath string, entry localcopy.Entry) error {
	destPath = strings.ReplaceAll(destPath, "'", "''")
	_, err := srcConn.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", destPath))
	if err != nil {
		return fmt.Errorf("failed to execute vacuum statement: %w", err)
	}
	return nil
}

// New creates the vacuum job handler around the config pointer. The
// handler reads the box on every run, so a configuration reload is
// visible at the next scheduled run. A nil logger falls back to
// slog.Default().
func New(pointer *atomic.Pointer[config.Config], logger *slog.Logger) *localcopy.Handler {
	return localcopy.NewHandler(&VacuumStrategy{cfgPointer: pointer}, logger)
}
