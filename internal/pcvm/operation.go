package pcvm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const operationJournalSchema = 1
const quarantineRestoreSchema = 1

type OperationJournal struct {
	Schema       int       `json:"schema"`
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Provider     string    `json:"provider"`
	InstallID    string    `json:"install_id,omitempty"`
	PreviousID   string    `json:"previous_install_id,omitempty"`
	RollbackMode string    `json:"rollback_mode,omitempty"`
	Stage        string    `json:"stage"`
	Quarantine   string    `json:"quarantine,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	LastUpdateAt time.Time `json:"last_update_at"`
}

func BeginOperation(control, kind, provider string, now time.Time) (*OperationJournal, error) {
	if current, err := LoadOperation(control); err != nil {
		return nil, err
	} else if current != nil {
		return nil, fmt.Errorf("PCVM-E4001 OPERATION_BUSY: unfinished %s operation %s is in stage %s", current.Kind, current.ID, current.Stage)
	}
	id, err := randomOperationID()
	if err != nil {
		return nil, err
	}
	journal := &OperationJournal{Schema: operationJournalSchema, ID: id, Kind: kind, Provider: provider, Stage: "prepared", StartedAt: now, LastUpdateAt: now}
	if err := saveOperation(control, *journal); err != nil {
		return nil, err
	}
	return journal, nil
}

func LoadOperation(control string) (*OperationJournal, error) {
	var journal OperationJournal
	err := readJSON(filepath.Join(control, "operation.json"), &journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if journal.Schema != operationJournalSchema || journal.ID == "" || strings.ContainsAny(journal.ID, `/\\`) ||
		(journal.Kind != "install" && journal.Kind != "reset" && journal.Kind != "legacy-reset") || !validOperationStage(journal.Stage) ||
		(journal.RollbackMode != "" && journal.RollbackMode != "staged" && journal.RollbackMode != "in-place" && journal.RollbackMode != "none") {
		return nil, fmt.Errorf("operation journal is invalid")
	}
	return &journal, nil
}

func validOperationStage(stage string) bool {
	switch stage {
	case "prepared", "quarantine", "installing", "validating", "validated", "committing", "installed", "tainted":
		return true
	default:
		return false
	}
}

func (j *OperationJournal) Advance(control, stage string, now time.Time) error {
	if j == nil || stage == "" {
		return fmt.Errorf("operation stage is invalid")
	}
	j.Stage = stage
	j.LastUpdateAt = now
	return saveOperation(control, *j)
}

func (j *OperationJournal) Complete(control string) error {
	if j == nil {
		return nil
	}
	path := filepath.Join(control, "operation.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func saveOperation(control string, journal OperationJournal) error {
	journal.Schema = operationJournalSchema
	return writeJSONAtomic(filepath.Join(control, "operation.json"), journal)
}

func randomOperationID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

type Quarantine struct {
	Home    string
	Control string
	Root    string
}

func BeginQuarantine(home, control, operationID string) (*Quarantine, error) {
	return beginQuarantineAt(home, control, operationID, "/home/container")
}

func beginQuarantineAt(home, control, operationID, expectedRoot string) (*Quarantine, error) {
	if operationID == "" || strings.ContainsAny(operationID, `/\`) {
		return nil, fmt.Errorf("quarantine operation ID is invalid")
	}
	clean, err := validateResetRoot(home, expectedRoot)
	if err != nil {
		return nil, err
	}
	home = clean
	if filepath.Clean(control) != filepath.Join(home, ".pcvm") {
		return nil, fmt.Errorf("quarantine control path must be inside the canonical server root")
	}
	root := filepath.Join(control, "quarantine", operationID)
	if err := secureMkdirAll(control, filepath.Join(root, "root"), 0o700); err != nil {
		return nil, err
	}
	if err := secureMkdirAll(control, filepath.Join(root, "control"), 0o700); err != nil {
		return nil, err
	}
	q := &Quarantine{Home: filepath.Clean(home), Control: filepath.Clean(control), Root: root}
	if err := q.moveIntoQuarantine(); err != nil {
		_ = q.RestorePartial()
		return nil, err
	}
	return q, nil
}

func OpenQuarantine(home, control, relative string) (*Quarantine, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "..") {
		return nil, fmt.Errorf("quarantine path is invalid")
	}
	root := filepath.Join(control, filepath.FromSlash(relative))
	if !pathWithin(control, root) {
		return nil, fmt.Errorf("quarantine path escapes control directory")
	}
	return &Quarantine{Home: filepath.Clean(home), Control: filepath.Clean(control), Root: root}, nil
}

func (q *Quarantine) Relative() string {
	rel, _ := filepath.Rel(q.Control, q.Root)
	return filepath.ToSlash(rel)
}

func (q *Quarantine) moveIntoQuarantine() error {
	entries, err := os.ReadDir(q.Home)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".pcvm" {
			continue
		}
		if err := os.Rename(filepath.Join(q.Home, entry.Name()), filepath.Join(q.Root, "root", entry.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(q.Control)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "cache", "quarantine", "operation.json", "lock":
			continue
		}
		if err := os.Rename(filepath.Join(q.Control, entry.Name()), filepath.Join(q.Root, "control", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (q *Quarantine) Restore() error {
	progress, err := q.loadOrPrepareRestore()
	if err != nil {
		return err
	}
	// Clearing and restoring are deliberately separate, journaled phases. If a
	// container dies after one quarantined entry has been renamed back, recovery
	// must not clear that already-restored entry on its next attempt.
	if progress.Phase == "prepared" {
		if err := clearDirectoryExcept(q.Home, map[string]bool{".pcvm": true}); err != nil {
			return err
		}
		progress.Phase = "root-cleared"
		if err := q.saveRestoreProgress(progress); err != nil {
			return err
		}
	}
	if progress.Phase == "root-cleared" {
		if err := clearDirectoryExcept(q.Control, map[string]bool{"cache": true, "quarantine": true, "operation.json": true, "lock": true}); err != nil {
			return err
		}
		progress.Phase = "targets-cleared"
		if err := q.saveRestoreProgress(progress); err != nil {
			return err
		}
	}
	if err := q.restoreArea(&progress, "root", q.Home); err != nil {
		return err
	}
	if err := q.restoreArea(&progress, "control", q.Control); err != nil {
		return err
	}
	progress.Phase = "restored"
	if err := q.saveRestoreProgress(progress); err != nil {
		return err
	}
	return os.RemoveAll(q.Root)
}

type quarantineRestoreProgress struct {
	Schema           int      `json:"schema"`
	Phase            string   `json:"phase"`
	RootEntries      []string `json:"root_entries"`
	ControlEntries   []string `json:"control_entries"`
	CompletedRoot    []string `json:"completed_root,omitempty"`
	CompletedControl []string `json:"completed_control,omitempty"`
	CurrentArea      string   `json:"current_area,omitempty"`
	CurrentEntry     string   `json:"current_entry,omitempty"`
}

func (q *Quarantine) restoreProgressPath() string {
	return filepath.Join(q.Root, "restore.json")
}

func (q *Quarantine) loadOrPrepareRestore() (quarantineRestoreProgress, error) {
	var progress quarantineRestoreProgress
	err := readJSON(q.restoreProgressPath(), &progress)
	if err == nil {
		if validateErr := validateRestoreProgress(progress); validateErr != nil {
			return quarantineRestoreProgress{}, validateErr
		}
		return progress, nil
	}
	if !os.IsNotExist(err) {
		return quarantineRestoreProgress{}, err
	}
	rootEntries, err := quarantineEntryNames(filepath.Join(q.Root, "root"))
	if err != nil {
		return quarantineRestoreProgress{}, err
	}
	controlEntries, err := quarantineEntryNames(filepath.Join(q.Root, "control"))
	if err != nil {
		return quarantineRestoreProgress{}, err
	}
	progress = quarantineRestoreProgress{Schema: quarantineRestoreSchema, Phase: "prepared", RootEntries: rootEntries, ControlEntries: controlEntries}
	if err := q.saveRestoreProgress(progress); err != nil {
		return quarantineRestoreProgress{}, err
	}
	return progress, nil
}

func (q *Quarantine) saveRestoreProgress(progress quarantineRestoreProgress) error {
	progress.Schema = quarantineRestoreSchema
	if err := validateRestoreProgress(progress); err != nil {
		return err
	}
	return writeJSONAtomic(q.restoreProgressPath(), progress)
}

func validateRestoreProgress(progress quarantineRestoreProgress) error {
	if progress.Schema != quarantineRestoreSchema {
		return fmt.Errorf("quarantine restore journal has unsupported schema")
	}
	switch progress.Phase {
	case "prepared", "root-cleared", "targets-cleared", "restored":
	default:
		return fmt.Errorf("quarantine restore journal has invalid phase %q", progress.Phase)
	}
	root := make(map[string]bool, len(progress.RootEntries))
	control := make(map[string]bool, len(progress.ControlEntries))
	for _, item := range progress.RootEntries {
		if !validQuarantineEntryName(item) || root[item] {
			return fmt.Errorf("quarantine restore journal has invalid root entry")
		}
		root[item] = true
	}
	for _, item := range progress.ControlEntries {
		if !validQuarantineEntryName(item) || control[item] {
			return fmt.Errorf("quarantine restore journal has invalid control entry")
		}
		control[item] = true
	}
	for _, item := range progress.CompletedRoot {
		if !root[item] {
			return fmt.Errorf("quarantine restore journal completed an unknown root entry")
		}
	}
	for _, item := range progress.CompletedControl {
		if !control[item] {
			return fmt.Errorf("quarantine restore journal completed an unknown control entry")
		}
	}
	if progress.CurrentArea == "" && progress.CurrentEntry == "" {
		return nil
	}
	if progress.CurrentArea == "root" && root[progress.CurrentEntry] || progress.CurrentArea == "control" && control[progress.CurrentEntry] {
		return nil
	}
	return fmt.Errorf("quarantine restore journal has invalid current entry")
}

func quarantineEntryNames(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !validQuarantineEntryName(entry.Name()) {
			return nil, fmt.Errorf("invalid quarantine entry %q", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func validQuarantineEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func (q *Quarantine) restoreArea(progress *quarantineRestoreProgress, area, target string) error {
	if progress.Phase != "targets-cleared" && progress.Phase != "restored" {
		return fmt.Errorf("quarantine restore targets are not cleared")
	}
	var entries, completed []string
	if area == "root" {
		entries, completed = progress.RootEntries, progress.CompletedRoot
	} else if area == "control" {
		entries, completed = progress.ControlEntries, progress.CompletedControl
	} else {
		return fmt.Errorf("invalid quarantine restore area")
	}
	done := make(map[string]bool, len(completed))
	for _, name := range completed {
		done[name] = true
	}
	for _, name := range entries {
		if done[name] {
			continue
		}
		progress.CurrentArea, progress.CurrentEntry = area, name
		if err := q.saveRestoreProgress(*progress); err != nil {
			return err
		}
		source := filepath.Join(q.Root, area, name)
		destination := filepath.Join(target, name)
		sourceExists, err := lstatExists(source)
		if err != nil {
			return err
		}
		targetExists, err := lstatExists(destination)
		if err != nil {
			return err
		}
		switch {
		case sourceExists && !targetExists:
			if err := os.Rename(source, destination); err != nil {
				return err
			}
		case !sourceExists && targetExists:
			// The rename completed but the container stopped before its completion
			// record was made durable. The persisted current-entry intent makes
			// this state unambiguous and safe to resume.
		case sourceExists && targetExists:
			return fmt.Errorf("cannot restore quarantine entry %q because target exists", name)
		default:
			return fmt.Errorf("quarantine entry %q disappeared during restore", name)
		}
		if area == "root" {
			progress.CompletedRoot = append(progress.CompletedRoot, name)
		} else {
			progress.CompletedControl = append(progress.CompletedControl, name)
		}
		progress.CurrentArea, progress.CurrentEntry = "", ""
		if err := q.saveRestoreProgress(*progress); err != nil {
			return err
		}
		done[name] = true
	}
	return nil
}

func lstatExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// RestorePartial is used only while the quarantine move itself is incomplete.
// It merges already-moved entries back without deleting entries which were
// never moved. This makes a crash between two top-level renames recoverable.
func (q *Quarantine) RestorePartial() error {
	if err := restoreDirectory(filepath.Join(q.Root, "root"), q.Home); err != nil {
		return err
	}
	if err := restoreDirectory(filepath.Join(q.Root, "control"), q.Control); err != nil {
		return err
	}
	return os.RemoveAll(q.Root)
}

func (q *Quarantine) Commit() error { return os.RemoveAll(q.Root) }

func clearDirectoryExcept(root string, keep map[string]bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if keep[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func restoreDirectory(source, target string) error {
	entries, err := os.ReadDir(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		to := filepath.Join(target, entry.Name())
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("cannot restore quarantine entry %q because target exists", entry.Name())
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(filepath.Join(source, entry.Name()), to); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func RecoverOperation(home, control string, state *State) error {
	journal, err := LoadOperation(control)
	if err != nil || journal == nil {
		return err
	}
	if journal.Quarantine != "" {
		q, err := OpenQuarantine(home, control, journal.Quarantine)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(q.Root); os.IsNotExist(err) {
			return journal.Complete(control)
		} else if err != nil {
			return err
		}
		if (journal.Stage == "validated" || journal.Stage == "installed") && state != nil && state.InstallID == journal.InstallID {
			if err := q.Commit(); err != nil {
				return err
			}
		} else if journal.Stage == "quarantine" {
			if err := q.RestorePartial(); err != nil {
				return fmt.Errorf("restore interrupted quarantine move %s: %w", journal.ID, err)
			}
		} else if err := q.Restore(); err != nil {
			return fmt.Errorf("restore interrupted reset %s: %w", journal.ID, err)
		}
	} else if journal.Stage == "tainted" {
		return fmt.Errorf("interrupted %s update is tainted (operation %s, rollback mode %q); validate the live installation or reset before launch", journal.Provider, journal.ID, journal.RollbackMode)
	} else if journal.Stage == "validated" && state != nil && state.InstallID == journal.InstallID {
		// Validation completed before atomic state activation. A matching state
		// proves that activation also completed; the release index is repaired by
		// runState before launch.
	} else if (journal.Stage == "installing" || journal.Stage == "validating" || journal.Stage == "committing" || journal.Stage == "validated") && journal.RollbackMode != "" && journal.RollbackMode != "staged" {
		return fmt.Errorf("interrupted %s update has rollback mode %q; validate or reinstall before launch", journal.Provider, journal.RollbackMode)
	} else if journal.Stage == "installed" && (state == nil || state.InstallID != journal.InstallID) {
		return fmt.Errorf("completed operation %s does not match canonical state; refusing recovery", journal.ID)
	}
	return journal.Complete(control)
}
