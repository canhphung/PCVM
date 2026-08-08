package pcvm

import (
	"regexp"
	"testing"
)

func TestEmbeddedCatalog(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 32 {
		t.Fatalf("providers=%d, want 32", len(c.Providers))
	}
	if _, ok := c.Provider("waterfall"); ok {
		t.Fatal("Waterfall must not be shipped")
	}
	available := c.Available("arm64", map[string]bool{"paper": true, "bedrock": true})
	if len(available) != 1 || available[0].ID != "paper" {
		t.Fatalf("unexpected ARM64 list: %#v", available)
	}
	vanilla, ok := c.Provider("vanilla")
	if !ok || len(vanilla.ReadyPatterns) == 0 {
		t.Fatal("Vanilla ready pattern missing")
	}
	re, err := regexp.Compile(vanilla.ReadyPatterns[0])
	if err != nil {
		t.Fatalf("Vanilla ready pattern is invalid: %v", err)
	}
	if !re.MatchString("[Server thread/INFO]: Done (1.234s)! For help, type \"help\"") {
		t.Fatalf("Vanilla ready pattern %q did not match a real startup line", vanilla.ReadyPatterns[0])
	}
}

func TestCatalogRejectsDuplicateAndUnpinnedRuntime(t *testing.T) {
	c := Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{{ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "x", Installer: "x", MenuPath: []string{"apps"}}, {ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "x", Installer: "x", MenuPath: []string{"apps"}}}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected duplicate error")
	}
	c.Providers = c.Providers[:1]
	c.RuntimePacks = []RuntimePackSpec{{Kind: "java", Version: "21", Architecture: "amd64", URL: "https://example.com/x", SHA256: "bad", Executable: "bin/java"}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected checksum pin error")
	}
}

func TestCatalogRejectsInvalidReadyPattern(t *testing.T) {
	c := Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{{
		ID: "broken", Name: "Broken", Family: "test", Architectures: []string{"amd64"},
		Runtime: "native", Resolver: "test", Installer: "test", ReadyPatterns: []string{"Done ("}, MenuPath: []string{"apps"},
	}}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected invalid ready pattern error")
	}
}

func TestCatalogRejectsInvalidSchemaTwoMetadata(t *testing.T) {
	base := ProviderSpec{ID: "test", Name: "Test", Family: "test", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "test", Installer: "test", MenuPath: []string{"apps"}}
	for name, mutate := range map[string]func(*ProviderSpec){
		"menu":    func(p *ProviderSpec) { p.MenuPath = []string{"games", "unknown"} },
		"arch":    func(p *ProviderSpec) { p.Architectures = []string{"amd64", "amd64"} },
		"tcp":     func(p *ProviderSpec) { p.Readiness = ReadinessSpec{Mode: "tcp", PortVariable: "BAD_PORT"} },
		"control": func(p *ProviderSpec) { p.Control = ControlSpec{Mode: "source-rcon"} },
	} {
		t.Run(name, func(t *testing.T) {
			provider := base
			mutate(&provider)
			if err := (Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{provider}}).Validate(); err == nil {
				t.Fatal("invalid metadata was accepted")
			}
		})
	}
}
