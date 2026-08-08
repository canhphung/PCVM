package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateLegacyControlAndRepairStatePaths(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyControlName)
	current := filepath.Join(home, ".pcvm")
	state := State{
		Provider:          "vanilla",
		Command:           []string{filepath.Join(legacy, "cache", "runtimes", "java"), "-jar", filepath.Join(legacy, "managed", "server.jar")},
		RuntimeExecutable: filepath.Join(legacy, "cache", "runtimes", "java"),
		WorkingDirectory:  filepath.Join(legacy, "managed", "vanilla"),
		Environment:       []string{"PATH=" + filepath.Join(legacy, "bin")},
		Metadata:          map[string]string{"path": filepath.Join(legacy, "metadata")},
	}
	if err := SaveState(legacy, state); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(legacy, "cache", "artifact.jar")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("cached"), 0o640); err != nil {
		t.Fatal(err)
	}
	migrated, err := migrateLegacyControl(home, current)
	if err != nil {
		t.Fatal(err)
	}
	if !migrated {
		t.Fatal("legacy control directory was not migrated")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy directory still exists: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(current, "cache", "artifact.jar")); err != nil || string(data) != "cached" {
		t.Fatalf("cache was not preserved: %q %v", data, err)
	}
	repaired, err := LoadState(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range append(append([]string{repaired.RuntimeExecutable, repaired.WorkingDirectory}, repaired.Command...), repaired.Environment...) {
		if strings.Contains(value, legacy) {
			t.Fatalf("legacy path remains in state: %q", value)
		}
	}
}

func TestMigrateLegacyControlReplacesEmptyInstallDirectory(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyControlName)
	current := filepath.Join(home, ".pcvm")
	if err := os.MkdirAll(filepath.Join(legacy, "cache"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(current, 0o750); err != nil {
		t.Fatal(err)
	}
	if migrated, err := migrateLegacyControl(home, current); err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(current, "cache")); err != nil {
		t.Fatal("legacy cache was not moved into the empty installation directory")
	}
}

func TestMigrateLegacyControlRejectsConflictsAndSymlinks(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		home := t.TempDir()
		legacy := filepath.Join(home, legacyControlName)
		current := filepath.Join(home, ".pcvm")
		if err := os.MkdirAll(legacy, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(current, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "state.json"), []byte("{}"), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := migrateLegacyControl(home, current); err == nil {
			t.Fatal("conflicting control directories were accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		home := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(home, legacyControlName)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := migrateLegacyControl(home, filepath.Join(home, ".pcvm")); err == nil {
			t.Fatal("legacy symlink was accepted")
		}
	})
}
