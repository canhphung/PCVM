package pcvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupConsumedInstallCache(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	for _, file := range []string{
		filepath.Join(control, "cache", "artifacts", "server.zip"),
		filepath.Join(control, "cache", "sources", "node-deadbeef", "index.js"),
		filepath.Join(control, "cache", "downloads", "java.tar.gz"),
		filepath.Join(control, "cache", "runtimes", "java-21-amd64", "bin", "java"),
	} {
		mustWrite(t, file)
	}
	if err := cleanupConsumedInstallCache(control); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"artifacts", "sources"} {
		entries, err := os.ReadDir(filepath.Join(control, "cache", removed))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s cache was not emptied: entries=%v err=%v", removed, entries, err)
		}
	}
	for _, retained := range []string{
		filepath.Join(control, "cache", "downloads", "java.tar.gz"),
		filepath.Join(control, "cache", "runtimes", "java-21-amd64", "bin", "java"),
	} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatalf("required runtime cache removed: %v", err)
		}
	}
}

func TestCleanupConsumedInstallCacheDoesNotFollowCategorySymlink(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"))
	if err := os.MkdirAll(filepath.Join(control, "cache"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(control, "cache", "artifacts")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := cleanupConsumedInstallCache(control); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("cleanup followed category symlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("category symlink was retained: %v", err)
	}
}

func TestCleanupConsumedInstallCacheRejectsCacheRootSymlink(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "artifacts", "keep"))
	if err := os.MkdirAll(control, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(control, "cache")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := cleanupConsumedInstallCache(control); err == nil {
		t.Fatal("accepted symlinked cache root")
	}
	if _, err := os.Stat(filepath.Join(outside, "artifacts", "keep")); err != nil {
		t.Fatalf("cleanup followed cache root symlink: %v", err)
	}
}
