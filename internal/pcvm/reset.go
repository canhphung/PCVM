package pcvm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func NewPending(from State, target ProviderSpec, targetVersion, reason string, now time.Time) (PendingSwitch, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return PendingSwitch{}, err
	}
	return PendingSwitch{Schema: StateSchema, FromProvider: from.Provider, ToProvider: target.ID,
		FromVersion: from.ResolvedVersion, ToVersion: targetVersion, Nonce: hex.EncodeToString(buf),
		ExpiresAt: now.Add(30 * time.Minute), Reason: reason}, nil
}

func ValidateReset(p PendingSwitch, confirm string, now time.Time) error {
	if now.After(p.ExpiresAt) {
		return fmt.Errorf("reset nonce expired")
	}
	if confirm != "DELETE:"+p.Nonce {
		return fmt.Errorf("reset confirmation does not match")
	}
	return nil
}

func GuardedReset(home string) error { return guardedReset(home, "/home/container") }

func guardedReset(home, expected string) error {
	clean, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	clean = filepath.Clean(clean)
	expected, err = filepath.Abs(expected)
	if err != nil {
		return err
	}
	expected = filepath.Clean(expected)
	if clean != expected {
		return fmt.Errorf("refusing reset outside canonical root %q", expected)
	}
	if clean == string(filepath.Separator) || filepath.Dir(clean) == clean || len(clean) < 8 {
		return fmt.Errorf("refusing unsafe reset root %q", clean)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("reset root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != clean {
		return fmt.Errorf("reset root resolves outside itself")
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return err
	}
	control := filepath.Join(clean, ".pcvm")
	if info, statErr := os.Lstat(control); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("control directory may not be a symlink")
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	for _, entry := range entries {
		if entry.Name() == ".pcvm" {
			if err := resetControl(filepath.Join(clean, entry.Name())); err != nil {
				return err
			}
			continue
		}
		target := filepath.Join(clean, entry.Name())
		if !strings.HasPrefix(target, clean+string(filepath.Separator)) {
			return fmt.Errorf("unsafe child path")
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

func resetControl(control string) error {
	info, err := os.Lstat(control)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("control directory may not be a symlink")
	}
	entries, err := os.ReadDir(control)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "cache" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(control, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
