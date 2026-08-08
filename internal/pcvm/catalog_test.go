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
	if len(c.Providers) != 14 {
		t.Fatalf("providers=%d, want 14", len(c.Providers))
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
	c := Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{{ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Resolver: "x", Installer: "x"}, {ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Resolver: "x", Installer: "x"}}}
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
		Resolver: "test", Installer: "test", ReadyPatterns: []string{"Done ("},
	}}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected invalid ready pattern error")
	}
}
