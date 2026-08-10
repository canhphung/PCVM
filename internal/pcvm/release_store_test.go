package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseStoreAtomicallyActivatesFileAndTreeProviders(t *testing.T) {
	t.Run("jar", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		spec := ProviderSpec{ID: "paper", Installer: "jar", RollbackMode: "staged"}
		legacy := filepath.Join(control, "managed", spec.ID, "1.21-build-server.jar")
		mustWriteBytes(t, legacy, []byte("verified jar"), 0o640)
		resolved := Resolved{Artifact: Artifact{Version: "1.21", Build: "build", SHA256: strings.Repeat("a", 64)}, WorkDir: home, Command: []string{filepath.Join(control, "cache", "java"), "-jar", legacy}}
		activated, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(10, 0))
		if err != nil {
			t.Fatal(err)
		}
		releaseID := releaseIDFor(spec.ID, lockArtifact(spec.ID, resolved.Artifact))
		want := filepath.Join(releasePayloadRoot(control, spec.ID, releaseID), filepath.Base(legacy))
		if activated.Command[2] != want {
			t.Fatalf("activated command=%v want=%s", activated.Command, want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
			t.Fatalf("legacy file survived activation: %v", err)
		}
		// Reinstalling the exact immutable artifact reuses only byte-identical
		// content and never replaces an already activated release in place.
		mustWriteBytes(t, legacy, []byte("verified jar"), 0o640)
		if _, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(11, 0)); err != nil {
			t.Fatalf("idempotent release activation failed: %v", err)
		}
		mustWriteBytes(t, legacy, []byte("different jar"), 0o640)
		if _, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(12, 0)); err == nil || !strings.Contains(err.Error(), "content differs") {
			t.Fatalf("different content replaced immutable release: %v", err)
		}
		mustWriteBytes(t, want, []byte("tampered"), 0o640)
		if err := validateRelease(releaseRoot(control, spec.ID, releaseID), spec.ID, releaseID, lockArtifact(spec.ID, resolved.Artifact)); err == nil || !strings.Contains(err.Error(), "tree checksum") {
			t.Fatalf("tampered activated release was accepted: %v", err)
		}
	})

	t.Run("tree", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		spec := ProviderSpec{ID: "bedrock", Installer: "zip", RollbackMode: "staged"}
		legacyRoot := filepath.Join(control, "managed", spec.ID, "1.21")
		executable := filepath.Join(legacyRoot, "bedrock_server")
		mustWriteBytes(t, executable, []byte("verified server"), 0o750)
		resolved := Resolved{Artifact: Artifact{Version: "1.21", Build: "release", SHA256: strings.Repeat("b", 64)}, WorkDir: legacyRoot, Command: []string{executable}}
		activated, err := activateStagedRelease(InstallContext{Home: home, ControlDir: control}, spec, resolved, time.Unix(10, 0))
		if err != nil {
			t.Fatal(err)
		}
		releaseID := releaseIDFor(spec.ID, lockArtifact(spec.ID, resolved.Artifact))
		payload := releasePayloadRoot(control, spec.ID, releaseID)
		if activated.WorkDir != payload || activated.Command[0] != filepath.Join(payload, "bedrock_server") {
			t.Fatalf("tree release was not rewritten: %+v", activated)
		}
		if _, err := os.Lstat(legacyRoot); !os.IsNotExist(err) {
			t.Fatalf("legacy tree survived activation: %v", err)
		}
	})
}

func TestReleaseStoreCrashOrphanPruneKeepsCurrentAndPrevious(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	spec := ProviderSpec{ID: "paper", Installer: "jar", RollbackMode: "staged"}
	locks := []ArtifactLock{
		{ID: "paper:old", Version: "1.20", Build: "1", Integrity: ArtifactIntegrity{SHA256: strings.Repeat("1", 64)}},
		{ID: "paper:current", Version: "1.21", Build: "2", Integrity: ArtifactIntegrity{SHA256: strings.Repeat("2", 64)}},
		{ID: "paper:orphan", Version: "1.22", Build: "3", Integrity: ArtifactIntegrity{SHA256: strings.Repeat("3", 64)}},
	}
	for _, lock := range locks[:2] {
		writeReleaseFixture(t, control, spec, lock)
	}
	current := State{Provider: spec.ID, ArtifactLock: locks[1]}
	previous := &State{Provider: spec.ID, ArtifactLock: locks[0]}
	if err := recordReleaseActivation(control, spec, current, previous); err != nil {
		t.Fatal(err)
	}
	writeReleaseFixture(t, control, spec, locks[2])
	stage := filepath.Join(releaseProviderRoot(control, spec.ID), ".stage-crash")
	mustWrite(t, filepath.Join(stage, "partial"))
	// The third release simulates a crash after its directory rename but before
	// validated state activation. Repairing from the old canonical state must
	// prune both that orphan and the partial stage.
	if err := repairReleaseActivation(control, spec, current); err != nil {
		t.Fatal(err)
	}
	currentID, previousID := releaseIDFor(spec.ID, locks[1]), releaseIDFor(spec.ID, locks[0])
	for _, id := range []string{currentID, previousID} {
		if _, err := os.Stat(releaseRoot(control, spec.ID, id)); err != nil {
			t.Fatalf("retained release %s is missing: %v", id, err)
		}
	}
	for _, path := range []string{releaseRoot(control, spec.ID, releaseIDFor(spec.ID, locks[2])), stage} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("crash orphan %s survived prune: %v", path, err)
		}
	}
	var index releaseIndex
	if err := readJSON(filepath.Join(releaseProviderRoot(control, spec.ID), "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	if index.Current != currentID || index.Previous != previousID {
		t.Fatalf("release pointer=%+v", index)
	}
	// A runtime-only activation has the same artifact release ID and must retain
	// the previous artifact rather than pruning it.
	if err := recordReleaseActivation(control, spec, current, &current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(releaseRoot(control, spec.ID, previousID)); err != nil {
		t.Fatalf("runtime-only activation pruned previous artifact: %v", err)
	}
}

func writeReleaseFixture(t *testing.T, control string, spec ProviderSpec, lock ArtifactLock) {
	t.Helper()
	id := releaseIDFor(spec.ID, lock)
	root := releaseRoot(control, spec.ID, id)
	mustWriteBytes(t, filepath.Join(root, "payload", "server.jar"), []byte(lock.ID), 0o640)
	treeDigest, err := releaseTreeDigest(filepath.Join(root, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := releaseMetadata{Schema: releaseMetadataSchema, Provider: spec.ID, ReleaseID: id, Artifact: lock, TreeSHA256: treeDigest, CreatedAt: time.Unix(1, 0).UTC()}
	if err := writeJSONAtomic(filepath.Join(root, ".pcvm-release.json"), metadata); err != nil {
		t.Fatal(err)
	}
}

func mustWriteBytes(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
