package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallReceiptDetectsManagedExecutableTamper(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	jar := filepath.Join(control, "managed", "paper", "1.21-release-server.jar")
	if err := os.MkdirAll(filepath.Dir(jar), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jar, []byte("trusted"), 0o640); err != nil {
		t.Fatal(err)
	}
	spec := ProviderSpec{ID: "paper", InstallFormat: 3, RollbackMode: "staged"}
	resolved := Resolved{Artifact: Artifact{Version: "1.21", Build: "release", SHA256: strings.Repeat("a", 64)}, RuntimeKind: "java", RuntimeVersion: "21", Command: []string{"/usr/bin/java", "-jar", jar}}
	state := newStateFromInstall(spec, Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, resolved, "amd64", time.Unix(100, 0))
	receipt, err := buildInstallReceipt(home, spec, state, resolved, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	name, err := SaveInstallReceipt(control, receipt)
	if err != nil {
		t.Fatal(err)
	}
	state.Receipt = name
	loaded, err := LoadInstallReceipt(control, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallReceipt(home, state, loaded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jar, []byte("replacement"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallReceipt(home, state, loaded); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered release was accepted: %v", err)
	}
}

func TestReceiptDoesNotSealUserUploadedSource(t *testing.T) {
	home := t.TempDir()
	entry := filepath.Join(home, "index.js")
	if err := os.WriteFile(entry, []byte("console.log('user source')"), 0o640); err != nil {
		t.Fatal(err)
	}
	files, _, err := receiptFiles(home, []string{entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == "index.js" {
			t.Fatal("user-editable source was sealed into receipt")
		}
	}
}

func TestLaunchReceiptRejectsOmittedOrRediscoveredExecutable(t *testing.T) {
	home := t.TempDir()
	trusted := filepath.Join(home, "game", "trusted", "Server.dll")
	other := filepath.Join(home, "game", "zzz", "Server.dll")
	for _, path := range []string{trusted, other} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	files, root, err := receiptFiles(home, []string{"dotnet", trusted})
	if err != nil {
		t.Fatal(err)
	}
	receipt := InstallReceipt{Files: files, RootSHA256: root}
	if err := verifyLaunchReceiptCompleteness(home, receipt, LaunchState{Command: []string{"dotnet", trusted}}); err != nil {
		t.Fatal(err)
	}
	receipt.Files = nil
	receipt.RootSHA256 = receiptRoot(nil)
	if err := verifyLaunchReceiptCompleteness(home, receipt, LaunchState{Command: []string{"dotnet", trusted}}); err == nil {
		t.Fatal("receipt with omitted managed executable was accepted")
	}
	receipt.Files = files
	receipt.RootSHA256 = root
	if err := verifyLaunchReceiptCompleteness(home, receipt, LaunchState{Command: []string{"dotnet", other}}); err == nil {
		t.Fatal("restart-selected unreceipted executable was accepted")
	}
}

func TestModrinthReceiptSealsInternalMetadataAndRelativeArgumentFiles(t *testing.T) {
	home := t.TempDir()
	managed := filepath.Join(home, ".pcvm", "managed", "modrinth-modpack")
	install := modrinthInstallReceipt{Schema: 2, ProjectID: "project", VersionID: "version", Minecraft: "1.21.8", Loader: "forge", LoaderVersion: "61.0.1", Managed: map[string]string{}}
	if err := writeJSONAtomic(filepath.Join(managed, "install.json"), install); err != nil {
		t.Fatal(err)
	}
	argument := filepath.Join(home, "libraries", "net", "minecraftforge", "forge", "1.21.8-61.0.1", "unix_args.txt")
	for path, body := range map[string]string{filepath.Join(home, "user_jvm_args.txt"): "-Xmx1G\n", argument: "-cp trusted.jar\n"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	spec := ProviderSpec{ID: "modrinth-modpack", Installer: "modrinth", InstallFormat: 3, RollbackMode: "staged"}
	resolved := Resolved{Artifact: Artifact{Version: "1.0", Build: "build", SHA512: strings.Repeat("a", 128), Metadata: map[string]string{"modrinth_project_id": "project", "modrinth_loader": "forge"}}, RuntimeKind: "java", RuntimeVersion: "21", WorkDir: home,
		Command: []string{"/usr/bin/java", "@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/1.21.8-61.0.1/unix_args.txt", "nogui"}}
	state := newStateFromInstall(spec, Request{}, resolved, "amd64", time.Unix(100, 0))
	receipt, err := buildInstallReceipt(home, spec, state, resolved, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range receipt.Files {
		paths[file.Path] = true
	}
	for _, want := range []string{".pcvm/managed/modrinth-modpack/install.json", "user_jvm_args.txt", "libraries/net/minecraftforge/forge/1.21.8-61.0.1/unix_args.txt"} {
		if !paths[want] {
			t.Fatalf("Modrinth receipt omitted %q: %+v", want, receipt.Files)
		}
	}
	if err := os.WriteFile(filepath.Join(managed, "install.json"), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallReceipt(home, state, receipt); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered Modrinth metadata was accepted: %v", err)
	}
}
