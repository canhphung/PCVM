package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperationJournalLifecycleAndBusyGuard(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	now := time.Unix(100, 0).UTC()
	journal, err := BeginOperation(control, "install", "paper", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginOperation(control, "reset", "purpur", now); err == nil || !strings.Contains(err.Error(), "OPERATION_BUSY") {
		t.Fatalf("second operation was not rejected: %v", err)
	}
	var nilJournal *OperationJournal
	if err := nilJournal.Advance(control, "installing", now); err == nil {
		t.Fatal("nil journal advanced")
	}
	if err := journal.Advance(control, "", now); err == nil {
		t.Fatal("empty stage advanced")
	}
	if err := journal.Advance(control, "committing", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOperation(control)
	if err != nil || loaded.Stage != "committing" || !loaded.LastUpdateAt.Equal(now.Add(time.Second)) {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if err := journal.Complete(control); err != nil {
		t.Fatal(err)
	}
	if err := journal.Complete(control); err != nil {
		t.Fatalf("completion is not idempotent: %v", err)
	}
	if err := nilJournal.Complete(control); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadOperation(control); err != nil || loaded != nil {
		t.Fatalf("completed journal still loads: %+v %v", loaded, err)
	}
}

func TestOperationJournalStrictValidation(t *testing.T) {
	valid := OperationJournal{
		Schema: operationJournalSchema, ID: "0123456789abcdef", Kind: "install", Provider: "paper",
		RollbackMode: "staged", Stage: "prepared", StartedAt: time.Unix(1, 0), LastUpdateAt: time.Unix(1, 0),
	}
	for name, mutate := range map[string]func(*OperationJournal){
		"schema":   func(j *OperationJournal) { j.Schema++ },
		"id-empty": func(j *OperationJournal) { j.ID = "" },
		"id-path":  func(j *OperationJournal) { j.ID = "../escape" },
		"kind":     func(j *OperationJournal) { j.Kind = "upgrade" },
		"stage":    func(j *OperationJournal) { j.Stage = "running" },
		"rollback": func(j *OperationJournal) { j.RollbackMode = "magic" },
	} {
		t.Run(name, func(t *testing.T) {
			control := filepath.Join(t.TempDir(), ".pcvm")
			journal := valid
			mutate(&journal)
			if err := writeJSONAtomic(filepath.Join(control, "operation.json"), journal); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOperation(control); err == nil {
				t.Fatal("invalid journal was accepted")
			}
		})
	}
	for _, stage := range []string{"prepared", "quarantine", "installing", "committing", "installed", "tainted"} {
		if !validOperationStage(stage) {
			t.Errorf("valid stage %q rejected", stage)
		}
	}
}

func TestQuarantineRestoreProgressRejectsAmbiguousRecovery(t *testing.T) {
	valid := quarantineRestoreProgress{
		Schema: quarantineRestoreSchema, Phase: "targets-cleared",
		RootEntries: []string{"world"}, ControlEntries: []string{"state.json"},
	}
	for name, mutate := range map[string]func(*quarantineRestoreProgress){
		"schema":            func(p *quarantineRestoreProgress) { p.Schema++ },
		"phase":             func(p *quarantineRestoreProgress) { p.Phase = "moving" },
		"root-path":         func(p *quarantineRestoreProgress) { p.RootEntries = []string{"../world"} },
		"root-duplicate":    func(p *quarantineRestoreProgress) { p.RootEntries = []string{"world", "world"} },
		"control-path":      func(p *quarantineRestoreProgress) { p.ControlEntries = []string{"state\\json"} },
		"control-duplicate": func(p *quarantineRestoreProgress) { p.ControlEntries = []string{"state.json", "state.json"} },
		"completed-root":    func(p *quarantineRestoreProgress) { p.CompletedRoot = []string{"missing"} },
		"completed-control": func(p *quarantineRestoreProgress) { p.CompletedControl = []string{"missing"} },
		"partial-current":   func(p *quarantineRestoreProgress) { p.CurrentArea = "root" },
		"unknown-current": func(p *quarantineRestoreProgress) {
			p.CurrentArea, p.CurrentEntry = "root", "missing"
		},
	} {
		t.Run(name, func(t *testing.T) {
			progress := valid
			mutate(&progress)
			if err := validateRestoreProgress(progress); err == nil {
				t.Fatal("ambiguous restore journal was accepted")
			}
		})
	}
	valid.CurrentArea, valid.CurrentEntry = "control", "state.json"
	if err := validateRestoreProgress(valid); err != nil {
		t.Fatalf("valid in-flight intent rejected: %v", err)
	}
}

func TestRecoverOperationCommittedAndMissingQuarantinePaths(t *testing.T) {
	t.Run("atomic-state-commit", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		journal, err := BeginOperation(control, "install", "paper", time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		journal.InstallID, journal.RollbackMode = "new-install", "staged"
		if err := journal.Advance(control, "committing", time.Unix(2, 0)); err != nil {
			t.Fatal(err)
		}
		if err := RecoverOperation(home, control, &State{InstallID: "new-install"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing-quarantine-is-complete", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		journal, err := BeginOperation(control, "reset", "paper", time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		journal.Quarantine = "quarantine/not-created"
		if err := journal.Advance(control, "installing", time.Unix(2, 0)); err != nil {
			t.Fatal(err)
		}
		if err := RecoverOperation(home, control, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("state-mismatch-fails-closed", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		journal, err := BeginOperation(control, "install", "paper", time.Unix(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		journal.InstallID = "new-install"
		if err := journal.Advance(control, "installed", time.Unix(2, 0)); err != nil {
			t.Fatal(err)
		}
		if err := RecoverOperation(home, control, &State{InstallID: "old-install"}); err == nil {
			t.Fatal("mismatched committed operation was accepted")
		}
	})
}

func TestQuarantinePathAndRestoreCollisionGuards(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	if err := os.MkdirAll(control, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "../escape", filepath.Join(control, "absolute")} {
		if _, err := OpenQuarantine(home, control, path); err == nil {
			t.Fatalf("unsafe quarantine path %q accepted", path)
		}
	}
	q, err := OpenQuarantine(home, control, "quarantine/operation")
	if err != nil || q.Relative() != "quarantine/operation" {
		t.Fatalf("valid quarantine path: q=%+v err=%v", q, err)
	}

	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "world"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "world"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDirectory(source, target); err == nil {
		t.Fatal("restore overwrote an existing target")
	}
}
