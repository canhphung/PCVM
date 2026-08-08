package pcvm

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigReadsPrimaryAllocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PCVM_HOME", home)
	t.Setenv("SERVER_PORT", "30123")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AllocationPort != 30123 {
		t.Fatalf("allocation port=%d", cfg.AllocationPort)
	}
	t.Setenv("SERVER_PORT", "70000")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("invalid allocation port accepted")
	}
}

func TestJavaAllocationPreservesProperties(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "server.properties")
	before := "# keep this comment\nmotd=Custom server\nserver-port=25565\nquery.port=25565\n"
	if err := os.WriteFile(path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	app := &App{Config: Config{Home: home, AllocationPort: 30123}}
	changed, err := app.syncPrimaryAllocation(State{Provider: "vanilla"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("allocation update was not reported")
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"# keep this comment", "motd=Custom server", "server-ip=0.0.0.0", "server-port=30123", "query.port=30123"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	changed, err = app.syncPrimaryAllocation(State{Provider: "vanilla"})
	if err != nil || changed {
		t.Fatalf("idempotent update changed=%v err=%v", changed, err)
	}
}

func TestVelocityAllocationUsesJarTemplate(t *testing.T) {
	home := t.TempDir()
	jar := filepath.Join(home, "velocity.jar")
	file, err := os.Create(jar)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entry, _ := zw.Create("default-velocity.toml")
	_, _ = io.WriteString(entry, "# default\nconfig-version = \"2.8\"\nbind = \"0.0.0.0:25565\"\n[servers]\nlobby = \"127.0.0.1:25565\"\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	app := &App{Config: Config{Home: home, AllocationPort: 30124}}
	changed, err := app.syncPrimaryAllocation(State{Provider: "velocity", Command: []string{"java", "-jar", jar}})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Velocity allocation did not change")
	}
	data, _ := os.ReadFile(filepath.Join(home, "velocity.toml"))
	text := string(data)
	if !strings.Contains(text, `bind = "0.0.0.0:30124"`) || !strings.Contains(text, `lobby = "127.0.0.1:25565"`) {
		t.Fatalf("unexpected Velocity config:\n%s", text)
	}
}

func TestBungeeAllocationOnlyChangesFirstListener(t *testing.T) {
	home := t.TempDir()
	config := "listeners:\n- query_port: 25577\n  motd: primary\n  host: 0.0.0.0:25577\n- query_port: 25578\n  host: 127.0.0.1:25578\nservers:\n  lobby:\n    address: localhost:25565\n"
	if err := os.WriteFile(filepath.Join(home, "config.yml"), []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	app := &App{Config: Config{Home: home, AllocationPort: 30125}}
	if _, err := app.syncPrimaryAllocation(State{Provider: "bungeecord"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.yml"))
	text := string(data)
	for _, want := range []string{"query_port: 30125", "host: 0.0.0.0:30125", "query_port: 25578", "host: 127.0.0.1:25578", "address: localhost:25565"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestBedrockProvidersAndLavalinkAllocation(t *testing.T) {
	tests := []struct {
		provider string
		initial  string
		file     string
		want     []string
	}{
		{provider: "bedrock", file: "server.properties", initial: "server-port=19132\nserver-portv6=19133\n", want: []string{"server-port=30126", "server-portv6=19133"}},
		{provider: "pocketmine", file: "server.properties", initial: "motd=custom\n", want: []string{"motd=custom", "server-ip=0.0.0.0", "server-port=30126", "query.port=30126"}},
		{provider: "powernukkitx", file: "server.properties", initial: "motd=custom\n", want: []string{"motd=custom", "server-ip=0.0.0.0", "server-port=30126", "query.port=30126"}},
		{provider: "cloudburst-nukkit", file: "server.properties", initial: "motd=custom\n", want: []string{"motd=custom", "server-ip=0.0.0.0", "server-port=30126", "query.port=30126"}},
		{provider: "endstone", file: "server.properties", initial: "server-port=19132\nserver-portv6=19133\n", want: []string{"server-port=30126", "server-portv6=30126"}},
		{provider: "lavalink", file: "application.yml", initial: "server:\n  port: 2333\nlavalink:\n  server:\n    password: custom\n", want: []string{"port: 30126", "password: custom"}},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, test.file), []byte(test.initial), 0o640); err != nil {
				t.Fatal(err)
			}
			app := &App{Config: Config{Home: home, AllocationPort: 30126}}
			if _, err := app.syncPrimaryAllocation(State{Provider: test.provider}); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(home, test.file))
			for _, want := range test.want {
				if !strings.Contains(string(data), want) {
					t.Fatalf("missing %q in:\n%s", want, data)
				}
			}
		})
	}
}

func TestBotReceivesCommonAllocationEnvironment(t *testing.T) {
	environment := allocationEnvironment("node-bot", []string{"PORT=1", "CUSTOM=yes"}, 30127)
	joined := strings.Join(environment, "\n")
	for _, want := range []string{"SERVER_PORT=30127", "PORT=30127", "HOST=0.0.0.0", "CUSTOM=yes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, environment)
		}
	}
}

func TestTerrariaReceivesWritableUserEnvironment(t *testing.T) {
	home := t.TempDir()
	environment, err := processUserEnvironment("terraria", home, []string{"HOME=/", "CUSTOM=yes"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"CUSTOM=yes",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, environment)
		}
	}
	if strings.Contains(joined, "HOME=/\n") {
		t.Fatalf("stale root HOME in %v", environment)
	}
	if info, err := os.Stat(filepath.Join(home, ".local", "share", "Terraria")); err != nil || !info.IsDir() {
		t.Fatalf("Terraria data directory was not created: info=%v err=%v", info, err)
	}
}

func TestRunStateSyncsAllocationBeforeStart(t *testing.T) {
	home := t.TempDir()
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	app := NewApp(Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 30128}, catalog, bytes.NewReader(nil), &output, &output)
	supervisor := &recordingSupervisor{}
	app.Supervisor = supervisor
	state := State{Provider: "vanilla", Command: []string{"server"}, WorkingDirectory: home, ReadyPatterns: []string{`Done \(`}}
	if err := app.runState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if !supervisor.called {
		t.Fatal("supervisor was not called")
	}
	data, _ := os.ReadFile(filepath.Join(home, "server.properties"))
	if !strings.Contains(string(data), "server-port=30128") {
		t.Fatalf("allocation not synchronized:\n%s", data)
	}
}

func TestManagedConfigRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.properties")
	if err := os.WriteFile(outside, []byte("server-port=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "server.properties")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	app := &App{Config: Config{Home: home, AllocationPort: 30129}}
	if _, err := app.syncPrimaryAllocation(State{Provider: "vanilla"}); err == nil {
		t.Fatal("managed allocation followed a symlink")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "server-port=1\n" {
		t.Fatal("outside symlink target was modified")
	}
}
