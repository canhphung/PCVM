package pcvm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func catalogSpec(t *testing.T, id string) ProviderSpec {
	t.Helper()
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := catalog.Provider(id)
	if !ok {
		t.Fatalf("missing provider %s", id)
	}
	return spec
}

func TestProviderPortRequirements(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 2456,
		Request: Request{MaxPlayers: 10, QueryPort: 2457}}
	if err := ValidateProviderRequest(catalogSpec(t, "valheim"), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Request.QueryPort = 2458
	if err := ValidateProviderRequest(catalogSpec(t, "valheim"), cfg); err == nil || !strings.Contains(err.Error(), "expected 2457") {
		t.Fatalf("wrong offset error: %v", err)
	}
	cfg.Request.QueryPort = 2456
	if err := ValidateProviderRequest(catalogSpec(t, "unturned"), cfg); err == nil {
		t.Fatal("duplicate primary/query port was accepted")
	}
}

func TestWebRequestSafety(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 8080,
		Request: Request{WebMode: "proxy", WebRoot: "public", UpstreamURL: "https://example.com"}}
	if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []string{"http://user:pass@example.com", "http://169.254.169.254/latest/meta-data", "ftp://example.com"} {
		cfg.Request.UpstreamURL = upstream
		if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err == nil {
			t.Fatalf("unsafe upstream accepted: %s", upstream)
		}
	}
	cfg.Request.WebMode, cfg.Request.WebRoot = "static", "../escape"
	if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err == nil {
		t.Fatal("escaping WEB_ROOT was accepted")
	}
}

func TestWebRootRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "public")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := validatedWebRoot(home, "public"); err == nil {
		t.Fatal("symlink WEB_ROOT was accepted")
	}
}

func TestCaddyConfigIsHTTPHostAgnosticAndStateless(t *testing.T) {
	config := caddyConfig("/home/container/.pcvm/web/caddy/conf.d", "/home/container/public", "6201", "static", "")
	for _, want := range []string{
		"\tpersist_config off\n",
		"\t\tprotocols h1\n",
		"\n:6201 {\n\tbind 0.0.0.0\n",
		"\troot * /home/container/public\n",
		"\tfile_server\n",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("Caddy config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "http://0.0.0.0") {
		t.Fatalf("Caddy config contains a Host-matching site address:\n%s", config)
	}

	proxy := caddyConfig("/extensions", "/public", "8080", "proxy", "http://upstream:3000")
	if !strings.Contains(proxy, "\treverse_proxy http://upstream:3000\n") {
		t.Fatalf("Caddy proxy config is invalid:\n%s", proxy)
	}
}

func TestGameExtraArgsCannotOverrideManagedValues(t *testing.T) {
	if args, err := safeGameExtraArgs(`-tickrate 128 +sv_cheats 0`); err != nil || len(args) != 4 {
		t.Fatalf("safe args=%v err=%v", args, err)
	}
	for _, raw := range []string{"-port=9999", "+rcon.password stolen", "+force_install_dir /tmp/game", "-TelnetPassword=bad"} {
		if _, err := safeGameExtraArgs(raw); err == nil {
			t.Fatalf("managed override accepted: %s", raw)
		}
	}
}

func TestSteamBuildIDLocations(t *testing.T) {
	root := t.TempDir()
	steamRoot := t.TempDir()
	manifest := filepath.Join(steamRoot, "steamapps", "appmanifest_730.acf")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`"AppState" { "buildid" "12345678" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	build, err := steamBuildID(root, steamRoot, "730")
	if err != nil || build != "12345678" {
		t.Fatalf("build=%q err=%v", build, err)
	}
}

func TestGeneratedAdminSecretIsPersistentAndPrivate(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm")}
	first, err := ensureAdminSecret(cfg, "game-rust")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureAdminSecret(cfg, "game-rust")
	if err != nil || first == "" || first != second {
		t.Fatalf("first=%q second=%q err=%v", first, second, err)
	}
	info, err := os.Stat(filepath.Join(cfg.Control, "secrets", "game-rust.secret"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode=%o", info.Mode().Perm())
	}
}

func TestTModLoaderBuildKeepsDotnetDLLArg(t *testing.T) {
	home := t.TempDir()
	runtime := filepath.Join(home, "dotnet")
	dll := filepath.Join(home, "game", "tModLoader.dll")
	if err := os.MkdirAll(filepath.Dir(dll), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{runtime, dll} {
		if err := os.WriteFile(path, []byte("fixture"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 7777,
		Request: Request{MaxPlayers: 8, GameWorld: "World"}}
	state := State{Provider: "tmodloader", Family: "game-tmodloader", WorkingDirectory: filepath.Join(home, "game"), Command: []string{runtime, dll}}
	process, err := NewProvider(catalogSpec(t, "tmodloader")).BuildProcess(context.Background(), cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(process.Command) < 2 || process.Command[0] != runtime || process.Command[1] != dll {
		t.Fatalf("command=%v", process.Command)
	}
}

func TestSteamInstallContractWithShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SteamCMD shim")
	}
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	runtimeRoot := filepath.Join(control, "cache", "runtimes", "steamcmd-1-amd64")
	shim := filepath.Join(runtimeRoot, "steamcmd.sh")
	if err := os.MkdirAll(runtimeRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
root=""
appid=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    +force_install_dir) shift; root="$1" ;;
    +app_update) shift; appid="$1" ;;
  esac
  shift
done
mkdir -p "$root/steamapps"
printf '"AppState" { "appid" "%s" "buildid" "424242" }\n' "$appid" > "$root/steamapps/appmanifest_${appid}.acf"
printf '#!/bin/sh\nexit 0\n' > "$root/RustDedicated"
chmod 0750 "$root/RustDedicated"
`
	if err := os.WriteFile(shim, []byte(script), 0o750); err != nil {
		t.Fatal(err)
	}
	spec := catalogSpec(t, "rust")
	provider := NewProvider(spec)
	resolved, err := provider.Resolve(context.Background(), Request{}, NewHTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	installed, err := provider.Install(context.Background(), InstallContext{
		Home: home, ControlDir: control, Runtime: shim, Request: Request{}, Out: io.Discard, Err: io.Discard,
	}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Artifact.Build != "424242" || len(installed.Command) != 1 || !strings.HasSuffix(installed.Command[0], "RustDedicated") {
		t.Fatalf("installed=%+v", installed)
	}
}

func TestNewProviderCatalogContracts(t *testing.T) {
	wantSteam := map[string]string{
		"cs2": "730", "gmod": "4020", "l4d2": "222860", "palworld": "2394010", "rust": "258550",
		"rust-umod": "258550", "project-zomboid": "380870", "valheim": "896660", "valheim-bepinex": "896660",
		"7dtd": "294420", "unturned": "1110390", "satisfactory": "1690800",
	}
	newProviders := []string{"nginx", "apache", "caddy", "cs2", "gmod", "l4d2", "palworld", "rust", "rust-umod", "project-zomboid", "valheim", "valheim-bepinex", "7dtd", "unturned", "terraria", "tmodloader", "satisfactory", "factorio", "powernukkitx", "cloudburst-nukkit", "endstone"}
	for _, id := range newProviders {
		spec := catalogSpec(t, id)
		if len(spec.MenuPath) == 0 || len(spec.Architectures) == 0 || spec.Readiness.Mode == "" || spec.Control.Mode == "" {
			t.Errorf("%s has incomplete schema-2 metadata: %+v", id, spec)
		}
		if appid := wantSteam[id]; appid != "" && spec.Options["appid"] != appid {
			t.Errorf("%s appid=%q, want %q", id, spec.Options["appid"], appid)
		}
	}
}

func TestGeneratedGameConfigs(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "game")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	template := `[/Script/Pal.PalGameWorldSettings]
OptionSettings=(ServerName="Default",ServerPassword="",AdminPassword="",ServerPlayerMaxNum=32,PublicPort=8211,RCONEnabled=False,RCONPort=25575)
`
	if err := os.WriteFile(filepath.Join(root, "DefaultPalWorldSettings.ini"), []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 8211,
		Request: Request{ServerName: `PCVM $1 "Server"`, ServerPassword: "player", MaxPlayers: 20, RCONPort: 29000, GameWorld: "World", GameMap: "Navezgane"}}
	if err := configurePalworld(root, cfg, "admin-secret"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "Pal", "Saved", "Config", "LinuxServer", "PalWorldSettings.ini"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`ServerName="PCVM $1 \"Server\""`, `ServerPlayerMaxNum=20`, `RCONEnabled=True`, `RCONPort=29000`} {
		if !strings.Contains(text, want) {
			t.Fatalf("Palworld config missing %q: %s", want, text)
		}
	}
	path, err := configure7DTD(cfg, "admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name="ServerPort" value="8211"`, `name="TelnetPassword" value="admin-secret"`, `name="GameWorld" value="Navezgane"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("7DTD config missing %q: %s", want, data)
		}
	}
}
