package pcvm

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

//go:embed catalog.json
var embeddedCatalog []byte

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var validVMImageID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,95}$`)

func LoadCatalog(extraRuntimeManifest []byte) (Catalog, error) {
	var catalog Catalog
	if err := decodeStrictJSON(embeddedCatalog, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode embedded catalog: %w", err)
	}
	if len(extraRuntimeManifest) > 0 {
		var manifest RuntimeManifest
		if err := decodeStrictJSON(extraRuntimeManifest, &manifest); err != nil {
			return Catalog{}, fmt.Errorf("decode runtime manifest: %w", err)
		}
		if manifest.Schema != RuntimeManifestSchema {
			return Catalog{}, fmt.Errorf("unsupported runtime manifest schema %d", manifest.Schema)
		}
		if manifest.Release != catalog.Version {
			return Catalog{}, fmt.Errorf("runtime manifest release %q does not match catalog %q", manifest.Release, catalog.Version)
		}
		if manifest.Compatibility != "pcvm>=2.0.0" {
			return Catalog{}, fmt.Errorf("unsupported runtime manifest compatibility %q", manifest.Compatibility)
		}
		catalog.RuntimePacks = manifest.Packs
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Validate() error {
	if c.Schema != CatalogSchema {
		return fmt.Errorf("unsupported catalog schema %d", c.Schema)
	}
	seen := map[string]bool{}
	vmImageIDs := map[string]bool{}
	for _, p := range c.Providers {
		if !validID.MatchString(p.ID) {
			return fmt.Errorf("invalid provider id %q", p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate provider %q", p.ID)
		}
		seen[p.ID] = true
		if p.Name == "" || p.Family == "" || p.Runtime == "" || p.Resolver == "" || p.Installer == "" {
			return fmt.Errorf("provider %q has incomplete metadata", p.ID)
		}
		if _, err := compiledProviderDrivers(p); err != nil {
			return err
		}
		if err := validateProviderPlatformMetadata(p); err != nil {
			return err
		}
		if len(p.Architectures) == 0 {
			return fmt.Errorf("provider %q has no architectures", p.ID)
		}
		if len(p.MenuPath) == 0 {
			return fmt.Errorf("provider %q has no menu path", p.ID)
		}
		if !validMenuPath(p.MenuPath) {
			return fmt.Errorf("provider %q has invalid menu path %q", p.ID, p.MenuPath)
		}
		archSeen := map[string]bool{}
		for _, arch := range p.Architectures {
			if arch != "amd64" && arch != "arm64" || archSeen[arch] {
				return fmt.Errorf("provider %q has invalid or duplicate architecture %q", p.ID, arch)
			}
			archSeen[arch] = true
		}
		if p.MinimumDisk < 1 {
			return fmt.Errorf("provider %q has invalid resource metadata", p.ID)
		}
		if err := validateMemorySpec(p); err != nil {
			return err
		}
		patterns := p.Readiness.Patterns
		if _, err := compileReadyPatterns(patterns); err != nil {
			return fmt.Errorf("provider %q has invalid readiness metadata: %w", p.ID, err)
		}
		switch p.Readiness.Mode {
		case "", "regex", "tcp", "delay":
		default:
			return fmt.Errorf("provider %q has unsupported readiness mode %q", p.ID, p.Readiness.Mode)
		}
		if p.Readiness.Mode == "regex" && len(patterns) == 0 {
			return fmt.Errorf("provider %q has regex readiness without patterns", p.ID)
		}
		if p.Readiness.Mode == "tcp" && !validPortVariable(p.Readiness.PortVariable) {
			return fmt.Errorf("provider %q has invalid TCP readiness port %q", p.ID, p.Readiness.PortVariable)
		}
		switch p.Control.Mode {
		case "", "stdin", "source-rcon", "telnet", "signal", "qmp":
		default:
			return fmt.Errorf("provider %q has unsupported control mode %q", p.ID, p.Control.Mode)
		}
		portSeen := map[string]bool{}
		for _, requirement := range p.Ports {
			if !validPortVariable(requirement.Variable) || portSeen[requirement.Variable] {
				return fmt.Errorf("provider %q has invalid or duplicate port variable %q", p.ID, requirement.Variable)
			}
			portSeen[requirement.Variable] = true
		}
		if (p.Control.Mode == "source-rcon" || p.Control.Mode == "telnet") && !validPortVariable(p.Control.PortVariable) {
			return fmt.Errorf("provider %q has invalid control port %q", p.ID, p.Control.PortVariable)
		}
		if p.Installer == "steamcmd" {
			if !regexp.MustCompile(`^[0-9]+$`).MatchString(p.Options.AppID) || p.Options.Executable == "" || !contains(p.Architectures, "amd64") || contains(p.Architectures, "arm64") {
				return fmt.Errorf("provider %q has invalid Steam metadata", p.ID)
			}
		}
		if p.Resolver == "github-release-arch" {
			if p.Options.Repository == "" {
				return fmt.Errorf("provider %q lacks a GitHub repository", p.ID)
			}
			for _, arch := range p.Architectures {
				pattern := p.Options.AssetRegexForArchitecture(arch)
				if pattern == "" {
					return fmt.Errorf("provider %q lacks a %s asset pattern", p.ID, arch)
				}
				if _, err := regexp.Compile(pattern); err != nil {
					return fmt.Errorf("provider %q has invalid %s asset pattern", p.ID, arch)
				}
			}
		}
		if p.Resolver == "mta-pinned" {
			if p.Installer != "mtasa" || p.Options.Version == "" || p.Options.Build == "" {
				return fmt.Errorf("provider %q has incomplete MTA metadata", p.ID)
			}
			for _, artifact := range []string{"main", "base", "resources"} {
				rawURL, checksum := p.Options.PinnedArtifact(artifact)
				u, err := url.Parse(rawURL)
				if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || !validHexDigest(checksum, 64) {
					return fmt.Errorf("provider %q has invalid pinned MTA %s artifact", p.ID, artifact)
				}
				switch strings.ToLower(u.Hostname()) {
				case "linux.multitheftauto.com", "mirror.multitheftauto.com", "mirror-cdn.multitheftauto.com":
				default:
					return fmt.Errorf("provider %q has unapproved MTA %s host", p.ID, artifact)
				}
			}
		}
		if p.Installer == "openmp" && (p.Resolver != "github-release" || len(p.Architectures) != 1 || p.Architectures[0] != "amd64") {
			return fmt.Errorf("provider %q has invalid open.mp metadata", p.ID)
		}
		if p.Installer == "qemu-vm" {
			if p.Resolver != "vm-image" || p.Runtime != "native" || len(p.VMImages) < 4 {
				return fmt.Errorf("provider %q has incomplete VM metadata", p.ID)
			}
			activeImages := map[string]bool{}
			versions := map[string]map[string]bool{}
			for _, image := range p.VMImages {
				key := image.Version + "/" + image.Architecture
				u, err := url.Parse(image.URL)
				if !validVMImageID.MatchString(image.ID) || !validID.MatchString(image.Variant) || vmImageIDs[image.ID] ||
					image.Version == "" || image.Build == "" || image.Format != "qcow2" ||
					(image.Architecture != "amd64" && image.Architecture != "arm64") || !contains(p.Architectures, image.Architecture) || err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !validVMImageHost(u.Hostname()) ||
					strings.Contains(strings.ToLower(image.URL), "/latest/") || !immutableVMBuildInURL(image.URL, image.Build) {
					return fmt.Errorf("provider %q has invalid immutable VM image %q", p.ID, key)
				}
				if (!validHexDigest(image.SHA256, 64) || image.SHA512 != "") && (!validHexDigest(image.SHA512, 128) || image.SHA256 != "") {
					return fmt.Errorf("provider %q VM image %q must have exactly one valid checksum", p.ID, key)
				}
				vmImageIDs[image.ID] = true
				if image.Deprecated {
					continue
				}
				if activeImages[key] {
					return fmt.Errorf("provider %q has duplicate active VM image %q", p.ID, key)
				}
				activeImages[key] = true
				if versions[image.Version] == nil {
					versions[image.Version] = map[string]bool{}
				}
				versions[image.Version][image.Architecture] = true
			}
			if len(versions) != 2 {
				return fmt.Errorf("provider %q must pin exactly two VM versions", p.ID)
			}
			for version, architectures := range versions {
				for _, architecture := range p.Architectures {
					if !architectures[architecture] {
						return fmt.Errorf("provider %q VM version %q lacks %s", p.ID, version, architecture)
					}
				}
			}
			if p.Control.Mode != "qmp" {
				return fmt.Errorf("provider %q VM must use QMP control", p.ID)
			}
		} else if len(p.VMImages) != 0 {
			return fmt.Errorf("provider %q has VM images but is not a VM", p.ID)
		}
	}
	packIDs := map[string]bool{}
	for _, pack := range c.RuntimePacks {
		if pack.ID == "" || pack.Kind == "" || pack.Version == "" || !validRuntimeUpstreamVersion(pack.UpstreamVersion) || pack.Architecture == "" {
			return fmt.Errorf("incomplete runtime pack")
		}
		if packIDs[pack.ID] {
			return fmt.Errorf("duplicate runtime pack %q", pack.ID)
		}
		packIDs[pack.ID] = true
		if pack.ID != runtimePackIdentity(pack.Kind, pack.Version, pack.Architecture) {
			return fmt.Errorf("runtime pack %q has inconsistent identity", pack.ID)
		}
		if pack.URL == "" || !validHexDigest(pack.SHA256, 64) || !validHexDigest(pack.TreeSHA256, 64) || pack.Executable == "" || pack.Size <= 0 {
			return fmt.Errorf("runtime %s/%s/%s lacks a pinned URL, sha256, or executable", pack.Kind, pack.Version, pack.Architecture)
		}
	}
	return nil
}

func validateProviderPlatformMetadata(provider ProviderSpec) error {
	switch provider.SupportTier {
	case "stable", "beta", "experimental":
	default:
		return fmt.Errorf("provider %q has invalid support tier %q", provider.ID, provider.SupportTier)
	}
	upstream, err := url.Parse(provider.Upstream)
	if err != nil || upstream.Scheme != "https" || upstream.Hostname() == "" || upstream.User != nil || provider.License == "" {
		return fmt.Errorf("provider %q has invalid upstream or license metadata", provider.ID)
	}
	expectedCapability := ProviderImageCapability(provider)
	if len(provider.Capabilities) != 1 || provider.Capabilities[0] != expectedCapability {
		return fmt.Errorf("provider %q must declare exactly the %q image capability", provider.ID, expectedCapability)
	}
	if provider.InstallFormat < 3 {
		return fmt.Errorf("provider %q has obsolete installation format %d", provider.ID, provider.InstallFormat)
	}
	if provider.RuntimePolicy.Default == "" || len(provider.RuntimePolicy.Allowed) == 0 {
		return fmt.Errorf("provider %q has no typed runtime policy", provider.ID)
	}
	runtimeSeen := map[string]bool{}
	for _, version := range provider.RuntimePolicy.Allowed {
		if version == "" || runtimeSeen[version] {
			return fmt.Errorf("provider %q has invalid or duplicate allowed runtime %q", provider.ID, version)
		}
		runtimeSeen[version] = true
	}
	if provider.RuntimePolicy.Default != "auto" && !runtimeSeen[provider.RuntimePolicy.Default] {
		return fmt.Errorf("provider %q runtime default %q is not allowed", provider.ID, provider.RuntimePolicy.Default)
	}
	switch provider.RollbackMode {
	case "staged", "in-place", "none":
	default:
		return fmt.Errorf("provider %q has invalid rollback mode %q", provider.ID, provider.RollbackMode)
	}
	if provider.RollbackMode == "staged" && !releaseStoreSupports(provider) {
		return fmt.Errorf("provider %q claims staged rollback without a compiled release-store adapter", provider.ID)
	}
	if provider.Installer == "steamcmd" && provider.RollbackMode != "none" {
		return fmt.Errorf("Steam provider %q may not claim transactional rollback", provider.ID)
	}
	if provider.Options.DependencyPolicy != "" && (provider.ID != "bun-app" || provider.Options.DependencyPolicy != "install --production --frozen-lockfile") {
		return fmt.Errorf("provider %q has unsupported dependency policy %q", provider.ID, provider.Options.DependencyPolicy)
	}
	switch provider.Installer {
	case "node-app", "python-app", "generic-app":
		// These installers intentionally preserve user-owned paths and therefore
		// write into the live server tree. Until they gain release-directory
		// activation, claiming staged rollback would allow a failed update to run
		// an old state over partially modified files.
		if provider.RollbackMode == "staged" {
			return fmt.Errorf("live-tree provider %q may not claim staged rollback", provider.ID)
		}
	}
	variableSeen := map[string]bool{}
	validVariable := regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	for _, variable := range provider.Variables {
		if !validVariable.MatchString(variable) || variableSeen[variable] {
			return fmt.Errorf("provider %q has invalid or duplicate variable %q", provider.ID, variable)
		}
		variableSeen[variable] = true
	}
	for _, list := range [][]string{provider.Preservation.Paths, provider.Preservation.ManagedPaths} {
		seen := map[string]bool{}
		for _, raw := range list {
			clean := strings.ReplaceAll(raw, "\\", "/")
			if clean == "" || clean == "." || strings.HasPrefix(clean, "/") || strings.Contains(clean, "../") || clean == ".." || seen[clean] {
				return fmt.Errorf("provider %q has unsafe or duplicate preservation path %q", provider.ID, raw)
			}
			seen[clean] = true
		}
	}
	return nil
}

func validateMemorySpec(p ProviderSpec) error {
	memory := p.Memory
	if memory.RecommendedMB <= 0 || memory.HardMinimumMB < 0 || memory.HardMinimumMB > memory.RecommendedMB {
		return fmt.Errorf("provider %q has invalid memory thresholds", p.ID)
	}
	valid := false
	switch memory.Strategy {
	case "jvm-heap":
		valid = p.Runtime == "java"
	case "node-heap":
		valid = p.Runtime == "node" || p.Installer == "code-server"
	case "deno-heap":
		valid = p.Runtime == "deno"
	case "go-limit":
		valid = p.Runtime == "go"
	case "php-limit":
		valid = p.Runtime == "php-pmmp"
	case "dotnet-gc":
		valid = p.Runtime == "dotnet"
	case "qemu-guest":
		valid = p.Installer == "qemu-vm"
	case "cgroup-only":
		valid = p.Runtime != "java" && p.Runtime != "node" && p.Runtime != "php-pmmp" && p.Runtime != "dotnet" && p.Installer != "code-server" && p.Installer != "qemu-vm"
	default:
		return fmt.Errorf("provider %q has unsupported memory strategy %q", p.ID, memory.Strategy)
	}
	if !valid {
		return fmt.Errorf("provider %q memory strategy %q is incompatible with runtime %q and installer %q", p.ID, memory.Strategy, p.Runtime, p.Installer)
	}
	return nil
}

func validHexDigest(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validVMImageHost(host string) bool {
	switch strings.ToLower(host) {
	case "cloud-images.ubuntu.com", "cloud.debian.org", "repo.almalinux.org", "download.rockylinux.org", "dl-cdn.alpinelinux.org":
		return true
	default:
		return false
	}
}

func immutableVMBuildInURL(rawURL, build string) bool {
	if strings.Contains(rawURL, build) {
		return true
	}
	for _, token := range strings.Split(build, "-") {
		if token == "" || !strings.Contains(rawURL, token) {
			return false
		}
	}
	return true
}

func validMenuPath(path []string) bool {
	if len(path) == 1 {
		switch path[0] {
		case "java", "proxy", "bedrock", "web", "apps":
			return true
		}
	}
	if len(path) == 2 && path[0] == "games" {
		switch path[1] {
		case "source", "gta", "survival", "sandbox":
			return true
		}
	}
	if len(path) == 2 && path[0] == "vms" {
		switch path[1] {
		case "debian-family", "enterprise-linux", "lightweight-linux":
			return true
		}
	}
	return false
}

func validPortVariable(value string) bool {
	switch value {
	case "SERVER_PORT", "QUERY_PORT", "STEAM_PORT", "RELIABLE_PORT", "GAME_PORT_2", "GAME_PORT_3", "RCON_PORT", "TELNET_PORT":
		return true
	default:
		return false
	}
}

func (c Catalog) Provider(id string) (ProviderSpec, bool) {
	for _, p := range c.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderSpec{}, false
}

func (c Catalog) Available(arch string, allowed map[string]bool) []ProviderSpec {
	var out []ProviderSpec
	for _, p := range c.Providers {
		if !allowed[p.ID] || !contains(p.Architectures, arch) || !c.providerRuntimeAvailable(p, arch) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (c Catalog) providerRuntimeAvailable(provider ProviderSpec, arch string) bool {
	// The embedded provider catalog is also loaded without a runtime lock by
	// schema/unit tooling. Production always supplies the release manifest;
	// only then can availability be filtered by actual pack presence.
	if provider.Runtime == "native" || len(c.RuntimePacks) == 0 {
		return true
	}
	for _, allowed := range provider.RuntimePolicy.Allowed {
		if c.HasRuntime(provider.Runtime, allowed, arch) {
			return true
		}
	}
	return false
}

func (c Catalog) HasRuntime(kind, version, arch string) bool {
	if kind == "native" {
		return true
	}
	for _, pack := range c.RuntimePacks {
		if pack.Kind == kind && pack.Architecture == arch && (version == "" || pack.Version == version) {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
