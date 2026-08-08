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
	if err := ValidateProviderRequest(spec, a.Config); err != nil {
		return err
	}
	if spec.RequiresEULA && !req.AcceptEULA {
		return fmt.Errorf("set ACCEPT_MINECRAFT_EULA=1 to install %s", spec.Name)
	}
	if (spec.ID == "node-bot" || spec.ID == "python-bot") && req.SourceMode == "git" {
		if err := a.Config.ValidateGitURL(req.GitURL); err != nil {
			return err
		}
	}
	needsResolve := state == nil || state.Provider != spec.ID || req.Version != "latest" && req.Version != state.RequestedVersion || req.Build != "latest" && req.Build != state.RequestedBuild || req.AutoUpdate || req.UpdateRequest != "" && req.UpdateRequest != state.LastUpdateRequest
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
		if err := a.handleReset(*state, spec, resolved.Artifact.Version, resetReason, req.ResetConfirm); err != nil {
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
	newState := State{Schema: StateSchema, Provider: spec.ID, Family: spec.Family, RequestedVersion: req.Version, RequestedBuild: req.Build, ResolvedVersion: installed.Artifact.Version, ResolvedBuild: installed.Artifact.Build, RuntimeKind: installed.RuntimeKind, RuntimeVersion: installed.RuntimeVersion, RuntimeExecutable: first(installed.Command), Architecture: a.Config.Arch, Artifact: installed.Artifact, Command: installed.Command, Environment: installed.Environment, WorkingDirectory: installed.WorkDir, ReadyPatterns: installed.ReadyPatterns, StopCommand: installed.StopCommand, LastUpdateRequest: req.UpdateRequest, InstalledAt: a.Now()}
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
	if state.Provider == target.Spec().ID && target.CompareVersions(resolved.Artifact.Version, state.ResolvedVersion) < 0 {
		return true, "downgrade requires reset"
	}
	return false, ""
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
	ic := InstallContext{Home: a.Config.Home, ControlDir: a.Config.Control, Artifact: artifactPath, Runtime: runtimePath, PreparedSource: preparedSource, Request: req, Log: a.Log, HTTP: a.HTTP, Out: a.Out, Err: a.Err}
	return resolved, ic, nil
}

func (a *App) prepareGitSource(ctx context.Context, providerID string, req Request) (string, error) {
	sum := sha256.Sum256([]byte(req.GitURL + "\x00" + req.GitBranch))
	root := filepath.Join(a.Config.Control, "cache", "sources")
	target := filepath.Join(root, fmt.Sprintf("%s-%x", providerID, sum[:8]))
	if err := os.MkdirAll(root, 0o750); err != nil {
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
	entry = filepath.Clean(entry)
	if filepath.IsAbs(entry) || entry == ".." || strings.HasPrefix(entry, ".."+string(filepath.Separator)) {
		return fmt.Errorf("ENTRY_FILE must stay inside source")
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
	if len(state.Command) == 0 {
		return fmt.Errorf("state contains no command")
	}
	spec, ok := a.Catalog.Provider(state.Provider)
	if !ok {
		return fmt.Errorf("state references unknown provider %q", state.Provider)
	}
	allocationChanged, err := a.syncPrimaryAllocation(state)
	if err != nil {
		return err
	}
	if allocationChanged {
		a.Log.Printf("configured primary allocation 0.0.0.0:%d for %s", a.Config.AllocationPort, state.Provider)
	}
	if _, err := compileReadyPatterns(state.ReadyPatterns); err != nil {
		if _, catalogErr := compileReadyPatterns(spec.ReadyPatterns); catalogErr != nil {
			return fmt.Errorf("repair stored readiness metadata for provider %q: %w", state.Provider, catalogErr)
		}
		a.Log.Printf("WARNING: repaired invalid stored readiness metadata for %s from the embedded catalog", state.Provider)
		state.ReadyPatterns = append([]string(nil), spec.ReadyPatterns...)
		state.StopCommand = spec.StopCommand
		if err := SaveState(a.Config.Control, state); err != nil {
			return fmt.Errorf("save repaired state: %w", err)
		}
	}
	process, err := NewProvider(spec).BuildProcess(ctx, a.Config, state)
	if err != nil {
		return fmt.Errorf("build process for %s: %w", state.Provider, err)
	}
	process.Environment = allocationEnvironment(state.Provider, process.Environment, a.Config.AllocationPort)
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
