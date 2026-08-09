package pcvm

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingSupervisor struct {
	called bool
	spec   ProcessSpec
}

func (r *recordingSupervisor) Run(_ context.Context, s ProcessSpec, _ io.Reader, _ io.Writer, _ io.Writer) error {
	r.called = true
	r.spec = s
	return nil
}

func TestInteractivePrefersExistingState(t *testing.T) {
	home := t.TempDir()
	control := home + "/.pcvm"
	state := State{Provider: "bedrock", Family: "forged-family", ResolvedVersion: "1.21.0", ResolvedBuild: "release", Architecture: "amd64"}
	if err := SaveState(control, state); err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := readJSON(filepath.Join(control, "state.json"), &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["command"] = []string{"/bin/sh", "-c", "id"}
	tampered["environment"] = []string{"LD_PRELOAD=/tmp/evil.so"}
	tampered["working_directory"] = "/tmp"
	if err := writeJSONAtomic(filepath.Join(control, "state.json"), tampered); err != nil {
		t.Fatal(err)
	}
	wantCommand := filepath.Join(control, "managed", "bedrock", "1.21.0", "bedrock_server")
	if err := os.MkdirAll(filepath.Dir(wantCommand), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantCommand, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: control, Arch: "amd64", Request: Request{Software: "interactive", AcceptEULA: true}, Policy: Policy{AllowedSoftware: map[string]bool{"bedrock": true}}}
	input := bytes.NewBufferString("invalid menu input")
	app := NewApp(cfg, catalog, input, io.Discard, io.Discard)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !supervisor.called || supervisor.spec.Command[0] != wantCommand || strings.Contains(strings.Join(supervisor.spec.Command, " "), "/bin/sh") {
		t.Fatalf("untrusted command was not rebuilt: %+v", supervisor.spec)
	}
}

func TestInteractiveMigratesLegacyStateBeforeStart(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyControlName)
	control := filepath.Join(home, ".pcvm")
	state := State{Provider: "bedrock", Family: "minecraft-bedrock-vanilla", ResolvedVersion: "1.21.0", ResolvedBuild: "release", Architecture: "amd64"}
	if err := SaveState(legacy, state); err != nil {
		t.Fatal(err)
	}
	legacyCommand := filepath.Join(legacy, "managed", "bedrock", "1.21.0", "bedrock_server")
	if err := os.MkdirAll(filepath.Dir(legacyCommand), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyCommand, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: control, Arch: "amd64", Request: Request{Software: "interactive", AcceptEULA: true}, Policy: Policy{AllowedSoftware: map[string]bool{"bedrock": true}}}
	var output bytes.Buffer
	app := NewApp(cfg, catalog, bytes.NewReader(nil), &output, &output)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCommand := filepath.Join(control, "managed", "bedrock", "1.21.0", "bedrock_server")
	if !supervisor.called || supervisor.spec.Command[0] != wantCommand {
		t.Fatalf("migrated command=%v, want %s", supervisor.spec.Command, wantCommand)
	}
	if !strings.Contains(output.String(), "migrated legacy launcher state and cache to .pcvm") {
		t.Fatalf("migration was not reported: %s", output.String())
	}
}

func TestTransitionCompatibilityAndDowngrade(t *testing.T) {
	catalog, _ := LoadCatalog(nil)
	paperSpec, _ := catalog.Provider("paper")
	purpurSpec, _ := catalog.Provider("purpur")
	state := &State{Provider: "paper", Family: paperSpec.Family, ResolvedVersion: "1.21.4"}
	if reset, reason := EvaluateTransition(state, NewProvider(purpurSpec), Resolved{Artifact: Artifact{Version: "1.21.5"}}); reset {
		t.Fatalf("Paper to Purpur reset: %s", reason)
	}
	if reset, reason := EvaluateTransition(state, NewProvider(paperSpec), Resolved{Artifact: Artifact{Version: "1.20.6"}}); !reset || reason != "downgrade requires reset" {
		t.Fatalf("downgrade result=%v %q", reset, reason)
	}
	node, _ := catalog.Provider("node-bot")
	if reset, reason := EvaluateTransition(state, NewProvider(node), Resolved{Artifact: Artifact{Version: "latest"}}); !reset || reason != "incompatible provider family" {
		t.Fatalf("cross-family result=%v %q", reset, reason)
	}
}

func TestPolicyBlocksProviderBeforeInstall(t *testing.T) {
	home := t.TempDir()
	catalog, _ := LoadCatalog(nil)
	cfg := Config{Home: home, Control: home + "/.pcvm", Arch: "amd64", Request: Request{Software: "paper", AcceptEULA: true}, Policy: Policy{AllowedSoftware: map[string]bool{"node-bot": true}}}
	app := NewApp(cfg, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	if err := app.Run(context.Background()); err == nil {
		t.Fatal("disabled provider was accepted")
	}
}

func TestPterodactylEULAGateBlocksThenStartsExistingProvider(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := State{Provider: "bedrock", Family: "minecraft-bedrock-vanilla", ResolvedVersion: "1.21.0", ResolvedBuild: "release", Architecture: "amd64"}
	command := filepath.Join(control, "managed", "bedrock", state.ResolvedVersion, "bedrock_server")
	if err := os.MkdirAll(filepath.Dir(command), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	app := NewApp(Config{Home: home, Control: control, Arch: "amd64", Request: Request{}, Policy: Policy{
		AllowedSoftware: map[string]bool{"bedrock": true},
	}}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.runState(context.Background(), state); err == nil || !strings.Contains(err.Error(), minecraftEULATrigger) {
		t.Fatalf("missing Pterodactyl EULA trigger: %v", err)
	}
	if supervisor.called {
		t.Fatal("provider started before EULA acceptance")
	}
	if err := os.WriteFile(filepath.Join(home, "eula.txt"), []byte("eula=true"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := app.runState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if !supervisor.called || supervisor.spec.Command[0] != command {
		t.Fatalf("provider did not start after Panel acceptance: %+v", supervisor.spec)
	}
}

func TestRunStateIgnoresStoredProcessMetadata(t *testing.T) {
	home := t.TempDir()
	control := home + "/.pcvm"
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := State{Provider: "bedrock", Family: "forged", ResolvedVersion: "1.21.0", ResolvedBuild: "release", Architecture: "amd64"}
	wantCommand := filepath.Join(control, "managed", "bedrock", "1.21.0", "bedrock_server")
	if err := os.MkdirAll(filepath.Dir(wantCommand), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantCommand, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	app := NewApp(Config{Home: home, Control: control, Arch: "amd64", Request: Request{WebMode: "static"}, Policy: Policy{AllowedSoftware: map[string]bool{"bedrock": true}}}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	launch, err := app.rebuildLaunchState(context.Background(), catalogSpec(t, "bedrock"), state)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Command[0] != wantCommand || strings.Contains(strings.Join(launch.Environment, "\n"), "LD_PRELOAD") || launch.StopCommand != "stop" {
		t.Fatalf("stored process metadata was trusted: %+v", launch)
	}
}

func TestInteractiveStateCannotBypassProviderPolicy(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	if err := SaveState(control, State{Provider: "bedrock", ResolvedVersion: "1", ResolvedBuild: "release", Architecture: "amd64"}); err != nil {
		t.Fatal(err)
	}
	catalog, _ := LoadCatalog(nil)
	app := NewApp(Config{Home: home, Control: control, Arch: "amd64", Request: Request{Software: "interactive"}, Policy: Policy{AllowedSoftware: map[string]bool{"paper": true}}}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	if err := app.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "disabled by host policy") {
		t.Fatalf("tampered state bypassed policy: %v", err)
	}
}

func TestRebuildLaunchStateRejectsPathTraversalTokens(t *testing.T) {
	home := t.TempDir()
	catalog, _ := LoadCatalog(nil)
	app := NewApp(Config{Home: home, Control: filepath.Join(home, ".pcvm"), Arch: "amd64"}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	spec := catalogSpec(t, "bedrock")
	for _, state := range []State{
		{Provider: "bedrock", ResolvedVersion: "../bin/sh", ResolvedBuild: "release"},
		{Provider: "bedrock", ResolvedVersion: "1.21", ResolvedBuild: `..\\evil`},
	} {
		if _, err := app.rebuildLaunchState(context.Background(), spec, state); err == nil {
			t.Fatalf("accepted traversal state: %+v", state)
		}
	}
}

func TestRebuildLaunchStateUsesFixedPathsForNewProviders(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(Config{Home: home, Control: control, Arch: "amd64"}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	tests := []struct {
		provider, version, want string
	}{
		{"samp", "v1.5.8.3079", filepath.Join(control, "managed", "samp", "v1.5.8.3079", "omp-server")},
		{"mtasa", "1.6.0", filepath.Join(control, "managed", "mtasa", "1.6.0", "mta-server64")},
		{"code-server", "v4.131.0", filepath.Join(control, "managed", "code-server", "v4.131.0", "bin", "code-server")},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			launch, err := app.rebuildLaunchState(context.Background(), catalogSpec(t, test.provider), State{
				Provider: test.provider, ResolvedVersion: test.version, ResolvedBuild: "release", Architecture: "amd64",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(launch.Command) != 1 || launch.Command[0] != test.want {
				t.Fatalf("trusted command=%v, want %s", launch.Command, test.want)
			}
		})
	}
}
