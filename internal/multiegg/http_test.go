package multiegg

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadChecksumAndAtomicFile(t *testing.T) {
	body := "verified payload"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	h := NewHTTPClient()
	h.AllowHTTP = true
	h.AllowedHosts = map[string]bool{strings.Split(host, ":")[0]: true}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	dest := filepath.Join(t.TempDir(), "artifact.bin")
	got, err := h.Download(context.Background(), Artifact{URL: server.URL, FileName: "artifact.bin", SHA256: sum}, dest)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != sum {
		t.Fatal("checksum not recorded")
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != body {
		t.Fatalf("file=%q err=%v", data, err)
	}
	if _, err = h.Download(context.Background(), Artifact{URL: server.URL, FileName: "bad", SHA256: strings.Repeat("0", 64)}, dest+".bad"); err == nil {
		t.Fatal("accepted bad checksum")
	}
}

func TestHTTPSAllowlist(t *testing.T) {
	h := NewHTTPClient()
	for _, raw := range []string{"http://github.com/a", "https://user:pass@github.com/a", "https://not-allowed.invalid/a"} {
		if err := h.validate(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
