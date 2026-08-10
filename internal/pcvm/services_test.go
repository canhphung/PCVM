package pcvm

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

func catalogMemoryPlan(t *testing.T, id string) MemoryPlan {
	t.Helper()
	spec := catalogSpec(t, id)
	plan, err := planMemory(spec.Memory, Request{VMMemoryMB: "auto"}, vmTestPolicy(), MemorySnapshot{Source: "test", LimitMB: spec.Memory.RecommendedMB})
	if err != nil {
		t.Fatal(err)
	}
	return plan
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
	cfg.AllocationPort, cfg.Request.QueryPort = 22003, 22126
	if err := ValidateProviderRequest(catalogSpec(t, "mtasa"), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Request.QueryPort = 22125
	if err := ValidateProviderRequest(catalogSpec(t, "mtasa"), cfg); err == nil || !strings.Contains(err.Error(), "expected 22126") {
		t.Fatalf("wrong MTA query offset error: %v", err)
	}
}

func TestGTAProviderConfigsUseManagedAllocation(t *testing.T) {
	home := t.TempDir()
	openmp := filepath.Join(home, "openmp")
	if err := os.MkdirAll(openmp, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(openmp, "config.json"), []byte(`{"name":"old","max_players":50,"network":{"bind":"","port":7777},"artwork":{"enable":true},"rcon":{"enable":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 7778,
		Request: Request{ServerName: `PCVM & Friends`, ServerPassword: `p<ass`, MaxPlayers: 64, QueryPort: 7901}}
	if err := configureOpenMP(openmp, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(openmp, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name": "PCVM \u0026 Friends"`, `"port": 7778`, `"max_players": 64`, `"enable": false`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("open.mp config missing %q: %s", want, data)
		}
	}
	mta := filepath.Join(home, "mtasa")
	configDir := filepath.Join(mta, "mods", "deathmatch")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatal(err)
	}
	template := `<config><servername>Old</servername><serverip>auto</serverip><serverport>22003</serverport><maxplayers>32</maxplayers><httpport>22005</httpport><ase>0</ase><password></password></config>`
	if err := os.WriteFile(filepath.Join(configDir, "mtaserver.conf"), []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configureMTA(mta, cfg); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(configDir, "mtaserver.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<servername>PCVM &amp; Friends</servername>", "<serverport>7778</serverport>", "<httpport>7778</httpport>", "<ase>1</ase>"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("MTA config missing %q: %s", want, data)
		}
	}
}

func TestCodeServerProcessUsesPrimaryPortAndPersistentPassword(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	binary := filepath.Join(home, "code-server")
	if err := os.WriteFile(binary, []byte("fixture"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, Control: control, AllocationPort: 8080, Request: Request{CodeServerPassword: "correct-horse-battery"}}
	process, err := NewProvider(catalogSpec(t, "code-server")).BuildProcess(context.Background(), cfg, LaunchState{Command: []string{binary}, WorkingDirectory: home}, catalogMemoryPlan(t, "code-server"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(process.Command, " ")
	if !strings.Contains(joined, "--bind-addr 0.0.0.0:8080") || !strings.Contains(joined, "--auth password") || !strings.Contains(joined, filepath.Join(control, "code-server", "config.yaml")) || process.Readiness.PortVariable != "8080" {
		t.Fatalf("process=%+v", process)
	}
	password, err := os.ReadFile(filepath.Join(home, "code-server-password.txt"))
	if err != nil || strings.TrimSpace(string(password)) != cfg.Request.CodeServerPassword {
		t.Fatalf("password file=%q err=%v", password, err)
	}
	if strings.Contains(joined, cfg.Request.CodeServerPassword) {
		t.Fatal("password leaked into argv")
	}
	if !contains(process.Environment, "NODE_OPTIONS=--max-old-space-size=384") {
		t.Fatalf("code-server memory environment=%v", process.Environment)
	}
}

func TestCodeServerPasswordValidation(t *testing.T) {
	cfg := Config{Home: t.TempDir(), AllocationPort: 8080, Request: Request{CodeServerPassword: "short"}}
	if err := ValidateProviderRequest(catalogSpec(t, "code-server"), cfg); err == nil {
		t.Fatal("short code-server password was accepted")
	}
}

func TestWebRequestSafety(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "public.example":
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		case "mixed.example":
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.8")}}, nil
		case "internal.example", "metadata", "metadata.google.internal":
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.8")}}, nil
		default:
			return []net.IPAddr{{IP: net.ParseIP(host)}}, nil
		}
	}
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 8080,
		Request:      Request{WebMode: "proxy", WebRoot: "public", UpstreamURL: "https://public.example/path?ok=1"},
		Dependencies: Dependencies{LookupIP: lookup}}
	if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []string{
		"http://user:pass@public.example", "ftp://public.example", "http://127.0.0.1", "http://[::1]",
		"http://10.0.0.1", "http://172.16.0.1", "http://192.168.1.1", "http://169.254.169.254/latest/meta-data",
		"http://100.64.0.1", "http://internal.example", "http://mixed.example",
		"http://public.example/; } location /leak/ { alias /; autoindex on; #",
	} {
		cfg.Request.UpstreamURL = upstream
		if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err == nil {
			t.Fatalf("unsafe upstream accepted: %s", upstream)
		}
	}
	cfg.Request.WebMode, cfg.Request.WebRoot = "static", "../escape"
	if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err == nil {
		t.Fatal("escaping WEB_ROOT was accepted")
	}
	for _, root := range []string{"public\n} include /etc/passwd;", `public"`, "public folder"} {
		cfg.Request.WebRoot = root
		if err := ValidateProviderRequest(catalogSpec(t, "nginx"), cfg); err == nil {
			t.Fatalf("unsafe WEB_ROOT accepted: %q", root)
		}
	}
}

func TestCanonicalWebProxyRejectsDNSRebindingAndConfigMetacharacters(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "rebinding.example" {
			return []net.IPAddr{{IP: net.ParseIP("192.168.50.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}
	if _, err := canonicalWebProxyWith("proxy", "https://rebinding.example", lookup); err == nil {
		t.Fatal("DNS name resolving to RFC1918 was accepted")
	}
	for _, raw := range []string{
		"http://public.example/;include/etc/passwd", "http://public.example/{x}",
		"http://public.example/$variable", "http://public.example/path#fragment",
	} {
		if _, err := canonicalWebProxyWith("proxy", raw, lookup); err == nil {
			t.Fatalf("config metacharacters accepted in %q", raw)
		}
	}
	want := "https://public.example:8443/api?x=1"
	if got, err := canonicalWebProxyWith("proxy", want, lookup); err != nil || got != want {
		t.Fatalf("canonical target=%q err=%v", got, err)
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
	config := caddyConfig("/home/container/public", "6201", "static", "")
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
	if strings.Contains(config, "import ") {
		t.Fatalf("Caddy config imports user-controlled snippets:\n%s", config)
	}

	proxy := caddyConfig("/public", "8080", "proxy", "http://127.0.0.1:31337")
	if !strings.Contains(proxy, "\treverse_proxy \"http://127.0.0.1:31337\"\n") {
		t.Fatalf("Caddy proxy config is invalid:\n%s", proxy)
	}
}

func TestGeneratedWebConfigsNeverLoadUserSnippets(t *testing.T) {
	configs := []string{
		nginxConfig("/home/container/.pcvm/managed/web/nginx", "/home/container/public", "8080", "proxy", "http://127.0.0.1:31001"),
		apacheConfig("/home/container/.pcvm/managed/web/apache", "/home/container/public", "8080", "proxy", "http://127.0.0.1:31002"),
		caddyConfig("/home/container/public", "8080", "proxy", "http://127.0.0.1:31003"),
	}
	for index, config := range configs {
		lower := strings.ToLower(config)
		if strings.Contains(lower, "conf.d") || strings.Contains(lower, "import ") {
			t.Fatalf("config %d loads user snippets:\n%s", index, config)
		}
		if !strings.Contains(config, "http://127.0.0.1:3100"+strconv.Itoa(index+1)) {
			t.Fatalf("config %d does not target the loopback safe proxy:\n%s", index, config)
		}
	}
}

func TestWebProxyConfigIsWrittenOnlyForSafeLoopbackProxy(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 8080,
		Request:      Request{WebMode: "proxy", WebRoot: "public", UpstreamURL: "https://public.example/base"},
		Dependencies: Dependencies{LookupIP: lookup}}
	process, err := NewProvider(catalogSpec(t, "caddy")).BuildProcess(context.Background(), cfg,
		LaunchState{Command: []string{os.Args[0]}}, catalogMemoryPlan(t, "caddy"))
	if err != nil {
		t.Fatal(err)
	}
	if process.BeforeStart == nil {
		t.Fatal("proxy process has no safe-proxy lifecycle hook")
	}
	cleanup, err := process.BeforeStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(filepath.Join(cfg.Control, "managed", "web", "caddy", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if strings.Contains(config, "public.example") || !strings.Contains(config, "reverse_proxy \"http://127.0.0.1:") {
		t.Fatalf("unsafe generated proxy config:\n%s", config)
	}
}

func TestGameExtraArgsCannotOverrideManagedValues(t *testing.T) {
	if args, err := safeGameExtraArgs("cs2", `-tickrate 128 +game_type 0`); err != nil || len(args) != 4 {
		t.Fatalf("safe args=%v err=%v", args, err)
	}
	for _, raw := range []string{"-port=9999", "+rcon.password stolen", "+force_install_dir /tmp/game", "-TelnetPassword=bad", "+sv_cheats 1"} {
		if _, err := safeGameExtraArgs("cs2", raw); err == nil {
			t.Fatalf("managed override accepted: %s", raw)
		}
	}
	if _, err := safeGameExtraArgs("palworld", "-useperfthreads"); err == nil {
		t.Fatal("provider without an allowlist accepted extra arguments")
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
	state := LaunchState{Provider: "tmodloader", WorkingDirectory: filepath.Join(home, "game"), Command: []string{runtime, dll}}
	process, err := NewProvider(catalogSpec(t, "tmodloader")).BuildProcess(context.Background(), cfg, state, catalogMemoryPlan(t, "tmodloader"))
	if err != nil {
		t.Fatal(err)
	}
	if len(process.Command) < 2 || process.Command[0] != runtime || process.Command[1] != dll {
		t.Fatalf("command=%v", process.Command)
	}
	if !contains(process.Environment, "DOTNET_GCHeapHardLimit=0x30000000") {
		t.Fatalf("tModLoader memory environment=%v", process.Environment)
	}
}

func TestTShockBuildKeepsDotnetDLLAndMemoryLimit(t *testing.T) {
	home := t.TempDir()
	runtime := filepath.Join(home, "dotnet")
	dll := filepath.Join(home, "game", "TShock.Server.dll")
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
	state := LaunchState{Provider: "tshock", WorkingDirectory: filepath.Join(home, "game"), Command: []string{runtime, dll}}
	process, err := NewProvider(catalogSpec(t, "tshock")).BuildProcess(context.Background(), cfg, state, catalogMemoryPlan(t, "tshock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(process.Command) < 2 || process.Command[0] != runtime || process.Command[1] != dll {
		t.Fatalf("command=%v", process.Command)
	}
	if !contains(process.Environment, "DOTNET_GCHeapHardLimit=0x30000000") {
		t.Fatalf("TShock memory environment=%v", process.Environment)
	}
}

func TestGameWorldRejectsTraversalAndBuildsManagedPath(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home, Control: filepath.Join(home, ".pcvm"), AllocationPort: 7777,
		Request: Request{MaxPlayers: 8, GameWorld: "World One"}}
	for _, value := range []string{"../outside", `..\outside`, "/absolute", `C:\outside`, ".", " world", "world\nname"} {
		cfg.Request.GameWorld = value
		if err := ValidateProviderRequest(catalogSpec(t, "terraria"), cfg); err == nil || !strings.Contains(err.Error(), "GAME_WORLD") {
			t.Errorf("unsafe GAME_WORLD %q was accepted: %v", value, err)
		}
	}
	root, target, err := managedGameWorldPath(home, "saves/Worlds", "World One", ".wld")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(home, "saves", "Worlds") || target != filepath.Join(root, "World One.wld") {
		t.Fatalf("root=%q target=%q", root, target)
	}
}

func TestGameWorldManagedDirectoryRejectsSymlink(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "saves")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, _, err := managedGameWorldPath(home, "saves/Worlds", "world", ".zip")
	if err != nil {
		t.Fatal(err)
	}
	if err := secureMkdirAll(home, root, 0o750); err == nil {
		t.Fatal("managed world directory followed a symlink")
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
	newProviders := []string{"nginx", "apache", "caddy", "cs2", "gmod", "l4d2", "samp", "mtasa", "palworld", "rust", "rust-umod", "project-zomboid", "valheim", "valheim-bepinex", "7dtd", "unturned", "terraria", "tmodloader", "satisfactory", "factorio", "powernukkitx", "cloudburst-nukkit", "endstone", "code-server"}
	for _, id := range newProviders {
		spec := catalogSpec(t, id)
		if len(spec.MenuPath) == 0 || len(spec.Architectures) == 0 || spec.Readiness.Mode == "" || spec.Control.Mode == "" {
			t.Errorf("%s has incomplete catalog metadata: %+v", id, spec)
		}
		if appid := wantSteam[id]; appid != "" && spec.Options.AppID != appid {
			t.Errorf("%s appid=%q, want %q", id, spec.Options.AppID, appid)
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
