package multiegg

import (
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	for _, raw := range []string{"token=abc123", "password hunter2", "https://user:pass@example.com/file"} {
		got := Redact(raw)
		if strings.Contains(got, "abc123") || strings.Contains(got, "hunter2") || strings.Contains(got, "user:pass") {
			t.Fatalf("secret leaked: %s", got)
		}
	}
}
