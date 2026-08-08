package pcvm

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func ValidateProviderRequest(spec ProviderSpec, cfg Config) error {
	if spec.Installer == "qemu-vm" {
		return validateVMRequest(cfg)
	}
	isGame := len(spec.MenuPath) > 0 && spec.MenuPath[0] == "games"
	if isGame && (cfg.Request.MaxPlayers < 1 || cfg.Request.MaxPlayers > 512) {
		return fmt.Errorf("MAX_PLAYERS must be between 1 and 512")
	}
	if (isGame || spec.Installer == "web") && (cfg.AllocationPort < 1 || cfg.AllocationPort > 65535) {
		return fmt.Errorf("SERVER_PORT must be between 1 and 65535")
	}
	ports := map[int]string{}
	if cfg.AllocationPort > 0 {
		ports[cfg.AllocationPort] = "SERVER_PORT"
	}
	for _, requirement := range spec.Ports {
		value := requestPort(cfg, requirement.Variable)
		if value < 1 || value > 65535 {
			return fmt.Errorf("%s requires %s to be an integer between 1 and 65535", spec.Name, requirement.Variable)
		}
		if requirement.Offset != 0 && cfg.AllocationPort > 0 && value != cfg.AllocationPort+requirement.Offset {
			return fmt.Errorf("%s requires %s=SERVER_PORT%+d (expected %d)", spec.Name, requirement.Variable, requirement.Offset, cfg.AllocationPort+requirement.Offset)
		}
		if previous := ports[value]; previous != "" {
			return fmt.Errorf("%s and %s must use different ports", previous, requirement.Variable)
		}
		ports[value] = requirement.Variable
	}
	if spec.Installer == "web" {
		if _, err := validatedWebRoot(cfg.Home, cfg.Request.WebRoot); err != nil {
			return err
		}
		if err := validateWebProxy(cfg.Request.WebMode, cfg.Request.UpstreamURL); err != nil {
			return err
		}
	}
	if _, err := safeGameExtraArgs(cfg.Request.GameExtraArgs); err != nil {
		return err
	}
	return nil
}

func requestPort(cfg Config, variable string) int {
	switch variable {
	case "SERVER_PORT":
		return cfg.AllocationPort
	case "QUERY_PORT":
		return cfg.Request.QueryPort
	case "STEAM_PORT":
		return cfg.Request.SteamPort
	case "RELIABLE_PORT":
		return cfg.Request.ReliablePort
	case "GAME_PORT_2":
		return cfg.Request.GamePort2
	case "GAME_PORT_3":
		return cfg.Request.GamePort3
	case "RCON_PORT":
		return cfg.Request.RCONPort
	case "TELNET_PORT":
		return cfg.Request.TelnetPort
	default:
		return 0
	}
}

func (p *catalogProvider) installWeb(ic InstallContext, resolved Resolved) (Resolved, error) {
	root, err := validatedWebRoot(ic.Home, ic.Request.WebRoot)
	if err != nil {
		return resolved, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return resolved, err
	}
	index := filepath.Join(root, "index.html")
	if _, err := os.Stat(index); os.IsNotExist(err) {
		body := "<!doctype html><html><head><meta charset=\"utf-8\"><title>PCVM</title></head><body><h1>PCVM web server is ready</h1></body></html>\n"
		if err := os.WriteFile(index, []byte(body), 0o640); err != nil {
			return resolved, err
		}
	}
	resolved.WorkDir = ic.Home
	if p.spec.ID == "caddy" {
		resolved.Command = []string{ic.Runtime}
	} else {
		resolved.Command = []string{p.spec.ID}
	}
	return resolved, nil
}

func (p *catalogProvider) installSteam(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	root := filepath.Join(ic.Home, "game")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return resolved, err
	}
	if p.spec.Options["overlay"] == "" {
		if err := deactivateOverlay(root); err != nil {
			return resolved, err
		}
	}
	appid := p.spec.Options["appid"]
	args := []string{"+force_install_dir", root, "+login", "anonymous", "+app_update", appid, "validate", "+quit"}
	cmd := exec.CommandContext(ctx, ic.Runtime, args...)
	cmd.Dir = filepath.Dir(ic.Runtime)
	steamHome := filepath.Join(ic.ControlDir, "managed", "steam-home")
	if err := os.MkdirAll(steamHome, 0o750); err != nil {
		return resolved, err
	}
	cmd.Env = append(os.Environ(), "HOME="+steamHome)
	cmd.Stdout, cmd.Stderr = ic.Out, ic.Err
	if err := cmd.Run(); err != nil {
		return resolved, fmt.Errorf("SteamCMD app_update %s: %w", appid, err)
	}
	buildID, err := steamBuildID(root, filepath.Dir(ic.Runtime), appid)
	if err != nil {
		return resolved, err
	}
	resolved.Artifact.Version = "latest"
	resolved.Artifact.Build = buildID
	resolved.Artifact.Metadata = map[string]string{"appid": appid}
	resolved.WorkDir = root
	resolved.Command = []string{filepath.Join(root, filepath.FromSlash(p.spec.Options["executable"]))}
	if overlay := p.spec.Options["overlay"]; overlay != "" {
		if err := p.applyOverlay(ctx, ic, root, overlay); err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}

var buildIDPattern = regexp.MustCompile(`(?m)"buildid"\s+"([0-9]+)"`)

func steamBuildID(root, steamRoot, appid string) (string, error) {
	paths := []string{
		filepath.Join(root, "steamapps", "appmanifest_"+appid+".acf"),
		filepath.Join(filepath.Dir(root), "steamapps", "appmanifest_"+appid+".acf"),
		filepath.Join(steamRoot, "steamapps", "appmanifest_"+appid+".acf"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		match := buildIDPattern.FindSubmatch(data)
		if len(match) == 2 {
			return string(match[1]), nil
		}
	}
	return "", fmt.Errorf("SteamCMD completed but appmanifest_%s.acf contains no buildid", appid)
}

func (p *catalogProvider) applyOverlay(ctx context.Context, ic InstallContext, root, overlay string) error {
	var artifact Artifact
	var err error
	switch overlay {
	case "umod-rust":
		artifact, err = resolveGitHub(ctx, Request{}, ic.HTTP, "OxideMod/Oxide.Rust", `(?i)^Oxide\.Rust-linux\.zip$`)
	case "bepinex-valheim":
		artifact, err = resolveGitHub(ctx, Request{}, ic.HTTP, "BepInEx/BepInEx", `(?i)^BepInEx_linux_x64_5.*\.zip$`)
	default:
		return fmt.Errorf("unsupported overlay %q", overlay)
	}
	if err != nil {
		return err
	}
	download := filepath.Join(ic.ControlDir, "cache", "artifacts", overlay+"-"+artifact.FileName)
	artifact, err = ic.HTTP.Download(ctx, artifact, download)
	if err != nil {
		return fmt.Errorf("download %s: %w", overlay, err)
	}
	files, err := zipFileNames(download)
	if err != nil {
		return err
	}
	if err := extractZipSafe(download, root); err != nil {
		return err
	}
	marker := filepath.Join(root, ".pcvm-overlay.json")
	data, _ := json.Marshal(struct {
		Name   string   `json:"name"`
		SHA256 string   `json:"sha256"`
		Files  []string `json:"files"`
	}{overlay, artifact.SHA256, files})
	return os.WriteFile(marker, append(data, '\n'), 0o600)
}

func zipFileNames(path string) ([]string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	out := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		name := filepath.Clean(filepath.FromSlash(file.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe overlay path %q", file.Name)
		}
		if !file.FileInfo().IsDir() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func deactivateOverlay(root string) error {
	marker := filepath.Join(root, ".pcvm-overlay.json")
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var manifest struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	for _, name := range manifest.Files {
		clean := filepath.Clean(name)
		lower := strings.ToLower(filepath.ToSlash(clean))
		if strings.HasPrefix(lower, "oxide/plugins/") || strings.HasPrefix(lower, "oxide/config/") || strings.HasPrefix(lower, "bepinex/plugins/") || strings.HasPrefix(lower, "bepinex/config/") {
			continue
		}
		target := filepath.Join(root, clean)
		if target != root && strings.HasPrefix(target, root+string(filepath.Separator)) {
			_ = os.Remove(target)
		}
	}
	return os.Remove(marker)
}

func (p *catalogProvider) installTerraria(ic InstallContext, resolved Resolved) (Resolved, error) {
	root := filepath.Join(ic.Home, "game")
	tmp, err := os.MkdirTemp(ic.ControlDir, ".terraria-*")
	if err != nil {
		return resolved, err
	}
	defer os.RemoveAll(tmp)
	if err := extractZipSafe(ic.Artifact, tmp); err != nil {
		return resolved, err
	}
	linuxRoot, err := findDirectory(tmp, "Linux")
	if err != nil {
		return resolved, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return resolved, err
	}
	if err := copyTree(linuxRoot, root); err != nil {
		return resolved, err
	}
	executable := filepath.Join(root, p.spec.Options["executable"])
	if err := os.Chmod(executable, 0o750); err != nil {
		return resolved, err
	}
	resolved.WorkDir, resolved.Command = root, []string{executable}
	return resolved, nil
}

func (p *catalogProvider) installFactorio(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	root := filepath.Join(ic.Home, "game")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return resolved, err
	}
	list := exec.CommandContext(ctx, "tar", "-tJf", ic.Artifact)
	listing, err := list.Output()
	if err != nil {
		return resolved, fmt.Errorf("inspect Factorio archive: %w", err)
	}
	for _, line := range strings.Split(string(listing), "\n") {
		clean := filepath.Clean(filepath.FromSlash(line))
		if line != "" && (filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))) {
			return resolved, fmt.Errorf("unsafe Factorio archive path %q", line)
		}
	}
	cmd := exec.CommandContext(ctx, "tar", "-xJf", ic.Artifact, "--strip-components=1", "-C", root)
	cmd.Stdout, cmd.Stderr = ic.Out, ic.Err
	if err := cmd.Run(); err != nil {
		return resolved, fmt.Errorf("extract Factorio: %w", err)
	}
	executable := filepath.Join(root, p.spec.Options["executable"])
	if err := os.Chmod(executable, 0o750); err != nil {
		return resolved, err
	}
	resolved.WorkDir, resolved.Command = root, []string{executable}
	return resolved, nil
}

func (p *catalogProvider) installTModLoader(ic InstallContext, resolved Resolved) (Resolved, error) {
	root := filepath.Join(ic.Home, "game")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return resolved, err
	}
	if err := extractZipSafe(ic.Artifact, root); err != nil {
		return resolved, err
	}
	dll, err := findFile(root, p.spec.Options["executable"])
	if err != nil {
		return resolved, err
	}
	resolved.WorkDir, resolved.Command = root, []string{ic.Runtime, dll}
	return resolved, nil
}

func findDirectory(root, base string) (string, error) {
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == base {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("archive contains no %s directory", base)
	}
	return found, nil
}

func findFile(root, base string) (string, error) {
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == base {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("installation contains no %s", base)
	}
	return found, nil
}

func copyTree(source, target string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in copied tree: %s", relative)
		}
		if info.IsDir() {
			return os.MkdirAll(destination, 0o750)
		}
		return copyFile(path, destination, info.Mode().Perm())
	})
}

func (p *catalogProvider) buildServiceProcess(ctx context.Context, cfg Config, state State) (ProcessSpec, error) {
	if p.spec.Installer == "web" {
		return p.buildWebProcess(cfg, state)
	}
	return p.buildGameProcess(ctx, cfg, state)
}

func (p *catalogProvider) buildWebProcess(cfg Config, state State) (ProcessSpec, error) {
	root, err := validatedWebRoot(cfg.Home, cfg.Request.WebRoot)
	if err != nil {
		return ProcessSpec{}, err
	}
	if err := validateWebProxy(cfg.Request.WebMode, cfg.Request.UpstreamURL); err != nil {
		return ProcessSpec{}, err
	}
	managed := filepath.Join(cfg.Control, "managed", "web", p.spec.ID)
	if err := os.MkdirAll(managed, 0o750); err != nil {
		return ProcessSpec{}, err
	}
	extensions := filepath.Join(cfg.Control, "web", p.spec.ID, "conf.d")
	if err := os.MkdirAll(extensions, 0o750); err != nil {
		return ProcessSpec{}, err
	}
	port := strconv.Itoa(cfg.AllocationPort)
	var command []string
	switch p.spec.ID {
	case "nginx":
		binary, err := exec.LookPath("nginx")
		if err != nil {
			return ProcessSpec{}, err
		}
		for _, directory := range []string{"client", "proxy", "fastcgi", "uwsgi", "scgi"} {
			if err := os.MkdirAll(filepath.Join(managed, "tmp", directory), 0o750); err != nil {
				return ProcessSpec{}, err
			}
		}
		config := nginxConfig(managed, extensions, root, port, cfg.Request.WebMode, cfg.Request.UpstreamURL)
		path := filepath.Join(managed, "nginx.conf")
		if err := writeAtomicFile(path, []byte(config), 0o640); err != nil {
			return ProcessSpec{}, err
		}
		command = []string{binary, "-c", path, "-g", "daemon off;"}
	case "apache":
		binary, err := exec.LookPath("apache2")
		if err != nil {
			return ProcessSpec{}, err
		}
		config := apacheConfig(managed, extensions, root, port, cfg.Request.WebMode, cfg.Request.UpstreamURL)
		path := filepath.Join(managed, "apache2.conf")
		if err := writeAtomicFile(path, []byte(config), 0o640); err != nil {
			return ProcessSpec{}, err
		}
		command = []string{binary, "-f", path, "-DFOREGROUND"}
	case "caddy":
		binary := first(state.Command)
		if binary == "" {
			return ProcessSpec{}, fmt.Errorf("Caddy runtime path is missing")
		}
		defaultExtension := filepath.Join(extensions, "00-pcvm.caddy")
		if _, err := os.Stat(defaultExtension); os.IsNotExist(err) {
			if err := os.WriteFile(defaultExtension, []byte("# Add persistent Caddy directives in this directory.\n"), 0o640); err != nil {
				return ProcessSpec{}, err
			}
		}
		config := caddyConfig(extensions, root, port, cfg.Request.WebMode, cfg.Request.UpstreamURL)
		path := filepath.Join(managed, "Caddyfile")
		if err := writeAtomicFile(path, []byte(config), 0o640); err != nil {
			return ProcessSpec{}, err
		}
		command = []string{binary, "run", "--config", path, "--adapter", "caddyfile"}
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported web provider %q", p.spec.ID)
	}
	readiness := p.spec.Readiness
	readiness.PortVariable = strconv.Itoa(cfg.AllocationPort)
	return ProcessSpec{Command: command, Directory: cfg.Home, Environment: state.Environment, Readiness: readiness,
		Control: p.spec.Control, ReadyTimeout: time.Duration(readiness.TimeoutSeconds) * time.Second}, nil
}

func (p *catalogProvider) buildGameProcess(_ context.Context, cfg Config, state State) (ProcessSpec, error) {
	root := state.WorkingDirectory
	if root == "" {
		root = filepath.Join(cfg.Home, "game")
	}
	executable := first(state.Command)
	if p.spec.Installer == "steamcmd" {
		executable = filepath.Join(root, filepath.FromSlash(p.spec.Options["executable"]))
	}
	if p.spec.ID == "satisfactory" {
		matches, _ := filepath.Glob(filepath.Join(root, "FactoryGame", "Binaries", "Linux", "*-Linux-Shipping"))
		if len(matches) > 0 {
			executable = matches[0]
		}
	}
	if _, err := os.Stat(executable); err != nil {
		return ProcessSpec{}, fmt.Errorf("game executable: %w", err)
	}
	port := strconv.Itoa(cfg.AllocationPort)
	players := strconv.Itoa(cfg.Request.MaxPlayers)
	name, password := cfg.Request.ServerName, cfg.Request.ServerPassword
	admin, err := ensureAdminSecret(cfg, p.spec.Family)
	if err != nil {
		return ProcessSpec{}, err
	}
	gameMap := cfg.Request.GameMap
	world := cfg.Request.GameWorld
	if world == "" {
		world = "Dedicated"
	}
	command := []string{executable}
	if p.spec.ID == "tmodloader" {
		command = append([]string(nil), state.Command...)
	}
	environment := append([]string(nil), state.Environment...)
	switch p.spec.ID {
	case "cs2":
		if gameMap == "" {
			gameMap = "de_dust2"
		}
		command = append(command, "-dedicated", "-ip", "0.0.0.0", "-port", port, "-maxplayers", players, "+map", gameMap, "+hostname", name)
		if password != "" {
			command = append(command, "+sv_password", password)
		}
		if cfg.Request.SteamGSLT != "" {
			command = append(command, "+sv_setsteamaccount", cfg.Request.SteamGSLT)
		}
		environment = upsertEnvironment(environment, "LD_LIBRARY_PATH", filepath.Join(root, "game", "bin", "linuxsteamrt64"))
	case "gmod":
		if gameMap == "" {
			gameMap = "gm_construct"
		}
		command = append(command, "-game", "garrysmod", "-console", "-port", port, "+ip", "0.0.0.0", "+map", gameMap, "-strictportbind", "-norestart", "+maxplayers", players)
		command = append(command, "+hostname", name)
		if password != "" {
			command = append(command, "+sv_password", password)
		}
		if cfg.Request.SteamGSLT != "" {
			command = append(command, "+sv_setsteamaccount", cfg.Request.SteamGSLT)
		}
	case "l4d2":
		if gameMap == "" {
			gameMap = "c1m1_hotel"
		}
		command = append(command, "-game", "left4dead2", "-console", "-port", port, "+map", gameMap, "+ip", "0.0.0.0", "-strictportbind", "-norestart", "+hostname", name, "+maxplayers", players)
		if password != "" {
			command = append(command, "+sv_password", password)
		}
	case "palworld":
		if err := configurePalworld(root, cfg, admin); err != nil {
			return ProcessSpec{}, err
		}
		command = append(command, "Pal", "-useperfthreads", "-NoAsyncLoadingThread", "-UseMultithreadForDS", "-port="+port, "-publicport="+port, "-players="+players)
	case "rust", "rust-umod":
		if gameMap == "" {
			gameMap = "Procedural Map"
		}
		command = append(command, "-batchmode", "+server.ip", "0.0.0.0", "+server.port", port, "+server.identity", "pcvm", "+server.hostname", name, "+server.level", gameMap, "+server.maxplayers", players, "+rcon.ip", "127.0.0.1", "+rcon.port", strconv.Itoa(cfg.Request.RCONPort), "+rcon.web", "0", "+rcon.password", admin)
		if password != "" {
			command = append(command, "+server.password", password)
		}
		if cfg.Request.GameSeed != "" {
			command = append(command, "+server.seed", cfg.Request.GameSeed)
		}
	case "project-zomboid":
		command = append(command, "-port", port, "-udpport", strconv.Itoa(cfg.Request.SteamPort), "-cachedir="+filepath.Join(cfg.Home, ".cache"), "-servername", name, "-adminusername", "admin", "-adminpassword", admin)
		environment = upsertEnvironment(environment, "PATH", filepath.Join(root, "jre64", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		environment = upsertEnvironment(environment, "LD_LIBRARY_PATH", strings.Join([]string{filepath.Join(root, "linux64"), filepath.Join(root, "natives"), root}, ":"))
	case "valheim", "valheim-bepinex":
		command = append(command, "-nographics", "-batchmode", "-name", name, "-port", port, "-world", world, "-public", "1")
		if password != "" {
			command = append(command, "-password", password)
		}
		environment = upsertEnvironment(environment, "SteamAppId", "892970")
		if p.spec.ID == "valheim-bepinex" {
			environment = upsertEnvironment(environment, "DOORSTOP_ENABLED", "1")
			environment = upsertEnvironment(environment, "DOORSTOP_TARGET_ASSEMBLY", filepath.Join(root, "BepInEx", "core", "BepInEx.Preloader.dll"))
			environment = upsertEnvironment(environment, "LD_PRELOAD", "libdoorstop_x64.so")
		}
	case "7dtd":
		configPath, err := configure7DTD(cfg, admin)
		if err != nil {
			return ProcessSpec{}, err
		}
		logPath := filepath.Join(cfg.Home, "logs", "latest.log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
			return ProcessSpec{}, err
		}
		command = append(command, "-quit", "-batchmode", "-nographics", "-dedicated", "-configfile="+configPath, "-logfile", logPath)
	case "unturned":
		command = append(command, "-batchmode", "-nographics", "-bind", "0.0.0.0", "-port", port, "-Name", name, "+InternetServer/PCVM")
		if password != "" {
			command = append(command, "-Password", password)
		}
		if cfg.Request.SteamGSLT != "" {
			command = append(command, "-GSLT", cfg.Request.SteamGSLT)
		}
	case "terraria", "tmodloader":
		worldDir := filepath.Join(cfg.Home, "saves", "Worlds")
		if err := os.MkdirAll(worldDir, 0o750); err != nil {
			return ProcessSpec{}, err
		}
		worldPath := filepath.Join(worldDir, world+".wld")
		command = append(command, "-ip", "0.0.0.0", "-port", port, "-maxplayers", players, "-world", worldPath, "-worldname", world, "-autocreate", "2")
		if password != "" {
			command = append(command, "-password", password)
		}
	case "satisfactory":
		command = append(command, "FactoryGame", "-Port="+port, "-ReliablePort="+strconv.Itoa(cfg.Request.ReliablePort), "-ExternalReliablePort="+strconv.Itoa(cfg.Request.ReliablePort))
	case "factorio":
		saves := filepath.Join(cfg.Home, "saves")
		if err := os.MkdirAll(saves, 0o750); err != nil {
			return ProcessSpec{}, err
		}
		save := filepath.Join(saves, world+".zip")
		if _, err := os.Stat(save); os.IsNotExist(err) {
			create := exec.Command(executable, "--create", save)
			create.Dir, create.Stdout, create.Stderr = root, os.Stdout, os.Stderr
			if err := create.Run(); err != nil {
				return ProcessSpec{}, fmt.Errorf("create Factorio save: %w", err)
			}
		}
		command = append(command, "--port", port, "--start-server", save)
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported game provider %q", p.spec.ID)
	}
	extra, err := safeGameExtraArgs(cfg.Request.GameExtraArgs)
	if err != nil {
		return ProcessSpec{}, err
	}
	command = append(command, extra...)
	readiness := p.spec.Readiness
	control := p.spec.Control
	control.Password = admin
	if control.PortVariable != "" {
		control.PortVariable = strconv.Itoa(requestPort(cfg, control.PortVariable))
	}
	return ProcessSpec{Command: command, Directory: root, Environment: environment, Readiness: readiness, ReadyPatterns: readiness.Patterns,
		Control: control, StopCommand: control.StopCommand, ReadyTimeout: time.Duration(readiness.TimeoutSeconds) * time.Second}, nil
}

func configurePalworld(root string, cfg Config, admin string) error {
	target := filepath.Join(root, "Pal", "Saved", "Config", "LinuxServer", "PalWorldSettings.ini")
	data, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(root, "DefaultPalWorldSettings.ini"))
	}
	if err != nil {
		return fmt.Errorf("Palworld settings template: %w", err)
	}
	text := string(data)
	replacements := map[string]string{
		`ServerName="[^"]*"`:        `ServerName="` + iniQuoted(cfg.Request.ServerName) + `"`,
		`ServerPassword="[^"]*"`:    `ServerPassword="` + iniQuoted(cfg.Request.ServerPassword) + `"`,
		`AdminPassword="[^"]*"`:     `AdminPassword="` + iniQuoted(admin) + `"`,
		`ServerPlayerMaxNum=[0-9]+`: "ServerPlayerMaxNum=" + strconv.Itoa(cfg.Request.MaxPlayers),
		`PublicPort=[0-9]+`:         "PublicPort=" + strconv.Itoa(cfg.AllocationPort),
		`RCONEnabled=(True|False)`:  "RCONEnabled=True",
		`RCONPort=[0-9]+`:           "RCONPort=" + strconv.Itoa(cfg.Request.RCONPort),
	}
	for pattern, replacement := range replacements {
		re := regexp.MustCompile(pattern)
		if !re.MatchString(text) {
			return fmt.Errorf("Palworld settings template lacks %s", strings.Split(pattern, "=")[0])
		}
		text = re.ReplaceAllStringFunc(text, func(string) string { return replacement })
	}
	return writeAtomicFile(target, []byte(text), 0o600)
}

func iniQuoted(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

type sevenDTDProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type sevenDTDSettings struct {
	XMLName    xml.Name           `xml:"ServerSettings"`
	Properties []sevenDTDProperty `xml:"property"`
}

func configure7DTD(cfg Config, admin string) (string, error) {
	world := cfg.Request.GameWorld
	if world == "" {
		world = "PCVM"
	}
	settings := sevenDTDSettings{Properties: []sevenDTDProperty{
		{Name: "ServerName", Value: cfg.Request.ServerName},
		{Name: "ServerPassword", Value: cfg.Request.ServerPassword},
		{Name: "ServerPort", Value: strconv.Itoa(cfg.AllocationPort)},
		{Name: "ServerMaxPlayerCount", Value: strconv.Itoa(cfg.Request.MaxPlayers)},
		{Name: "TelnetEnabled", Value: "true"},
		{Name: "TelnetPort", Value: strconv.Itoa(cfg.Request.TelnetPort)},
		{Name: "TelnetPassword", Value: admin},
		{Name: "TerminalWindowEnabled", Value: "false"},
		{Name: "GameWorld", Value: envValue(cfg.Request.GameMap, "Navezgane")},
		{Name: "WorldGenSeed", Value: cfg.Request.GameSeed},
		{Name: "GameName", Value: world},
	}}
	data, err := xml.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(cfg.Control, "managed", "games", "7dtd", "serverconfig.xml")
	if err := writeAtomicFile(path, append([]byte(xml.Header), append(data, '\n')...), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func envValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func safeGameExtraArgs(raw string) ([]string, error) {
	args, err := SplitArgs(raw)
	if err != nil {
		return nil, fmt.Errorf("GAME_EXTRA_ARGS: %w", err)
	}
	protected := []string{
		"-port", "+server.port", "-serverport", "-publicport", "-queryport", "-steamport", "-udpport",
		"-gameport", "-gameport2", "-gameport3", "-rconport", "+rcon.port", "-telnetport", "-reliableport",
		"-externalreliableport", "-ip", "+server.ip", "-bind", "-force_install_dir", "+force_install_dir",
		"+login", "+app_update", "-adminpassword", "-rconpassword", "+rcon.password", "-telnetpassword",
		"-cachedir", "-logfile",
	}
	for _, arg := range args {
		lower := strings.ToLower(arg)
		for _, prefix := range protected {
			if lower == prefix || strings.HasPrefix(lower, prefix+"=") {
				return nil, fmt.Errorf("GAME_EXTRA_ARGS may not override managed option %q", arg)
			}
		}
	}
	return args, nil
}

func ensureAdminSecret(cfg Config, family string) (string, error) {
	if cfg.Request.AdminPassword != "" {
		return cfg.Request.AdminPassword, nil
	}
	dir := filepath.Join(cfg.Control, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, family+".secret")
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data)), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(bytes)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		data, readErr := os.ReadFile(path)
		return strings.TrimSpace(string(data)), readErr
	}
	if err != nil {
		return "", err
	}
	_, writeErr := io.WriteString(file, secret+"\n")
	closeErr := file.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return secret, nil
}

func validatedWebRoot(home, relative string) (string, error) {
	if relative == "" {
		relative = "public"
	}
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("WEB_ROOT must stay inside /home/container")
	}
	root := filepath.Join(home, clean)
	homeClean := filepath.Clean(home)
	if root != homeClean && !strings.HasPrefix(root, homeClean+string(filepath.Separator)) {
		return "", fmt.Errorf("WEB_ROOT escapes /home/container")
	}
	for current := root; current != homeClean; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("WEB_ROOT may not traverse symlinks")
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return root, nil
}

func validateWebProxy(mode, upstream string) error {
	if mode != "static" && mode != "proxy" {
		return fmt.Errorf("WEB_MODE must be static or proxy")
	}
	if mode == "static" {
		return nil
	}
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return fmt.Errorf("UPSTREAM_URL must be an HTTP(S) URL without credentials")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "metadata.google.internal" || host == "metadata" {
		return fmt.Errorf("UPSTREAM_URL may not target a metadata service")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return fmt.Errorf("UPSTREAM_URL may not target link-local or unspecified addresses")
	}
	return nil
}

func nginxConfig(managed, extensions, root, port, mode, upstream string) string {
	location := "try_files $uri $uri/ =404;"
	if mode == "proxy" {
		location = "proxy_pass " + upstream + ";\n            proxy_set_header Host $host;\n            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;"
	}
	return fmt.Sprintf("worker_processes 1;\npid %s;\nerror_log /dev/stderr info;\nevents { worker_connections 1024; }\nhttp {\n    include /etc/nginx/mime.types;\n    access_log /dev/stdout;\n    client_body_temp_path %s;\n    proxy_temp_path %s;\n    fastcgi_temp_path %s;\n    uwsgi_temp_path %s;\n    scgi_temp_path %s;\n    server {\n        listen 0.0.0.0:%s;\n        root %s;\n        index index.html;\n        location / { %s }\n        include %s/*.conf;\n    }\n}\n", filepath.ToSlash(filepath.Join(managed, "nginx.pid")), filepath.ToSlash(filepath.Join(managed, "tmp", "client")), filepath.ToSlash(filepath.Join(managed, "tmp", "proxy")), filepath.ToSlash(filepath.Join(managed, "tmp", "fastcgi")), filepath.ToSlash(filepath.Join(managed, "tmp", "uwsgi")), filepath.ToSlash(filepath.Join(managed, "tmp", "scgi")), port, filepath.ToSlash(root), location, filepath.ToSlash(extensions))
}

func apacheConfig(managed, extensions, root, port, mode, upstream string) string {
	location := fmt.Sprintf("<Directory %q>\nRequire all granted\n</Directory>\n", root)
	if mode == "proxy" {
		location = fmt.Sprintf("ProxyRequests Off\nProxyPass / %s\nProxyPassReverse / %s\n", upstream, upstream)
	}
	return fmt.Sprintf("ServerRoot /etc/apache2\nDefaultRuntimeDir %s\nPidFile %s\nMutex file:%s default\nListen 0.0.0.0:%s\nServerName localhost\nIncludeOptional /etc/apache2/mods-enabled/*.load\nIncludeOptional /etc/apache2/mods-enabled/*.conf\nTypesConfig /etc/mime.types\nErrorLog /proc/self/fd/2\nLogLevel warn\nLogFormat \"%%h %%l %%u %%t \\\"%%r\\\" %%>s %%b\" combined\nCustomLog /proc/self/fd/1 combined\nDocumentRoot %q\n%sIncludeOptional %s/*.conf\n", managed, filepath.Join(managed, "apache.pid"), managed, port, root, location, extensions)
}

func caddyConfig(extensions, root, port, mode, upstream string) string {
	body := fmt.Sprintf("\troot * %s\n\tfile_server", filepath.ToSlash(root))
	if mode == "proxy" {
		body = "\treverse_proxy " + upstream
	}
	return fmt.Sprintf("{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n\tservers {\n\t\tprotocols h1\n\t}\n}\n\n:%s {\n\tbind 0.0.0.0\n%s\n\timport %s/*.caddy\n}\n", port, body, filepath.ToSlash(extensions))
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pcvm-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
