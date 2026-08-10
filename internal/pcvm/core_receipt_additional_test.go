package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaperGeyserReceiptSealsCompositeLaunchArtifacts(t *testing.T) {
	home := t.TempDir()
	server := filepath.Join(home, ".pcvm", "managed", "paper-geyser", "server.jar")
	geyser := filepath.Join(home, "plugins", "Geyser-Spigot.jar")
	floodgate := filepath.Join(home, "plugins", "floodgate-spigot.jar")
	for _, path := range []string{server, geyser, floodgate} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	files, root, err := receiptFilesForInstall(home, ProviderSpec{ID: "paper-geyser"}, State{}, []string{"java", "-jar", server}, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || !validHexDigest(root, 64) {
		t.Fatalf("composite receipt files=%+v root=%q", files, root)
	}
	for _, want := range []string{".pcvm/managed/paper-geyser/server.jar", "plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
		found := false
		for _, file := range files {
			found = found || file.Path == want
		}
		if !found {
			t.Errorf("receipt lacks %q: %+v", want, files)
		}
	}
}

func TestSealedReceiptFilePathAndTypeGuards(t *testing.T) {
	home := t.TempDir()
	managed := filepath.Join(home, ".pcvm", "managed", "server")
	if err := os.MkdirAll(filepath.Dir(managed), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("server"), 0o640); err != nil {
		t.Fatal(err)
	}
	var files []ReceiptFile
	if err := appendSealedReceiptFile(home, managed, &files); err != nil {
		t.Fatal(err)
	}
	if err := appendSealedReceiptFile(home, managed, &files); err != nil || len(files) != 1 {
		t.Fatalf("duplicate seal changed receipt: %+v %v", files, err)
	}
	if err := appendSealedReceiptFile(home, filepath.Join(filepath.Dir(home), "outside"), &files); err == nil {
		t.Fatal("outside path was sealed")
	}
	directory := filepath.Join(home, ".pcvm", "managed", "directory")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := appendSealedReceiptFile(home, directory, &files); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("directory seal error=%v", err)
	}
	if _, err := hashRegularFile(directory); err == nil {
		t.Fatal("directory was hashed as a regular file")
	}
}

func TestStagedReceiptSealsReleaseActivationMetadata(t *testing.T) {
	home := t.TempDir()
	spec := ProviderSpec{ID: "paper", Installer: "jar", RollbackMode: "staged"}
	state := State{ArtifactLock: ArtifactLock{ID: "paper:1.21.4:123", Version: "1.21.4", Build: "123"}}
	release := filepath.Join(home, ".pcvm", "releases", spec.ID, releaseIDFor(spec.ID, state.ArtifactLock))
	server := filepath.Join(release, "server.jar")
	metadata := filepath.Join(release, ".pcvm-release.json")
	for _, path := range []string{server, metadata} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	files, root, err := receiptFilesForInstall(home, spec, state, []string{"java", "-jar", server}, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || !validHexDigest(root, 64) {
		t.Fatalf("staged release receipt=%+v root=%q", files, root)
	}
	wantMetadata, _ := filepath.Rel(home, metadata)
	wantMetadata = filepath.ToSlash(wantMetadata)
	found := false
	for _, file := range files {
		found = found || file.Path == wantMetadata
	}
	if !found {
		t.Fatalf("release activation metadata %q is not sealed: %+v", wantMetadata, files)
	}
}

func TestRollbackDefaultsRemainConservative(t *testing.T) {
	for _, test := range []struct {
		spec ProviderSpec
		want string
	}{
		{ProviderSpec{Installer: "jar"}, "staged"},
		{ProviderSpec{Installer: "steamcmd"}, "none"},
		{ProviderSpec{Installer: "web"}, "in-place"},
		{ProviderSpec{Installer: "qemu-vm"}, "in-place"},
		{ProviderSpec{Installer: "jar", RollbackMode: "none"}, "none"},
	} {
		if got := effectiveRollbackMode(test.spec); got != test.want {
			t.Errorf("installer=%q rollback=%q, want %q", test.spec.Installer, got, test.want)
		}
	}
}
