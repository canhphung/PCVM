package pcvm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// providerDriverContract is the compile-time registry boundary. An embedded
// catalog entry is accepted only when its identity and public driver references
// match this table. The remaining capabilities are composed locally from the
// trusted provider class; a catalog edit cannot opt into a different updater,
// configurator, control protocol, transition policy, or validator.
type providerDriverContract struct {
	Resolver      string
	Installer     string
	VersionDomain string
}

var compiledProviderContracts = map[string]providerDriverContract{
	"vanilla":           {Resolver: "mojang", Installer: "jar", VersionDomain: "minecraft"},
	"paper":             {Resolver: "papermc", Installer: "jar", VersionDomain: "minecraft"},
	"purpur":            {Resolver: "purpur", Installer: "jar", VersionDomain: "minecraft"},
	"pufferfish":        {Resolver: "pufferfish", Installer: "jar", VersionDomain: "minecraft"},
	"folia":             {Resolver: "papermc", Installer: "jar", VersionDomain: "minecraft"},
	"canvas":            {Resolver: "canvas", Installer: "jar", VersionDomain: "minecraft"},
	"fabric":            {Resolver: "fabric", Installer: "jar", VersionDomain: "minecraft"},
	"quilt":             {Resolver: "quilt", Installer: "quilt", VersionDomain: "minecraft"},
	"forge":             {Resolver: "forge", Installer: "java-installer", VersionDomain: "minecraft"},
	"neoforge":          {Resolver: "neoforge", Installer: "java-installer", VersionDomain: "minecraft"},
	"paper-geyser":      {Resolver: "paper-geyser", Installer: "paper-geyser", VersionDomain: "minecraft"},
	"modrinth-modpack":  {Resolver: "modrinth", Installer: "modrinth", VersionDomain: "modrinth"},
	"velocity":          {Resolver: "papermc", Installer: "jar", VersionDomain: "minecraft"},
	"bungeecord":        {Resolver: "bungeecord", Installer: "jar", VersionDomain: "minecraft"},
	"bedrock":           {Resolver: "bedrock", Installer: "zip", VersionDomain: "minecraft"},
	"pocketmine":        {Resolver: "pocketmine", Installer: "phar", VersionDomain: "minecraft"},
	"powernukkitx":      {Resolver: "github-release", Installer: "jar", VersionDomain: "minecraft"},
	"cloudburst-nukkit": {Resolver: "cloudburst-nukkit", Installer: "jar", VersionDomain: "minecraft"},
	"endstone":          {Resolver: "pypi-endstone", Installer: "endstone", VersionDomain: "minecraft"},
	"cs2":               {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"gmod":              {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"l4d2":              {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"samp":              {Resolver: "github-release", Installer: "openmp", VersionDomain: "semver"},
	"mtasa":             {Resolver: "mta-pinned", Installer: "mtasa", VersionDomain: "semver"},
	"palworld":          {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"rust":              {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"rust-umod":         {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"project-zomboid":   {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"valheim":           {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"valheim-bepinex":   {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"7dtd":              {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"unturned":          {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"terraria":          {Resolver: "terraria", Installer: "terraria", VersionDomain: "semver"},
	"tmodloader":        {Resolver: "github-release", Installer: "tmodloader", VersionDomain: "semver"},
	"tshock":            {Resolver: "github-release-arch", Installer: "tshock", VersionDomain: "semver"},
	"satisfactory":      {Resolver: "steamcmd", Installer: "steamcmd", VersionDomain: "opaque"},
	"factorio":          {Resolver: "factorio", Installer: "factorio", VersionDomain: "semver"},
	"nginx":             {Resolver: "local-service", Installer: "web", VersionDomain: "opaque"},
	"apache":            {Resolver: "local-service", Installer: "web", VersionDomain: "opaque"},
	"caddy":             {Resolver: "local-service", Installer: "web", VersionDomain: "opaque"},
	"node-bot":          {Resolver: "local-app", Installer: "node-app", VersionDomain: "opaque"},
	"python-bot":        {Resolver: "local-app", Installer: "python-app", VersionDomain: "opaque"},
	"bun-app":           {Resolver: "local-app", Installer: "generic-app", VersionDomain: "semver"},
	"deno-app":          {Resolver: "local-app", Installer: "generic-app", VersionDomain: "semver"},
	"go-app":            {Resolver: "local-app", Installer: "generic-app", VersionDomain: "semver"},
	"dotnet-app":        {Resolver: "local-app", Installer: "generic-app", VersionDomain: "semver"},
	"lavalink":          {Resolver: "github-release", Installer: "jar", VersionDomain: "semver"},
	"code-server":       {Resolver: "github-release-arch", Installer: "code-server", VersionDomain: "semver"},
	"vm-ubuntu":         {Resolver: "vm-image", Installer: "qemu-vm", VersionDomain: "semver"},
	"vm-debian":         {Resolver: "vm-image", Installer: "qemu-vm", VersionDomain: "semver"},
	"vm-almalinux":      {Resolver: "vm-image", Installer: "qemu-vm", VersionDomain: "semver"},
	"vm-rocky":          {Resolver: "vm-image", Installer: "qemu-vm", VersionDomain: "semver"},
	"vm-alpine":         {Resolver: "vm-image", Installer: "qemu-vm", VersionDomain: "semver"},
}

var compiledUpdaters = map[string]UpdaterDriver{
	"staged":   updaterDriverFunc(delegateProviderUpdate),
	"in-place": updaterDriverFunc(delegateProviderUpdate),
	"none":     updaterDriverFunc(delegateProviderUpdate),
}

func delegateProviderUpdate(ctx context.Context, provider *catalogProvider, install InstallContext, resolved Resolved) (Resolved, error) {
	if provider.drivers.Installer == nil {
		return Resolved{}, fmt.Errorf("provider %q has no installer for update", provider.spec.ID)
	}
	return provider.drivers.Installer.Install(ctx, provider, install, resolved)
}

var compiledConfigurators = map[string]ConfiguratorDriver{
	"identity": configuratorDriverFunc(func(_ context.Context, _ *catalogProvider, _ Config, state LaunchState) (LaunchState, error) {
		return cloneLaunchState(state), nil
	}),
	"powernukkitx": configuratorDriverFunc(func(_ context.Context, _ *catalogProvider, cfg Config, state LaunchState) (LaunchState, error) {
		state = cloneLaunchState(state)
		state.Command = append(state.Command, "--skip-setup", "--accept-license", "--language", "eng", "--server-name", cfg.Request.ServerName, "--port", strconv.Itoa(cfg.AllocationPort))
		return state, nil
	}),
	"cloudburst-nukkit": configuratorDriverFunc(func(_ context.Context, _ *catalogProvider, _ Config, state LaunchState) (LaunchState, error) {
		state = cloneLaunchState(state)
		state.Command = append(state.Command, "--language", "eng")
		return state, nil
	}),
}

func cloneLaunchState(state LaunchState) LaunchState {
	state.Command = append([]string(nil), state.Command...)
	state.Environment = append([]string(nil), state.Environment...)
	state.Readiness.Patterns = append([]string(nil), state.Readiness.Patterns...)
	return state
}

var compiledControls = map[string]ControlDriver{
	"catalog": controlDriverFunc(applyCatalogControl),
	"qmp":     controlDriverFunc(applyCatalogControl),
}

func applyCatalogControl(provider *catalogProvider, _ Config, state LaunchState, process ProcessSpec) (ProcessSpec, error) {
	if process.Readiness.Mode == "" {
		process.Readiness = state.Readiness
		if process.Readiness.Mode == "" {
			process.Readiness = provider.spec.Readiness
		}
	}
	process.Readiness.Patterns = append([]string(nil), process.Readiness.Patterns...)
	if process.Control.Mode == "" {
		process.Control = state.Control
		if process.Control.Mode == "" {
			process.Control = provider.spec.Control
		}
	}
	if process.ReadyTimeout == 0 && process.Readiness.TimeoutSeconds > 0 {
		process.ReadyTimeout = time.Duration(process.Readiness.TimeoutSeconds) * time.Second
	}
	if _, err := compileReadyPatterns(process.Readiness.Patterns); err != nil {
		return ProcessSpec{}, fmt.Errorf("provider %q readiness: %w", provider.spec.ID, err)
	}
	return process, nil
}

var compiledValidators = map[string]ValidatorDriver{
	"standard": validatorDriverFuncs{config: func(provider *catalogProvider, cfg Config) error {
		return ValidateProviderRequest(provider.spec, cfg)
	}},
	"powernukkitx": validatorDriverFuncs{
		request: func(_ *catalogProvider, req Request) error {
			if req.RuntimeVersion != "" && req.RuntimeVersion != "auto" && req.RuntimeVersion != "21" {
				return fmt.Errorf("PowerNukkitX requires RUNTIME_VERSION=auto or 21")
			}
			return nil
		},
		config: func(provider *catalogProvider, cfg Config) error { return ValidateProviderRequest(provider.spec, cfg) },
	},
	"cloudburst-nukkit": validatorDriverFuncs{
		request: func(_ *catalogProvider, req Request) error {
			if req.RuntimeVersion != "" && req.RuntimeVersion != "auto" && req.RuntimeVersion != "8" {
				return fmt.Errorf("Cloudburst Nukkit requires RUNTIME_VERSION=auto or 8")
			}
			return nil
		},
		config: func(provider *catalogProvider, cfg Config) error { return ValidateProviderRequest(provider.spec, cfg) },
	},
}

var compiledTransitions = map[string]TransitionPolicyDriver{
	"default":  transitionPolicyDriverFunc(defaultTransitionPlan),
	"vm":       transitionPolicyDriverFunc(vmTransitionPlan),
	"modrinth": transitionPolicyDriverFunc(modrinthTransitionPlan),
}

func defaultTransitionPlan(provider *catalogProvider, current *State, _ Request, resolved Resolved) ActionPlan {
	if current.Family != "" && current.Family != provider.spec.Family {
		return ActionPlan{Kind: ActionReset, Reason: "incompatible provider family"}
	}
	comparator := provider.drivers.Comparator
	if comparator == nil {
		comparator = compiledComparators[strings.ToLower(provider.spec.VersionDomain)]
	}
	if comparator == nil {
		comparator = versionComparatorFunc(CompareVersions)
	}
	versionOrder := comparator.Compare(resolved.Artifact.Version, current.ResolvedVersion)
	if versionOrder < 0 || versionOrder == 0 && CompareVersions(resolved.Artifact.Build, current.ResolvedBuild) < 0 {
		return ActionPlan{Kind: ActionReset, Reason: "downgrade requires reset"}
	}
	return ActionPlan{Kind: ActionUpdate}
}

func vmTransitionPlan(provider *catalogProvider, current *State, req Request, resolved Resolved) ActionPlan {
	if current.Family != "" && current.Family != provider.spec.Family {
		return ActionPlan{Kind: ActionReset, Reason: "incompatible provider family"}
	}
	if current.ResolvedVersion != resolved.Artifact.Version || current.ResolvedBuild != resolved.Artifact.Build {
		return ActionPlan{Kind: ActionReset, Reason: "changing a VM distro version or image build requires reset"}
	}
	if vmArtifactIdentityChanged(current.Artifact, resolved.Artifact) {
		return ActionPlan{Kind: ActionReset, Reason: "changing a VM image variant requires reset"}
	}
	if current.ImmutableConfigHash != "" && immutableConfigFingerprint(provider.spec, req) != current.ImmutableConfigHash {
		return ActionPlan{Kind: ActionReset, Reason: "changing VM install configuration requires reset"}
	}
	return defaultTransitionPlan(provider, current, req, resolved)
}

func modrinthTransitionPlan(provider *catalogProvider, current *State, _ Request, resolved Resolved) ActionPlan {
	if current.Family != "" && current.Family != provider.spec.Family {
		return ActionPlan{Kind: ActionReset, Reason: "incompatible provider family"}
	}
	targetID := lockArtifact(provider.spec.ID, resolved.Artifact).ID
	if !strings.HasPrefix(current.ArtifactLock.ID, "modrinth:") || !strings.HasPrefix(targetID, "modrinth:") || current.ArtifactLock.ID != targetID {
		return ActionPlan{Kind: ActionReset, Reason: "changing a Modrinth project requires reset"}
	}
	targetLock := lockArtifact(provider.spec.ID, resolved.Artifact)
	if strings.HasPrefix(targetID, "modrinth:upload-") {
		// Upload packs have no authoritative publication timestamp. Their stable
		// manifest name + loader binds project identity; compare versionId only
		// and never order the unrelated content digest in Build.
		if CompareVersions(resolved.Artifact.Version, current.ResolvedVersion) < 0 {
			return ActionPlan{Kind: ActionReset, Reason: "downgrade requires reset"}
		}
		return ActionPlan{Kind: ActionUpdate}
	}
	currentPublished, currentErr := time.Parse(time.RFC3339Nano, current.ArtifactLock.Revision)
	targetPublished, targetErr := time.Parse(time.RFC3339Nano, targetLock.Revision)
	if currentErr != nil || targetErr != nil {
		if current.ArtifactLock == targetLock {
			return ActionPlan{Kind: ActionUpdate}
		}
		return ActionPlan{Kind: ActionReset, Reason: "Modrinth publication order cannot be verified; reset required"}
	}
	if targetPublished.Before(currentPublished) {
		return ActionPlan{Kind: ActionReset, Reason: "downgrade requires reset"}
	}
	return ActionPlan{Kind: ActionUpdate}
}

func updaterDriverName(spec ProviderSpec) string {
	switch spec.RollbackMode {
	case "staged", "in-place", "none":
		return spec.RollbackMode
	default:
		// Minimal test/provider specs predate the public rollback field. Catalog
		// validation still rejects an empty value.
		return "in-place"
	}
}

func configuratorDriverName(spec ProviderSpec) string {
	if _, ok := compiledConfigurators[spec.ID]; ok {
		return spec.ID
	}
	return "identity"
}

func controlDriverName(spec ProviderSpec) string {
	if spec.Control.Mode == "qmp" || spec.Installer == "qemu-vm" {
		return "qmp"
	}
	return "catalog"
}

func transitionDriverName(spec ProviderSpec) string {
	switch spec.Installer {
	case "qemu-vm":
		return "vm"
	case "modrinth":
		return "modrinth"
	default:
		return "default"
	}
}

func validatorDriverName(spec ProviderSpec) string {
	if _, ok := compiledValidators[spec.ID]; ok {
		return spec.ID
	}
	return "standard"
}

func transitionPolicyForSpec(spec ProviderSpec) TransitionPolicyDriver {
	return compiledTransitions[transitionDriverName(spec)]
}
