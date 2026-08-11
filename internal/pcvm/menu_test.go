package pcvm

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
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

func TestFreshInteractiveServerShutsDownCleanlyAfterMenuTimeout(t *testing.T) {
	home := t.TempDir()
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cfg := Config{
		Home: home, Control: home + "/.pcvm", Arch: "amd64", ImageProfile: ImageProfileFull,
		Request: Request{Software: "interactive"},
		Policy:  Policy{AllowedSoftware: map[string]bool{"paper": true}, BrandName: "PCVM", AllowSystemPath: true},
	}
	app := NewApp(cfg, catalog, reader, output, io.Discard)
	app.MenuTimeout = 25 * time.Millisecond
	app.ResetRoot = home

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("interactive timeout must be a clean shutdown: %v", err)
	}
	if !strings.Contains(output.String(), "No software was selected within 25ms; shutting down cleanly") {
		t.Fatalf("timeout shutdown was not explained to the user:\n%s", output.String())
	}
	state, err := LoadState(cfg.Control)
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("menu timeout unexpectedly installed state: %#v", state)
	}
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
	for _, want := range []string{"____  _______", "server shuts down after 5 minutes", "SELECT A SOFTWARE CATEGORY", "Minecraft Java", "Minecraft Proxies", "Minecraft Bedrock", "Applications & Bots", "Selected: Paper [paper]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("menu missing %q:\n%s", want, text)
		}
	}
}

func TestMenuFramesAreContinuousAndFixedWidth(t *testing.T) {
	app, output := menuTestApp(t, "1\n1\n", map[string]bool{
		"velocity": true, "bungeecord": true,
	})
	if _, err := app.menu(); err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(output.String(), "\n") {
		if !strings.HasPrefix(line, "╭") && !strings.HasPrefix(line, "│") && !strings.HasPrefix(line, "╰") {
			continue
		}
		if width := len([]rune(line)); width != menuFrameInnerWidth+2 {
			t.Fatalf("frame line width=%d, want %d: %q", width, menuFrameInnerWidth+2, line)
		}
		if strings.HasPrefix(line, "╭") {
			lastSpace := strings.LastIndex(line, " ")
			if lastSpace < 0 || !strings.HasSuffix(line[lastSpace+1:], "╮") || strings.TrimSuffix(line[lastSpace+1:], "╮") == "" {
				t.Fatalf("top border does not continue after its title: %q", line)
			}
			if strings.Trim(strings.TrimSuffix(line[lastSpace+1:], "╮"), "─") != "" {
				t.Fatalf("top border contains a gap after its title: %q", line)
			}
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

func TestMenuFiltersProvidersByEmbeddedImageProfile(t *testing.T) {
	app, output := menuTestApp(t, "1\n1\n", map[string]bool{"paper": true, "rust": true, "vm-debian": true})
	app.Config.ImageProfile = ImageProfileMinecraft
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "paper" {
		t.Fatalf("selected=%q", selected)
	}
	text := output.String()
	if strings.Contains(text, "Game Servers") || strings.Contains(text, "Virtual Machines") {
		t.Fatalf("core image exposed unavailable categories:\n%s", text)
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

func TestGameMenuOffersGTAMultiplayer(t *testing.T) {
	app, output := menuTestApp(t, "1\n1\n1\n", map[string]bool{"samp": true, "mtasa": true})
	selected, err := app.menu()
	if err != nil {
		t.Fatal(err)
	}
	if selected != "mtasa" {
		t.Fatalf("selected=%q, want alphabetically first MTA provider", selected)
	}
	for _, want := range []string{"Game Servers", "GTA Multiplayer", "SA-MP / open.mp", "Selected: Multi Theft Auto [mtasa]"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("GTA menu missing %q:\n%s", want, output.String())
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
