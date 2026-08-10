package pcvm

import (
	"crypto/rand"
	"crypto/sha256"
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
	return PendingSwitch{Schema: PendingSwitchSchema, FromProvider: from.Provider, ToProvider: target.ID,
		FromVersion: from.ResolvedVersion, ToVersion: targetVersion, Nonce: hex.EncodeToString(buf),
		ExpiresAt: now.Add(30 * time.Minute), Reason: reason,
		SourceHash: resetSourceFingerprint(from), TargetHash: resetTargetFingerprint(target.ID, targetVersion)}, nil
}

func ValidateReset(p PendingSwitch, confirm string, now time.Time) error {
	if p.Schema != PendingSwitchSchema || p.SourceHash == "" || p.TargetHash == "" {
		return fmt.Errorf("reset authorization is invalid")
	}
	if now.After(p.ExpiresAt) {
		return fmt.Errorf("reset nonce expired")
	}
	if confirm != "DELETE:"+p.Nonce {
		return fmt.Errorf("reset confirmation does not match")
	}
	return nil
}

func ValidateResetBinding(p PendingSwitch, confirm, sourceHash, targetHash string, now time.Time) error {
	if err := ValidateReset(p, confirm, now); err != nil {
		return err
	}
	if !constantTimeHexEqual(p.SourceHash, sourceHash) || !constantTimeHexEqual(p.TargetHash, targetHash) {
		return fmt.Errorf("reset authorization does not match the current installation and exact target")
	}
	return nil
}

func resetSourceFingerprint(state State) string {
	seed := strings.Join([]string{state.Provider, state.InstallID, state.ResolvedVersion, state.ResolvedBuild,
		state.ArtifactLock.ID, state.ArtifactLock.Revision, state.ImmutableConfigHash}, "\x00")
	return sha256Hex(seed)
}

func resetTargetFingerprint(provider, version string) string {
	return sha256Hex(provider + "\x00" + version)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func constantTimeHexEqual(left, right string) bool {
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	var different byte
	for i := range left {
		different |= left[i] ^ right[i]
	}
	return different == 0
}

func validateResetRoot(home, expected string) (string, error) {
	clean, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	clean = filepath.Clean(clean)
	expected, err = filepath.Abs(expected)
	if err != nil {
		return "", err
	}
	expected = filepath.Clean(expected)
	if clean != expected {
		return "", fmt.Errorf("refusing reset outside canonical root %q", expected)
	}
	if clean == string(filepath.Separator) || filepath.Dir(clean) == clean || len(clean) < 8 {
		return "", fmt.Errorf("refusing unsafe reset root %q", clean)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("reset root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != clean {
		return "", fmt.Errorf("reset root resolves outside itself")
	}
	return clean, nil
}
