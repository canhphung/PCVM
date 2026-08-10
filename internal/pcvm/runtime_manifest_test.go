package pcvm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func runtimeZipFixture(t *testing.T, entries []struct{ name, body string }) string {
	t.Helper()
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	for _, entry := range entries {
		writer, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "runtime.zip")
	if err := os.WriteFile(archive, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestFinalizeRuntimePackBindsArchiveAndTree(t *testing.T) {
	archive := runtimeZipFixture(t, []struct{ name, body string }{
		{"runtime/bin/tool", "binary"},
		{"runtime/lib/data", "data"},
	})
	pack, err := FinalizeRuntimePack(archive, RuntimePackSpec{
		Kind: "example", Version: "1", UpstreamVersion: "1.0.7", Architecture: "amd64",
		URL: "https://example.com/runtime.zip", Executable: "runtime/bin/tool", Archive: "zip",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pack.ID != "example/1/amd64" || pack.Size <= 0 || len(pack.SHA256) != 64 || len(pack.TreeSHA256) != 64 {
		t.Fatalf("incomplete finalized pack: %+v", pack)
	}
	again, err := FinalizeRuntimePack(archive, pack)
	if err != nil || again != pack {
		t.Fatalf("finalization is not deterministic: %+v err=%v", again, err)
	}
}

func TestRuntimeTreeHashIgnoresArchiveOrdering(t *testing.T) {
	first := runtimeZipFixture(t, []struct{ name, body string }{{"runtime/bin/tool", "binary"}, {"runtime/lib/data", "data"}})
	second := runtimeZipFixture(t, []struct{ name, body string }{{"runtime/lib/data", "data"}, {"runtime/bin/tool", "binary"}})
	a, err := FinalizeRuntimePack(first, RuntimePackSpec{Kind: "x", Version: "1", UpstreamVersion: "1.0", Architecture: "amd64", Executable: "runtime/bin/tool", Archive: "zip"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := FinalizeRuntimePack(second, RuntimePackSpec{Kind: "x", Version: "1", UpstreamVersion: "1.0", Architecture: "amd64", Executable: "runtime/bin/tool", Archive: "zip"})
	if err != nil {
		t.Fatal(err)
	}
	if a.TreeSHA256 != b.TreeSHA256 {
		t.Fatalf("tree hash depends on archive order: %s != %s", a.TreeSHA256, b.TreeSHA256)
	}
	if a.SHA256 == b.SHA256 {
		t.Fatal("archive digest unexpectedly ignored ZIP ordering")
	}
}

func TestFinalizeRuntimePackRejectsTamperAndMissingExecutable(t *testing.T) {
	archive := runtimeZipFixture(t, []struct{ name, body string }{{"runtime/bin/tool", "binary"}})
	if _, err := FinalizeRuntimePack(archive, RuntimePackSpec{
		Kind: "x", Version: "1", UpstreamVersion: "1.0", Architecture: "amd64", SHA256: stringsOf('0', 64), Executable: "runtime/bin/tool", Archive: "zip",
	}); err == nil {
		t.Fatal("accepted an archive that did not match the pinned checksum")
	}
	if _, err := FinalizeRuntimePack(archive, RuntimePackSpec{
		Kind: "x", Version: "1", UpstreamVersion: "1.0", Architecture: "amd64", Executable: "missing", Archive: "zip",
	}); err == nil {
		t.Fatal("accepted a runtime without its declared executable")
	}
	for _, upstreamVersion := range []string{"", "   ", "1.0\nspoof"} {
		if _, err := FinalizeRuntimePack(archive, RuntimePackSpec{
			Kind: "x", Version: "1", UpstreamVersion: upstreamVersion, Architecture: "amd64", Executable: "runtime/bin/tool", Archive: "zip",
		}); err == nil {
			t.Fatalf("accepted invalid upstream version %q", upstreamVersion)
		}
	}
}

func TestFinalizeRuntimePackIgnoresExplicitArchiveRootDirectory(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "runtime.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	body := []byte("runtime")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "./bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	pack, err := FinalizeRuntimePack(archive, RuntimePackSpec{Kind: "test", Version: "1", UpstreamVersion: "1.0", Architecture: "amd64", Archive: "tar.gz", Executable: "bin/tool"})
	if err != nil {
		t.Fatalf("explicit root directory was rejected: %v", err)
	}
	if !validHexDigest(pack.TreeSHA256, 64) {
		t.Fatalf("invalid tree digest %q", pack.TreeSHA256)
	}
}

func TestLoadRuntimeManifestRequiresPrintableUpstreamVersion(t *testing.T) {
	manifest := RuntimeManifest{
		Schema: RuntimeManifestSchema, Release: "2.0.0", Compatibility: "pcvm>=2.0.0",
		Packs: []RuntimePackSpec{{ID: "java/21/amd64", UpstreamVersion: "jdk-21.0.12+8"}},
	}
	path := filepath.Join(t.TempDir(), "runtime-manifest.json")
	write := func() {
		t.Helper()
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := LoadRuntimeManifest(path); err != nil {
		t.Fatalf("valid upstream version rejected: %v", err)
	}
	for _, invalid := range []string{"", " ", "v21\u007fspoof", "v21\nspoof"} {
		manifest.Packs[0].UpstreamVersion = invalid
		write()
		if _, err := LoadRuntimeManifest(path); err == nil {
			t.Fatalf("accepted invalid upstream version %q", invalid)
		}
	}
}

func TestReleaseManifestPinsDotnet9ForTShock(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "runtime-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.RuntimePacks) != 38 {
		t.Fatalf("runtime packs=%d, want 38", len(catalog.RuntimePacks))
	}
	want := map[string]struct {
		upstream, checksum, tree string
		size                     int64
	}{
		"dotnet/9/amd64": {"9.0.18", "219e6fe801871b18c7b79e4971c2864d7ce0e48593e292f293dae41241510009", "0bb2de725b085333f68f24c0d5e2811587c40cd0a8a2876ca5d37ab3016e88dd", 33509705},
		"dotnet/9/arm64": {"9.0.18", "05cf45971341feb040c6c8e59ddbebabb6553f5e19d2adb355f36cc6e2fa9554", "e480ee066590a48ab35d443f714c2ff13712b9b0f7730968f1e86360812c472f", 31822675},
	}
	for id, expected := range want {
		found := false
		for _, pack := range catalog.RuntimePacks {
			if pack.ID != id {
				continue
			}
			found = true
			if pack.UpstreamVersion != expected.upstream || pack.SHA256 != expected.checksum || pack.TreeSHA256 != expected.tree || pack.Size != expected.size {
				t.Fatalf("pack %s does not match the finalized official archive: %+v", id, pack)
			}
		}
		if !found {
			t.Fatalf("missing %s", id)
		}
	}
	tshock, ok := catalog.Provider("tshock")
	if !ok || tshock.Runtime != "dotnet" || tshock.RuntimePolicy.Default != "9" || !contains(tshock.RuntimePolicy.Allowed, "9") {
		t.Fatalf("TShock runtime contract=%+v", tshock)
	}
	for _, arch := range tshock.Architectures {
		if !catalog.HasRuntime("dotnet", "9", arch) {
			t.Fatalf("TShock has no pinned .NET 9 runtime for %s", arch)
		}
	}
}

func stringsOf(value byte, count int) string { return string(bytes.Repeat([]byte{value}, count)) }
