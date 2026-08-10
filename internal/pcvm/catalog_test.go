package pcvm

import (
	"regexp"
	"testing"
)

var catalogTestMemory = MemorySpec{Strategy: "cgroup-only", RecommendedMB: 128}

func TestEmbeddedCatalog(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 53 {
		t.Fatalf("providers=%d, want 53", len(c.Providers))
	}
	strategies := map[string]int{}
	for _, provider := range c.Providers {
		strategies[provider.Memory.Strategy]++
	}
	if strategies["jvm-heap"] != 17 || strategies["node-heap"] != 2 || strategies["php-limit"] != 1 ||
		strategies["deno-heap"] != 1 || strategies["go-limit"] != 1 || strategies["dotnet-gc"] != 3 ||
		strategies["qemu-guest"] != 5 || strategies["cgroup-only"] != 23 {
		t.Fatalf("unexpected memory strategy matrix: %#v", strategies)
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
	if !ok || len(vanilla.Readiness.Patterns) == 0 {
		t.Fatal("Vanilla ready pattern missing")
	}
	re, err := regexp.Compile(vanilla.Readiness.Patterns[0])
	if err != nil {
		t.Fatalf("Vanilla ready pattern is invalid: %v", err)
	}
	if !re.MatchString("[Server thread/INFO]: Done (1.234s)! For help, type \"help\"") {
		t.Fatalf("Vanilla ready pattern %q did not match a real startup line", vanilla.Readiness.Patterns[0])
	}
}

func TestCatalogAvailabilityRequiresCompatibleRuntimePack(t *testing.T) {
	catalog := Catalog{
		Providers: []ProviderSpec{
			{ID: "native", Name: "Native", Architectures: []string{"arm64"}, Runtime: "native", RuntimePolicy: RuntimePolicySpec{Allowed: []string{"native"}}},
			{ID: "php", Name: "PHP", Architectures: []string{"arm64"}, Runtime: "php-pmmp", RuntimePolicy: RuntimePolicySpec{Allowed: []string{"pmmp"}}},
		},
		RuntimePacks: []RuntimePackSpec{{Kind: "php-pmmp", Version: "pmmp", Architecture: "amd64"}},
	}
	available := catalog.Available("arm64", map[string]bool{"native": true, "php": true})
	if len(available) != 1 || available[0].ID != "native" {
		t.Fatalf("provider without ARM64 runtime was exposed: %+v", available)
	}
	catalog.RuntimePacks = append(catalog.RuntimePacks, RuntimePackSpec{Kind: "php-pmmp", Version: "pmmp", Architecture: "arm64"})
	available = catalog.Available("arm64", map[string]bool{"native": true, "php": true})
	if len(available) != 2 {
		t.Fatalf("provider with compatible runtime was hidden: %+v", available)
	}
}

func TestEmbeddedCatalogMemoryProfiles(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]MemorySpec{
		"paper":       {Strategy: "jvm-heap", RecommendedMB: 1024, HardMinimumMB: 384},
		"forge":       {Strategy: "jvm-heap", RecommendedMB: 2048, HardMinimumMB: 384},
		"velocity":    {Strategy: "jvm-heap", RecommendedMB: 512, HardMinimumMB: 384},
		"bedrock":     {Strategy: "cgroup-only", RecommendedMB: 1024},
		"pocketmine":  {Strategy: "php-limit", RecommendedMB: 512, HardMinimumMB: 256},
		"lavalink":    {Strategy: "jvm-heap", RecommendedMB: 1024, HardMinimumMB: 384},
		"nginx":       {Strategy: "cgroup-only", RecommendedMB: 128},
		"node-bot":    {Strategy: "node-heap", RecommendedMB: 256, HardMinimumMB: 256},
		"python-bot":  {Strategy: "cgroup-only", RecommendedMB: 256},
		"code-server": {Strategy: "node-heap", RecommendedMB: 512, HardMinimumMB: 256},
		"tmodloader":  {Strategy: "dotnet-gc", RecommendedMB: 1024, HardMinimumMB: 512},
		"tshock":      {Strategy: "dotnet-gc", RecommendedMB: 1024, HardMinimumMB: 512},
		"vm-alpine":   {Strategy: "qemu-guest", RecommendedMB: 1536, HardMinimumMB: 1536},
	}
	for id, want := range wants {
		provider, ok := c.Provider(id)
		if !ok || provider.Memory != want {
			t.Fatalf("provider %s memory=%+v, want %+v", id, provider.Memory, want)
		}
	}
}

func TestEmbeddedVMCatalogHasOnlyActivePinnedImages(t *testing.T) {
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
	if total != 20 || active != 20 || deprecated != 0 {
		t.Fatalf("VM images total=%d active=%d deprecated=%d", total, active, deprecated)
	}
}

func TestCatalogRejectsDuplicateAndUnpinnedRuntime(t *testing.T) {
	c := Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{{ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "x", Installer: "x", MenuPath: []string{"apps"}, Memory: catalogTestMemory}, {ID: "x", Name: "x", Family: "x", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "x", Installer: "x", MenuPath: []string{"apps"}, Memory: catalogTestMemory}}}
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
		Runtime: "native", Resolver: "test", Installer: "test", Readiness: ReadinessSpec{Mode: "regex", Patterns: []string{"Done ("}}, MenuPath: []string{"apps"}, Memory: catalogTestMemory,
	}}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected invalid ready pattern error")
	}
}

func TestCatalogRejectsInvalidSchemaFiveMetadata(t *testing.T) {
	base := ProviderSpec{ID: "test", Name: "Test", Family: "test", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "test", Installer: "test", MenuPath: []string{"apps"}, Memory: catalogTestMemory}
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

func TestCatalogRejectsInvalidMemoryMetadata(t *testing.T) {
	base := ProviderSpec{ID: "test", Name: "Test", Family: "test", Architectures: []string{"amd64"}, Runtime: "native", Resolver: "test", Installer: "test", MenuPath: []string{"apps"}, Memory: catalogTestMemory}
	for name, memory := range map[string]MemorySpec{
		"missing":      {},
		"thresholds":   {Strategy: "cgroup-only", RecommendedMB: 128, HardMinimumMB: 256},
		"unknown":      {Strategy: "dynamic", RecommendedMB: 128},
		"incompatible": {Strategy: "jvm-heap", RecommendedMB: 512, HardMinimumMB: 384},
	} {
		t.Run(name, func(t *testing.T) {
			provider := base
			provider.Memory = memory
			if err := (Catalog{Schema: CatalogSchema, Providers: []ProviderSpec{provider}}).Validate(); err == nil {
				t.Fatal("invalid memory metadata was accepted")
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
