package multiegg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNonceExpiryAndConfirmation(t *testing.T) {
	now := time.Unix(1000, 0)
	p, err := NewPending(State{Provider: "paper", ResolvedVersion: "1.21"}, ProviderSpec{ID: "node-bot"}, "latest", "family", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReset(p, "DELETE:"+p.Nonce, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReset(p, "DELETE:wrong", now); err == nil {
		t.Fatal("accepted wrong nonce")
	}
	if err := ValidateReset(p, "DELETE:"+p.Nonce, now.Add(31*time.Minute)); err == nil {
		t.Fatal("accepted expired nonce")
	}
}

func TestGuardedResetPreservesOnlyCacheAndDoesNotFollowSymlink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(home, "world", "level.dat"))
	mustWrite(t, filepath.Join(home, ".multiegg", "state.json"))
	mustWrite(t, filepath.Join(home, ".multiegg", "cache", "runtimes", "java", "bin", "java"))
	mustWrite(t, filepath.Join(outside, "keep.txt"))
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := guardedReset(home, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "world")); !os.IsNotExist(err) {
		t.Fatal("world not removed")
	}
	if _, err := os.Stat(filepath.Join(home, ".multiegg", "state.json")); !os.IsNotExist(err) {
		t.Fatal("state not removed")
	}
	if _, err := os.Stat(filepath.Join(home, ".multiegg", "cache", "runtimes", "java", "bin", "java")); err != nil {
		t.Fatal("runtime cache removed")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("followed symlink")
	}
}

func TestGuardedResetRejectsControlSymlink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep.txt"))
	if err := os.Symlink(outside, filepath.Join(home, ".multiegg")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := guardedReset(home, home); err == nil {
		t.Fatal("accepted symlink control directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("touched symlink target")
	}
}
func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
