package pcvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type App struct {
	Config      Config
	Catalog     Catalog
	HTTP        *HTTPClient
	Log         *Logger
	In          io.Reader
	Out         io.Writer
	Err         io.Writer
	Now         func() time.Time
	MenuTimeout time.Duration
	ResetRoot   string
	Supervisor  Supervisor
}

func NewApp(cfg Config, catalog Catalog, in io.Reader, out, errOut io.Writer) *App {
	cfg.Dependencies = cfg.Dependencies.withDefaults()
	log := NewLogger(out)
	httpClient := NewHTTPClient()
	httpClient.Log = log
	return &App{Config: cfg, Catalog: catalog, HTTP: httpClient, Log: log, In: in, Out: out, Err: errOut, Now: time.Now, MenuTimeout: defaultMenuSelectionTimeout, ResetRoot: "/home/container", Supervisor: ProcessSupervisor{Log: log}}
}

func (a *App) Run(ctx context.Context) error {
	req := a.Config.Request
	if err := DetectLegacyControl(a.Config.Home, a.Config.Control); err != nil {
		var legacy *LegacyStateError
		if errors.As(err, &legacy) {
			return a.runLegacyResetFlow(ctx, legacy, req)
		}
		return err
	}
	state, err := LoadState(a.Config.Control)
	if err != nil {
		var legacy *LegacyStateError
		if errors.As(err, &legacy) {
			return a.runLegacyResetFlow(ctx, legacy, req)
		}
		return err
	}
	if err := os.MkdirAll(a.Config.Control, 0o750); err != nil {
		return err
	}
	lock, err := AcquireProcessLock(a.Config.Control)
	if err != nil {
		return err
	}
	defer lock.Release()
	// Re-read after locking so two concurrently started containers cannot both
	// reconcile against a stale installation index.
	state, err = LoadState(a.Config.Control)
	if err != nil {
		return err
	}
	if err := recoverPendingInstallOverlays(a.Config.Home, a.Config.Control, state); err != nil {
		return fmt.Errorf("PCVM-E4002 OVERLAY_RECOVERY: %w", err)
	}
	if state != nil {
		installedSpec, ok := a.Catalog.Provider(state.Provider)
		if !ok {
			return fmt.Errorf("PCVM-E2007 STATE_PROVIDER_UNKNOWN: state references provider %q which is not compiled into this release", state.Provider)
		}
		state.Family = installedSpec.Family
	}
	taintedResetFlow := false
	operation, err := LoadOperation(a.Config.Control)
	if err != nil {
		return fmt.Errorf("PCVM-E4002 OPERATION_RECOVERY: %w", err)
	}
	if operation != nil && operation.Stage == "tainted" && req.ResetConfirm != "" {
		if !a.Config.Policy.AllowUserReset {
			return fmt.Errorf("PCVM-E4002 OPERATION_RECOVERY: operation %s is tainted and resets are disabled by host policy", operation.ID)
		}
		taintedResetFlow = true
		a.Log.Printf("WARNING: operation %s is tainted; only the confirmed quarantine reset flow is allowed", operation.ID)
	} else if err := RecoverOperation(a.Config.Home, a.Config.Control, state); err != nil {
		return fmt.Errorf("PCVM-E4002 OPERATION_RECOVERY: %w", err)
	}
	// Recovery may restore or activate a different canonical state file. Never
	// continue reconciliation with the pre-recovery pointer.
	state, err = LoadState(a.Config.Control)
	if err != nil {
		return err
	}
	if state != nil {
		installedSpec, ok := a.Catalog.Provider(state.Provider)
		if !ok {
			return fmt.Errorf("PCVM-E2007 STATE_PROVIDER_UNKNOWN: recovered state references provider %q which is not compiled into this release", state.Provider)
		}
		state.Family = installedSpec.Family
	}
	if req.Software == "interactive" {
		if state != nil {
			req.Software = state.Provider
		} else {
			chosen, err := a.menuContext(ctx)
			if err != nil {
				if errors.Is(err, errMenuSelectionTimeout) {
					a.Log.Printf("No software was selected within %s; shutting down cleanly", formatMenuTimeout(a.menuSelectionTimeout()))
					return nil
				}
				return err
			}
			req.Software = chosen
		}
	}
	spec, err := a.validateTarget(req)
	if err != nil {
		return err
	}
	a.Config.Request = req
	provider := NewProvider(spec)
	plan := Reconcile(state, spec, req, nil)
	if taintedResetFlow {
		plan = ActionPlan{Kind: ActionUpdate, RequiresResolve: true, Reason: "tainted live-tree update requires reset"}
	}
	if plan.Kind == ActionRun {
		return a.runState(ctx, *state)
	}
	resolved, err := provider.Resolve(ctx, req, a.HTTP)
	if err != nil {
		if state != nil && !taintedResetFlow {
			a.Log.Printf("WARNING: update resolution failed; starting installed release: %v", err)
			return a.runState(ctx, *state)
		}
		return fmt.Errorf("PCVM-E3001 RESOLVE_FAILED: resolve %s: %w", spec.ID, err)
	}
	diskPreflighted := false
	if spec.Installer == "modrinth" {
		// A Modrinth API version response does not carry the exact loader
		// dependency version. Verify and inspect the immutable .mrpack before
		// transition planning, otherwise project/loader resets and Java runtime
		// selection would be based on an "unknown" artifact identity.
		if err := a.preflightDisk(ctx, spec, resolved, state == nil); err != nil {
			return err
		}
		diskPreflighted = true
		resolved, err = a.prepareModrinthIdentity(ctx, spec, req, resolved)
		if err != nil {
			if state != nil && !taintedResetFlow {
				a.Log.Printf("WARNING: Modrinth identity preparation failed; starting installed release: %v", err)
				return a.runState(ctx, *state)
			}
			return fmt.Errorf("PCVM-E3002 PREPARE_FAILED: %w", err)
		}
	}
	plan = Reconcile(state, spec, req, &resolved)
	if taintedResetFlow {
		plan = ActionPlan{Kind: ActionReset, Reason: "tainted live-tree update requires quarantine reset"}
	}
	if plan.Kind == ActionReset && !a.Config.Policy.AllowUserReset {
		return fmt.Errorf("%s; resets are disabled by host policy", plan.Reason)
	}
	if plan.Kind != ActionReset && state != nil && installationMatches(*state, spec, req, resolved, a.Config.Arch) {
		state.Selector = Selector{Version: req.Version, Build: req.Build, Runtime: normalizeRuntimeSelector(req.RuntimeVersion)}
		state.UpdateRequestHash = hashToken(req.UpdateRequest)
		state.ImmutableConfigHash = immutableConfigFingerprint(spec, req)
		state.UpdatedAt = a.Now()
		if err := SaveState(a.Config.Control, *state); err != nil {
			return err
		}
		return a.runState(ctx, *state)
	}
	if !diskPreflighted {
		if err := a.preflightDisk(ctx, spec, resolved, state == nil); err != nil {
			return err
		}
	}
	prepared, installContext, err := a.prepare(ctx, provider, req, resolved)
	if err != nil {
		if state != nil && plan.Kind != ActionReset {
			a.Log.Printf("WARNING: update preparation failed; starting installed release: %v", err)
			return a.runState(ctx, *state)
		}
		return fmt.Errorf("PCVM-E3002 PREPARE_FAILED: %w", err)
	}
	if plan.Kind == ActionReset {
		return a.installWithReset(ctx, state, spec, provider, req, prepared, installContext, plan.Reason)
	}
	return a.installTransaction(ctx, state, spec, provider, req, prepared, installContext)
}

func (a *App) validateTarget(req Request) (ProviderSpec, error) {
	spec, ok := a.Catalog.Provider(strings.ToLower(strings.TrimSpace(req.Software)))
	if !ok {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1001 UNKNOWN_SOFTWARE: unknown SOFTWARE %q", req.Software)
	}
	if !a.Config.Policy.AllowedSoftware[spec.ID] {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1002 POLICY_DENIED: provider %q is disabled by host policy", spec.ID)
	}
	if !contains(spec.Architectures, a.Config.Arch) {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1003 ARCH_UNSUPPORTED: provider %q has no %s artifact", spec.ID, a.Config.Arch)
	}
	if !a.Catalog.providerRuntimeAvailable(spec, a.Config.Arch) {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1003 RUNTIME_UNAVAILABLE: provider %q has no allowed %s runtime pack for %s", spec.ID, spec.Runtime, a.Config.Arch)
	}
	if err := ValidateProviderImageProfile(a.Config.ImageProfile, spec); err != nil {
		return ProviderSpec{}, err
	}
	cfg := a.Config
	cfg.Request = req
	if err := ValidateProviderRequest(spec, cfg); err != nil {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1004 INVALID_CONFIG: %w", err)
	}
	if _, err := planMemory(spec.Memory, req, a.Config.Policy, readMemorySnapshotWith(a.Config.Dependencies)); err != nil {
		return ProviderSpec{}, fmt.Errorf("PCVM-E1005 MEMORY_CONFIG: memory plan for %s: %w", spec.ID, err)
	}
	if spec.Resolver == "local-app" && req.SourceMode == "git" {
		if err := a.Config.ValidateGitURL(req.GitURL); err != nil {
			return ProviderSpec{}, err
		}
	}
	return spec, nil
}

func installationMatches(state State, spec ProviderSpec, req Request, resolved Resolved, arch string) bool {
	return state.Provider == spec.ID && state.Architecture == arch && state.ArtifactLock == lockArtifact(spec.ID, resolved.Artifact) &&
		state.RuntimePackID == runtimePackIdentity(resolved.RuntimeKind, resolved.RuntimeVersion, arch) &&
		state.ImmutableConfigHash == immutableConfigFingerprint(spec, req)
}

type providerUpdater interface {
	Update(context.Context, InstallContext, Resolved) (Resolved, error)
}

func installOrUpdateProvider(ctx context.Context, provider Provider, ic InstallContext, prepared Resolved, update bool) (Resolved, error) {
	if update {
		updater, ok := provider.(providerUpdater)
		if !ok {
			return prepared, fmt.Errorf("compiled provider %s has no updater driver", provider.Spec().ID)
		}
		return updater.Update(ctx, ic, prepared)
	}
	return provider.Install(ctx, ic, prepared)
}

func (a *App) installTransaction(ctx context.Context, previous *State, spec ProviderSpec, provider Provider, req Request, prepared Resolved, ic InstallContext) error {
	journal, err := BeginOperation(a.Config.Control, "install", spec.ID, a.Now())
	if err != nil {
		return err
	}
	rollbackMode := effectiveRollbackModeForResolved(spec, prepared)
	journal.RollbackMode = rollbackMode
	a.logInstallPhase(spec.ID, journal.ID, "start", rollbackMode)
	if previous != nil {
		journal.PreviousID = previous.InstallID
	}
	if err := journal.Advance(a.Config.Control, "installing", a.Now()); err != nil {
		return err
	}
	installed, err := installOrUpdateProvider(ctx, provider, ic, prepared, previous != nil)
	if err != nil {
		_ = rollbackPendingInstallOverlay(a.Config.Home, a.Config.Control, spec.ID)
		if previous != nil && rollbackMode == "staged" {
			_ = journal.Complete(a.Config.Control)
			a.Log.Printf("WARNING: staged update failed; starting installed release: %v", err)
			return a.runState(ctx, *previous)
		}
		if previous != nil {
			// A live-tree installer may have changed files before reporting its
			// error. Keep a durable taint journal so a subsequent boot cannot
			// silently launch the previous state over that partial installation.
			if advanceErr := journal.Advance(a.Config.Control, "tainted", a.Now()); advanceErr != nil {
				return fmt.Errorf("PCVM-E4005 IN_PLACE_UPDATE_FAILED: update %s failed and its taint journal could not be persisted (%v); refusing to start: %w", spec.ID, advanceErr, err)
			}
			return fmt.Errorf("PCVM-E4005 IN_PLACE_UPDATE_FAILED: update %s failed; operation %s is tainted and the previous release will not be started: %w", spec.ID, journal.ID, err)
		}
		_ = journal.Complete(a.Config.Control)
		return fmt.Errorf("PCVM-E4003 INSTALL_FAILED: install %s: %w", spec.ID, err)
	}
	a.logInstallPhase(spec.ID, journal.ID, "installed", rollbackMode)
	state, err := a.sealInstalled(spec, req, installed, journal)
	if err != nil {
		rollbackErr := rollbackPendingInstallOverlay(a.Config.Home, a.Config.Control, spec.ID)
		if rollbackErr != nil {
			return fmt.Errorf("%v; additionally failed to restore staged overlay: %w", err, rollbackErr)
		}
		_ = journal.Complete(a.Config.Control)
		if previous != nil && rollbackMode == "staged" {
			a.Log.Printf("WARNING: staged release sealing failed; starting installed release: %v", err)
			return a.runState(ctx, *previous)
		}
		return err
	}
	a.logInstallPhase(spec.ID, journal.ID, "sealed", rollbackMode)
	if err := a.validateCommittedInstall(ctx, spec, state); err != nil {
		validationErr := fmt.Errorf("PCVM-E4004 INSTALL_VALIDATION_FAILED: %w", err)
		if rollbackMode == "staged" {
			if rollbackErr := rollbackPendingInstallOverlay(a.Config.Home, a.Config.Control, spec.ID); rollbackErr != nil {
				return fmt.Errorf("%v; additionally failed to restore staged overlay: %w", validationErr, rollbackErr)
			}
			_ = journal.Complete(a.Config.Control)
			if previous != nil {
				a.Log.Printf("WARNING: staged release validation failed; starting installed release: %v", err)
				return a.runState(ctx, *previous)
			}
			return validationErr
		}
		if previous != nil {
			if advanceErr := journal.Advance(a.Config.Control, "tainted", a.Now()); advanceErr != nil {
				return fmt.Errorf("%v; additionally failed to persist taint: %w", validationErr, advanceErr)
			}
			return validationErr
		}
		_ = journal.Complete(a.Config.Control)
		return validationErr
	}
	a.logInstallPhase(spec.ID, journal.ID, "validated", rollbackMode)
	committed, err := a.activateValidatedState(spec, state, journal, previous)
	if err != nil {
		if committed {
			return fmt.Errorf("PCVM-E4008 ACTIVATION_BOOKKEEPING: state %s was committed; restart PCVM to finish recovery without rolling it back: %w", state.InstallID, err)
		}
		canonical, loadErr := LoadState(a.Config.Control)
		if loadErr != nil {
			return fmt.Errorf("%v; additionally could not inspect state for overlay recovery: %w", err, loadErr)
		}
		if recoveryErr := recoverPendingInstallOverlays(a.Config.Home, a.Config.Control, canonical); recoveryErr != nil {
			return fmt.Errorf("%v; additionally failed staged overlay recovery: %w", err, recoveryErr)
		}
		return err
	}
	a.logInstallPhase(spec.ID, journal.ID, "activated", rollbackMode)
	if err := commitPendingInstallOverlay(a.Config.Control, spec.ID, state.InstallID); err != nil {
		return fmt.Errorf("commit staged install overlay: %w", err)
	}
	if err := journal.Complete(a.Config.Control); err != nil {
		return err
	}
	if err := cleanupConsumedInstallCache(a.Config.Control); err != nil {
		a.Log.Printf("WARNING: installed successfully but could not remove consumed cache: %v", err)
	}
	a.logInstallPhase(spec.ID, journal.ID, "complete", rollbackMode)
	return a.runState(ctx, state)
}

func (a *App) logInstallPhase(provider, operation, phase, rollbackMode string) {
	if a.Log == nil {
		return
	}
	a.Log.Printf("INSTALL phase=%s provider=%s operation=%s rollback=%s", phase, provider, operation, rollbackMode)
}

func (a *App) sealInstalled(spec ProviderSpec, req Request, installed Resolved, journal *OperationJournal) (State, error) {
	now := a.Now()
	state := newStateFromInstall(spec, req, installed, a.Config.Arch, now)
	receipt, err := buildInstallReceipt(a.Config.Home, spec, state, installed, now)
	if err != nil {
		return State{}, fmt.Errorf("seal installed release: %w", err)
	}
	receiptName, err := SaveInstallReceipt(a.Config.Control, receipt)
	if err != nil {
		return State{}, err
	}
	state.Receipt = receiptName
	journal.InstallID = state.InstallID
	if err := journal.Advance(a.Config.Control, "validating", now); err != nil {
		return State{}, err
	}
	return state, nil
}

func (a *App) activateValidatedState(spec ProviderSpec, state State, journal *OperationJournal, previous *State) (bool, error) {
	now := a.Now()
	// The durable validated stage is written before state activation. Recovery
	// only commits a quarantine when both this stage and the atomically-written
	// state identity are present; otherwise it restores the old installation.
	if err := journal.Advance(a.Config.Control, "validated", now); err != nil {
		return false, err
	}
	// Validate and record all release-store bookkeeping before the canonical
	// state commit. A crash here may leave an index pointing at the candidate,
	// but recovery still sees the old state, restores live data, and runState
	// repairs that index. No fallible release validation is allowed afterward.
	if err := recordReleaseActivation(a.Config.Control, spec, state, previous); err != nil {
		return false, fmt.Errorf("record staged release activation: %w", err)
	}
	if err := SaveState(a.Config.Control, state); err != nil {
		return false, err
	}
	// SaveState is the durable commit point. From here onward recovery must
	// finish activation; callers must never restore an overlay or quarantine
	// over the newly committed state, even if journal cleanup fails.
	if err := journal.Advance(a.Config.Control, "installed", now); err != nil {
		return true, err
	}
	return true, nil
}

func (a *App) validateCommittedInstall(ctx context.Context, spec ProviderSpec, state State) error {
	if err := validateStateAgainstCatalog(a.Catalog, spec, state, a.Config.Arch); err != nil {
		return err
	}
	receipt, err := LoadInstallReceipt(a.Config.Control, state.Receipt)
	if err != nil {
		return err
	}
	if err := verifyInstallReceipt(a.Config.Home, state, receipt); err != nil {
		return err
	}
	launch, err := a.rebuildLaunchState(ctx, spec, state)
	if err != nil {
		return err
	}
	if err := verifyLaunchReceiptCompleteness(a.Config.Home, receipt, launch); err != nil {
		return err
	}
	memory, err := planMemory(spec.Memory, a.Config.Request, a.Config.Policy, readMemorySnapshotWith(a.Config.Dependencies))
	if err != nil {
		return err
	}
	process, err := NewProvider(spec).BuildProcess(ctx, a.Config, launch, memory)
	if err != nil {
		return err
	}
	if len(process.Command) == 0 || process.Command[0] == "" {
		return fmt.Errorf("compiled provider produced an empty process command")
	}
	return nil
}

func (a *App) installWithReset(ctx context.Context, current *State, spec ProviderSpec, provider Provider, req Request, prepared Resolved, ic InstallContext, reason string) error {
	if current == nil {
		return fmt.Errorf("reset requested without an installed state")
	}
	sourceHash := resetSourceFingerprint(*current)
	targetHash := resolvedTargetFingerprint(spec, req, prepared, a.Config.Arch)
	targetVersion := prepared.Artifact.Version + "@" + prepared.Artifact.Build
	pending, err := loadPending(a.Config.Control)
	if err != nil {
		return err
	}
	now := a.Now()
	if req.ResetConfirm == "REQUEST" || pending == nil || now.After(pending.ExpiresAt) ||
		!constantTimeHexEqual(pending.SourceHash, sourceHash) || !constantTimeHexEqual(pending.TargetHash, targetHash) {
		created, err := NewPending(*current, spec, targetVersion, reason, now)
		if err != nil {
			return err
		}
		created.SourceHash, created.TargetHash = sourceHash, targetHash
		if err := savePending(a.Config.Control, created); err != nil {
			return err
		}
		return fmt.Errorf("PCVM-E2002 RESET_REQUIRED: %s; back up your data, then set RESET_CONFIRM=DELETE:%s within 30 minutes", reason, created.Nonce)
	}
	if err := ValidateResetBinding(*pending, req.ResetConfirm, sourceHash, targetHash, now); err != nil {
		return fmt.Errorf("PCVM-E2003 RESET_CONFIRMATION: %s; use RESET_CONFIRM=DELETE:%s", err, pending.Nonce)
	}
	if interrupted, err := LoadOperation(a.Config.Control); err != nil {
		return err
	} else if interrupted != nil {
		if interrupted.Stage != "tainted" || interrupted.PreviousID != current.InstallID {
			return fmt.Errorf("PCVM-E4001 OPERATION_BUSY: operation %s cannot be replaced by this reset", interrupted.ID)
		}
		// Authorization is now bound to both the exact old state and verified
		// target. It is safe to replace the taint guard with the quarantine reset
		// journal; before this point the guard must remain durable.
		if err := interrupted.Complete(a.Config.Control); err != nil {
			return err
		}
	}
	journal, err := BeginOperation(a.Config.Control, "reset", spec.ID, now)
	if err != nil {
		return err
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.RollbackMode = effectiveRollbackMode(spec)
	journal.PreviousID = current.InstallID
	a.logInstallPhase(spec.ID, journal.ID, "reset-quarantine", journal.RollbackMode)
	if err := journal.Advance(a.Config.Control, "quarantine", now); err != nil {
		return err
	}
	q, err := beginQuarantineAt(a.Config.Home, a.Config.Control, journal.ID, a.ResetRoot)
	if err != nil {
		// Keep the quarantine-stage journal. Even when the best-effort partial
		// restore succeeded, the next boot must verify/finish recovery before any
		// server process is allowed to start.
		return fmt.Errorf("PCVM-E4006 QUARANTINE_FAILED: %w", err)
	}
	restore := func(cause error) error {
		if overlayErr := rollbackPendingInstallOverlay(a.Config.Home, a.Config.Control, spec.ID); overlayErr != nil {
			return fmt.Errorf("%v; additionally failed to restore staged overlay: %w", cause, overlayErr)
		}
		if restoreErr := q.Restore(); restoreErr != nil {
			return fmt.Errorf("%v; additionally failed to restore quarantine: %w", cause, restoreErr)
		}
		_ = journal.Complete(a.Config.Control)
		return cause
	}
	if err := journal.Advance(a.Config.Control, "installing", a.Now()); err != nil {
		return restore(err)
	}
	installed, err := provider.Install(ctx, ic, prepared)
	if err != nil {
		return restore(fmt.Errorf("PCVM-E4003 INSTALL_FAILED: install %s after quarantine: %w", spec.ID, err))
	}
	a.logInstallPhase(spec.ID, journal.ID, "installed", journal.RollbackMode)
	state, err := a.sealInstalled(spec, req, installed, journal)
	if err != nil {
		return restore(err)
	}
	if err := a.validateCommittedInstall(ctx, spec, state); err != nil {
		return restore(fmt.Errorf("PCVM-E4004 INSTALL_VALIDATION_FAILED: %w", err))
	}
	a.logInstallPhase(spec.ID, journal.ID, "validated", journal.RollbackMode)
	committed, err := a.activateValidatedState(spec, state, journal, nil)
	if err != nil {
		if committed {
			return fmt.Errorf("PCVM-E4008 ACTIVATION_BOOKKEEPING: reset target %s was committed; restart PCVM to finish quarantine recovery without restoring old data: %w", state.InstallID, err)
		}
		return restore(err)
	}
	if err := q.Commit(); err != nil {
		return fmt.Errorf("purge committed quarantine: %w", err)
	}
	if err := commitPendingInstallOverlay(a.Config.Control, spec.ID, state.InstallID); err != nil {
		return fmt.Errorf("commit staged install overlay: %w", err)
	}
	_ = os.Remove(filepath.Join(a.Config.Control, "pending-switch.json"))
	if err := journal.Complete(a.Config.Control); err != nil {
		return err
	}
	a.logInstallPhase(spec.ID, journal.ID, "complete", journal.RollbackMode)
	a.Log.Printf("transactional reset committed inside %s", a.Config.Home)
	return a.runState(ctx, state)
}

func resolvedTargetFingerprint(spec ProviderSpec, req Request, resolved Resolved, arch string) string {
	lock := lockArtifact(spec.ID, resolved.Artifact)
	seed := strings.Join([]string{spec.ID, lock.ID, lock.Version, lock.Build, lock.Revision, lock.Integrity.SHA256,
		lock.Integrity.SHA512, runtimePackIdentity(resolved.RuntimeKind, resolved.RuntimeVersion, arch),
		immutableConfigFingerprint(spec, req), arch}, "\x00")
	return sha256Hex(seed)
}

func (a *App) runLegacyResetFlow(ctx context.Context, legacy *LegacyStateError, req Request) error {
	if req.ResetConfirm == "" {
		return legacy
	}
	if !a.Config.Policy.AllowUserReset {
		return fmt.Errorf("%s; resets are disabled by host policy", legacy)
	}
	if req.Software == "" || req.Software == "interactive" {
		return fmt.Errorf("%s; set an explicit SOFTWARE target before requesting reset", legacy)
	}
	spec, err := a.validateTarget(req)
	if err != nil {
		return err
	}
	a.Config.Request = req
	provider := NewProvider(spec)
	resolved, err := provider.Resolve(ctx, req, a.HTTP)
	if err != nil {
		return fmt.Errorf("PCVM-E3001 RESOLVE_FAILED: %w", err)
	}
	targetHash := resolvedTargetFingerprint(spec, req, resolved, a.Config.Arch)
	if err := os.MkdirAll(a.Config.Control, 0o750); err != nil {
		return err
	}
	lock, err := AcquireProcessLock(a.Config.Control)
	if err != nil {
		return err
	}
	defer lock.Release()
	// Bind destructive authorization to the exact legacy bytes observed while
	// holding PCVM's process lock. Never reuse a pre-lock fingerprint.
	sourceHash, err := legacyStateFingerprint(legacy.Path)
	if err != nil {
		return err
	}
	pending, err := loadPending(a.Config.Control)
	if err != nil {
		return err
	}
	now := a.Now()
	needsRequest := pending == nil || now.After(pending.ExpiresAt) ||
		!constantTimeHexEqual(pending.SourceHash, sourceHash) || !constantTimeHexEqual(pending.TargetHash, targetHash)
	if req.ResetConfirm == "REQUEST" {
		pseudo := State{Provider: "legacy", ResolvedVersion: fmt.Sprintf("schema-%d", legacy.Schema)}
		created, err := NewPending(pseudo, spec, resolved.Artifact.Version+"@"+resolved.Artifact.Build, "legacy PCVM state requires a clean reset", now)
		if err != nil {
			return err
		}
		created.SourceHash, created.TargetHash = sourceHash, targetHash
		if err := savePending(a.Config.Control, created); err != nil {
			return err
		}
		return fmt.Errorf("PCVM-E2001 LEGACY_STATE: reset requested; back up your data, then set RESET_CONFIRM=DELETE:%s within 30 minutes", created.Nonce)
	}
	if needsRequest {
		return fmt.Errorf("PCVM-E2001 LEGACY_STATE: no valid reset request is pending; set RESET_CONFIRM=REQUEST after backing up the server")
	}
	if err := ValidateResetBinding(*pending, req.ResetConfirm, sourceHash, targetHash, now); err != nil {
		return fmt.Errorf("PCVM-E2003 RESET_CONFIRMATION: %s; use RESET_CONFIRM=DELETE:%s", err, pending.Nonce)
	}
	prepared, ic, err := a.prepare(ctx, provider, req, resolved)
	if err != nil {
		return fmt.Errorf("PCVM-E3002 PREPARE_FAILED: %w", err)
	}
	legacyState := State{Schema: StateSchema, Provider: "legacy", InstallID: sourceHash, ResolvedVersion: fmt.Sprintf("schema-%d", legacy.Schema)}
	return a.installWithLegacyReset(ctx, &legacyState, spec, provider, req, prepared, ic, sourceHash, targetHash)
}

func (a *App) installWithLegacyReset(ctx context.Context, legacy *State, spec ProviderSpec, provider Provider, req Request, prepared Resolved, ic InstallContext, sourceHash, targetHash string) error {
	// The nonce was already verified after the target artifact was resolved and
	// downloaded. Reuse the transactional reset engine with the exact binding.
	journal, err := BeginOperation(a.Config.Control, "legacy-reset", spec.ID, a.Now())
	if err != nil {
		return err
	}
	journal.Quarantine = filepath.ToSlash(filepath.Join("quarantine", journal.ID))
	journal.RollbackMode = effectiveRollbackMode(spec)
	journal.PreviousID = legacy.InstallID
	a.logInstallPhase(spec.ID, journal.ID, "legacy-reset-quarantine", journal.RollbackMode)
	if err := journal.Advance(a.Config.Control, "quarantine", a.Now()); err != nil {
		return err
	}
	q, err := beginQuarantineAt(a.Config.Home, a.Config.Control, journal.ID, a.ResetRoot)
	if err != nil {
		return fmt.Errorf("PCVM-E4006 QUARANTINE_FAILED: %w", err)
	}
	restore := func(cause error) error {
		if overlayErr := rollbackPendingInstallOverlay(a.Config.Home, a.Config.Control, spec.ID); overlayErr != nil {
			return fmt.Errorf("%v; additionally failed to restore staged overlay: %w", cause, overlayErr)
		}
		if restoreErr := q.Restore(); restoreErr != nil {
			return fmt.Errorf("%v; additionally failed to restore legacy data: %w", cause, restoreErr)
		}
		_ = journal.Complete(a.Config.Control)
		return cause
	}
	if err := journal.Advance(a.Config.Control, "installing", a.Now()); err != nil {
		return restore(err)
	}
	installed, err := provider.Install(ctx, ic, prepared)
	if err != nil {
		return restore(fmt.Errorf("PCVM-E4003 INSTALL_FAILED: %w", err))
	}
	a.logInstallPhase(spec.ID, journal.ID, "installed", journal.RollbackMode)
	state, err := a.sealInstalled(spec, req, installed, journal)
	if err != nil {
		return restore(err)
	}
	if err := a.validateCommittedInstall(ctx, spec, state); err != nil {
		return restore(fmt.Errorf("PCVM-E4004 INSTALL_VALIDATION_FAILED: %w", err))
	}
	a.logInstallPhase(spec.ID, journal.ID, "validated", journal.RollbackMode)
	committed, err := a.activateValidatedState(spec, state, journal, nil)
	if err != nil {
		if committed {
			return fmt.Errorf("PCVM-E4008 ACTIVATION_BOOKKEEPING: legacy reset target %s was committed; restart PCVM to finish quarantine recovery without restoring old data: %w", state.InstallID, err)
		}
		return restore(err)
	}
	if err := q.Commit(); err != nil {
		return err
	}
	if err := commitPendingInstallOverlay(a.Config.Control, spec.ID, state.InstallID); err != nil {
		return fmt.Errorf("commit staged install overlay: %w", err)
	}
	_ = os.Remove(filepath.Join(a.Config.Control, "pending-switch.json"))
	if err := journal.Complete(a.Config.Control); err != nil {
		return err
	}
	a.logInstallPhase(spec.ID, journal.ID, "complete", journal.RollbackMode)
	return a.runState(ctx, state)
}

func legacyStateFingerprint(path string) (string, error) {
	statePath := path
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		statePath = filepath.Join(path, "state.json")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		data = []byte(filepath.Clean(path))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func EvaluateTransition(state *State, target Provider, resolved Resolved) (bool, string) {
	if state == nil {
		return false, ""
	}
	if state.Family != target.Spec().Family {
		return true, "incompatible provider family"
	}
	if target.Spec().Installer == "qemu-vm" && (state.ResolvedVersion != resolved.Artifact.Version || state.ResolvedBuild != resolved.Artifact.Build) {
		return true, "changing a VM distro version or image build requires reset"
	}
	if target.Spec().Installer == "qemu-vm" && vmArtifactIdentityChanged(state.Artifact, resolved.Artifact) {
		return true, "changing a VM image variant requires reset"
	}
	if target.Spec().ID == "modrinth-modpack" {
		targetID := lockArtifact(target.Spec().ID, resolved.Artifact).ID
		if !strings.HasPrefix(state.ArtifactLock.ID, "modrinth:") || state.ArtifactLock.ID != targetID {
			return true, "changing a Modrinth project requires reset"
		}
	}
	if CompareVersionsForDomain(target.Spec().VersionDomain, resolved.Artifact.Version, state.ResolvedVersion) < 0 {
		return true, "downgrade requires reset"
	}
	return false, ""
}

func vmArtifactIdentityChanged(current, target Artifact) bool {
	currentID, targetID := "", ""
	if current.Metadata != nil {
		currentID = current.Metadata["vm_image_id"]
	}
	if target.Metadata != nil {
		targetID = target.Metadata["vm_image_id"]
	}
	if currentID != "" && targetID != "" && currentID != targetID {
		return true
	}
	if current.URL != "" && target.URL != "" && current.URL != target.URL {
		return true
	}
	if current.SHA512 != "" && target.SHA512 != "" && !strings.EqualFold(current.SHA512, target.SHA512) {
		return true
	}
	return current.SHA256 != "" && target.SHA256 != "" && !strings.EqualFold(current.SHA256, target.SHA256)
}

func (a *App) prepareModrinthIdentity(ctx context.Context, spec ProviderSpec, req Request, resolved Resolved) (Resolved, error) {
	packPath := ""
	if resolved.Artifact.Kind == "mrpack-upload" {
		clean, err := cleanRelativeEntry(resolved.Artifact.FileName)
		if err != nil {
			return resolved, err
		}
		packPath = filepath.Join(a.Config.Home, clean)
	} else {
		if resolved.Artifact.URL == "" {
			return resolved, fmt.Errorf("resolved Modrinth project has no immutable pack URL")
		}
		packPath = filepath.Join(a.Config.Control, "cache", "artifacts", spec.ID+"-"+resolved.Artifact.Version+"-"+resolved.Artifact.FileName)
		if err := secureMkdirAll(a.Config.Control, filepath.Dir(packPath), 0o750); err != nil {
			return resolved, fmt.Errorf("prepare Modrinth artifact cache: %w", err)
		}
		var err error
		resolved.Artifact, err = a.HTTP.Download(ctx, resolved.Artifact, packPath)
		if err != nil {
			return resolved, err
		}
	}
	bound, err := bindModrinthPackRuntime(packPath, spec, req, resolved)
	if err != nil {
		return resolved, err
	}
	bound.PreparedArtifact = packPath
	return bound, nil
}

func (a *App) prepare(ctx context.Context, p Provider, req Request, resolved Resolved) (Resolved, InstallContext, error) {
	artifactPath := resolved.PreparedArtifact
	preparedSource := ""
	if resolved.Artifact.URL != "" && artifactPath == "" {
		artifactPath = filepath.Join(a.Config.Control, "cache", "artifacts", p.Spec().ID+"-"+resolved.Artifact.Version+"-"+resolved.Artifact.FileName)
		if err := secureMkdirAll(a.Config.Control, filepath.Dir(artifactPath), 0o750); err != nil {
			return resolved, InstallContext{}, fmt.Errorf("prepare artifact cache: %w", err)
		}
		var err error
		resolved.Artifact, err = a.HTTP.Download(ctx, resolved.Artifact, artifactPath)
		if err != nil {
			return resolved, InstallContext{}, err
		}
	}
	if p.Spec().Installer == "modrinth" && resolved.Artifact.Metadata["modrinth_loader"] == "" {
		packPath := artifactPath
		if resolved.Artifact.Kind == "mrpack-upload" {
			clean, err := cleanRelativeEntry(resolved.Artifact.FileName)
			if err != nil {
				return resolved, InstallContext{}, err
			}
			packPath = filepath.Join(a.Config.Home, clean)
		}
		var err error
		resolved, err = bindModrinthPackRuntime(packPath, p.Spec(), req, resolved)
		if err != nil {
			return resolved, InstallContext{}, err
		}
	}
	if p.Spec().Resolver == "local-app" && req.SourceMode == "git" {
		var commit string
		var err error
		preparedSource, commit, err = a.prepareGitSource(ctx, p.Spec(), req)
		if err != nil {
			return resolved, InstallContext{}, err
		}
		if resolved.Artifact.Metadata == nil {
			resolved.Artifact.Metadata = map[string]string{}
		}
		resolved.Artifact.Metadata["source_commit"] = commit
		resolved.Artifact.Build = commit[:12]
	}
	runtimePath := ""
	if resolved.RuntimeKind != "native" {
		manager := RuntimeManager{Catalog: a.Catalog, Config: a.Config, HTTP: a.HTTP, Log: a.Log}
		var err error
		runtimePath, err = manager.Ensure(ctx, resolved.RuntimeKind, resolved.RuntimeVersion)
		if err != nil {
			return resolved, InstallContext{}, err
		}
	}
	ic := InstallContext{Home: a.Config.Home, ControlDir: a.Config.Control, AllocationPort: a.Config.AllocationPort, Artifact: artifactPath, Runtime: runtimePath, PreparedSource: preparedSource, Request: req, Log: a.Log, HTTP: a.HTTP, Out: a.Out, Err: a.Err, Dependencies: a.Config.Dependencies}
	return resolved, ic, nil
}

func bindModrinthPackRuntime(packPath string, spec ProviderSpec, req Request, resolved Resolved) (Resolved, error) {
	index, reader, err := readMRPack(packPath)
	if err != nil {
		return resolved, err
	}
	_ = reader.Close()
	loader, loaderVersion, err := modrinthLoader(index.Dependencies)
	if err != nil {
		return resolved, err
	}
	minecraft := index.Dependencies["minecraft"]
	runtimeArtifact := resolved.Artifact
	runtimeArtifact.Version = minecraft
	runtimeVersion, err := resolveRuntimeVersion(spec, req.RuntimeVersion, runtimeArtifact)
	if err != nil {
		return resolved, err
	}
	if resolved.Artifact.Metadata == nil {
		resolved.Artifact.Metadata = map[string]string{}
	}
	resolved.Artifact.Metadata["minecraft_version"] = minecraft
	resolved.Artifact.Metadata["modrinth_loader"] = loader
	resolved.Artifact.Metadata["modrinth_loader_version"] = loaderVersion
	if resolved.Artifact.Kind == "mrpack-upload" {
		digest, err := regularFileSHA512(packPath)
		if err != nil {
			return resolved, err
		}
		resolved.Artifact.SHA512 = digest
		resolved.Artifact.Version = index.VersionID
		resolved.Artifact.Build = digest[:16]
		projectID, err := modrinthUploadProjectID(index)
		if err != nil {
			return resolved, err
		}
		resolved.Artifact.Metadata["modrinth_project_id"] = projectID
		resolved.Artifact.Metadata["modrinth_version_id"] = index.VersionID
	}
	resolved.RuntimeVersion = runtimeVersion
	return resolved, nil
}

func (a *App) prepareGitSource(ctx context.Context, spec ProviderSpec, req Request) (string, string, error) {
	if err := validateGitBranch(req.GitBranch); err != nil {
		return "", "", err
	}
	lookup := exec.CommandContext(ctx, "git", "ls-remote", "--refs", "--exit-code", "--", req.GitURL, "refs/heads/"+req.GitBranch)
	lookup.Env = sanitizedGitEnvironment()
	output, err := lookup.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve Git branch to immutable commit: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[1] != "refs/heads/"+req.GitBranch || !validGitCommit(fields[0]) {
		return "", "", fmt.Errorf("Git branch did not resolve to one immutable commit")
	}
	commit := strings.ToLower(fields[0])
	sum := sha256.Sum256([]byte(req.GitURL + "\x00" + commit))
	root := filepath.Join(a.Config.Control, "cache", "sources")
	target := filepath.Join(root, fmt.Sprintf("%s-%x", spec.ID, sum[:8]))
	if err := secureMkdirAll(a.Config.Control, root, 0o750); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", target, "rev-parse", "HEAD")
		cmd.Env = sanitizedGitEnvironment()
		head, err := cmd.Output()
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(head)), commit) {
			return "", "", fmt.Errorf("cached Git source does not match resolved commit")
		}
		if err := validatePreparedEntry(target, defaultEntry(spec), req.EntryFile); err != nil {
			return "", "", err
		}
		return target, commit, nil
	}
	tmp, err := os.MkdirTemp(root, ".source-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--filter=blob:none", "--depth", "1", "--branch", req.GitBranch, "--", req.GitURL, tmp)
	cmd.Env = sanitizedGitEnvironment()
	cmd.Stdout = a.Out
	cmd.Stderr = a.Err
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("prepare Git source: %w", err)
	}
	cmd = exec.CommandContext(ctx, "git", "-C", tmp, "checkout", "--detach", commit)
	cmd.Env = sanitizedGitEnvironment()
	cmd.Stdout, cmd.Stderr = a.Out, a.Err
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("checkout resolved Git commit: %w", err)
	}
	if err := validatePreparedEntry(tmp, defaultEntry(spec), req.EntryFile); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", "", err
	}
	return target, commit, nil
}

func validateGitBranch(branch string) error {
	if branch == "" || len(branch) > 128 || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, "\x00\r\n ~^:?*[\\") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("GIT_BRANCH is not a safe Git branch name")
	}
	return nil
}

func validGitCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sanitizedGitEnvironment() []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=/home/container",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=",
	}
	return environment
}

func validatePreparedEntry(root, fallback, entry string) error {
	if entry == "" {
		entry = fallback
	}
	entry, err := cleanRelativeEntry(entry)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(root, entry))
	if err != nil {
		return fmt.Errorf("prepared source entry: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("prepared source entry is a directory")
	}
	return nil
}

func defaultEntry(spec ProviderSpec) string {
	if entry := strings.TrimSpace(spec.Options.DefaultEntry); entry != "" {
		return entry
	}
	if spec.ID == "node-bot" {
		return "index.js"
	}
	return "main.py"
}

func (a *App) runState(ctx context.Context, state State) error {
	spec, ok := a.Catalog.Provider(state.Provider)
	if !ok {
		return fmt.Errorf("state references unknown provider %q", state.Provider)
	}
	if !a.Config.Policy.AllowedSoftware[spec.ID] {
		return fmt.Errorf("provider %q is disabled by host policy", spec.ID)
	}
	if !contains(spec.Architectures, a.Config.Arch) {
		return fmt.Errorf("provider %q has no %s artifact", spec.ID, a.Config.Arch)
	}
	if err := ValidateProviderImageProfile(a.Config.ImageProfile, spec); err != nil {
		return err
	}
	if state.Architecture != "" && state.Architecture != a.Config.Arch {
		return fmt.Errorf("state architecture %q does not match container architecture %q", state.Architecture, a.Config.Arch)
	}
	var receipt InstallReceipt
	if state.Schema == StateSchema {
		if err := validateStateAgainstCatalog(a.Catalog, spec, state, a.Config.Arch); err != nil {
			return err
		}
		loadedReceipt, err := LoadInstallReceipt(a.Config.Control, state.Receipt)
		if err != nil {
			return fmt.Errorf("PCVM-E2006 RELEASE_MISSING: load install receipt: %w", err)
		}
		receipt = loadedReceipt
		if err := verifyInstallReceipt(a.Config.Home, state, receipt); err != nil {
			return err
		}
		if receipt.RollbackMode != effectiveRollbackModeForState(spec, state) {
			return fmt.Errorf("PCVM-E2004 RECEIPT_MISMATCH: receipt rollback mode does not match the compiled provider")
		}
	}
	if err := ValidateProviderRequest(spec, a.Config); err != nil {
		return err
	}
	if spec.RequiresEULA {
		accepted, err := ensureMinecraftEULA(a.Config.Home)
		if err != nil {
			return fmt.Errorf("check Minecraft EULA acceptance: %w", err)
		}
		if !accepted {
			return fmt.Errorf("%s", minecraftEULATrigger)
		}
	}
	memorySnapshot := readMemorySnapshotWith(a.Config.Dependencies)
	memoryPlan, err := planMemory(spec.Memory, a.Config.Request, a.Config.Policy, memorySnapshot)
	if err != nil {
		return fmt.Errorf("memory plan for %s: %w", state.Provider, err)
	}
	a.logMemoryPlan(memoryPlan)
	launch, err := a.rebuildLaunchState(ctx, spec, state)
	if err != nil {
		return fmt.Errorf("rebuild trusted process metadata for %s: %w", state.Provider, err)
	}
	if state.Schema == StateSchema {
		if err := verifyLaunchReceiptCompleteness(a.Config.Home, receipt, launch); err != nil {
			return err
		}
	}
	if state.Schema == StateSchema {
		if err := repairReleaseActivation(a.Config.Control, spec, state); err != nil {
			return fmt.Errorf("PCVM-E4008 RELEASE_INDEX: %w", err)
		}
	}
	if err := cleanupConsumedInstallCache(a.Config.Control); err != nil {
		a.Log.Printf("WARNING: consumed cache cleanup failed: %v", err)
	}
	manager := RuntimeManager{Catalog: a.Catalog, Config: a.Config, HTTP: a.HTTP, Log: a.Log}
	// Runtime kind is catalog policy. Never let the user-writable state choose
	// which integrity-checked runtime is retained or removed.
	if err := manager.Prune(manager.runtimeRoot(spec.Runtime, state.RuntimeVersion)); err != nil {
		a.Log.Printf("WARNING: cache pruning failed: %v", err)
	}
	allocationChanged, err := a.syncPrimaryAllocation(launch)
	if err != nil {
		return err
	}
	if allocationChanged {
		a.Log.Printf("configured primary allocation 0.0.0.0:%d for %s", a.Config.AllocationPort, state.Provider)
	}
	process, err := NewProvider(spec).BuildProcess(ctx, a.Config, launch, memoryPlan)
	if err != nil {
		return fmt.Errorf("build process for %s: %w", state.Provider, err)
	}
	process.Environment = allocationEnvironment(launch.Provider, process.Environment, a.Config.AllocationPort)
	process.Environment, err = processUserEnvironment(launch.Provider, a.Config.Home, process.Environment)
	if err != nil {
		return fmt.Errorf("prepare process environment for %s: %w", launch.Provider, err)
	}
	if process.ReadyAfter == 0 {
		process.ReadyAfter = 5 * time.Second
	}
	if process.ReadyTimeout == 0 {
		process.ReadyTimeout = 2 * time.Minute
	}
	if process.StopTimeout == 0 {
		process.StopTimeout = 30 * time.Second
	}
	runErr := a.Supervisor.Run(ctx, process, a.In, a.Out, a.Err)
	if after := readMemorySnapshotWith(a.Config.Dependencies); memoryOOMKilled(memorySnapshot, after) {
		if runErr != nil {
			return fmt.Errorf("process was OOM-killed by the container memory cgroup: %w", runErr)
		}
		return fmt.Errorf("process was OOM-killed by the container memory cgroup")
	}
	return runErr
}

func (a *App) logMemoryPlan(plan MemoryPlan) {
	limit, target, reserve := "unknown", "runtime-default", "unknown"
	if plan.LimitMB > 0 {
		limit = fmt.Sprintf("%dMB", plan.LimitMB)
		reserve = fmt.Sprintf("%dMB", plan.ReserveMB)
	}
	if plan.TargetMB > 0 {
		target = fmt.Sprintf("%dMB", plan.TargetMB)
	}
	a.Log.Printf("MEMORY source=%s limit=%s target=%s reserve=%s strategy=%s recommended=%dMB", plan.Source, limit, target, reserve, plan.Strategy, plan.RecommendedMB)
	if plan.CurrentKnown || plan.OOMSource != "" {
		current, oomKills := "unknown", "unknown"
		if plan.CurrentKnown {
			current = fmt.Sprintf("%dMB", plan.CurrentMB)
		}
		if plan.OOMSource != "" {
			oomKills = fmt.Sprintf("%d", plan.OOMKills)
		}
		a.Log.Printf("MEMORY diagnostics current=%s oom_kill=%s", current, oomKills)
	}
	if plan.BelowRecommended {
		a.Log.Printf("WARNING: memory allocation %d MB is below the recommended %d MB for this provider", plan.LimitMB, plan.RecommendedMB)
	}
	if plan.UnknownLimit {
		a.Log.Printf("WARNING: container memory limit is unknown; using the %s fallback", plan.Strategy)
	}
}

func first(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}
func LoadRuntimeManifest(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var check RuntimeManifest
	if err = decodeStrictJSON(data, &check); err != nil {
		return nil, err
	}
	if check.Schema != RuntimeManifestSchema || check.Release == "" || check.Compatibility == "" || len(check.Packs) == 0 {
		return nil, fmt.Errorf("runtime manifest envelope is incomplete")
	}
	for _, pack := range check.Packs {
		if !validRuntimeUpstreamVersion(pack.UpstreamVersion) {
			return nil, fmt.Errorf("runtime manifest pack %q has an invalid upstream version", pack.ID)
		}
	}
	return data, nil
}
