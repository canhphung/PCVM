package pcvm

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

//go:embed catalog.json
var embeddedCatalog []byte

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func LoadCatalog(extraRuntimeManifest []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(embeddedCatalog, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode embedded catalog: %w", err)
	}
	if len(extraRuntimeManifest) > 0 {
		var packs []RuntimePackSpec
		if err := json.Unmarshal(extraRuntimeManifest, &packs); err != nil {
			return Catalog{}, fmt.Errorf("decode runtime manifest: %w", err)
		}
		catalog.RuntimePacks = packs
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
		if p.MinimumMemory < 0 || p.MinimumDisk < 0 {
			return fmt.Errorf("provider %q has invalid resource metadata", p.ID)
		}
		patterns := p.ReadyPatterns
		if len(p.Readiness.Patterns) > 0 {
			patterns = p.Readiness.Patterns
		}
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
		case "", "stdin", "source-rcon", "telnet", "signal":
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
			if !regexp.MustCompile(`^[0-9]+$`).MatchString(p.Options["appid"]) || p.Options["executable"] == "" || !contains(p.Architectures, "amd64") || contains(p.Architectures, "arm64") {
				return fmt.Errorf("provider %q has invalid Steam metadata", p.ID)
			}
		}
	}
	for _, pack := range c.RuntimePacks {
		if pack.Kind == "" || pack.Version == "" || pack.Architecture == "" {
			return fmt.Errorf("incomplete runtime pack")
		}
		if pack.URL == "" || len(pack.SHA256) != 64 || pack.Executable == "" {
			return fmt.Errorf("runtime %s/%s/%s lacks a pinned URL, sha256, or executable", pack.Kind, pack.Version, pack.Architecture)
		}
	}
	return nil
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
		case "source", "survival", "sandbox":
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
		if !allowed[p.ID] || !contains(p.Architectures, arch) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
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
