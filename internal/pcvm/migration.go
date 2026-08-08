package pcvm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const legacyControlName = ".multiegg"

func migrateLegacyControl(home, control string) (bool, error) {
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return false, err
	}
	homeAbs = filepath.Clean(homeAbs)
	controlAbs, err := filepath.Abs(control)
	if err != nil {
		return false, err
	}
	controlAbs = filepath.Clean(controlAbs)
	wantControl := filepath.Join(homeAbs, ".pcvm")
	if controlAbs != wantControl {
		return false, nil
	}
	legacy := filepath.Join(homeAbs, legacyControlName)
	legacyInfo, legacyErr := os.Lstat(legacy)
	if legacyErr != nil && !os.IsNotExist(legacyErr) {
		return false, legacyErr
	}
	currentInfo, currentErr := os.Lstat(controlAbs)
	if currentErr != nil && !os.IsNotExist(currentErr) {
		return false, currentErr
	}
	if currentErr == nil && (currentInfo.Mode()&os.ModeSymlink != 0 || !currentInfo.IsDir()) {
		return false, fmt.Errorf(".pcvm must be a real directory")
	}
	migrated := false
	if legacyErr == nil {
		if legacyInfo.Mode()&os.ModeSymlink != 0 || !legacyInfo.IsDir() {
			return false, fmt.Errorf("legacy control path must be a real directory")
		}
		if currentErr == nil {
			entries, err := os.ReadDir(controlAbs)
			if err != nil {
				return false, err
			}
			if len(entries) != 0 {
				return false, fmt.Errorf("both %s and .pcvm contain data; refusing to choose one", legacyControlName)
			}
			if err := os.Remove(controlAbs); err != nil {
				return false, err
			}
		}
		if err := os.Rename(legacy, controlAbs); err != nil {
			return false, err
		}
		migrated = true
	}
	repaired, err := repairLegacyStatePaths(controlAbs, legacy, controlAbs)
	if err != nil {
		return false, err
	}
	return migrated || repaired, nil
}

func repairLegacyStatePaths(control, legacyRoot, currentRoot string) (bool, error) {
	state, err := LoadState(control)
	if err != nil {
		return false, err
	}
	if state == nil {
		return false, nil
	}
	changed := replaceString(&state.RuntimeExecutable, legacyRoot, currentRoot)
	changed = replaceString(&state.WorkingDirectory, legacyRoot, currentRoot) || changed
	for i := range state.Command {
		changed = replaceString(&state.Command[i], legacyRoot, currentRoot) || changed
	}
	for i := range state.Environment {
		changed = replaceString(&state.Environment[i], legacyRoot, currentRoot) || changed
	}
	for key, value := range state.Metadata {
		replaced := strings.ReplaceAll(value, legacyRoot, currentRoot)
		if replaced != value {
			state.Metadata[key] = replaced
			changed = true
		}
	}
	for key, value := range state.Artifact.Metadata {
		replaced := strings.ReplaceAll(value, legacyRoot, currentRoot)
		if replaced != value {
			state.Artifact.Metadata[key] = replaced
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := SaveState(control, *state); err != nil {
		return false, err
	}
	return true, nil
}

func replaceString(value *string, old, replacement string) bool {
	updated := strings.ReplaceAll(*value, old, replacement)
	if updated == *value {
		return false
	}
	*value = updated
	return true
}
