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
	if len(c.Providers) != 43 {
		t.Fatalf("providers=%d, want 43", len(c.Providers))
	}
	if _, ok := c.Provider("waterfall"); ok {
		t.Fatal("Waterfall must not be shipped")
	}
	if _, ok := c.Provider("levilamina"); ok {
		t.Fatal("Windows-only LeviLamina must not be offered by the Linux launcher")
	}
	available := c.Available("arm64", map[string]bool{"paper": true, "bedrock": true})
	if len(available) != 1 || available[0].ID != "paper" {
		t.Fatalf("unexpected ARM64 list: %#v", available)
	}
	bedrockARM := c.Available("arm64", map[string]bool{"powernukkitx": true, "cloudburst-nukkit": true, "endstone": true})
	if len(bedrockARM) != 2 || bedrockARM[0].ID != "cloudburst-nukkit" || bedrockARM[1].ID != "powernukkitx" {
		t.Fatalf("unexpected Bedrock ARM64 list: %#v", bedrockARM)
	}
	appsARM := c.Available("arm64", map[string]bool{"samp": true, "mtasa": true, "code-server": true})
	if len(appsARM) != 1 || appsARM[0].ID != "code-server" {
		t.Fatalf("unexpected new provider ARM64 list: %#v", appsARM)
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

func TestEmbeddedVMCatalogHasActiveAndLegacyPinnedImages(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	total, active, deprecated := 0, 0, 0
	imageIDs := map[string]bool{}
	for _, id := range []string{"vm-ubuntu", "vm-debian", "vm-almalinux", "vm-rocky", "vm-alpine"} {
		provider, ok := c.Provider(id)
		if !ok || provider.Installer != "qemu-vm" || len(provider.VMImages) < 4 {
			t.Fatalf("invalid VM provider %s: %#v", id, provider)
		}
		seen := map[string]bool{}
		for _, image := range provider.VMImages {
			key := image.Version + "/" + image.Architecture
			if image.ID == "" || image.Variant == "" || imageIDs[image.ID] || image.URL == "" || image.Build == "" || (image.SHA256 == "" && image.SHA512 == "") {
				t.Fatalf("invalid VM image %s/%s: %#v", id, key, image)
			}
			imageIDs[image.ID] = true
			if image.Deprecated {
				deprecated++
			} else {
				if seen[key] {
					t.Fatalf("duplicate active VM image %s/%s", id, key)
				}
				seen[key] = true
				active++
			}
			total++
		}
	}
	if total != 24 || active != 20 || deprecated != 4 {
		t.Fatalf("VM images total=%d active=%d deprecated=%d", total, active, deprecated)
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

func TestCatalogRejectsInvalidSchemaFourMetadata(t *testing.T) {
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

func TestCatalogRejectsMutableOrUnpinnedVMImages(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := c.Provider("vm-debian")
	provider.VMImages = append([]VMImageSpec(nil), provider.VMImages...)
	provider.VMImages[0].URL = "https://cloud.debian.org/images/cloud/bookworm/latest/image.qcow2"
	if err := (Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{provider}}).Validate(); err == nil {
		t.Fatal("mutable VM image URL was accepted")
	}
	provider, _ = c.Provider("vm-debian")
	provider.VMImages = append([]VMImageSpec(nil), provider.VMImages...)
	provider.VMImages[0].SHA512 = "bad"
	if err := (Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{provider}}).Validate(); err == nil {
		t.Fatal("unpinned VM checksum was accepted")
	}
	provider, _ = c.Provider("vm-debian")
	provider.VMImages = append([]VMImageSpec(nil), provider.VMImages...)
	provider.VMImages[1].ID = provider.VMImages[0].ID
	if err := (Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{provider}}).Validate(); err == nil {
		t.Fatal("duplicate VM image ID was accepted")
	}
}
