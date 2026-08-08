package pcvm

import (
	"bytes"
	"context"
	"io"
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
	state := State{Provider: "paper", Family: "minecraft-java-bukkit", Command: []string{"existing-server"}, WorkingDirectory: home}
	if err := SaveState(control, state); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: control, Arch: "amd64", Request: Request{Software: "interactive"}, Policy: Policy{AllowedSoftware: map[string]bool{"paper": true}}}
	input := bytes.NewBufferString("invalid menu input")
	app := NewApp(cfg, catalog, input, io.Discard, io.Discard)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !supervisor.called || supervisor.spec.Command[0] != "existing-server" {
		t.Fatalf("state was not selected: %+v", supervisor.spec)
	}
}

func TestInteractiveMigratesLegacyStateBeforeStart(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, legacyControlName)
	control := filepath.Join(home, ".pcvm")
	legacyCommand := filepath.Join(legacy, "managed", "server")
	state := State{Provider: "paper", Family: "minecraft-java-bukkit", Command: []string{legacyCommand}, WorkingDirectory: home, ReadyPatterns: []string{`Done \(`}}
	if err := SaveState(legacy, state); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: control, Arch: "amd64", Request: Request{Software: "interactive"}, Policy: Policy{AllowedSoftware: map[string]bool{"paper": true}}}
	var output bytes.Buffer
	app := NewApp(cfg, catalog, bytes.NewReader(nil), &output, &output)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCommand := filepath.Join(control, "managed", "server")
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

func TestRunStateRepairsInvalidStoredReadyPattern(t *testing.T) {
	home := t.TempDir()
	control := home + "/.pcvm"
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := State{Provider: "vanilla", Family: "minecraft-java-vanilla", Command: []string{"server"}, WorkingDirectory: home, ReadyPatterns: []string{"Done ("}}
	if err := SaveState(control, state); err != nil {
		t.Fatal(err)
	}
	app := NewApp(Config{Home: home, Control: control}, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	if err := app.runState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if !supervisor.called || len(supervisor.spec.ReadyPatterns) == 0 || supervisor.spec.ReadyPatterns[0] != `Done \(` {
		t.Fatalf("stored pattern was not repaired: %+v", supervisor.spec.ReadyPatterns)
	}
	repaired, err := LoadState(control)
	if err != nil {
		t.Fatal(err)
	}
	if repaired == nil || len(repaired.ReadyPatterns) == 0 || repaired.ReadyPatterns[0] != `Done \(` {
		t.Fatalf("repaired state was not persisted: %+v", repaired)
	}
}
