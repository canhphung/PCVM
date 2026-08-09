package pcvm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type App struct {
	Config     Config
	Catalog    Catalog
	HTTP       *HTTPClient
	Log        *Logger
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	Now        func() time.Time
	Supervisor Supervisor
}

func NewApp(cfg Config, catalog Catalog, in io.Reader, out, errOut io.Writer) *App {
	log := NewLogger(out)
	return &App{Config: cfg, Catalog: catalog, HTTP: NewHTTPClient(), Log: log, In: in, Out: out, Err: errOut, Now: time.Now, Supervisor: ProcessSupervisor{Log: log}}
}

func (a *App) Run(ctx context.Context) error {
	migrated, err := migrateLegacyControl(a.Config.Home, a.Config.Control)
	if err != nil {
		return fmt.Errorf("migrate PCVM control directory: %w", err)
	}
	if migrated {
		a.Log.Printf("migrated legacy launcher state and cache to .pcvm")
	}
	if err := os.MkdirAll(a.Config.Control, 0o750); err != nil {
		return err
	}
	state, err := LoadState(a.Config.Control)
	if err != nil {
		return err
	}
	if state != nil {
		if installedSpec, ok := a.Catalog.Provider(state.Provider); ok {
			// Family is catalog policy, never an authority supplied by state.json.
			state.Family = installedSpec.Family
		}
	}
	req := a.Config.Request
	if req.Software == "interactive" {
		if state != nil {
			return a.runState(ctx, *state)
		}
		chosen, err := a.menu()
		if err != nil {
			return err
		}
		req.Software = chosen
	}
	spec, ok := a.Catalog.Provider(strings.ToLower(req.Software))
	if !ok {
		return fmt.Errorf("unknown SOFTWARE %q", req.Software)
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
	if err := ValidateProviderRequest(spec, a.Config); err != nil {
		return err
	}
	if (spec.ID == "node-bot" || spec.ID == "python-bot") && req.SourceMode == "git" {
		if err := a.Config.ValidateGitURL(req.GitURL); err != nil {
			return err
		}
	}
	needsResolve := state == nil || state.Provider != spec.ID || req.Version != "latest" && req.Version != state.RequestedVersion || req.Build != "latest" && req.Build != state.RequestedBuild || req.AutoUpdate || req.UpdateRequest != "" && req.UpdateRequest != state.LastUpdateRequest
	if state != nil && spec.Installer == "qemu-vm" && (req.Version != state.RequestedVersion || req.Build != state.RequestedBuild) {
		needsResolve = true
	}
	if state != nil && spec.Installer == "qemu-vm" && normalizeVMCompression(req.VMDiskCompression) != stateVMCompression(*state) {
		needsResolve = true
	}
	if !needsResolve {
		return a.runState(ctx, *state)
	}
	provider := NewProvider(spec)
	resolved, err := provider.Resolve(ctx, req, a.HTTP)
	if err != nil {
		if state != nil {
			a.Log.Printf("WARNING: update resolution failed; starting last-known-good: %v", err)
			return a.runState(ctx, *state)
		}
		return fmt.Errorf("resolve %s: %w", spec.ID, err)
	}
	requiresReset, resetReason := EvaluateTransition(state, provider, resolved)
	if requiresReset {
		if !a.Config.Policy.AllowUserReset {
			return fmt.Errorf("%s; resets are disabled by host policy", resetReason)
		}
	}
	prepared, installContext, err := a.prepare(ctx, provider, req, resolved)
	if err != nil {
		if state != nil {
			a.Log.Printf("WARNING: update failed; starting last-known-good: %v", err)
			return a.runState(ctx, *state)
		}
		return err
	}
	if requiresReset {
		if err := a.handleReset(*state, spec, resolved.Artifact.Version+"@"+resolved.Artifact.Build, resetReason, req.ResetConfirm); err != nil {
			return err
		}
		state = nil
	}
	installed, err := provider.Install(ctx, installContext, prepared)
	if err != nil {
		if state != nil && spec.Installer != "steamcmd" {
			a.Log.Printf("WARNING: update failed; starting last-known-good: %v", err)
			return a.runState(ctx, *state)
		}
		if state != nil && spec.Installer == "steamcmd" {
			return fmt.Errorf("update %s failed; refusing to start the possibly partial in-place Steam installation: %w", provider.Spec().ID, err)
		}
		return fmt.Errorf("install %s: %w", provider.Spec().ID, err)
	}
	newState := State{Schema: StateSchema, Provider: spec.ID, Family: spec.Family, RequestedVersion: req.Version, RequestedBuild: req.Build, ResolvedVersion: installed.Artifact.Version, ResolvedBuild: installed.Artifact.Build, RuntimeKind: installed.RuntimeKind, RuntimeVersion: installed.RuntimeVersion, Architecture: a.Config.Arch, Artifact: installed.Artifact, LastUpdateRequest: req.UpdateRequest, InstalledAt: a.Now()}
	if err := SaveState(a.Config.Control, newState); err != nil {
		return err
	}
	return a.runState(ctx, newState)
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
	if target.Spec().Installer == "qemu-vm" && stateVMCompression(*state) != normalizeVMCompression(resolved.Artifact.Metadata["disk_compression"]) {
		return true, "changing VM disk compression requires reset"
	}
	if state.Provider == target.Spec().ID && target.CompareVersions(resolved.Artifact.Version, state.ResolvedVersion) < 0 {
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

func (a *App) prepare(ctx context.Context, p Provider, req Request, resolved Resolved) (Resolved, InstallContext, error) {
	runtimePath := ""
	if resolved.RuntimeKind != "native" {
		manager := RuntimeManager{Catalog: a.Catalog, Config: a.Config, HTTP: a.HTTP, Log: a.Log}
		var err error
		runtimePath, err = manager.Ensure(ctx, resolved.RuntimeKind, resolved.RuntimeVersion)
		if err != nil {
			return resolved, InstallContext{}, err
		}
	}
	artifactPath := ""
	preparedSource := ""
	if resolved.Artifact.URL != "" {
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
	if p.Spec().Resolver == "local-app" && req.SourceMode == "git" {
		var err error
		preparedSource, err = a.prepareGitSource(ctx, p.Spec().ID, req)
		if err != nil {
			return resolved, InstallContext{}, err
		}
	}
	ic := InstallContext{Home: a.Config.Home, ControlDir: a.Config.Control, AllocationPort: a.Config.AllocationPort, Artifact: artifactPath, Runtime: runtimePath, PreparedSource: preparedSource, Request: req, Log: a.Log, HTTP: a.HTTP, Out: a.Out, Err: a.Err}
	return resolved, ic, nil
}

func (a *App) prepareGitSource(ctx context.Context, providerID string, req Request) (string, error) {
	sum := sha256.Sum256([]byte(req.GitURL + "\x00" + req.GitBranch))
	root := filepath.Join(a.Config.Control, "cache", "sources")
	target := filepath.Join(root, fmt.Sprintf("%s-%x", providerID, sum[:8]))
	if err := secureMkdirAll(a.Config.Control, root, 0o750); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", target, "pull", "--ff-only", "origin", req.GitBranch)
		cmd.Stdout = a.Out
		cmd.Stderr = a.Err
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("prepare Git update: %w", err)
		}
		if err := validatePreparedEntry(target, providerID, req.EntryFile); err != nil {
			return "", err
		}
		return target, nil
	}
	tmp, err := os.MkdirTemp(root, ".source-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", req.GitBranch, "--", req.GitURL, tmp)
	cmd.Stdout = a.Out
	cmd.Stderr = a.Err
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("prepare Git source: %w", err)
	}
	if err := validatePreparedEntry(tmp, providerID, req.EntryFile); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	return target, nil
}

func validatePreparedEntry(root, providerID, entry string) error {
	if entry == "" {
		if providerID == "node-bot" {
			entry = "index.js"
		} else {
			entry = "main.py"
		}
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

func (a *App) handleReset(state State, target ProviderSpec, targetVersion, reason, confirm string) error {
	pending, err := loadPending(a.Config.Control)
	if err != nil {
		return err
	}
	now := a.Now()
	if pending == nil || pending.ToProvider != target.ID || pending.ToVersion != targetVersion || now.After(pending.ExpiresAt) {
		created, err := NewPending(state, target, targetVersion, reason, now)
		if err != nil {
			return err
		}
		if err = savePending(a.Config.Control, created); err != nil {
			return err
		}
		return fmt.Errorf("%s; back up your data, then set RESET_CONFIRM=DELETE:%s within 30 minutes", reason, created.Nonce)
	}
	if err := ValidateReset(*pending, confirm, now); err != nil {
		return fmt.Errorf("%s; use RESET_CONFIRM=DELETE:%s", err, pending.Nonce)
	}
	if err := GuardedReset(a.Config.Home); err != nil {
		return fmt.Errorf("guarded reset: %w", err)
	}
	a.Log.Printf("confirmed reset completed inside %s", a.Config.Home)
	return nil
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
	if err := ValidateProviderRequest(spec, a.Config); err != nil {
		return err
	}
	if spec.RequiresEULA {
		accepted, err := ensureMinecraftEULA(a.Config.Home, a.Config.Request.AcceptEULA)
		if err != nil {
			return fmt.Errorf("check Minecraft EULA acceptance: %w", err)
		}
		if !accepted {
			return fmt.Errorf("%s", minecraftEULATrigger)
		}
	}
	if spec.Installer == "qemu-vm" {
		repaired, err := repairLegacyVMInstallMetadata(a.Config.Home, spec, state, a.Config.Arch)
		if err != nil {
			return fmt.Errorf("repair legacy VM install metadata: %w", err)
		}
		if repaired {
			a.Log.Printf("migrated legacy %s VM install metadata", spec.ID)
		}
	}
	launch, err := a.rebuildLaunchState(ctx, spec, state)
	if err != nil {
		return fmt.Errorf("rebuild trusted process metadata for %s: %w", state.Provider, err)
	}
	allocationChanged, err := a.syncPrimaryAllocation(launch)
	if err != nil {
		return err
	}
	if allocationChanged {
		a.Log.Printf("configured primary allocation 0.0.0.0:%d for %s", a.Config.AllocationPort, state.Provider)
	}
	process, err := NewProvider(spec).BuildProcess(ctx, a.Config, launch)
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
	return a.Supervisor.Run(ctx, process, a.In, a.Out, a.Err)
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
	var check []RuntimePackSpec
	if err = json.Unmarshal(data, &check); err != nil {
		return nil, err
	}
	return data, nil
}
