package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRenderEggsBuildsFiveScopedAssetsAndUniversalAlias(t *testing.T) {
	catalog := fixtureCatalog()
	registry := fixtureRegistry()
	opts := options{Version: "2.0.0", Image: "ghcr.io/example/pcvm", ExportedAt: "2026-08-09T00:00:00Z"}

	files, sets, err := renderEggs(catalog, registry, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Fatalf("got %d generated files, want 6", len(files))
	}
	for profile, count := range map[string]int{"universal": 53, "minecraft": 19, "games": 18, "apps": 11, "vm": 5} {
		if got := len(sets[profile]); got != count {
			t.Errorf("%s count=%d want=%d", profile, got, count)
		}
	}
	if string(files["egg-pcvm.json"]) != string(files["egg-pcvm-universal.json"]) {
		t.Fatal("universal alias differs from canonical universal Egg")
	}
	for name, data := range files {
		var egg eggFile
		if err := json.Unmarshal(data, &egg); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if len(egg.FileDenylist) == 0 || egg.FileDenylist[0] != ".pcvm" {
			t.Errorf("%s does not protect the PCVM control directory: %v", name, egg.FileDenylist)
		}
	}

	var minecraft eggFile
	if err := json.Unmarshal(files["egg-pcvm-minecraft.json"], &minecraft); err != nil {
		t.Fatal(err)
	}
	if got := onlyImage(minecraft); got != "ghcr.io/example/pcvm:2.0.0-minecraft" {
		t.Fatalf("minecraft image=%q", got)
	}
	if len(minecraft.Features) != 1 || minecraft.Features[0] != "eula" {
		t.Fatalf("minecraft features=%v", minecraft.Features)
	}
	software := findVariable(t, minecraft, "SOFTWARE")
	if got := strings.Count(software.Rules, ","); got != 19 {
		t.Fatalf("software rules contain %d provider entries, want 19", got)
	}
	if findVariable(t, minecraft, "ALLOWED_SOFTWARE").DefaultValue != strings.TrimPrefix(software.Rules, "required|string|in:interactive,") {
		t.Fatal("software rules and admin allowlist drifted")
	}
	if hasVariable(minecraft, "WEB_MODE") {
		t.Fatal("minecraft Egg exposes an Apps-only variable")
	}

	var apps eggFile
	if err := json.Unmarshal(files["egg-pcvm-apps.json"], &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps.Features) != 0 || !hasVariable(apps, "WEB_MODE") || hasVariable(apps, "VM_CPUS") {
		t.Fatalf("unexpected Apps Egg feature/variable selection: features=%v variables=%v", apps.Features, apps.Variables)
	}
}

func TestReleaseNames(t *testing.T) {
	files, _, err := renderEggs(fixtureCatalog(), fixtureRegistry(), options{
		Version: "2.0.0", Image: "ghcr.io/example/pcvm", ExportedAt: "2026-08-09T00:00:00Z", ReleaseNames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"egg-pcvm-2.0.0.json",
		"egg-pcvm-universal-2.0.0.json",
		"egg-pcvm-minecraft-2.0.0.json",
		"egg-pcvm-games-2.0.0.json",
		"egg-pcvm-apps-2.0.0.json",
		"egg-pcvm-vm-2.0.0.json",
	} {
		if _, ok := files[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
}

func TestProviderProfileRejectsUnknownMenuRoot(t *testing.T) {
	_, err := providerProfile(catalogProvider{ID: "bad", MenuPath: []string{"databases"}})
	if err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegistryRejectsUnknownVariables(t *testing.T) {
	registry := fixtureRegistry()
	registry.PCVM.Groups["apps"] = append(registry.PCVM.Groups["apps"], "MISSING")
	if _, err := validateRegistry(registry); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProviderVariablesAreAutomaticallyAddedToItsEggScope(t *testing.T) {
	catalog := fixtureCatalog()
	catalog.Providers[0].Variables = []string{"WEB_MODE"}
	files, _, err := renderEggs(catalog, fixtureRegistry(), options{
		Version: "2.0.0", Image: "ghcr.io/example/pcvm", ExportedAt: "2026-08-09T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	var minecraft eggFile
	if err := json.Unmarshal(files["egg-pcvm-minecraft.json"], &minecraft); err != nil {
		t.Fatal(err)
	}
	if !hasVariable(minecraft, "WEB_MODE") {
		t.Fatal("catalog-declared provider variable was not added to category Egg")
	}
}

func fixtureCatalog() catalogFile {
	catalog := catalogFile{Version: "2.0.0"}
	for _, group := range []struct {
		root  string
		count int
	}{
		{"java", 19}, {"games", 18}, {"apps", 11}, {"vms", 5},
	} {
		for i := 0; i < group.count; i++ {
			catalog.Providers = append(catalog.Providers, catalogProvider{
				ID: fmt.Sprintf("%s-%02d", group.root, i), Name: fmt.Sprintf("Provider %d", i), MenuPath: []string{group.root},
			})
		}
	}
	return catalog
}

func fixtureRegistry() registryFile {
	variables := []eggVariable{
		{Name: "Software", EnvVariable: "SOFTWARE", Rules: "old"},
		{Name: "Allowed Software", EnvVariable: "ALLOWED_SOFTWARE"},
		{Name: "Reset", EnvVariable: "RESET_CONFIRM"},
		{Name: "Minecraft", EnvVariable: "MODPACK_MODE"},
		{Name: "Games", EnvVariable: "GAME_MAP"},
		{Name: "Apps", EnvVariable: "WEB_MODE"},
		{Name: "VM", EnvVariable: "VM_CPUS"},
	}
	return registryFile{
		Author: "test@example.com", ExportedAt: "2026-08-09T00:00:00Z", Variables: variables,
		PCVM: registryMeta{
			Groups: map[string][]string{
				"common": {"SOFTWARE", "RESET_CONFIRM"}, "admin": {"ALLOWED_SOFTWARE"},
				"minecraft": {"MODPACK_MODE"}, "games": {"GAME_MAP"}, "apps": {"WEB_MODE"}, "vm": {"VM_CPUS"},
			},
			Profiles: map[string][]string{
				"universal": {"*"},
				"minecraft": {"common", "minecraft", "admin"},
				"games":     {"common", "games", "admin"},
				"apps":      {"common", "apps", "admin"},
				"vm":        {"common", "vm", "admin"},
			},
		},
	}
}

func onlyImage(egg eggFile) string {
	for _, image := range egg.DockerImages {
		return image
	}
	return ""
}

func findVariable(t *testing.T, egg eggFile, env string) eggVariable {
	t.Helper()
	for _, variable := range egg.Variables {
		if variable.EnvVariable == env {
			return variable
		}
	}
	t.Fatalf("missing variable %s", env)
	return eggVariable{}
}

func hasVariable(egg eggFile, env string) bool {
	for _, variable := range egg.Variables {
		if variable.EnvVariable == env {
			return true
		}
	}
	return false
}
