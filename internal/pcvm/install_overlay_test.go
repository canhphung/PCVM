package pcvm

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallOverlayRollbackRestoresEveryTouchedFile(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	provider := "modrinth-modpack"
	mustWriteBytes(t, filepath.Join(home, "mods", "managed.jar"), []byte("old"), 0o640)
	mustWriteBytes(t, filepath.Join(home, "mods", "stale.jar"), []byte("stale"), 0o600)
	mustWriteBytes(t, filepath.Join(home, "mods", "user.jar"), []byte("user"), 0o640)
	newRoot := installOverlayNewRoot(control, provider)
	mustWriteBytes(t, filepath.Join(newRoot, "mods", "managed.jar"), []byte("new"), 0o640)
	mustWriteBytes(t, filepath.Join(newRoot, "mods", "added.jar"), []byte("added"), 0o640)

	previous := map[string]string{"mods/managed.jar": strings.Repeat("a", 128), "mods/stale.jar": strings.Repeat("b", 128)}
	installID := strings.Repeat("c", 32)
	if err := applyInstallOverlay(home, control, provider, installID, previous); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(home, "mods", "managed.jar"), "new")
	assertFileBody(t, filepath.Join(home, "mods", "added.jar"), "added")
	assertFileBody(t, filepath.Join(home, "mods", "user.jar"), "user")
	if _, err := os.Lstat(filepath.Join(home, "mods", "stale.jar")); !os.IsNotExist(err) {
		t.Fatalf("stale managed file survived commit: %v", err)
	}

	if err := rollbackPendingInstallOverlay(home, control, provider); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(home, "mods", "managed.jar"), "old")
	assertFileBody(t, filepath.Join(home, "mods", "stale.jar"), "stale")
	assertFileBody(t, filepath.Join(home, "mods", "user.jar"), "user")
	if _, err := os.Lstat(filepath.Join(home, "mods", "added.jar")); !os.IsNotExist(err) {
		t.Fatalf("new managed file survived rollback: %v", err)
	}
}

func TestInstallOverlayRejectsUserOwnedCollisionBeforeWriting(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	provider := "modrinth-modpack"
	mustWriteBytes(t, filepath.Join(home, "mods", "managed.jar"), []byte("old"), 0o640)
	mustWriteBytes(t, filepath.Join(home, "config", "user.yml"), []byte("keep"), 0o640)
	newRoot := installOverlayNewRoot(control, provider)
	mustWriteBytes(t, filepath.Join(newRoot, "mods", "managed.jar"), []byte("new"), 0o640)
	mustWriteBytes(t, filepath.Join(newRoot, "config", "user.yml"), []byte("replace"), 0o640)
	err := applyInstallOverlay(home, control, provider, strings.Repeat("d", 32), map[string]string{"mods/managed.jar": strings.Repeat("a", 128)})
	if err == nil || !strings.Contains(err.Error(), "user-owned") {
		t.Fatalf("user-owned collision was accepted: %v", err)
	}
	assertFileBody(t, filepath.Join(home, "mods", "managed.jar"), "old")
	assertFileBody(t, filepath.Join(home, "config", "user.yml"), "keep")
}

func TestInstallOverlayCopyFailureRestoresAlreadyTouchedFiles(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	provider := "modrinth-modpack"
	for _, name := range []string{"a.jar", "b.jar"} {
		mustWriteBytes(t, filepath.Join(home, "mods", name), []byte("old-"+name), 0o640)
		mustWriteBytes(t, filepath.Join(installOverlayNewRoot(control, provider), "mods", name), []byte("new-"+name), 0o640)
	}
	writes := 0
	failingWriter := func(root *os.Root, relative string, source io.Reader, mode os.FileMode, size int64) error {
		writes++
		if writes == 2 {
			return errors.New("injected copy failure")
		}
		return writeArchiveRegularAt(root, relative, source, mode, size)
	}
	previous := map[string]string{"mods/a.jar": strings.Repeat("a", 128), "mods/b.jar": strings.Repeat("b", 128)}
	err := applyInstallOverlayWithWriter(home, control, provider, strings.Repeat("f", 32), previous, failingWriter)
	if err == nil || !strings.Contains(err.Error(), "injected copy failure") {
		t.Fatalf("copy failure was not returned: %v", err)
	}
	assertFileBody(t, filepath.Join(home, "mods", "a.jar"), "old-a.jar")
	assertFileBody(t, filepath.Join(home, "mods", "b.jar"), "old-b.jar")
	if _, err := os.Lstat(installOverlayRoot(control, provider)); !os.IsNotExist(err) {
		t.Fatalf("failed transaction survived rollback: %v", err)
	}
}

func TestInstallOverlayCrashRecoveryUsesCanonicalInstallID(t *testing.T) {
	for _, test := range []struct {
		name      string
		canonical bool
		want      string
	}{
		{name: "old-state-restores", want: "old"},
		{name: "candidate-state-commits", canonical: true, want: "new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			control := filepath.Join(home, ".pcvm")
			provider := "modrinth-modpack"
			installID := strings.Repeat("e", 32)
			mustWriteBytes(t, filepath.Join(home, "mods", "a.jar"), []byte("old"), 0o640)
			mustWriteBytes(t, filepath.Join(installOverlayNewRoot(control, provider), "mods", "a.jar"), []byte("new"), 0o640)
			if err := applyInstallOverlay(home, control, provider, installID, map[string]string{"mods/a.jar": strings.Repeat("a", 128)}); err != nil {
				t.Fatal(err)
			}
			state := &State{Provider: provider, InstallID: "old"}
			if test.canonical {
				state.InstallID = installID
			}
			if err := recoverPendingInstallOverlays(home, control, state); err != nil {
				t.Fatal(err)
			}
			assertFileBody(t, filepath.Join(home, "mods", "a.jar"), test.want)
			if _, err := os.Lstat(installOverlayRoot(control, provider)); !os.IsNotExist(err) {
				t.Fatalf("transaction survived recovery: %v", err)
			}
		})
	}
}

func TestGenericGitResolvedUsesReleaseStoreWithoutChangingUploadMode(t *testing.T) {
	spec := ProviderSpec{ID: "bun-app", Installer: "generic-app", RollbackMode: "in-place"}
	git := Resolved{RollbackMode: "staged", Artifact: Artifact{Metadata: map[string]string{"source_commit": strings.Repeat("a", 40)}}}
	if effectiveRollbackModeForResolved(spec, git) != "staged" || !releaseStoreSupportsResolved(spec, git) {
		t.Fatal("immutable Git source did not select staged release semantics")
	}
	upload := Resolved{Artifact: Artifact{Metadata: map[string]string{}}}
	if effectiveRollbackModeForResolved(spec, upload) != "in-place" || releaseStoreSupportsResolved(spec, upload) {
		t.Fatal("user-mutable upload source falsely claimed staged rollback")
	}
}

func TestGenericGitReleaseActivatesOnlyAfterStagedTreeIsComplete(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	spec := ProviderSpec{ID: "deno-app", Installer: "generic-app", RollbackMode: "in-place"}
	commit := strings.Repeat("a", 40)
	artifact := Artifact{Version: "git", Build: commit[:12], Metadata: map[string]string{"source_commit": commit}}
	source := filepath.Join(control, "managed", spec.ID, ".source-fixture")
	mustWriteBytes(t, filepath.Join(source, "main.ts"), []byte("console.log('ready')\n"), 0o640)
	mustWriteBytes(t, filepath.Join(home, "app", "user.txt"), []byte("untouched"), 0o640)
	resolved := Resolved{Artifact: artifact, RollbackMode: "staged", WorkDir: source, Command: []string{"/runtime/deno", "run", "main.ts"}}
	activated, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	releaseID := releaseIDFor(spec.ID, lockArtifact(spec.ID, artifact))
	wantRoot := releasePayloadRoot(control, spec.ID, releaseID)
	if activated.WorkDir != wantRoot {
		t.Fatalf("workdir=%q want immutable payload %q", activated.WorkDir, wantRoot)
	}
	assertFileBody(t, filepath.Join(wantRoot, "main.ts"), "console.log('ready')\n")
	assertFileBody(t, filepath.Join(home, "app", "user.txt"), "untouched")
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatalf("mutable staging source survived activation: %v", err)
	}
}

func TestInstallTransactionReceiptFailureRollsBackAppliedOverlay(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	providerID := "fixture-overlay"
	mustWriteBytes(t, filepath.Join(home, "managed.txt"), []byte("old"), 0o640)
	mustWriteBytes(t, filepath.Join(installOverlayNewRoot(control, providerID), "managed.txt"), []byte("new"), 0o640)
	provider := overlayReceiptFailureProvider{spec: ProviderSpec{ID: providerID, RollbackMode: "staged"}, home: home, control: control}
	app := &App{Config: Config{Home: home, Control: control, Arch: "amd64"}, Log: NewLogger(io.Discard), Now: func() time.Time { return time.Unix(10, 0) }}
	err := app.installTransaction(context.Background(), nil, provider.spec, provider, Request{}, Resolved{}, InstallContext{Home: home, ControlDir: control})
	if err == nil || !strings.Contains(err.Error(), "seal installed release") {
		t.Fatalf("receipt failure was not returned: %v", err)
	}
	assertFileBody(t, filepath.Join(home, "managed.txt"), "old")
}

type overlayReceiptFailureProvider struct {
	spec          ProviderSpec
	home, control string
}

func (p overlayReceiptFailureProvider) Spec() ProviderSpec { return p.spec }
func (p overlayReceiptFailureProvider) Resolve(context.Context, Request, *HTTPClient) (Resolved, error) {
	return Resolved{}, nil
}
func (p overlayReceiptFailureProvider) Install(context.Context, InstallContext, Resolved) (Resolved, error) {
	if err := applyInstallOverlay(p.home, p.control, p.spec.ID, strings.Repeat("1", 32), map[string]string{"managed.txt": strings.Repeat("a", 128)}); err != nil {
		return Resolved{}, err
	}
	return Resolved{Artifact: Artifact{Version: "1", Build: "1"}, RuntimeKind: "native", WorkDir: p.home, Command: []string{filepath.Join(p.control, "missing-executable")}}, nil
}
func (p overlayReceiptFailureProvider) BuildProcess(context.Context, Config, LaunchState, MemoryPlan) (ProcessSpec, error) {
	return ProcessSpec{}, nil
}
func (p overlayReceiptFailureProvider) CompareVersions(a, b string) int { return CompareVersions(a, b) }

func assertFileBody(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s=%q want=%q", path, data, want)
	}
}
