package pcvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDotnetPublishCandidateReplacesWholeOutputTree(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "publish")
	mustWriteBytes(t, filepath.Join(old, "App.dll"), []byte("old"), 0o640)
	mustWriteBytes(t, filepath.Join(old, "Stale.Dependency.dll"), []byte("stale"), 0o640)
	candidate, err := os.MkdirTemp(root, ".publish-candidate-")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteBytes(t, filepath.Join(candidate, "App.dll"), []byte("new"), 0o640)
	mustWriteBytes(t, filepath.Join(candidate, "App.runtimeconfig.json"), []byte("{}"), 0o640)
	if _, err := findDotnetLaunchDLL(candidate); err != nil {
		t.Fatalf("candidate should be validated before activation: %v", err)
	}
	if err := activateDotnetPublishCandidate(root, candidate); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(root, "publish", "App.dll"), "new")
	if _, err := os.Lstat(filepath.Join(root, "publish", "Stale.Dependency.dll")); !os.IsNotExist(err) {
		t.Fatalf("stale output survived candidate activation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "publish.previous")); !os.IsNotExist(err) {
		t.Fatalf("previous publish was not pruned: %v", err)
	}
}

func TestDotnetPublishActivationRecoversInterruptedSwap(t *testing.T) {
	root := t.TempDir()
	mustWriteBytes(t, filepath.Join(root, "publish.previous", "App.dll"), []byte("recover"), 0o640)
	if err := recoverDotnetPublishActivation(root); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, filepath.Join(root, "publish", "App.dll"), "recover")
	if _, err := os.Lstat(filepath.Join(root, "publish.previous")); !os.IsNotExist(err) {
		t.Fatalf("recovered previous directory still exists: %v", err)
	}
}

func TestDotnetPublishActivationRejectsSymlinkCandidate(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	candidate := filepath.Join(root, ".publish-candidate-link")
	if err := os.Symlink(target, candidate); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := activateDotnetPublishCandidate(root, candidate); err == nil {
		t.Fatal("symlink publish candidate was accepted")
	}
}
