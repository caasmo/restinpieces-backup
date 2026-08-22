// Package sqlitersync sends the label of the database to back up from the
// client to the server.
//
// The sync library used in this project (package sqlitersync) knows how
// to sync a database once both sides are open, but it never says which
// database is being synced. In this project the client and the server
// are separate programs, so before a sync can start the client must
// tell the server which database to serve. This package is that
// message: the client writes the name of the database, and the server
// accepts it or writes back an error and closes the connection.
//
// A database is named by its label, a short name chosen by the operator
// in the backup configuration. A message carries a label or an error,
// and both have the same shape; the first byte tells them apart:
//
//	+------------+----------------------+---------------------+
//	| first byte | length               | text                |
//	| 1 byte     | 4 bytes, big-endian  | `length` bytes      |
//	+------------+----------------------+---------------------+
//
// A first byte of 0x01 names a label, 0x02 names an error. The label
// "app_db", for example, is the bytes:
//
//	0x01 00 00 00 06 61 70 70 5f 64 62
//
// The preamble is a two-phase handshake, and the sync protocol starts
// only after the preamble completes:
//
// 1. The client writes the label (0x01 plus the database name).
// 2. The server answers with a preamble response: it echoes the label
//    (0x01 plus the same name) to accept the sync, or writes an error
//    (0x02 plus the reason) to reject it and closes the connection.
//
// The echo is the acceptance signal. The 0x01 and 0x02 bytes never
// clash with the sync protocol: after an accepted preamble the origin
// starts the sync protocol, whose first byte is always 0x41
// (ORIGIN_BEGIN). The client reads the preamble response itself and
// starts the sync only after an accepted echo; a rejection is reported
// with its text.
package sqlitersync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// LabelByte is the first byte of a message naming a database.
	LabelByte = 0x01
	// ErrorByte is the first byte of a message rejecting the request.
	ErrorByte = 0x02
)

// MaxLen bounds the length of a message text sent over the connection.
// Labels and error texts are short; 64 bytes is generous.
const MaxLen = 64

// ErrInvalid is returned for messages that are not well-formed: a text
// that is too long.
var ErrInvalid = errors.New("sqlitersync: invalid message")

// Write writes one message: the first byte, the message length as four
// big-endian bytes, then the message text.
func Write(w io.Writer, first byte, text string) error {
	if len(text) > MaxLen {
		return fmt.Errorf("%w: message of %d bytes exceeds %d", ErrInvalid, len(text), MaxLen)
	}
	var hdr [5]byte
	hdr[0] = first
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(text)))
	_, err := w.Write(hdr[:])
	if err != nil {
		return err
	}
	n, err := io.WriteString(w, text)
	if err != nil {
		return err
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	return nil
}

// Read reads one message and returns its first byte and its text.
func Read(r io.Reader) (byte, string, error) {
	var hdr [5]byte
	_, err := io.ReadFull(r, hdr[:])
	if err != nil {
		return 0, "", err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxLen {
		return 0, "", fmt.Errorf("%w: message length %d exceeds %d", ErrInvalid, n, MaxLen)
	}
	buf := make([]byte, n)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return 0, "", err
	}
	return hdr[0], string(buf), nil
}
