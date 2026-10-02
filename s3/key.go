package s3

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// KeyPrefix is the leading key segment of every backup object:
//
//	backup/<label>/<pad>/<filename>
//
// For example, label "app-s3", pad 8209066599 and file app.db:
//
//	backup/app-s3/8209066599/app.db
const KeyPrefix = "backup"

// MaxUnixTimestamp is the newest time the inverted pad encodes:
// 9999-12-31T23:59:59Z. The pad is MaxUnixTimestamp minus the file's
// modification time, so a bucket listing returns the newest object.
const MaxUnixTimestamp = 253402300799

// Pad returns the pad of a backup taken at modTime: the time counted down
// from year 9999 and zero-padded, so a bucket listing returns the newest
// object. The zero-padded width keeps the pad in its own key
// segment.
func Pad(modTime time.Time) string {
	return fmt.Sprintf("%012d", MaxUnixTimestamp-modTime.Unix())
}

// ObjectKey returns the bucket key of one backup: the key prefix, the
// entry label, the pad, and the file name. Example:
//
//	backup/app-s3/8209066599/app.db
func ObjectKey(label, pad, name string) string {
	return path.Join(KeyPrefix, label, pad, name)
}

// PadAndName splits an object key into its pad and its file name, the
// last two segments. ok is false when the key has fewer than two
// segments.
func PadAndName(objectKey string) (pad, name string, ok bool) {
	segments := strings.Split(objectKey, "/")
	if len(segments) < 2 {
		return "", "", false
	}
	return segments[len(segments)-2], segments[len(segments)-1], true
}
