package s3

import (
	"testing"
	"time"
)

func TestPad_NewestKeySortsFirst(t *testing.T) {
	newer := Pad(time.Unix(2000, 0))
	older := Pad(time.Unix(1000, 0))
	if newer >= older {
		t.Errorf("pad of newer = %q, want it to sort before older pad %q", newer, older)
	}
}

func TestObjectKey_Layout(t *testing.T) {
	got := ObjectKey("app-s3", "8209066599", "app.db")
	want := "backup/app-s3/8209066599/app.db"
	if got != want {
		t.Errorf("ObjectKey: got %q, want %q", got, want)
	}
}

func TestPadAndName_RoundTrip(t *testing.T) {
	key := ObjectKey("app-s3", "8209066599", "app.db")

	pad, name, ok := PadAndName(key)
	if !ok {
		t.Fatalf("PadAndName(%q): expected ok", key)
	}
	if pad != "8209066599" {
		t.Errorf("pad: got %q, want %q", pad, "8209066599")
	}
	if name != "app.db" {
		t.Errorf("name: got %q, want %q", name, "app.db")
	}
}

func TestPadAndName_TooShort(t *testing.T) {
	_, _, ok := PadAndName("app.db")
	if ok {
		t.Error("expected ok false for a key without two segments")
	}
}
