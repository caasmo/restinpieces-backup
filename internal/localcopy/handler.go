package localcopy

import (
	"context"
	"log/slog"
	"time"

	"github.com/caasmo/restinpieces/db"
)

// Handler runs the shared backup pass when the scheduler fires the job.
// One pass walks every configured entry; the engine's due check skips
// the entries whose frequency has not elapsed.
type Handler struct {
	*Engine
}

// NewHandler creates the handler around the strategy. A nil logger
// falls back to slog.Default().
func NewHandler(strategy Strategy, logger *slog.Logger) *Handler {
	return &Handler{Engine: NewEngine(strategy, logger)}
}

// Handle runs one pass over every configured database. The job payload
// is not used.
func (h *Handler) Handle(ctx context.Context, job db.Job) error {
	return h.handle(ctx, time.Now())
}
