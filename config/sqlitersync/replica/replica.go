// Package replica holds the replica configuration document: the root
// shape the sqlite-rsync replica command reads from its -config TOML
// file. The types are shared so the wiring main that parses the file
// and the daemon that consumes the parsed entries describe the data
// once. The TOML tags make the types the document: main unmarshals
// straight into them, the same way the origin command unmarshals into
// restinpieces' backup config.
//
// The document mirrors the origin side it pulls from: the origin
// serves databases under [backup.sqlite-rsync.entries.<name>], the
// replica pulls them under [entries.<name>]. Only the replica-side
// concerns live here: where each pulled database lands locally
// (path), how often it is pulled (frequency), how long one pull may
// run (sync_timeout), and how to reach a remote origin ([ssh] — the
// canonical config.SSH, its fields tagged with these key names).
package replica

import (
	"fmt"
	"time"

	"github.com/caasmo/restinpieces-backup/config"
)

// Duration is a wrapper around time.Duration that supports TOML
// unmarshalling from a string value (e.g., "30s", "15m") via
// encoding.TextUnmarshaler, which pelletier/go-toml/v2 honors.
// time.Duration itself is an int64 and does not implement
// TextUnmarshaler; without this wrapper a TOML string would fail
// with "cannot store TOML string into Go int64" (see
// pelletier/go-toml#767).
type Duration struct {
	time.Duration
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("failed to parse duration '%s': %w", string(text), err)
	}
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Config is the replica configuration document root. OriginAddr is
// the origin listener's dial target — reachable directly in local
// mode and through the SSH channel in remote mode. SyncTimeout caps
// every single pull regardless of entry; it is required. SSH holds
// the remote-mode transport block; nil means the section is absent.
// Entries maps the origin's entry name to where the replica writes it
// and how often it is pulled; a zero frequency disables the entry.
type Config struct {
	OriginAddr  string           `toml:"origin_addr"`
	SyncTimeout Duration         `toml:"sync_timeout"`
	SSH         *config.SSH      `toml:"ssh"`
	Entries     map[string]Entry `toml:"entries"`
}

// Entry is one configured database pull: the local destination of the
// received pages and the cadence. A missing file is created by the
// first sync; afterwards its mtime records the last completed sync.
type Entry struct {
	Frequency Duration `toml:"frequency"`
	Path      string   `toml:"path"`
}
