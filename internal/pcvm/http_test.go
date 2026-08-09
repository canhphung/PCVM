package pcvm

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
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

func TestDownloadSHA512(t *testing.T) {
	body := "debian cloud image fixture"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	h := NewHTTPClient()
	h.AllowHTTP = true
	h.AllowedHosts = map[string]bool{"127.0.0.1": true}
	sum := fmt.Sprintf("%x", sha512.Sum512([]byte(body)))
	dest := filepath.Join(t.TempDir(), "image.qcow2")
	if _, err := h.Download(context.Background(), Artifact{URL: server.URL, FileName: "image.qcow2", SHA512: sum}, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Download(context.Background(), Artifact{URL: server.URL, FileName: "bad", SHA512: strings.Repeat("0", 128)}, dest+".bad"); err == nil {
		t.Fatal("accepted bad SHA-512")
	}
}

func TestHTTPSAllowlist(t *testing.T) {
	h := NewHTTPClient()
	for _, raw := range []string{"https://linux.multitheftauto.com/dl/baseconfig.tar.gz", "https://mirror.multitheftauto.com/mtasa/resources/mtasa-resources-latest.zip", "https://mirror-cdn.multitheftauto.com/file.tar.gz"} {
		if err := h.validate(raw); err != nil {
			t.Fatalf("rejected official MTA host %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://github.com/a", "https://user:pass@github.com/a", "https://not-allowed.invalid/a"} {
		if err := h.validate(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestDebianCloudRedirectAllowlist(t *testing.T) {
	h := NewHTTPClient()
	request := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	origin := request("https://cloud.debian.org/images/cloud/trixie/image.qcow2")

	for _, raw := range []string{
		"https://laotzu.ftp.acc.umu.se/images/cloud/trixie/image.qcow2",
		"https://chuangtzu.ftp.acc.umu.se/images/cloud/trixie/image.qcow2",
	} {
		if err := h.validateRedirect(request(raw), []*http.Request{origin}); err != nil {
			t.Fatalf("rejected approved Debian mirror %q: %v", raw, err)
		}
	}

	for _, tc := range []struct {
		name   string
		target string
		origin string
	}{
		{name: "arbitrary host", target: "https://attacker.invalid/image.qcow2", origin: origin.URL.String()},
		{name: "wrong origin", target: "https://laotzu.ftp.acc.umu.se/image.qcow2", origin: "https://github.com/example/image.qcow2"},
		{name: "insecure mirror", target: "http://laotzu.ftp.acc.umu.se/image.qcow2", origin: origin.URL.String()},
		{name: "credentialed mirror", target: "https://user:pass@laotzu.ftp.acc.umu.se/image.qcow2", origin: origin.URL.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.validateRedirect(request(tc.target), []*http.Request{request(tc.origin)}); err == nil {
				t.Fatalf("accepted redirect to %q from %q", tc.target, tc.origin)
			}
		})
	}
}
