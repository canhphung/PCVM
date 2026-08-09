package pcvm

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func menuTestApp(t *testing.T, input string, allowed map[string]bool) (*App, *bytes.Buffer) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cfg := Config{Arch: "amd64", Policy: Policy{AllowedSoftware: allowed, BrandName: "PCVM", AllowSystemPath: true}}
	return NewApp(cfg, catalog, strings.NewReader(input), output, io.Discard), output
}

func TestGroupedMenuRendersFigletAndSelectsProvider(t *testing.T) {
	app, output := menuTestApp(t, "1\n1\n", map[string]bool{
		"paper": true, "velocity": true, "bedrock": true, "node-bot": true,
	})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "paper" {
		t.Fatalf("selected=%q, want alphabetically first Java provider paper", selected)
	}
	text := output.String()
	for _, want := range []string{"____  _______", "SELECT A SOFTWARE CATEGORY", "Minecraft Java", "Minecraft Proxies", "Minecraft Bedrock", "Applications & Bots", "Selected: Paper [paper]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("menu missing %q:\n%s", want, text)
		}
	}
}

func TestGroupedMenuCanGoBack(t *testing.T) {
	app, _ := menuTestApp(t, "1\nb\n2\n1\n", map[string]bool{"paper": true, "velocity": true, "bungeecord": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "bungeecord" {
		t.Fatalf("selected=%q, want bungeecord", selected)
	}
}

func TestGroupedMenuHidesEmptyCategoriesAndRetries(t *testing.T) {
	app, output := menuTestApp(t, "invalid\n1\n99\n1\n", map[string]bool{"paper": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "paper" {
		t.Fatalf("selected=%q", selected)
	}
	text := output.String()
	if strings.Contains(text, "Minecraft Proxies") || strings.Contains(text, "Minecraft Bedrock") {
		t.Fatalf("empty categories were rendered:\n%s", text)
	}
	if strings.Count(text, "[PCVM]") != 2 {
		t.Fatalf("invalid choices were not retried:\n%s", text)
	}
}

func TestGameMenuUsesThreeLevels(t *testing.T) {
	app, output := menuTestApp(t, "1\n2\n1\n", map[string]bool{"cs2": true, "rust": true, "factorio": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "rust" {
		t.Fatalf("selected=%q, want rust", selected)
	}
	text := output.String()
	for _, want := range []string{"Game Servers", "Source & FPS", "Survival", "Sandbox & Automation", "Selected: Rust [rust]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("game menu missing %q:\n%s", want, text)
		}
	}
}

func TestVMMenuUsesThreeLevels(t *testing.T) {
	app, output := menuTestApp(t, "1\n2\n1\n", map[string]bool{"vm-ubuntu": true, "vm-debian": true, "vm-rocky": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "vm-rocky" {
		t.Fatalf("selected=%q, want vm-rocky", selected)
	}
	text := output.String()
	for _, want := range []string{"Virtual Machines", "Debian Family", "Enterprise Linux", "Selected: Rocky Linux [vm-rocky]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("VM menu missing %q:\n%s", want, text)
		}
	}
}

func TestVMMenuOffersLightweightAlpine(t *testing.T) {
	app, output := menuTestApp(t, "1\n1\n1\n", map[string]bool{"vm-alpine": true, "vm-ubuntu": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "vm-alpine" {
		t.Fatalf("selected=%q, want vm-alpine", selected)
	}
	for _, want := range []string{"Virtual Machines", "Lightweight Linux", "Selected: Alpine Linux [vm-alpine]"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("VM menu missing %q:\n%s", want, output.String())
		}
	}
}
