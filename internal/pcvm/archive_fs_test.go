package pcvm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeZipFixture(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, body := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestZipExtractionRejectsCompressionBomb(t *testing.T) {
	archive := writeZipFixture(t, map[string]string{"zeros.bin": strings.Repeat("\x00", 1<<20)})
	limits := defaultArchiveLimits
	limits.MaxCompressionRatio = 20
	err := extractZipSafeWithLimits(archive, t.TempDir(), limits)
	if err == nil || !strings.Contains(err.Error(), "compression ratio") {
		t.Fatalf("compression-bomb error=%v", err)
	}
}

func TestCompressedTarExtractionRejectsCompressionBomb(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bomb.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	body := bytes.Repeat([]byte{0}, 4<<20)
	if err := tw.WriteHeader(&tar.Header{Name: "zeros.bin", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	err = extractTarGzipSafe(archive, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "compression ratio") {
		t.Fatalf("compression-bomb error=%v", err)
	}
}

func TestXZExtractionRejectsCompressionBomb(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz is not installed")
	}
	plain := filepath.Join(t.TempDir(), "bomb.tar")
	file, err := os.Create(plain)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(file)
	body := bytes.Repeat([]byte{0}, 4<<20)
	if err := tw.WriteHeader(&tar.Header{Name: "root/zeros.bin", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("xz", "-z", "--keep", "--", plain)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("xz fixture: %v: %s", err, output)
	}
	err = extractTarXZSafe(context.Background(), plain+".xz", t.TempDir(), 1)
	if err == nil || !strings.Contains(err.Error(), "compression ratio") {
		t.Fatalf("compression-bomb error=%v", err)
	}
}

func TestZipExtractionEnforcesEntryAndExpansionQuotas(t *testing.T) {
	archive := writeZipFixture(t, map[string]string{"one.txt": "12345", "two.txt": "67890"})
	for name, test := range map[string]struct {
		limits archiveLimits
		want   string
	}{
		"entries": {archiveLimits{MaxEntries: 1, MaxTotalBytes: 100, MaxFileBytes: 100, MaxPathBytes: 100, MaxSymlinks: 0}, "more than 1 entries"},
		"total":   {archiveLimits{MaxEntries: 10, MaxTotalBytes: 9, MaxFileBytes: 100, MaxPathBytes: 100, MaxSymlinks: 0}, "total limit"},
		"file":    {archiveLimits{MaxEntries: 10, MaxTotalBytes: 100, MaxFileBytes: 4, MaxPathBytes: 100, MaxSymlinks: 0}, "per-file limit"},
		"path":    {archiveLimits{MaxEntries: 10, MaxTotalBytes: 100, MaxFileBytes: 100, MaxPathBytes: 3, MaxSymlinks: 0}, "path exceeds"},
	} {
		t.Run(name, func(t *testing.T) {
			err := extractZipSafeWithLimits(archive, t.TempDir(), test.limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestZipExtractionRejectsDuplicateAndPreexistingSymlinkParent(t *testing.T) {
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, body := range []string{"first", "second"} {
		entry, err := writer.Create("duplicate.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "duplicate.zip")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractZipSafe(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "duplicate path") {
		t.Fatalf("duplicate error=%v", err)
	}

	destination, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(destination, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	archive = writeZipFixture(t, map[string]string{"linked/payload.txt": "blocked"})
	err := extractZipSafe(archive, destination)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "real directory") {
		t.Fatalf("symlink-parent error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload.txt")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped through symlink: %v", err)
	}
}

func TestExtractionRejectsSymlinkInRootPath(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(base, "linked-root")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	destination := filepath.Join(link, "extract")
	if err := os.MkdirAll(filepath.Join(outside, "extract"), 0o750); err != nil {
		t.Fatal(err)
	}
	archive := writeZipFixture(t, map[string]string{"payload.txt": "blocked"})
	err := extractZipSafe(archive, destination)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink component") {
		t.Fatalf("root-symlink error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "extract", "payload.txt")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped through root symlink: %v", err)
	}
}

func TestTarExtractionDoesNotFollowArchiveSymlinkAsParent(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	entries := []struct {
		header tar.Header
		body   string
	}{
		{tar.Header{Name: "real", Typeflag: tar.TypeDir, Mode: 0o755}, ""},
		{tar.Header{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "real", Mode: 0o777}, ""},
		{tar.Header{Name: "alias/payload", Typeflag: tar.TypeReg, Mode: 0o644, Size: 7}, "payload"},
	}
	for _, entry := range entries {
		if err := writer.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			_, _ = writer.Write([]byte(entry.body))
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	err := extractTarStreamSafe(tar.NewReader(bytes.NewReader(data.Bytes())), destination, tarExtractOptions{
		Limits: defaultArchiveLimits, AllowSymlinks: true,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "real directory") {
		t.Fatalf("symlink-parent error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "real", "payload")); !os.IsNotExist(err) {
		t.Fatalf("archive followed internal symlink parent: %v", err)
	}
}

func TestTarExtractionRejectsHardLinksAndQuotaBeforeWrite(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0o644, Size: 8}); err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("12345678"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	err := extractTarStreamSafe(tar.NewReader(bytes.NewReader(data.Bytes())), destination, tarExtractOptions{Limits: archiveLimits{
		MaxEntries: 10, MaxTotalBytes: 7, MaxFileBytes: 10, MaxPathBytes: 100, MaxSymlinks: 0,
	}})
	if err == nil || !strings.Contains(err.Error(), "total limit") {
		t.Fatalf("quota error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "large")); !os.IsNotExist(err) {
		t.Fatalf("oversized file was created: %v", err)
	}

	data.Reset()
	writer = tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "target"}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	err = extractTarStreamSafe(tar.NewReader(bytes.NewReader(data.Bytes())), t.TempDir(), tarExtractOptions{Limits: defaultArchiveLimits})
	if err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hard-link error=%v", err)
	}
}
