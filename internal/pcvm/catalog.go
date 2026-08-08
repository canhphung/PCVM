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
		if p.Name == "" || p.Family == "" || p.Resolver == "" || p.Installer == "" {
			return fmt.Errorf("provider %q has incomplete metadata", p.ID)
		}
		if len(p.Architectures) == 0 {
			return fmt.Errorf("provider %q has no architectures", p.ID)
		}
		if _, err := compileReadyPatterns(p.ReadyPatterns); err != nil {
			return fmt.Errorf("provider %q has invalid readiness metadata: %w", p.ID, err)
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
