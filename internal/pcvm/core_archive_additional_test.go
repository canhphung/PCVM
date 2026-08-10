package pcvm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuccessfulZipExtractionPreservesUserBedrockConfig(t *testing.T) {
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	directory := &zip.FileHeader{Name: "nested/", Method: zip.Store}
	directory.SetMode(os.ModeDir | 0o755)
	if _, err := writer.CreateHeader(directory); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"nested/server.bin": "binary", "server.properties": "upstream=true"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "server.zip")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "server.properties"), []byte("user=true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractZipSafe(archive, destination); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "nested", "server.bin")); err != nil || string(data) != "binary" {
		t.Fatalf("extracted payload=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "server.properties")); err != nil || string(data) != "user=true" {
		t.Fatalf("user config was replaced: %q %v", data, err)
	}
}

func TestArchivePathAndStripContracts(t *testing.T) {
	for _, raw := range []string{"../escape", "/absolute", "C:\\absolute"} {
		if _, _, err := archiveTarget(t.TempDir(), raw); err == nil {
			t.Errorf("unsafe archive target %q accepted", raw)
		}
	}
	for _, test := range []struct {
		name  string
		strip int
		want  string
		keep  bool
	}{
		{"root/bin/tool", 1, "bin/tool", true},
		{"root", 1, "", false},
		{"./", 0, "", false},
	} {
		got, keep, err := stripArchiveComponents(test.name, test.strip)
		if err != nil || got != test.want || keep != test.keep {
			t.Errorf("strip(%q)=(%q,%v,%v), want (%q,%v,nil)", test.name, got, keep, err, test.want, test.keep)
		}
	}
	if _, _, err := stripArchiveComponents("../../escape", 1); err == nil {
		t.Fatal("unsafe stripped path accepted")
	}
}

func TestTarExtractionEntryKindsAndSymlinkQuota(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	entries := []tar.Header{
		{Name: "root", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "root/data", Typeflag: tar.TypeReg, Mode: 0o640, Size: 4},
	}
	for _, header := range entries {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			_, _ = writer.Write([]byte("data"))
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractTarStreamSafe(tar.NewReader(bytes.NewReader(data.Bytes())), destination, tarExtractOptions{Limits: defaultArchiveLimits}); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "root", "data")); err != nil || string(content) != "data" {
		t.Fatalf("tar payload=%q err=%v", content, err)
	}

	data.Reset()
	writer = tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "device", Typeflag: tar.TypeChar}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := extractTarStreamSafe(tar.NewReader(bytes.NewReader(data.Bytes())), t.TempDir(), tarExtractOptions{Limits: defaultArchiveLimits}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("special TAR entry error=%v", err)
	}

	budget := newArchiveBudget(archiveLimits{MaxEntries: 2, MaxTotalBytes: 10, MaxFileBytes: 10, MaxPathBytes: 20, MaxSymlinks: 0})
	if err := budget.add("dir", archiveEntryDirectory, 0); err != nil {
		t.Fatal(err)
	}
	if err := budget.add("dir", archiveEntryDirectory, 0); err != nil {
		t.Fatalf("duplicate directory marker should be idempotent: %v", err)
	}
	if err := budget.add("link", archiveEntrySymlink, 0); err == nil {
		t.Fatal("symlink quota was not enforced")
	}
}

func TestArchiveOutputAndBedrockConfigHelpers(t *testing.T) {
	var output strings.Builder
	writer := &limitedStringWriter{builder: &output, remaining: 4}
	if written, err := writer.Write([]byte("123456")); err != nil || written != 6 || output.String() != "1234" {
		t.Fatalf("limited writer=(%d,%q,%v)", written, output.String(), err)
	}
	if written, err := writer.Write([]byte("ignored")); err != nil || written != 7 || output.String() != "1234" {
		t.Fatalf("exhausted writer=(%d,%q,%v)", written, output.String(), err)
	}
	for _, name := range []string{"server.properties", "allowlist.json", "permissions.json", "nested/server.properties"} {
		if !isBedrockConfig(name) {
			t.Errorf("Bedrock config %q not recognized", name)
		}
	}
	if isBedrockConfig("server.jar") {
		t.Fatal("ordinary file treated as Bedrock config")
	}
}
