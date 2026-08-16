package backup

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestWriteReadLabel(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, LabelByte, "app_db")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	first, text, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != LabelByte {
		t.Fatalf("first byte = 0x%02x, want 0x%02x", first, LabelByte)
	}
	if text != "app_db" {
		t.Fatalf("text = %q, want %q", text, "app_db")
	}
}

func TestWriteReadError(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, ErrorByte, "unknown database")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	first, text, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first != ErrorByte {
		t.Fatalf("first byte = 0x%02x, want 0x%02x", first, ErrorByte)
	}
	if text != "unknown database" {
		t.Fatalf("text = %q, want %q", text, "unknown database")
	}
}

func TestWriteTooLong(t *testing.T) {
	err := Write(io.Discard, LabelByte, strings.Repeat("x", MaxLen+1))
	if err == nil {
		t.Fatal("Write accepted an over-long message")
	}
}

func TestReadOverLongLength(t *testing.T) {
	buf := []byte{LabelByte, 0, 0, 0, MaxLen + 1}
	_, _, err := Read(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("Read accepted an over-long length")
	}
}
