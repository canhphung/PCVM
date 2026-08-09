package pcvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func LoadState(control string) (*State, error) {
	var state State
	err := readJSON(filepath.Join(control, "state.json"), &state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if state.Schema == 1 || state.Schema == 2 {
		state.Schema = StateSchema
		if err := writeJSONAtomic(filepath.Join(control, "state.json"), state); err != nil {
			return nil, fmt.Errorf("migrate state schema: %w", err)
		}
	} else if state.Schema != StateSchema {
		return nil, fmt.Errorf("unsupported state schema %d", state.Schema)
	}
	return &state, nil
}

func SaveState(control string, state State) error {
	state.Schema = StateSchema
	return writeJSONAtomic(filepath.Join(control, "state.json"), state)
}

func loadPending(control string) (*PendingSwitch, error) {
	var pending PendingSwitch
	err := readJSON(filepath.Join(control, "pending-switch.json"), &pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return &pending, err
}

func savePending(control string, pending PendingSwitch) error {
	return writeJSONAtomic(filepath.Join(control, "pending-switch.json"), pending)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
