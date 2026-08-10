package ssh

import "testing"

// TestQuote verifies Quote wraps its input in single quotes, escaping
// embedded single quotes, so a POSIX shell treats the value literally.
func TestQuote(t *testing.T) {
	testCases := []struct {
		in   string
		want string
	}{
		{"/var/backups", "'/var/backups'"},
		{"a b; rm -rf /", "'a b; rm -rf /'"},
		{"/path/it's", "'/path/it'\\''s'"},
		{"", "''"},
	}
	for _, tc := range testCases {
		got := Quote(tc.in)
		if got != tc.want {
			t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
