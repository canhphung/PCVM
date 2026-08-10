package pcvm

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCompiledPlatformRegistersEveryCatalogProvider(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiledProviderContracts) != 53 || len(compiledProviderContracts) != len(catalog.Providers) {
		t.Fatalf("compiled=%d catalog=%d", len(compiledProviderContracts), len(catalog.Providers))
	}
	for _, spec := range catalog.Providers {
		drivers, err := compiledProviderDrivers(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec.ID, err)
		}
		if drivers.Resolver == nil || drivers.Installer == nil || drivers.Updater == nil || drivers.Configurator == nil ||
			drivers.Process == nil || drivers.Control == nil || drivers.Transition == nil || drivers.Validator == nil || drivers.Comparator == nil {
			t.Fatalf("%s has an incomplete driver composition: %#v", spec.ID, drivers)
		}
	}
}

func TestCompiledPlatformRejectsCatalogDriverSubstitution(t *testing.T) {
	spec := catalogSpec(t, "paper")
	spec.Resolver = "local-app"
	if _, err := compiledProviderDrivers(spec); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("resolver substitution error=%v", err)
	}
	spec = catalogSpec(t, "paper")
	spec.VersionDomain = "opaque"
	if _, err := compiledProviderDrivers(spec); err == nil || !strings.Contains(err.Error(), "version domain") {
		t.Fatalf("version-domain substitution error=%v", err)
	}
}

func TestCompiledConfiguratorAndControlAreApplied(t *testing.T) {
	spec := catalogSpec(t, "powernukkitx")
	provider := NewProvider(spec)
	state := LaunchState{
		Command:          []string{"java", "-jar", "server.jar"},
		WorkingDirectory: "/home/container",
		Readiness:        spec.Readiness,
		Control:          spec.Control,
	}
	process, err := provider.BuildProcess(context.Background(), Config{
		AllocationPort: 19132,
		Request:        Request{ServerName: "Typed PCVM", RuntimeVersion: "21"},
	}, state, MemoryPlan{Strategy: spec.Memory.Strategy, TargetMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(process.Command, " ")
	if !strings.Contains(joined, "--server-name Typed PCVM") || !strings.Contains(joined, "--port 19132") {
		t.Fatalf("compiled configurator was not applied: %v", process.Command)
	}
	if process.Readiness.Mode != spec.Readiness.Mode || process.Control.Mode != spec.Control.Mode || process.ReadyTimeout <= 0 {
		t.Fatalf("compiled control was not applied: %+v", process)
	}
	if len(state.Command) != 3 {
		t.Fatalf("configurator mutated caller state: %v", state.Command)
	}
}

func TestCompiledUpdaterDelegatesToTypedInstaller(t *testing.T) {
	spec := catalogSpec(t, "node-bot")
	provider := NewProvider(spec).(*catalogProvider)
	home := t.TempDir()
	resolved := Resolved{Artifact: Artifact{Version: "latest", Build: "latest"}, RuntimeKind: "node", RuntimeVersion: "24"}
	updated, err := provider.Update(context.Background(), InstallContext{
		Home: home, ControlDir: home, Runtime: "node", Request: Request{SourceMode: "upload", EntryFile: "index.js"},
		Log: NewLogger(io.Discard), Out: io.Discard, Err: io.Discard,
	}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Command) < 2 || updated.Command[1] != "index.js" {
		t.Fatalf("updated=%+v", updated)
	}
}

func TestStateRuntimeMustBeAllowedByProviderPolicy(t *testing.T) {
	spec := catalogSpec(t, "powernukkitx")
	state := newStateFromInstall(spec, Request{Version: "1", Build: "1", RuntimeVersion: "17"}, Resolved{
		Artifact: Artifact{Version: "1", Build: "1"}, RuntimeKind: "java", RuntimeVersion: "17",
	}, "amd64", testTime())
	catalog := Catalog{RuntimePacks: []RuntimePackSpec{{ID: "java/17/amd64", Kind: "java", Version: "17", Architecture: "amd64"}}}
	if err := validateStateAgainstCatalog(catalog, spec, state, "amd64"); err == nil || !strings.Contains(err.Error(), "runtime pack") {
		t.Fatalf("disallowed but installed runtime accepted: %v", err)
	}
}

func TestGitBranchIsInstallImmutable(t *testing.T) {
	spec := catalogSpec(t, "bun-app")
	request := Request{Version: "latest", Build: "latest", RuntimeVersion: "1", SourceMode: "git", GitURL: "https://github.com/acme/app.git", GitBranch: "main"}
	state := newStateFromInstall(spec, request, Resolved{
		Artifact: Artifact{Version: "latest", Build: "latest"}, RuntimeKind: "bun", RuntimeVersion: "1",
	}, "amd64", testTime())
	state.Family = spec.Family
	request.GitBranch = "next"
	plan := Reconcile(&state, spec, request, nil)
	if plan.Kind != ActionReset || plan.Reason != "install-immutable configuration changed" {
		t.Fatalf("branch change plan=%+v", plan)
	}
}
