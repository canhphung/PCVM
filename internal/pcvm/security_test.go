package pcvm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func runtimeTarFixture(t *testing.T, name, content string) []byte {
	return runtimeTarFilesFixture(t, map[string]string{name: content})
}

func runtimeTarFilesFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestRuntimeCacheTreeIntegrityIsReverified(t *testing.T) {
	payload := runtimeTarFilesFixture(t, map[string]string{"runtime/bin/tool": "trusted runtime\n", "runtime/lib/loader.so": "trusted library\n"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(payload) }))
	defer server.Close()
	h := NewHTTPClient()
	h.AllowHTTP = true
	h.AllowedHosts = map[string]bool{"127.0.0.1": true}
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	home := t.TempDir()
	manager := RuntimeManager{
		Catalog: Catalog{RuntimePacks: []RuntimePackSpec{{Kind: "test", Version: "1", Architecture: "amd64", URL: server.URL + "/runtime.tar.gz", SHA256: sum, Executable: "runtime/bin/tool", Archive: "tar.gz"}}},
		Config:  Config{Home: home, Control: filepath.Join(home, ".pcvm"), Arch: "amd64", Policy: Policy{CacheLimitBytes: 1 << 20}},
		HTTP:    h,
	}
	executable, err := manager.Ensure(context.Background(), "test", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("forged runtime\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	repaired, err := manager.Ensure(context.Background(), "test", "1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(repaired)
	if err != nil || string(data) != "trusted runtime\n" {
		t.Fatalf("tampered runtime was trusted: %q err=%v", data, err)
	}
	library := filepath.Join(filepath.Dir(filepath.Dir(repaired)), "lib", "loader.so")
	if err := os.WriteFile(library, []byte("forged library\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), "test", "1"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(library)
	if err != nil || string(data) != "trusted library\n" {
		t.Fatalf("tampered runtime library was trusted: %q err=%v", data, err)
	}
	extra := filepath.Join(filepath.Dir(library), "evil.so")
	if err := os.WriteFile(extra, []byte("unexpected library\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), "test", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(extra); !os.IsNotExist(err) {
		t.Fatalf("unexpected runtime entry survived verification: %v", err)
	}
}

func TestRuntimeCacheExecutableSymlinkIsRejected(t *testing.T) {
	payload := runtimeTarFixture(t, "runtime/bin/tool", "trusted runtime\n")
	archive := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(archive, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractRuntime(archive, root, "tar.gz"); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "runtime", "bin", "tool")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("forged runtime\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, executable); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	manifest, err := runtimeArchiveManifest(archive, "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := installedRuntimeTreeMatches(root, manifest); err == nil || ok {
		t.Fatalf("escaping runtime symlink accepted: ok=%v err=%v", ok, err)
	}
}

func TestZipExtractionRejectsExistingSymlinkTargets(t *testing.T) {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	file, err := zw.Create("nested/payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, "archive payload")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "fixture.zip")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []string{"final", "parent"} {
		t.Run(tc, func(t *testing.T) {
			destination := t.TempDir()
			outside := t.TempDir()
			outsideFile := filepath.Join(outside, "payload.txt")
			if err := os.WriteFile(outsideFile, []byte("outside sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc == "parent" {
				if err := os.Symlink(outside, filepath.Join(destination, "nested")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(destination, "nested"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideFile, filepath.Join(destination, "nested", "payload.txt")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			if err := extractZipSafe(archive, destination); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") && !strings.Contains(strings.ToLower(err.Error()), "real directory") {
				t.Fatalf("existing symlink target was accepted: %v", err)
			}
			outsideData, err := os.ReadFile(outsideFile)
			if err != nil || string(outsideData) != "outside sentinel" {
				t.Fatalf("archive wrote through symlink: %q err=%v", outsideData, err)
			}
		})
	}
}
