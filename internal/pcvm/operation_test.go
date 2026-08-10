package pcvm

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInterruptedQuarantineRestoresOriginalInstallation(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	mustWrite(t, filepath.Join(home, "world", "level.dat"))
	mustWrite(t, filepath.Join(control, "state.json"))
	journal, err := BeginOperation(control, "reset", "node-bot", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.PreviousID = "old-install"
	journal.RollbackMode = "staged"
	if err := journal.Advance(control, "quarantine", time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := beginQuarantineAt(home, control, journal.ID, home); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "new-install", "partial"))
	if err := journal.Advance(control, "installing", time.Unix(102, 0)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverOperation(home, control, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "world", "level.dat")); err != nil {
		t.Fatal("original data was not restored")
	}
	if _, err := os.Stat(filepath.Join(home, "new-install")); !os.IsNotExist(err) {
		t.Fatal("partial target survived rollback")
	}
	if operation, err := LoadOperation(control); err != nil || operation != nil {
		t.Fatalf("journal was not completed: %+v %v", operation, err)
	}
}

func TestCrashHalfwayThroughQuarantineMoveDoesNotDeleteUnmovedEntries(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	mustWrite(t, filepath.Join(home, "already-moved", "data"))
	mustWrite(t, filepath.Join(home, "not-yet-moved", "data"))
	journal, err := BeginOperation(control, "reset", "paper", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	if err := journal.Advance(control, "quarantine", time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(control, "quarantine", journal.ID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(control, "quarantine", journal.ID, "control"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, "already-moved"), filepath.Join(root, "already-moved")); err != nil {
		t.Fatal(err)
	}
	if err := RecoverOperation(home, control, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"already-moved", "not-yet-moved"} {
		if _, err := os.Stat(filepath.Join(home, name, "data")); err != nil {
			t.Fatalf("%s was lost during partial quarantine recovery: %v", name, err)
		}
	}
}

func TestInterruptedFullRestoreResumesWithoutClearingRestoredData(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	mustWrite(t, filepath.Join(home, "world", "level.dat"))
	mustWrite(t, filepath.Join(home, "server.properties"))
	mustWrite(t, filepath.Join(control, "state.json"))
	journal, err := BeginOperation(control, "reset", "paper", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.RollbackMode = "staged"
	if err := journal.Advance(control, "quarantine", time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	q, err := beginQuarantineAt(home, control, journal.ID, home)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "partial-install", "new"))
	if err := journal.Advance(control, "installing", time.Unix(102, 0)); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the first restore rename, but before its completion
	// is appended to the durable restore journal.
	progress, err := q.loadOrPrepareRestore()
	if err != nil {
		t.Fatal(err)
	}
	if err := clearDirectoryExcept(home, map[string]bool{".pcvm": true}); err != nil {
		t.Fatal(err)
	}
	progress.Phase = "root-cleared"
	if err := q.saveRestoreProgress(progress); err != nil {
		t.Fatal(err)
	}
	if err := clearDirectoryExcept(control, map[string]bool{"cache": true, "quarantine": true, "operation.json": true, "lock": true}); err != nil {
		t.Fatal(err)
	}
	progress.Phase = "targets-cleared"
	if err := q.saveRestoreProgress(progress); err != nil {
		t.Fatal(err)
	}
	if len(progress.RootEntries) == 0 {
		t.Fatal("quarantine unexpectedly has no root entries")
	}
	name := progress.RootEntries[0]
	progress.CurrentArea, progress.CurrentEntry = "root", name
	if err := q.saveRestoreProgress(progress); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(q.Root, "root", name), filepath.Join(home, name)); err != nil {
		t.Fatal(err)
	}

	if err := RecoverOperation(home, control, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, "world", "level.dat"), filepath.Join(home, "server.properties"), filepath.Join(control, "state.json")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("restored data %s was lost across retry: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, "partial-install")); !os.IsNotExist(err) {
		t.Fatalf("partial install survived restore: %v", err)
	}
}

func TestQuarantineCommittingStageNeverPurgesBeforeValidation(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	mustWrite(t, filepath.Join(home, "old-world", "level.dat"))
	mustWrite(t, filepath.Join(control, "state.json"))
	journal, err := BeginOperation(control, "reset", "paper", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.RollbackMode = "staged"
	journal.InstallID = "candidate"
	if err := journal.Advance(control, "quarantine", time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := beginQuarantineAt(home, control, journal.ID, home); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "candidate", "server.jar"))
	mustWrite(t, filepath.Join(control, "state.json"))
	if err := journal.Advance(control, "committing", time.Unix(102, 0)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverOperation(home, control, &State{InstallID: "candidate"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "old-world", "level.dat")); err != nil {
		t.Fatalf("unvalidated candidate purged old data: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "candidate")); !os.IsNotExist(err) {
		t.Fatalf("unvalidated candidate survived rollback: %v", err)
	}
}

func TestQuarantineValidatedStageCommitsOnlyWithMatchingState(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	mustWrite(t, filepath.Join(home, "old-world", "level.dat"))
	mustWrite(t, filepath.Join(control, "state.json"))
	journal, err := BeginOperation(control, "reset", "paper", time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.RollbackMode = "staged"
	journal.InstallID = "candidate"
	if err := journal.Advance(control, "quarantine", time.Unix(101, 0)); err != nil {
		t.Fatal(err)
	}
	q, err := beginQuarantineAt(home, control, journal.ID, home)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(home, "candidate", "server.jar"))
	mustWrite(t, filepath.Join(control, "state.json"))
	if err := journal.Advance(control, "validated", time.Unix(102, 0)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverOperation(home, control, &State{InstallID: "candidate"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "candidate", "server.jar")); err != nil {
		t.Fatalf("validated candidate was not committed: %v", err)
	}
	if _, err := os.Lstat(q.Root); !os.IsNotExist(err) {
		t.Fatalf("validated quarantine was not purged: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "old-world")); !os.IsNotExist(err) {
		t.Fatalf("old data survived validated commit: %v", err)
	}
}

func TestActivateValidatedStateDoesNotCommitBeforeReleaseBookkeepingSucceeds(t *testing.T) {
	control := t.TempDir()
	spec := ProviderSpec{
		ID: "paper", Family: "paper", Installer: "jar", Runtime: "java",
		InstallFormat: 3, RollbackMode: "staged",
	}
	resolved := Resolved{
		Artifact:    Artifact{Version: "1.21.4", Build: "100", SHA256: strings.Repeat("a", 64)},
		RuntimeKind: "java", RuntimeVersion: "21",
	}
	state := newStateFromInstall(spec, Request{Version: "1.21.4", Build: "100", RuntimeVersion: "21"}, resolved, "amd64", time.Unix(10, 0).UTC())
	journal, err := BeginOperation(control, "install", spec.ID, time.Unix(10, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	journal.RollbackMode = "staged"
	journal.InstallID = state.InstallID
	app := &App{Config: Config{Control: control}, Now: func() time.Time { return time.Unix(11, 0).UTC() }}
	committed, err := app.activateValidatedState(spec, state, journal, nil)
	if err == nil || committed {
		t.Fatalf("committed=%v err=%v, want pre-commit release-index error", committed, err)
	}
	loaded, loadErr := LoadState(control)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded != nil {
		t.Fatalf("state was committed before release bookkeeping: %#v", loaded)
	}
}

func TestInterruptedInPlaceUpdateFailsClosed(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	journal, err := BeginOperation(control, "install", "rust", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	journal.RollbackMode = "none"
	if err := journal.Advance(control, "installing", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := RecoverOperation(filepath.Dir(control), control, nil); err == nil || !strings.Contains(err.Error(), "rollback mode") {
		t.Fatalf("interrupted in-place update did not fail closed: %v", err)
	}
}

func TestFailedLiveTreeInstallerPersistsTaintAndNeverRunsPreviousState(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	app := &App{Config: Config{Home: home, Control: control}, Log: NewLogger(io.Discard), Now: func() time.Time { return time.Unix(100, 0) }}
	spec := ProviderSpec{ID: "modrinth-modpack", RollbackMode: "in-place"}
	provider := failingLiveTreeProvider{spec: spec, path: filepath.Join(home, "partially-mutated")}
	err := app.installTransaction(context.Background(), &State{InstallID: "old-install"}, spec, provider, Request{}, Resolved{}, InstallContext{Home: home, ControlDir: control})
	if err == nil || !strings.Contains(err.Error(), "PCVM-E4005") {
		t.Fatalf("live-tree failure did not fail closed: %v", err)
	}
	journal, err := LoadOperation(control)
	if err != nil || journal == nil || journal.Stage != "tainted" {
		t.Fatalf("taint journal was not retained: %+v %v", journal, err)
	}
	if err := RecoverOperation(home, control, &State{InstallID: "old-install"}); err == nil || !strings.Contains(err.Error(), "tainted") {
		t.Fatalf("next boot did not remain blocked: %v", err)
	}
}

type failingLiveTreeProvider struct {
	spec ProviderSpec
	path string
}

func (p failingLiveTreeProvider) Spec() ProviderSpec { return p.spec }
func (p failingLiveTreeProvider) Resolve(context.Context, Request, *HTTPClient) (Resolved, error) {
	return Resolved{}, errors.New("not used")
}
func (p failingLiveTreeProvider) Install(_ context.Context, _ InstallContext, resolved Resolved) (Resolved, error) {
	if err := os.WriteFile(p.path, []byte("partial"), 0o600); err != nil {
		return resolved, err
	}
	return resolved, errors.New("installer failed after mutation")
}
func (p failingLiveTreeProvider) BuildProcess(context.Context, Config, LaunchState, MemoryPlan) (ProcessSpec, error) {
	return ProcessSpec{}, errors.New("not used")
}
func (p failingLiveTreeProvider) CompareVersions(a, b string) int { return strings.Compare(a, b) }

func TestProcessLockRejectsConcurrentLauncher(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".pcvm")
	if err := os.MkdirAll(control, 0o750); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireProcessLock(control)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if second, err := AcquireProcessLock(control); err == nil {
		_ = second.Release()
		t.Fatal("concurrent launcher acquired the same process lock")
	}
}
