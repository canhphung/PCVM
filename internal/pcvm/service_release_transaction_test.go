package pcvm

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelocatableServiceProvidersUseReleaseStore(t *testing.T) {
	for _, id := range []string{"endstone", "samp", "mtasa", "terraria", "tmodloader", "tshock", "factorio", "code-server"} {
		t.Run(id, func(t *testing.T) {
			spec := catalogSpec(t, id)
			if spec.RollbackMode != "staged" || !releaseStoreSupports(spec) {
				t.Fatalf("provider is not backed by the staged release store: rollback=%q installer=%q", spec.RollbackMode, spec.Installer)
			}
		})
	}
}

func TestServiceTreeActivationPreservesUserDataAndRewritesEnvironment(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	spec := ProviderSpec{ID: "endstone", Installer: "endstone", RollbackMode: "staged"}
	candidate := filepath.Join(control, "managed", spec.ID, ".candidate-fixture")
	mustWriteBytes(t, filepath.Join(candidate, "site-packages", "endstone", "__init__.py"), []byte("fixture"), 0o640)
	world := filepath.Join(home, "worlds", "level.dat")
	mustWriteBytes(t, world, []byte("persistent world"), 0o640)
	artifact := Artifact{Version: "1.0.0", Build: "release", SHA256: strings.Repeat("a", 64)}
	resolved := Resolved{
		Artifact:    artifact,
		WorkDir:     candidate,
		Command:     []string{"/trusted/python", "-m", "endstone", "--server-folder", home},
		Environment: []string{"PYTHONPATH=" + filepath.Join(candidate, "site-packages")},
	}
	activated, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(20, 0))
	if err != nil {
		t.Fatal(err)
	}
	releaseID := releaseIDFor(spec.ID, lockArtifact(spec.ID, artifact))
	payload := releasePayloadRoot(control, spec.ID, releaseID)
	if activated.WorkDir != payload {
		t.Fatalf("work directory=%q want %q", activated.WorkDir, payload)
	}
	wantPythonPath := "PYTHONPATH=" + filepath.Join(payload, "site-packages")
	if !contains(activated.Environment, wantPythonPath) {
		t.Fatalf("environment was not relocated: %v", activated.Environment)
	}
	if data, err := os.ReadFile(world); err != nil || string(data) != "persistent world" {
		t.Fatalf("persistent user data changed: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("unpublished candidate survived activation: %v", err)
	}
}

func TestTerrariaAndTModLoaderInstallDirectlyIntoImmutableReleases(t *testing.T) {
	tests := []struct {
		id      string
		entries map[string]zipFixture
	}{
		{
			id: "terraria",
			entries: map[string]zipFixture{
				"1453/Linux/TerrariaServer.bin.x86_64": {body: "terraria", mode: 0o750},
				"1453/Linux/System.dll":                {body: "library", mode: 0o640},
			},
		},
		{
			id: "tmodloader",
			entries: map[string]zipFixture{
				"tModLoader.dll":  {body: "managed assembly", mode: 0o640},
				"Libraries/a.dll": {body: "managed library", mode: 0o640},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			home := t.TempDir()
			archive := filepath.Join(t.TempDir(), test.id+".zip")
			writeServiceZipFixture(t, archive, test.entries)
			spec := catalogSpec(t, test.id)
			resolved := Resolved{
				Artifact:       Artifact{Version: "9.9.9", Build: "fixture", SHA256: strings.Repeat("b", 64)},
				RuntimeVersion: "8",
			}
			installed, err := NewProvider(spec).Install(context.Background(), InstallContext{
				Home: home, ControlDir: filepath.Join(home, ".pcvm"), Artifact: archive, Runtime: "/trusted/dotnet",
			}, resolved)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(filepath.ToSlash(installed.WorkDir), "/.pcvm/releases/"+test.id+"/") {
				t.Fatalf("installer did not activate an immutable release: %+v", installed)
			}
			if _, err := os.Lstat(filepath.Join(home, "game")); !os.IsNotExist(err) {
				t.Fatalf("legacy live game tree was created: %v", err)
			}
			if err := validateRelease(releaseRoot(filepath.Join(home, ".pcvm"), spec.ID, releaseIDFor(spec.ID, lockArtifact(spec.ID, installed.Artifact))), spec.ID, releaseIDFor(spec.ID, lockArtifact(spec.ID, installed.Artifact)), lockArtifact(spec.ID, installed.Artifact)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodeServerInstallerDoesNotOverwriteCurrentTree(t *testing.T) {
	home := t.TempDir()
	archive := filepath.Join(t.TempDir(), "code-server.tar.gz")
	writeTarGzipFixture(t, archive, map[string]tarFixture{
		"code-server-9.9.9-linux-amd64/bin/code-server": {body: "new executable", mode: 0o750},
		"code-server-9.9.9-linux-amd64/lib/node":        {body: "library", mode: 0o640},
	})
	spec := catalogSpec(t, "code-server")
	resolved := Resolved{Artifact: Artifact{Version: "9.9.9", Build: "fixture", SHA256: strings.Repeat("c", 64)}}
	installed, err := NewProvider(spec).Install(context.Background(), InstallContext{
		Home: home, ControlDir: filepath.Join(home, ".pcvm"), Artifact: archive,
	}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if installed.WorkDir == home || !strings.Contains(filepath.ToSlash(installed.Command[0]), "/.pcvm/releases/code-server/") {
		t.Fatalf("code-server remained a live-tree install: %+v", installed)
	}
	if _, err := os.Lstat(filepath.Join(home, ".pcvm", "managed", "code-server", "9.9.9")); !os.IsNotExist(err) {
		t.Fatalf("legacy stable version tree exists: %v", err)
	}
}

type zipFixture struct {
	body string
	mode os.FileMode
}

func writeServiceZipFixture(t *testing.T, path string, entries map[string]zipFixture) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, entry := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		out, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type tarFixture struct {
	body string
	mode int64
}

func writeTarGzipFixture(t *testing.T, path string, entries map[string]tarFixture) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, entry := range entries {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
