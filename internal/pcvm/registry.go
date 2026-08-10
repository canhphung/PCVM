package pcvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Provider drivers are compiled Go capabilities. Catalog data can select a
// registered driver but can never inject code, argv, or a remote installer.
type ResolverDriver interface {
	Resolve(context.Context, *catalogProvider, Request, *HTTPClient) (Artifact, error)
}

type InstallerDriver interface {
	Install(context.Context, *catalogProvider, InstallContext, Resolved) (Resolved, error)
}

// UpdaterDriver is deliberately separate from InstallerDriver. Providers may
// share the same implementation today, but the orchestration contract can now
// distinguish a staged update from an in-place/non-rollback update without
// inferring that policy from an installer name.
type UpdaterDriver interface {
	Update(context.Context, *catalogProvider, InstallContext, Resolved) (Resolved, error)
}

// ConfiguratorDriver applies provider-owned, typed launch configuration before
// argv construction. It never receives arbitrary catalog code or shell text.
type ConfiguratorDriver interface {
	Configure(context.Context, *catalogProvider, Config, LaunchState) (LaunchState, error)
}

type ProcessBuilderDriver interface {
	Build(context.Context, *catalogProvider, Config, LaunchState, MemoryPlan) (ProcessSpec, error)
}

// ControlDriver finalizes readiness and shutdown control after the process
// builder has produced argv. Dynamic values (for example a QMP socket or RCON
// port) are preserved, while absent values are restored from trusted catalog
// metadata.
type ControlDriver interface {
	Apply(*catalogProvider, Config, LaunchState, ProcessSpec) (ProcessSpec, error)
}

// TransitionPolicyDriver owns compatibility and downgrade semantics for a
// provider class. Reconcile delegates resolved transitions to this driver.
type TransitionPolicyDriver interface {
	Plan(*catalogProvider, *State, Request, Resolved) ActionPlan
}

// ValidatorDriver is the compiled validation boundary used both before
// resolution and before process construction.
type ValidatorDriver interface {
	ValidateRequest(*catalogProvider, Request) error
	ValidateConfig(*catalogProvider, Config) error
}

type VersionComparator interface {
	Compare(a, b string) int
}

type resolverDriverFunc func(context.Context, *catalogProvider, Request, *HTTPClient) (Artifact, error)

func (f resolverDriverFunc) Resolve(ctx context.Context, provider *catalogProvider, req Request, client *HTTPClient) (Artifact, error) {
	return f(ctx, provider, req, client)
}

type installerDriverFunc func(context.Context, *catalogProvider, InstallContext, Resolved) (Resolved, error)

func (f installerDriverFunc) Install(ctx context.Context, provider *catalogProvider, install InstallContext, resolved Resolved) (Resolved, error) {
	return f(ctx, provider, install, resolved)
}

type updaterDriverFunc func(context.Context, *catalogProvider, InstallContext, Resolved) (Resolved, error)

func (f updaterDriverFunc) Update(ctx context.Context, provider *catalogProvider, install InstallContext, resolved Resolved) (Resolved, error) {
	return f(ctx, provider, install, resolved)
}

type configuratorDriverFunc func(context.Context, *catalogProvider, Config, LaunchState) (LaunchState, error)

func (f configuratorDriverFunc) Configure(ctx context.Context, provider *catalogProvider, cfg Config, state LaunchState) (LaunchState, error) {
	return f(ctx, provider, cfg, state)
}

type processDriverFunc func(context.Context, *catalogProvider, Config, LaunchState, MemoryPlan) (ProcessSpec, error)

func (f processDriverFunc) Build(ctx context.Context, provider *catalogProvider, cfg Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
	return f(ctx, provider, cfg, state, memory)
}

type controlDriverFunc func(*catalogProvider, Config, LaunchState, ProcessSpec) (ProcessSpec, error)

func (f controlDriverFunc) Apply(provider *catalogProvider, cfg Config, state LaunchState, process ProcessSpec) (ProcessSpec, error) {
	return f(provider, cfg, state, process)
}

type transitionPolicyDriverFunc func(*catalogProvider, *State, Request, Resolved) ActionPlan

func (f transitionPolicyDriverFunc) Plan(provider *catalogProvider, state *State, req Request, resolved Resolved) ActionPlan {
	return f(provider, state, req, resolved)
}

type validatorDriverFuncs struct {
	request func(*catalogProvider, Request) error
	config  func(*catalogProvider, Config) error
}

func (v validatorDriverFuncs) ValidateRequest(provider *catalogProvider, req Request) error {
	if v.request == nil {
		return nil
	}
	return v.request(provider, req)
}

func (v validatorDriverFuncs) ValidateConfig(provider *catalogProvider, cfg Config) error {
	if v.config == nil {
		return nil
	}
	return v.config(provider, cfg)
}

type versionComparatorFunc func(string, string) int

func (f versionComparatorFunc) Compare(a, b string) int { return f(a, b) }

type providerDrivers struct {
	Resolver     ResolverDriver
	Installer    InstallerDriver
	Updater      UpdaterDriver
	Configurator ConfiguratorDriver
	Process      ProcessBuilderDriver
	Control      ControlDriver
	Transition   TransitionPolicyDriver
	Validator    ValidatorDriver
	Comparator   VersionComparator
}

var compiledResolvers = map[string]ResolverDriver{
	"mojang": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveMojang(ctx, req, h)
	}),
	"papermc": resolverDriverFunc(func(ctx context.Context, p *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolvePaper(ctx, p.spec.Options.Project, req, h)
	}),
	"canvas": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveCanvas(ctx, req, h)
	}),
	"purpur": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolvePurpur(ctx, req, h)
	}),
	"pufferfish": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolvePufferfish(ctx, req, h)
	}),
	"fabric": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveFabric(ctx, req, h)
	}),
	"quilt": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveQuilt(ctx, req, h)
	}),
	"forge": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveMaven(ctx, req, h, "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml", "https://maven.minecraftforge.net/net/minecraftforge/forge/%s/forge-%s-installer.jar")
	}),
	"neoforge": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveMaven(ctx, req, h, "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml", "https://maven.neoforged.net/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar")
	}),
	"bungeecord": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveBungee(ctx, req, h)
	}),
	"bedrock": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, _ Request, h *HTTPClient) (Artifact, error) {
		return resolveBedrock(ctx, h)
	}),
	"cloudburst-nukkit": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveCloudburstNukkit(ctx, req, h)
	}),
	"pypi-endstone": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveEndstone(ctx, req, h)
	}),
	"pocketmine": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveGitHub(ctx, req, h, "pmmp/PocketMine-MP", `(?i)\.phar$`)
	}),
	"github-release": resolverDriverFunc(func(ctx context.Context, p *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveGitHub(ctx, req, h, p.spec.Options.Repository, p.spec.Options.AssetRegex)
	}),
	"github-release-arch": resolverDriverFunc(func(ctx context.Context, p *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		pattern := p.spec.Options.AssetRegexForArchitecture(req.Architecture)
		if pattern == "" {
			return Artifact{}, fmt.Errorf("provider %q has no %s release artifact", p.spec.ID, req.Architecture)
		}
		return resolveGitHub(ctx, req, h, p.spec.Options.Repository, pattern)
	}),
	"mta-pinned": resolverDriverFunc(func(_ context.Context, p *catalogProvider, req Request, _ *HTTPClient) (Artifact, error) {
		return resolvePinnedMTA(req, p.spec.Options)
	}),
	"local-app": resolverDriverFunc(func(_ context.Context, _ *catalogProvider, req Request, _ *HTTPClient) (Artifact, error) {
		return Artifact{Kind: "source", Version: envLatest(req.Version), Build: envLatest(req.Build)}, nil
	}),
	"paper-geyser": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolvePaperGeyser(ctx, req, h)
	}),
	"modrinth": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveModrinth(ctx, req, h)
	}),
	"local-service": resolverDriverFunc(func(_ context.Context, _ *catalogProvider, _ Request, _ *HTTPClient) (Artifact, error) {
		return Artifact{Kind: "local", Version: "system", Build: "release"}, nil
	}),
	"steamcmd": resolverDriverFunc(func(_ context.Context, p *catalogProvider, req Request, _ *HTTPClient) (Artifact, error) {
		return Artifact{Kind: "steam-app", Version: envLatest(req.Version), Build: envLatest(req.Build), Metadata: map[string]string{"appid": p.spec.Options.AppID}}, nil
	}),
	"terraria": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveTerraria(ctx, req, h)
	}),
	"factorio": resolverDriverFunc(func(ctx context.Context, _ *catalogProvider, req Request, h *HTTPClient) (Artifact, error) {
		return resolveFactorio(ctx, req, h)
	}),
	"vm-image": resolverDriverFunc(func(_ context.Context, p *catalogProvider, req Request, _ *HTTPClient) (Artifact, error) {
		return resolveVMImage(p.spec, req)
	}),
}

var compiledInstallers = map[string]InstallerDriver{
	"jar": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installJar(ctx, ic, resolved)
	}),
	"phar": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installPHAR(ic, resolved)
	}),
	"zip": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installBedrockZip(ic, resolved)
	}),
	"java-installer": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installJavaLoader(ctx, ic, resolved)
	}),
	"node-app": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installApp(ctx, ic, resolved)
	}),
	"python-app": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installApp(ctx, ic, resolved)
	}),
	"generic-app": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installGenericApp(ctx, ic, resolved)
	}),
	"quilt": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installQuilt(ctx, ic, resolved)
	}),
	"paper-geyser": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installPaperGeyser(ctx, ic, resolved)
	}),
	"modrinth": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installModrinth(ctx, ic, resolved)
	}),
	"web": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installWeb(ic, resolved)
	}),
	"steamcmd": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installSteam(ctx, ic, resolved)
	}),
	"terraria": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installTerraria(ic, resolved)
	}),
	"factorio": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installFactorio(ctx, ic, resolved)
	}),
	"tmodloader": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installTModLoader(ic, resolved)
	}),
	"tshock": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installTShock(ic, resolved)
	}),
	"endstone": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installEndstone(ctx, ic, resolved)
	}),
	"openmp": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installOpenMP(ic, resolved)
	}),
	"mtasa": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installMTA(ctx, ic, resolved)
	}),
	"code-server": installerDriverFunc(func(_ context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installCodeServer(ic, resolved)
	}),
	"qemu-vm": installerDriverFunc(func(ctx context.Context, p *catalogProvider, ic InstallContext, resolved Resolved) (Resolved, error) {
		return p.installVM(ctx, ic, resolved)
	}),
}

var compiledProcesses = map[string]ProcessBuilderDriver{
	"standard": processDriverFunc(buildStandardProcess),
	"service": processDriverFunc(func(ctx context.Context, p *catalogProvider, cfg Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
		process, err := p.buildServiceProcess(ctx, cfg, state)
		if err != nil {
			return ProcessSpec{}, err
		}
		return applyMemoryPlan(p.spec, process, memory)
	}),
	"qemu": processDriverFunc(func(_ context.Context, p *catalogProvider, cfg Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
		process, err := p.buildVMProcess(cfg, state, memory)
		if err != nil {
			return ProcessSpec{}, err
		}
		return applyMemoryPlan(p.spec, process, memory)
	}),
}

var installerProcessDriver = map[string]string{
	"web": "service", "code-server": "service", "steamcmd": "service", "terraria": "service", "factorio": "service",
	"tmodloader": "service", "tshock": "service", "openmp": "service", "mtasa": "service", "qemu-vm": "qemu",
}

var compiledComparators = map[string]VersionComparator{
	"": versionComparatorFunc(CompareVersions), "semver": versionComparatorFunc(CompareVersions),
	"minecraft": versionComparatorFunc(CompareVersions), "forge": versionComparatorFunc(CompareVersions),
	"modrinth": versionComparatorFunc(CompareVersions),
	"opaque": versionComparatorFunc(func(a, b string) int {
		if a == b {
			return 0
		}
		return 1
	}),
}

var runtimeDefaults = map[string]func(Artifact) string{
	"java":     func(artifact Artifact) string { return JavaVersionFor(artifact.Version) },
	"node":     func(Artifact) string { return "24" },
	"bun":      func(Artifact) string { return "1" },
	"deno":     func(Artifact) string { return "2" },
	"go":       func(Artifact) string { return "1.26" },
	"python":   func(Artifact) string { return "3.13" },
	"php-pmmp": func(Artifact) string { return "pmmp" },
	"steamcmd": func(Artifact) string { return "1" },
	"dotnet":   func(Artifact) string { return "8" },
	"caddy":    func(Artifact) string { return "2" },
	"native":   func(Artifact) string { return "native" },
}

func resolveRuntimeVersion(spec ProviderSpec, requested string, artifact Artifact) (string, error) {
	if requested != "" && requested != "auto" {
		if !contains(spec.RuntimePolicy.Allowed, requested) {
			return "", fmt.Errorf("RUNTIME_VERSION=%q is not allowed for provider %q (allowed: %s)", requested, spec.ID, strings.Join(spec.RuntimePolicy.Allowed, ", "))
		}
		if spec.Runtime == "java" {
			required := JavaVersionFor(artifact.Version)
			requestedNumber, requestedErr := strconv.Atoi(requested)
			requiredNumber, requiredErr := strconv.Atoi(required)
			if requestedErr == nil && requiredErr == nil && requestedNumber < requiredNumber {
				return "", fmt.Errorf("RUNTIME_VERSION=%s is too old for %s; Java %s or newer is required", requested, artifact.Version, required)
			}
		}
		return requested, nil
	}
	defaultForRuntime := runtimeDefaults[spec.Runtime]
	if defaultForRuntime == nil {
		return "", fmt.Errorf("provider %q references uncompiled runtime policy %q", spec.ID, spec.Runtime)
	}
	resolved := defaultForRuntime(artifact)
	if spec.RuntimePolicy.Default != "" && spec.RuntimePolicy.Default != "auto" {
		resolved = spec.RuntimePolicy.Default
	}
	if !contains(spec.RuntimePolicy.Allowed, resolved) {
		return "", fmt.Errorf("compiled runtime default %q is not allowed for provider %q", resolved, spec.ID)
	}
	return resolved, nil
}

func compiledProviderDrivers(spec ProviderSpec) (providerDrivers, error) {
	contract, ok := compiledProviderContracts[spec.ID]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q is not registered in the compiled provider platform", spec.ID)
	}
	if spec.Resolver != contract.Resolver || spec.Installer != contract.Installer {
		return providerDrivers{}, fmt.Errorf("provider %q driver identity does not match the compiled registry", spec.ID)
	}
	domain := strings.ToLower(strings.TrimSpace(spec.VersionDomain))
	if domain == "" {
		domain = contract.VersionDomain
	} else if domain != contract.VersionDomain {
		return providerDrivers{}, fmt.Errorf("provider %q version domain %q does not match compiled registry %q", spec.ID, domain, contract.VersionDomain)
	}
	resolver, ok := compiledResolvers[spec.Resolver]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled resolver %q", spec.ID, spec.Resolver)
	}
	installer, ok := compiledInstallers[spec.Installer]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled installer %q", spec.ID, spec.Installer)
	}
	processID := installerProcessDriver[spec.Installer]
	if processID == "" {
		processID = "standard"
	}
	process, ok := compiledProcesses[processID]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled process builder %q", spec.ID, processID)
	}
	comparator, ok := compiledComparators[domain]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled version domain %q", spec.ID, domain)
	}
	updaterName := updaterDriverName(spec)
	updater, ok := compiledUpdaters[updaterName]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled updater %q", spec.ID, updaterName)
	}
	configuratorName := configuratorDriverName(spec)
	configurator, ok := compiledConfigurators[configuratorName]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled configurator %q", spec.ID, configuratorName)
	}
	controlName := controlDriverName(spec)
	control, ok := compiledControls[controlName]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled control driver %q", spec.ID, controlName)
	}
	transitionName := transitionDriverName(spec)
	transition, ok := compiledTransitions[transitionName]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled transition policy %q", spec.ID, transitionName)
	}
	validatorName := validatorDriverName(spec)
	validator, ok := compiledValidators[validatorName]
	if !ok {
		return providerDrivers{}, fmt.Errorf("provider %q references uncompiled validator %q", spec.ID, validatorName)
	}
	return providerDrivers{
		Resolver: resolver, Installer: installer, Updater: updater, Configurator: configurator,
		Process: process, Control: control, Transition: transition, Validator: validator, Comparator: comparator,
	}, nil
}

func buildStandardProcess(_ context.Context, p *catalogProvider, _ Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
	readiness := state.Readiness
	readiness.Patterns = append([]string(nil), state.Readiness.Patterns...)
	control := state.Control
	command := append([]string(nil), state.Command...)
	process := ProcessSpec{Command: command, Directory: state.WorkingDirectory,
		Environment: append([]string(nil), state.Environment...), Readiness: readiness, Control: control}
	return applyMemoryPlan(p.spec, process, memory)
}

func (p *catalogProvider) installJar(_ context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	target := filepath.Join(managed, resolved.Artifact.Version+"-"+resolved.Artifact.Build+"-server.jar")
	if err := copyFile(ic.Artifact, target, 0o640); err != nil {
		return resolved, err
	}
	resolved.WorkDir = ic.Home
	resolved.Command = []string{ic.Runtime, "-jar", target}
	if strings.HasPrefix(p.spec.Family, "minecraft-java-") {
		resolved.Command = append(resolved.Command, "nogui")
	}
	if p.spec.ID == "lavalink" {
		config := filepath.Join(ic.Home, "application.yml")
		if _, err := os.Stat(config); os.IsNotExist(err) {
			port := envDefault("SERVER_PORT", "2333")
			body := fmt.Sprintf("server:\n  port: %s\nlavalink:\n  server:\n    password: youshallnotpass\n", port)
			if err := os.WriteFile(config, []byte(body), 0o640); err != nil {
				return resolved, err
			}
		}
	}
	if err := stagePaperGeyserRemoval(ic, p.spec, resolved); err != nil {
		return resolved, fmt.Errorf("prepare Paper-Geyser transition: %w", err)
	}
	return resolved, nil
}

func (p *catalogProvider) installPHAR(ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	target := filepath.Join(managed, resolved.Artifact.Version+"-PocketMine-MP.phar")
	if err := copyFile(ic.Artifact, target, 0o640); err != nil {
		return resolved, err
	}
	resolved.WorkDir = ic.Home
	resolved.Command = []string{ic.Runtime, target, "--no-wizard"}
	resolved.Environment = []string{"PHPRC="}
	return resolved, nil
}

func (p *catalogProvider) installBedrockZip(ic InstallContext, resolved Resolved) (Resolved, error) {
	versionRoot := filepath.Join(ic.ControlDir, "managed", p.spec.ID, resolved.Artifact.Version)
	if err := os.MkdirAll(versionRoot, 0o750); err != nil {
		return resolved, err
	}
	if err := extractZipSafe(ic.Artifact, versionRoot); err != nil {
		return resolved, err
	}
	if err := linkMutableData(ic.Home, versionRoot, []string{"worlds"}, []string{"server.properties", "allowlist.json", "permissions.json"}); err != nil {
		return resolved, err
	}
	resolved.WorkDir = versionRoot
	resolved.Command = []string{filepath.Join(versionRoot, "bedrock_server")}
	resolved.Environment = []string{"LD_LIBRARY_PATH=."}
	return resolved, nil
}

func (p *catalogProvider) installJavaLoader(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID, resolved.Artifact.Version)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	cmd := exec.CommandContext(ctx, ic.Runtime, "-jar", ic.Artifact, "--installServer")
	cmd.Dir, cmd.Stdout, cmd.Stderr = managed, ic.Out, ic.Err
	if err := cmd.Run(); err != nil {
		return resolved, fmt.Errorf("installer: %w", err)
	}
	argsFile := "@libraries/net/minecraftforge/forge/" + resolved.Artifact.Version + "/unix_args.txt"
	if p.spec.ID == "neoforge" {
		argsFile = "@libraries/net/neoforged/neoforge/" + resolved.Artifact.Version + "/unix_args.txt"
	}
	resolved.WorkDir = managed
	resolved.Command = []string{ic.Runtime, "@user_jvm_args.txt", argsFile, "nogui"}
	if err := linkMutableData(ic.Home, managed, []string{"world", "world_nether", "world_the_end", "mods", "config", "defaultconfigs"}, []string{"server.properties", "eula.txt", "ops.json", "whitelist.json", "banned-ips.json", "banned-players.json"}); err != nil {
		return resolved, err
	}
	return resolved, nil
}
