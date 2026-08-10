// Command egggen renders every supported PTDL_v2 Egg from the embedded
// provider catalog and the single variable registry in egg/registry.json.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultCatalog  = "internal/pcvm/catalog.json"
	defaultRegistry = "egg/registry.json"
	defaultImage    = "ghcr.io/canhphung/pcvm"
)

type catalogFile struct {
	Version   string            `json:"version"`
	Providers []catalogProvider `json:"providers"`
}

type catalogProvider struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	MenuPath  []string `json:"menu_path"`
	Variables []string `json:"variables"`
}

type registryFile struct {
	ExportedAt string        `json:"exported_at"`
	Author     string        `json:"author"`
	Variables  []eggVariable `json:"variables"`
	PCVM       registryMeta  `json:"_pcvm"`
}

type registryMeta struct {
	Groups   map[string][]string `json:"groups"`
	Profiles map[string][]string `json:"profiles"`
}

type eggVariable struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	EnvVariable  string `json:"env_variable"`
	DefaultValue string `json:"default_value"`
	UserViewable bool   `json:"user_viewable"`
	UserEditable bool   `json:"user_editable"`
	Rules        string `json:"rules"`
	FieldType    string `json:"field_type"`
}

type eggFile struct {
	Comment      string            `json:"_comment"`
	Meta         eggMeta           `json:"meta"`
	ExportedAt   string            `json:"exported_at"`
	Name         string            `json:"name"`
	Author       string            `json:"author"`
	Description  string            `json:"description"`
	Features     []string          `json:"features"`
	DockerImages map[string]string `json:"docker_images"`
	FileDenylist []string          `json:"file_denylist"`
	Startup      string            `json:"startup"`
	Config       eggConfig         `json:"config"`
	Scripts      eggScripts        `json:"scripts"`
	Variables    []eggVariable     `json:"variables"`
}

type eggMeta struct {
	Version   string  `json:"version"`
	UpdateURL *string `json:"update_url"`
}

type eggConfig struct {
	Files   string `json:"files"`
	Startup string `json:"startup"`
	Logs    string `json:"logs"`
	Stop    string `json:"stop"`
}

type eggScripts struct {
	Installation eggInstallation `json:"installation"`
}

type eggInstallation struct {
	Script     string `json:"script"`
	Container  string `json:"container"`
	Entrypoint string `json:"entrypoint"`
}

type profileSpec struct {
	ID          string
	Title       string
	ImageSuffix string
	EULA        bool
}

var profiles = []profileSpec{
	{ID: "universal", Title: "Universal", EULA: true},
	{ID: "minecraft", Title: "Minecraft", ImageSuffix: "-minecraft", EULA: true},
	{ID: "games", Title: "Games", ImageSuffix: "-games"},
	{ID: "apps", Title: "Apps & Web", ImageSuffix: "-apps"},
	{ID: "vm", Title: "Virtual Machines", ImageSuffix: "-vm"},
}

type options struct {
	CatalogPath  string
	RegistryPath string
	OutDir       string
	DocsPath     string
	Version      string
	Image        string
	ExportedAt   string
	Check        bool
	ReleaseNames bool
}

func main() {
	var opts options
	flag.StringVar(&opts.CatalogPath, "catalog", defaultCatalog, "provider catalog JSON")
	flag.StringVar(&opts.RegistryPath, "registry", defaultRegistry, "Egg variable registry JSON")
	flag.StringVar(&opts.OutDir, "out", "egg", "output directory")
	flag.StringVar(&opts.DocsPath, "docs", "docs/provider-matrix.md", "generated provider matrix, or empty to disable")
	flag.StringVar(&opts.Version, "version", "", "release version (defaults to catalog version)")
	flag.StringVar(&opts.Image, "image", defaultImage, "container image repository without a tag")
	flag.StringVar(&opts.ExportedAt, "exported-at", "", "RFC3339 export time (defaults to registry value)")
	flag.BoolVar(&opts.Check, "check", false, "fail when tracked generated assets differ")
	flag.BoolVar(&opts.ReleaseNames, "release-names", false, "include version in every output filename")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "egggen:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	catalog, registry, err := loadInputs(opts.CatalogPath, opts.RegistryPath)
	if err != nil {
		return err
	}
	if opts.Version == "" {
		opts.Version = catalog.Version
	}
	if opts.ExportedAt == "" {
		opts.ExportedAt = registry.ExportedAt
	}
	if opts.Version == "" || opts.ExportedAt == "" || opts.Image == "" {
		return errors.New("version, exported-at and image must not be empty")
	}

	rendered, providerSets, err := renderEggs(catalog, registry, opts)
	if err != nil {
		return err
	}
	if err := writeOrCheck(opts.OutDir, rendered, opts.Check); err != nil {
		return err
	}
	if opts.DocsPath != "" {
		docs := renderProviderDocs(opts.Version, opts.Image, providerSets)
		if opts.Check {
			return checkFile(opts.DocsPath, docs)
		}
		if err := os.MkdirAll(filepath.Dir(opts.DocsPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(opts.DocsPath, docs, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func loadInputs(catalogPath, registryPath string) (catalogFile, registryFile, error) {
	var catalog catalogFile
	var registry registryFile
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		return catalog, registry, fmt.Errorf("read catalog: %w", err)
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return catalog, registry, fmt.Errorf("decode catalog: %w", err)
	}
	data, err = os.ReadFile(registryPath)
	if err != nil {
		return catalog, registry, fmt.Errorf("read registry: %w", err)
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return catalog, registry, fmt.Errorf("decode registry: %w", err)
	}
	return catalog, registry, nil
}

func renderEggs(catalog catalogFile, registry registryFile, opts options) (map[string][]byte, map[string][]catalogProvider, error) {
	providerSets := make(map[string][]catalogProvider, len(profiles))
	providerSets["universal"] = append([]catalogProvider(nil), catalog.Providers...)
	seen := make(map[string]bool, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		if provider.ID == "" || seen[provider.ID] {
			return nil, nil, fmt.Errorf("catalog contains empty or duplicate provider ID %q", provider.ID)
		}
		seen[provider.ID] = true
		profile, err := providerProfile(provider)
		if err != nil {
			return nil, nil, err
		}
		providerSets[profile] = append(providerSets[profile], provider)
	}
	variableIndex, err := validateRegistry(registry)
	if err != nil {
		return nil, nil, err
	}
	rendered := make(map[string][]byte, len(profiles)+1)
	for _, profile := range profiles {
		providers := providerSets[profile.ID]
		ids := make([]string, 0, len(providers))
		for _, provider := range providers {
			ids = append(ids, provider.ID)
		}
		variables, err := variablesForProfile(registry, variableIndex, profile.ID, providers)
		if err != nil {
			return nil, nil, err
		}
		emittedVariables := make(map[string]bool, len(variables))
		for _, variable := range variables {
			emittedVariables[variable.EnvVariable] = true
		}
		for _, provider := range providers {
			for _, variable := range provider.Variables {
				if isPanelVariable(variable) {
					continue
				}
				if _, ok := variableIndex[variable]; !ok {
					return nil, nil, fmt.Errorf("provider %q references variable %q which is absent from the Egg registry", provider.ID, variable)
				}
				if !emittedVariables[variable] {
					return nil, nil, fmt.Errorf("provider %q variable %q is not exposed by the %s Egg", provider.ID, variable, profile.ID)
				}
			}
		}
		features := []string{}
		if profile.EULA {
			features = []string{"eula"}
		}
		tag := opts.Version + profile.ImageSuffix
		egg := eggFile{
			Comment:      "DO NOT EDIT: generated by go run ./cmd/egggen from internal/pcvm/catalog.json and egg/registry.json",
			Meta:         eggMeta{Version: "PTDL_v2"},
			ExportedAt:   opts.ExportedAt,
			Name:         "PCVM " + profile.Title,
			Author:       registry.Author,
			Description:  fmt.Sprintf("PCVM v%s %s Egg: %d checksum-verified providers with typed policy, safe lifecycle management and no telemetry.", opts.Version, profile.Title, len(providers)),
			Features:     features,
			DockerImages: map[string]string{fmt.Sprintf("PCVM %s - %s (%d providers)", opts.Version, profile.Title, len(providers)): opts.Image + ":" + tag},
			FileDenylist: []string{".pcvm"},
			Startup:      "/usr/local/bin/pcvm run",
			Config:       eggConfig{Files: "{}", Startup: `{"done":"[PCVM] READY"}`, Logs: "{}", Stop: "^C"},
			Scripts: eggScripts{Installation: eggInstallation{
				Script:    "#!/bin/bash\nset -euo pipefail\ninstall -d -m 0750 /mnt/server/.pcvm\necho '[PCVM] Control directory initialized.'",
				Container: "debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241", Entrypoint: "bash",
			}},
			Variables: variables,
		}
		data, err := json.MarshalIndent(egg, "", "  ")
		if err != nil {
			return nil, nil, err
		}
		data = append(data, '\n')
		name := outputName(profile.ID, opts.Version, opts.ReleaseNames)
		rendered[name] = data
		if profile.ID == "universal" {
			alias := "egg-pcvm.json"
			if opts.ReleaseNames {
				alias = "egg-pcvm-" + opts.Version + ".json"
			}
			rendered[alias] = data
		}
	}
	return rendered, providerSets, nil
}

func isPanelVariable(name string) bool {
	switch name {
	case "SERVER_IP", "SERVER_PORT", "SERVER_MEMORY":
		return true
	default:
		return false
	}
}

func providerProfile(provider catalogProvider) (string, error) {
	if len(provider.MenuPath) == 0 {
		return "", fmt.Errorf("provider %q has no menu path", provider.ID)
	}
	switch provider.MenuPath[0] {
	case "java", "proxy", "bedrock":
		return "minecraft", nil
	case "games":
		return "games", nil
	case "apps", "web":
		return "apps", nil
	case "vms":
		return "vm", nil
	default:
		return "", fmt.Errorf("provider %q has unclassified menu root %q", provider.ID, provider.MenuPath[0])
	}
}

func validateRegistry(registry registryFile) (map[string]eggVariable, error) {
	variables := make(map[string]eggVariable, len(registry.Variables))
	for _, variable := range registry.Variables {
		if variable.EnvVariable == "" || variables[variable.EnvVariable].EnvVariable != "" {
			return nil, fmt.Errorf("registry contains empty or duplicate variable %q", variable.EnvVariable)
		}
		variables[variable.EnvVariable] = variable
	}
	for group, names := range registry.PCVM.Groups {
		for _, name := range names {
			if _, ok := variables[name]; !ok {
				return nil, fmt.Errorf("registry group %q references unknown variable %q", group, name)
			}
		}
	}
	for _, profile := range profiles {
		groups, ok := registry.PCVM.Profiles[profile.ID]
		if !ok || len(groups) == 0 {
			return nil, fmt.Errorf("registry does not define profile %q", profile.ID)
		}
		for _, group := range groups {
			if group != "*" {
				if _, ok := registry.PCVM.Groups[group]; !ok {
					return nil, fmt.Errorf("registry profile %q references unknown group %q", profile.ID, group)
				}
			}
		}
	}
	return variables, nil
}

func variablesForProfile(registry registryFile, index map[string]eggVariable, profile string, providers []catalogProvider) ([]eggVariable, error) {
	groups := registry.PCVM.Profiles[profile]
	selected := make(map[string]bool)
	if len(groups) == 1 && groups[0] == "*" {
		for _, variable := range registry.Variables {
			selected[variable.EnvVariable] = true
		}
	} else {
		for _, group := range groups {
			for _, name := range registry.PCVM.Groups[group] {
				selected[name] = true
			}
		}
	}
	providerIDs := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerIDs = append(providerIDs, provider.ID)
		for _, name := range provider.Variables {
			if isPanelVariable(name) {
				continue
			}
			if _, ok := index[name]; !ok {
				return nil, fmt.Errorf("provider %q references variable %q which is absent from the Egg registry", provider.ID, name)
			}
			selected[name] = true
		}
	}
	result := make([]eggVariable, 0, len(selected))
	for _, variable := range registry.Variables {
		if !selected[variable.EnvVariable] {
			continue
		}
		switch variable.EnvVariable {
		case "SOFTWARE":
			variable.Rules = "required|string|in:interactive," + strings.Join(providerIDs, ",")
		case "ALLOWED_SOFTWARE":
			variable.DefaultValue = strings.Join(providerIDs, ",")
		}
		result = append(result, variable)
	}
	if len(result) != len(selected) {
		return nil, fmt.Errorf("profile %q variable selection is incomplete", profile)
	}
	if _, ok := index["SOFTWARE"]; !ok {
		return nil, errors.New("registry has no SOFTWARE variable")
	}
	return result, nil
}

func outputName(profile, version string, release bool) string {
	if release {
		return fmt.Sprintf("egg-pcvm-%s-%s.json", profile, version)
	}
	return "egg-pcvm-" + profile + ".json"
}

func writeOrCheck(dir string, files map[string][]byte, check bool) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if !check {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if check {
			if err := checkFile(path, files[name]); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

func checkFile(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read generated asset %s: %w", path, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("generated asset %s is stale; run go run ./cmd/egggen", path)
	}
	return nil
}

func renderProviderDocs(version, image string, providerSets map[string][]catalogProvider) []byte {
	var out strings.Builder
	out.WriteString("<!-- Code generated by cmd/egggen; DO NOT EDIT. -->\n")
	out.WriteString("# PCVM Egg and image matrix\n\n")
	out.WriteString("All assets use catalog version `" + version + "` and the same launcher contract.\n\n")
	out.WriteString("| Egg | Providers | Default image | Architectures |\n")
	out.WriteString("|---|---:|---|---|\n")
	for _, profile := range profiles {
		architectures := "AMD64, ARM64"
		if profile.ID == "games" {
			architectures = "AMD64"
		}
		out.WriteString(fmt.Sprintf("| %s | %d | `%s:%s%s` | %s |\n", profile.Title, len(providerSets[profile.ID]), image, version, profile.ImageSuffix, architectures))
	}
	for _, profile := range profiles[1:] {
		out.WriteString("\n## " + profile.Title + "\n\n")
		ids := make([]string, 0, len(providerSets[profile.ID]))
		for _, provider := range providerSets[profile.ID] {
			ids = append(ids, "`"+provider.ID+"`")
		}
		out.WriteString(strings.Join(ids, ", ") + ".\n")
	}
	return []byte(out.String())
}
