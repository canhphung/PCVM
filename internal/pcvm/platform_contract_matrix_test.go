package pcvm

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var expectedPlatformProviderIDs = []string{
	"7dtd", "apache", "bedrock", "bungeecord", "bun-app", "caddy", "canvas", "cloudburst-nukkit", "code-server", "cs2",
	"deno-app", "dotnet-app", "endstone", "fabric", "factorio", "folia", "forge", "gmod", "go-app", "l4d2",
	"lavalink", "modrinth-modpack", "mtasa", "neoforge", "nginx", "node-bot", "palworld", "paper", "paper-geyser", "pocketmine",
	"powernukkitx", "project-zomboid", "pufferfish", "purpur", "python-bot", "quilt", "rust", "rust-umod", "samp", "satisfactory",
	"terraria", "tmodloader", "tshock", "unturned", "valheim", "valheim-bepinex", "vanilla", "velocity", "vm-almalinux", "vm-alpine",
	"vm-debian", "vm-rocky", "vm-ubuntu",
}

// TestProviderPlatformContractMatrix is the release contract for the complete
// provider registry, not a sample-count assertion. Every shipped provider must
// bind a compiled resolver, installer, process builder and comparator; declare
// a canonical transition/runtime/preservation policy; and build a safe launch
// plan when its process driver is independent of host packages.
func TestProviderPlatformContractMatrix(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := append([]string(nil), expectedPlatformProviderIDs...)
	sort.Strings(wantIDs)
	gotIDs := make([]string, 0, len(catalog.Providers))
	for _, spec := range catalog.Providers {
		gotIDs = append(gotIDs, spec.ID)
	}
	sort.Strings(gotIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("provider registry changed without updating the contract\n got: %v\nwant: %v", gotIDs, wantIDs)
	}

	for _, spec := range catalog.Providers {
		spec := spec
		t.Run(spec.ID, func(t *testing.T) {
			drivers, err := compiledProviderDrivers(spec)
			if err != nil {
				t.Fatal(err)
			}
			if drivers.Resolver == nil || drivers.Installer == nil || drivers.Process == nil || drivers.Comparator == nil {
				t.Fatalf("incomplete compiled driver set: %#v", drivers)
			}
			if drivers.Comparator.Compare("same", "same") != 0 {
				t.Fatal("version comparator violates equality")
			}
			if spec.Family == "" || spec.VersionDomain == "" || spec.InstallFormat != 3 {
				t.Fatalf("incomplete transition identity: family=%q domain=%q format=%d", spec.Family, spec.VersionDomain, spec.InstallFormat)
			}
			if spec.RollbackMode != "staged" && spec.RollbackMode != "in-place" && spec.RollbackMode != "none" {
				t.Fatalf("invalid rollback contract %q", spec.RollbackMode)
			}
			if len(spec.Preservation.Paths) == 0 {
				t.Fatal("provider has no explicit user-data preservation policy")
			}
			assertUniqueContractPaths(t, spec.Preservation.Paths, "preserved")
			assertUniqueContractPaths(t, spec.Preservation.ManagedPaths, "managed")

			artifact := Artifact{Version: "1.21.4", Build: "1"}
			runtimeVersion, err := resolveRuntimeVersion(spec, "auto", artifact)
			if err != nil {
				t.Fatalf("runtime policy cannot resolve: %v", err)
			}
			if !contains(spec.RuntimePolicy.Allowed, runtimeVersion) {
				t.Fatalf("resolved runtime %q is outside %v", runtimeVersion, spec.RuntimePolicy.Allowed)
			}

			req := Request{Version: artifact.Version, Build: artifact.Build, RuntimeVersion: runtimeVersion}
			state := newStateFromInstall(spec, req, Resolved{Artifact: artifact, RuntimeKind: spec.Runtime, RuntimeVersion: runtimeVersion}, "amd64", testTime())
			if plan := Reconcile(&state, spec, req, nil); plan.Kind != ActionRun || plan.RequiresResolve {
				t.Fatalf("unchanged canonical state does not reconcile to run: %+v", plan)
			}

			processDriver := installerProcessDriver[spec.Installer]
			if processDriver == "" {
				processDriver = "standard"
			}
			if processDriver == "standard" {
				assertStandardLaunchContract(t, spec)
			}
		})
	}
}

func assertUniqueContractPaths(t *testing.T, paths []string, kind string) {
	t.Helper()
	seen := map[string]bool{}
	for _, path := range paths {
		clean := filepath.ToSlash(filepath.Clean(path))
		if clean == "." || filepath.IsAbs(path) || strings.HasPrefix(clean, "../") || seen[clean] {
			t.Fatalf("unsafe or duplicate %s path %q", kind, path)
		}
		seen[clean] = true
	}
}

func assertStandardLaunchContract(t *testing.T, spec ProviderSpec) {
	t.Helper()
	command := []string{"/opt/pcvm/fixture"}
	switch spec.Memory.Strategy {
	case "jvm-heap":
		command = []string{"/opt/pcvm/java", "-jar", "/opt/pcvm/server.jar"}
	case "php-limit":
		command = []string{"/opt/pcvm/php", "/opt/pcvm/server.phar"}
	}
	process, err := NewProvider(spec).BuildProcess(context.Background(), Config{AllocationPort: 25565}, LaunchState{
		Command: command, WorkingDirectory: "/home/container", Readiness: spec.Readiness, Control: spec.Control,
	}, MemoryPlan{Strategy: spec.Memory.Strategy, TargetMB: 1024})
	if err != nil {
		t.Fatalf("representative launch construction: %v", err)
	}
	if len(process.Command) == 0 || process.Directory == "" || process.Readiness.Mode != spec.Readiness.Mode || process.Control.Mode != spec.Control.Mode {
		t.Fatalf("incomplete representative process: %+v", process)
	}
}

func TestRepresentativeSpecializedProcessBuilders(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("web", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		if err := os.MkdirAll(filepath.Join(home, "public"), 0o750); err != nil {
			t.Fatal(err)
		}
		spec, _ := catalog.Provider("caddy")
		process, err := NewProvider(spec).BuildProcess(context.Background(), Config{
			Home: home, Control: control, AllocationPort: 8080, Request: Request{WebMode: "static", WebRoot: "public"},
		}, LaunchState{Command: []string{filepath.Join(home, "caddy")}}, MemoryPlan{Strategy: spec.Memory.Strategy, TargetMB: 256})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(process.Command, " "), "Caddyfile") || process.Readiness.PortVariable != "8080" {
			t.Fatalf("web process=%+v", process)
		}
	})

	t.Run("native-game", func(t *testing.T) {
		home := t.TempDir()
		control := filepath.Join(home, ".pcvm")
		if err := os.MkdirAll(control, 0o750); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(home, "game", "TerrariaServer")
		if err := os.MkdirAll(filepath.Dir(binary), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, []byte("fixture"), 0o750); err != nil {
			t.Fatal(err)
		}
		spec, _ := catalog.Provider("terraria")
		process, err := NewProvider(spec).BuildProcess(context.Background(), Config{
			Home: home, Control: control, AllocationPort: 7777,
			Request: Request{ServerName: "PCVM", MaxPlayers: 8, AdminPassword: "fixture-secret", GameWorld: "contract"},
		}, LaunchState{Command: []string{binary}, WorkingDirectory: filepath.Dir(binary)}, MemoryPlan{Strategy: spec.Memory.Strategy, TargetMB: 1024})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(process.Command, " "), "-port 7777") {
			t.Fatalf("game process=%+v", process)
		}
	})
}

func TestOfflineResolverDriverRepresentatives(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"node-bot", "nginx", "cs2", "mtasa", "vm-debian"} {
		id := id
		t.Run(id, func(t *testing.T) {
			spec, ok := catalog.Provider(id)
			if !ok {
				t.Fatal("missing provider")
			}
			resolved, err := NewProvider(spec).Resolve(context.Background(), Request{
				Version: "latest", Build: "latest", RuntimeVersion: "auto", Architecture: "amd64",
			}, NewHTTPClient())
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Artifact.Version == "" || resolved.Artifact.Build == "" || resolved.RuntimeVersion == "" {
				t.Fatalf("incomplete offline resolution: %+v", resolved)
			}
		})
	}
}
