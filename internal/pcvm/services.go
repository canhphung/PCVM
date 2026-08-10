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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func ValidateProviderRequest(spec ProviderSpec, cfg Config) error {
	if spec.Installer == "qemu-vm" {
		return validateVMRequest(spec, cfg)
	}
	isGame := len(spec.MenuPath) > 0 && spec.MenuPath[0] == "games"
	if isGame && (cfg.Request.MaxPlayers < 1 || cfg.Request.MaxPlayers > 512) {
		return fmt.Errorf("MAX_PLAYERS must be between 1 and 512")
	}
	if isGame {
		if err := validateGameWorldName(cfg.Request.GameWorld); err != nil {
			return err
		}
	}
	if (isGame || spec.Installer == "web" || spec.Installer == "code-server") && (cfg.AllocationPort < 1 || cfg.AllocationPort > 65535) {
		return fmt.Errorf("SERVER_PORT must be between 1 and 65535")
	}
	if spec.Installer == "code-server" && cfg.Request.CodeServerPassword != "" {
		password := cfg.Request.CodeServerPassword
		if len(password) < 12 || len(password) > 128 || strings.ContainsAny(password, "\x00\r\n") {
			return fmt.Errorf("CODE_SERVER_PASSWORD must contain 12 to 128 characters without newlines")
		}
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
		if err := validateWebProxyWith(cfg.Request.WebMode, cfg.Request.UpstreamURL, cfg.Dependencies.withDefaults().LookupIP); err != nil {
			return err
		}
	}
	if _, err := safeGameExtraArgs(spec.ID, cfg.Request.GameExtraArgs); err != nil {
		return err
	}
	return nil
}

func validateGameWorldName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 128 || name == "." || name == ".." || strings.TrimSpace(name) != name || strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("GAME_WORLD must be a single managed world name without path separators or traversal")
	}
	for _, character := range name {
		if character == 0 || unicode.IsControl(character) {
			return fmt.Errorf("GAME_WORLD must not contain control characters")
		}
	}
	return nil
}

func managedGameWorldPath(home, relativeDirectory, name, extension string) (string, string, error) {
	if err := validateGameWorldName(name); err != nil {
		return "", "", err
	}
	if name == "" {
		return "", "", fmt.Errorf("GAME_WORLD must not be empty when used as a save path")
	}
	root := filepath.Join(filepath.Clean(home), filepath.FromSlash(relativeDirectory))
	target := filepath.Join(root, name+extension)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("GAME_WORLD path escapes its managed directory")
	}
	return root, target, nil
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
	if err := secureMkdirAll(ic.Home, root, 0o750); err != nil {
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
	if err := secureMkdirAll(ic.Home, root, 0o750); err != nil {
		return resolved, err
	}
	if p.spec.Options.Overlay == "" {
		if err := deactivateOverlay(root); err != nil {
			return resolved, err
		}
	}
	appid := p.spec.Options.AppID
	args := []string{"+force_install_dir", root, "+login", "anonymous", "+app_update", appid, "validate", "+quit"}
	cmd := exec.CommandContext(ctx, ic.Runtime, args...)
	cmd.Dir = filepath.Dir(ic.Runtime)
	steamHome := filepath.Join(ic.ControlDir, "managed", "steam-home")
	if err := secureMkdirAll(ic.ControlDir, steamHome, 0o750); err != nil {
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
	resolved.Command = []string{filepath.Join(root, filepath.FromSlash(p.spec.Options.Executable))}
	if overlay := p.spec.Options.Overlay; overlay != "" {
		if err := p.applyOverlay(ctx, ic, root, overlay); err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}

func (p *catalogProvider) installOpenMP(ic InstallContext, resolved Resolved) (Resolved, error) {
	if err := validateStatePathToken("open.mp version", resolved.Artifact.Version); err != nil {
		return resolved, err
	}
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	staged, err := os.MkdirTemp(managed, ".extract-*")
	if err != nil {
		return resolved, err
	}
	defer os.RemoveAll(staged)
	if err := extractRuntime(ic.Artifact, staged, "tar.gz"); err != nil {
		return resolved, fmt.Errorf("extract open.mp: %w", err)
	}
	source := filepath.Join(staged, "Server")
	if err := requireRegularExecutable(filepath.Join(source, "omp-server")); err != nil {
		return resolved, fmt.Errorf("open.mp archive: %w", err)
	}
	candidate, err := moveIntoManagedCandidate(managed, source)
	if err != nil {
		return resolved, err
	}
	candidateLive := true
	defer func() {
		if candidateLive {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := linkMutableData(ic.Home, candidate,
		[]string{"gamemodes", "filterscripts", "scriptfiles", "models", "npcmodes", "recordings"},
		[]string{"config.json", "bans.json"}); err != nil {
		return resolved, err
	}
	resolved.WorkDir = candidate
	resolved.Command = []string{filepath.Join(candidate, "omp-server")}
	candidateLive = false
	return resolved, nil
}

func (p *catalogProvider) installMTA(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	if err := validateStatePathToken("MTA version", resolved.Artifact.Version); err != nil {
		return resolved, err
	}
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	staged, err := os.MkdirTemp(managed, ".extract-*")
	if err != nil {
		return resolved, err
	}
	defer os.RemoveAll(staged)
	if err := extractRuntime(ic.Artifact, staged, "tar.gz"); err != nil {
		return resolved, fmt.Errorf("extract MTA server: %w", err)
	}
	source := filepath.Join(staged, "multitheftauto_linux_x64")
	if err := requireRegularExecutable(filepath.Join(source, "mta-server64")); err != nil {
		return resolved, fmt.Errorf("MTA server archive: %w", err)
	}
	deathmatch := filepath.Join(ic.Home, "mods", "deathmatch")
	if err := secureMkdirAll(ic.Home, deathmatch, 0o750); err != nil {
		return resolved, err
	}
	baseArchive, err := p.downloadPinnedOption(ctx, ic, "base", "tar.gz")
	if err != nil {
		return resolved, err
	}
	baseStage := filepath.Join(staged, "base-stage")
	if err := os.MkdirAll(baseStage, 0o750); err != nil {
		return resolved, err
	}
	if err := extractRuntime(baseArchive, baseStage, "tar.gz"); err != nil {
		return resolved, fmt.Errorf("extract MTA base config: %w", err)
	}
	if err := copyTreeMissing(filepath.Join(baseStage, "baseconfig"), deathmatch); err != nil {
		return resolved, fmt.Errorf("install MTA base config: %w", err)
	}
	resourcesArchive, err := p.downloadPinnedOption(ctx, ic, "resources", "zip")
	if err != nil {
		return resolved, err
	}
	resourcesStage := filepath.Join(staged, "resources-stage")
	if err := os.MkdirAll(resourcesStage, 0o750); err != nil {
		return resolved, err
	}
	if err := extractZipSafe(resourcesArchive, resourcesStage); err != nil {
		return resolved, fmt.Errorf("extract MTA resources: %w", err)
	}
	if err := copyTreeMissing(resourcesStage, filepath.Join(deathmatch, "resources")); err != nil {
		return resolved, fmt.Errorf("install MTA resources: %w", err)
	}
	candidate, err := moveIntoManagedCandidate(managed, source)
	if err != nil {
		return resolved, fmt.Errorf("stage MTA server: %w", err)
	}
	candidateLive := true
	defer func() {
		if candidateLive {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := linkMutableData(ic.Home, candidate, []string{"mods"}, nil); err != nil {
		return resolved, err
	}
	resolved.WorkDir = candidate
	resolved.Command = []string{filepath.Join(candidate, "mta-server64")}
	candidateLive = false
	return resolved, nil
}

func (p *catalogProvider) downloadPinnedOption(ctx context.Context, ic InstallContext, name, kind string) (string, error) {
	rawURL, checksum := p.spec.Options.PinnedArtifact(name)
	if rawURL == "" || !validHexDigest(checksum, 64) {
		return "", fmt.Errorf("%s catalog artifact is not pinned", name)
	}
	fileName := "mtasa-" + name + "." + kind
	target := filepath.Join(ic.ControlDir, "cache", "artifacts", fileName)
	if err := secureMkdirAll(ic.ControlDir, filepath.Dir(target), 0o750); err != nil {
		return "", err
	}
	_, err := ic.HTTP.Download(ctx, Artifact{URL: rawURL, FileName: fileName, Kind: kind, SHA256: checksum}, target)
	if err != nil {
		return "", fmt.Errorf("download MTA %s: %w", name, err)
	}
	return target, nil
}

func (p *catalogProvider) installCodeServer(ic InstallContext, resolved Resolved) (Resolved, error) {
	if err := validateStatePathToken("code-server version", resolved.Artifact.Version); err != nil {
		return resolved, err
	}
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	staged, err := os.MkdirTemp(managed, ".extract-*")
	if err != nil {
		return resolved, err
	}
	defer os.RemoveAll(staged)
	if err := extractRuntime(ic.Artifact, staged, "tar.gz"); err != nil {
		return resolved, fmt.Errorf("extract code-server: %w", err)
	}
	matches, err := filepath.Glob(filepath.Join(staged, "code-server-*-linux-*"))
	if err != nil || len(matches) != 1 {
		return resolved, fmt.Errorf("code-server archive has an unexpected layout")
	}
	if err := requireRegularExecutable(filepath.Join(matches[0], "bin", "code-server")); err != nil {
		return resolved, fmt.Errorf("code-server archive: %w", err)
	}
	candidate, err := moveIntoManagedCandidate(managed, matches[0])
	if err != nil {
		return resolved, err
	}
	resolved.WorkDir = candidate
	resolved.Command = []string{filepath.Join(candidate, "bin", "code-server")}
	return resolved, nil
}

// moveIntoManagedCandidate gives a fully extracted tree a unique, unpublished
// location. The ReleaseStore consumes this directory only after the provider
// installer has validated it; no stable current path is overwritten here.
func moveIntoManagedCandidate(managed, source string) (string, error) {
	managed, source = filepath.Clean(managed), filepath.Clean(source)
	if !pathWithin(managed, source) || source == managed {
		return "", fmt.Errorf("candidate source escapes its managed root")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("candidate source is not a real directory")
	}
	candidate, err := os.MkdirTemp(managed, ".candidate-*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(candidate); err != nil {
		return "", err
	}
	if err := os.Rename(source, candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

func requireRegularExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not a regular executable", path)
	}
	return nil
}

func copyTreeMissing(source, target string) error {
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
			return secureMkdirAll(target, destination, 0o750)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file in copied tree: %s", relative)
		}
		if current, statErr := os.Lstat(destination); statErr == nil {
			if !current.Mode().IsRegular() {
				return fmt.Errorf("refusing non-regular existing file: %s", relative)
			}
			return nil
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		return copyFile(path, destination, info.Mode().Perm())
	})
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
	if err := secureMkdirAll(ic.ControlDir, filepath.Dir(download), 0o750); err != nil {
		return fmt.Errorf("prepare overlay cache: %w", err)
	}
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
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	tmp, err := os.MkdirTemp(managed, ".extract-*")
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
	candidate, err := moveIntoManagedCandidate(managed, linuxRoot)
	if err != nil {
		return resolved, err
	}
	candidateLive := true
	defer func() {
		if candidateLive {
			_ = os.RemoveAll(candidate)
		}
	}()
	executable := filepath.Join(candidate, filepath.FromSlash(p.spec.Options.Executable))
	if err := os.Chmod(executable, 0o750); err != nil {
		return resolved, err
	}
	if err := requireRegularExecutable(executable); err != nil {
		return resolved, fmt.Errorf("Terraria executable: %w", err)
	}
	resolved.WorkDir, resolved.Command = candidate, []string{executable}
	candidateLive = false
	return resolved, nil
}

func (p *catalogProvider) installFactorio(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	candidate, err := os.MkdirTemp(managed, ".candidate-*")
	if err != nil {
		return resolved, err
	}
	candidateLive := true
	defer func() {
		if candidateLive {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := extractTarXZSafe(ctx, ic.Artifact, candidate, 1); err != nil {
		return resolved, fmt.Errorf("extract Factorio: %w", err)
	}
	executable := filepath.Join(candidate, filepath.FromSlash(p.spec.Options.Executable))
	if err := os.Chmod(executable, 0o750); err != nil {
		return resolved, err
	}
	if err := requireRegularExecutable(executable); err != nil {
		return resolved, fmt.Errorf("Factorio executable: %w", err)
	}
	resolved.WorkDir, resolved.Command = candidate, []string{executable}
	candidateLive = false
	return resolved, nil
}

func (p *catalogProvider) installTModLoader(ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	candidate, err := os.MkdirTemp(managed, ".candidate-*")
	if err != nil {
		return resolved, err
	}
	candidateLive := true
	defer func() {
		if candidateLive {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := extractZipSafe(ic.Artifact, candidate); err != nil {
		return resolved, err
	}
	dll, err := findFile(candidate, p.spec.Options.Executable)
	if err != nil {
		return resolved, err
	}
	resolved.WorkDir, resolved.Command = candidate, []string{ic.Runtime, dll}
	candidateLive = false
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

func (p *catalogProvider) buildServiceProcess(ctx context.Context, cfg Config, state LaunchState) (ProcessSpec, error) {
	if p.spec.Installer == "web" {
		return p.buildWebProcess(cfg, state)
	}
	if p.spec.Installer == "code-server" {
		return p.buildCodeServerProcess(cfg, state)
	}
	return p.buildGameProcess(ctx, cfg, state)
}

func (p *catalogProvider) buildCodeServerProcess(cfg Config, state LaunchState) (ProcessSpec, error) {
	binary := first(state.Command)
	if err := requireRegularExecutable(binary); err != nil {
		return ProcessSpec{}, fmt.Errorf("code-server executable: %w", err)
	}
	password := cfg.Request.CodeServerPassword
	if password == "" {
		var err error
		password, err = ensureAdminSecret(cfg, "app-code-server")
		if err != nil {
			return ProcessSpec{}, err
		}
	}
	passwordFile := filepath.Join(cfg.Home, "code-server-password.txt")
	if err := writeAtomicFile(passwordFile, []byte(password+"\n"), 0o600); err != nil {
		return ProcessSpec{}, fmt.Errorf("write code-server password file: %w", err)
	}
	managedDir := filepath.Join(cfg.Control, "code-server")
	dataDir := filepath.Join(managedDir, "data")
	configFile := filepath.Join(managedDir, "config.yaml")
	extensionsDir := filepath.Join(cfg.Home, "code-server-extensions")
	for _, directory := range []string{managedDir, dataDir, extensionsDir} {
		root := cfg.Home
		if strings.HasPrefix(directory, cfg.Control+string(filepath.Separator)) {
			root = cfg.Control
		}
		if err := secureMkdirAll(root, directory, 0o750); err != nil {
			return ProcessSpec{}, err
		}
	}
	command := []string{binary,
		"--bind-addr", "0.0.0.0:" + strconv.Itoa(cfg.AllocationPort),
		"--auth", "password", "--disable-telemetry", "--disable-update-check",
		"--config", configFile, "--user-data-dir", dataDir, "--extensions-dir", extensionsDir, cfg.Home,
	}
	readiness := p.spec.Readiness
	readiness.PortVariable = strconv.Itoa(cfg.AllocationPort)
	environment := append([]string(nil), state.Environment...)
	environment = upsertEnvironment(environment, "HOME", cfg.Home)
	environment = upsertEnvironment(environment, "PASSWORD", password)
	return ProcessSpec{Command: command, Directory: cfg.Home, Environment: environment, Readiness: readiness,
		Control: p.spec.Control, ReadyTimeout: time.Duration(readiness.TimeoutSeconds) * time.Second}, nil
}

func (p *catalogProvider) buildWebProcess(cfg Config, state LaunchState) (ProcessSpec, error) {
	root, err := validatedWebRoot(cfg.Home, cfg.Request.WebRoot)
	if err != nil {
		return ProcessSpec{}, err
	}
	lookup := cfg.Dependencies.withDefaults().LookupIP
	upstream, err := canonicalWebProxyWith(cfg.Request.WebMode, cfg.Request.UpstreamURL, lookup)
	if err != nil {
		return ProcessSpec{}, err
	}
	managed := filepath.Join(cfg.Control, "managed", "web", p.spec.ID)
	if err := secureMkdirAll(cfg.Control, managed, 0o750); err != nil {
		return ProcessSpec{}, err
	}
	port := strconv.Itoa(cfg.AllocationPort)
	var command []string
	var configPath string
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
		configPath = filepath.Join(managed, "nginx.conf")
		command = []string{binary, "-c", configPath, "-g", "daemon off;"}
	case "apache":
		binary, err := exec.LookPath("apache2")
		if err != nil {
			return ProcessSpec{}, err
		}
		configPath = filepath.Join(managed, "apache2.conf")
		command = []string{binary, "-f", configPath, "-DFOREGROUND"}
	case "caddy":
		binary := first(state.Command)
		if binary == "" {
			return ProcessSpec{}, fmt.Errorf("Caddy runtime path is missing")
		}
		configPath = filepath.Join(managed, "Caddyfile")
		command = []string{binary, "run", "--config", configPath, "--adapter", "caddyfile"}
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported web provider %q", p.spec.ID)
	}
	writeConfig := func(proxyTarget string) error {
		var config string
		switch p.spec.ID {
		case "nginx":
			config = nginxConfig(managed, root, port, cfg.Request.WebMode, proxyTarget)
		case "apache":
			config = apacheConfig(managed, root, port, cfg.Request.WebMode, proxyTarget)
		case "caddy":
			config = caddyConfig(root, port, cfg.Request.WebMode, proxyTarget)
		}
		return writeAtomicFile(configPath, []byte(config), 0o640)
	}
	var beforeStart func(context.Context) (func() error, error)
	if cfg.Request.WebMode == "proxy" {
		beforeStart = func(ctx context.Context) (func() error, error) {
			loopbackTarget, closeProxy, err := startSafeProxyWith(ctx, upstream, lookup)
			if err != nil {
				return nil, err
			}
			if err := writeConfig(loopbackTarget); err != nil {
				_ = closeProxy()
				return nil, err
			}
			return closeProxy, nil
		}
	} else if err := writeConfig(""); err != nil {
		return ProcessSpec{}, err
	}
	readiness := p.spec.Readiness
	readiness.PortVariable = strconv.Itoa(cfg.AllocationPort)
	return ProcessSpec{Command: command, Directory: cfg.Home, Environment: state.Environment, Readiness: readiness,
		Control: p.spec.Control, ReadyTimeout: time.Duration(readiness.TimeoutSeconds) * time.Second, BeforeStart: beforeStart}, nil
}

func (p *catalogProvider) buildGameProcess(_ context.Context, cfg Config, state LaunchState) (ProcessSpec, error) {
	root := state.WorkingDirectory
	if root == "" {
		root = filepath.Join(cfg.Home, "game")
	}
	executable := first(state.Command)
	if p.spec.Installer == "steamcmd" {
		executable = filepath.Join(root, filepath.FromSlash(p.spec.Options.Executable))
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
	if p.spec.ID == "tmodloader" || p.spec.ID == "tshock" {
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
		worldDir, worldPath, err := managedGameWorldPath(cfg.Home, "saves/Worlds", world, ".wld")
		if err != nil {
			return ProcessSpec{}, err
		}
		if err := secureMkdirAll(cfg.Home, worldDir, 0o750); err != nil {
			return ProcessSpec{}, err
		}
		command = append(command, "-ip", "0.0.0.0", "-port", port, "-maxplayers", players, "-world", worldPath, "-worldname", world, "-autocreate", "2")
		if password != "" {
			command = append(command, "-password", password)
		}
	case "tshock":
		worldName := cfg.Request.GameWorld
		if worldName == "" {
			worldName = "world"
		}
		worldDir, worldPath, err := managedGameWorldPath(cfg.Home, "world", worldName, ".wld")
		if err != nil {
			return ProcessSpec{}, err
		}
		if err := secureMkdirAll(cfg.Home, worldDir, 0o750); err != nil {
			return ProcessSpec{}, err
		}
		command = append(command, "-port", port, "-maxplayers", players, "-world", worldPath, "-autocreate", "2")
	case "satisfactory":
		command = append(command, "FactoryGame", "-Port="+port, "-ReliablePort="+strconv.Itoa(cfg.Request.ReliablePort), "-ExternalReliablePort="+strconv.Itoa(cfg.Request.ReliablePort))
	case "factorio":
		saves, save, err := managedGameWorldPath(cfg.Home, "saves", world, ".zip")
		if err != nil {
			return ProcessSpec{}, err
		}
		if err := secureMkdirAll(cfg.Home, saves, 0o750); err != nil {
			return ProcessSpec{}, err
		}
		if _, err := os.Stat(save); os.IsNotExist(err) {
			create := exec.Command(executable, "--create", save)
			create.Dir, create.Stdout, create.Stderr = root, os.Stdout, os.Stderr
			if err := create.Run(); err != nil {
				return ProcessSpec{}, fmt.Errorf("create Factorio save: %w", err)
			}
		}
		command = append(command, "--port", port, "--start-server", save)
	case "samp":
		if err := configureOpenMP(root, cfg); err != nil {
			return ProcessSpec{}, err
		}
	case "mtasa":
		if err := configureMTA(root, cfg); err != nil {
			return ProcessSpec{}, err
		}
		command = append(command, "--port", port, "--httpport", port, "-n")
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported game provider %q", p.spec.ID)
	}
	extra, err := safeGameExtraArgs(p.spec.ID, cfg.Request.GameExtraArgs)
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
	return ProcessSpec{Command: command, Directory: root, Environment: environment, Readiness: readiness,
		Control: control, ReadyTimeout: time.Duration(readiness.TimeoutSeconds) * time.Second}, nil
}

func configureOpenMP(root string, cfg Config) error {
	path := filepath.Join(root, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("open.mp config: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode open.mp config: %w", err)
	}
	config["name"] = cfg.Request.ServerName
	config["max_players"] = cfg.Request.MaxPlayers
	config["password"] = cfg.Request.ServerPassword
	setJSONMapValues(config, "network", map[string]any{"bind": "0.0.0.0", "port": cfg.AllocationPort})
	setJSONMapValues(config, "artwork", map[string]any{"enable": false})
	setJSONMapValues(config, "rcon", map[string]any{"enable": false})
	encoded, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return err
	}
	return writeAtomicFile(path, append(encoded, '\n'), 0o600)
}

func setJSONMapValues(root map[string]any, key string, values map[string]any) {
	nested, ok := root[key].(map[string]any)
	if !ok {
		nested = map[string]any{}
		root[key] = nested
	}
	for name, value := range values {
		nested[name] = value
	}
}

func configureMTA(root string, cfg Config) error {
	path := filepath.Join(root, "mods", "deathmatch", "mtaserver.conf")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("MTA config: %w", err)
	}
	values := map[string]string{
		"servername": cfg.Request.ServerName, "serverip": "auto",
		"serverport": strconv.Itoa(cfg.AllocationPort), "httpport": strconv.Itoa(cfg.AllocationPort),
		"maxplayers": strconv.Itoa(cfg.Request.MaxPlayers), "ase": "1", "password": cfg.Request.ServerPassword,
	}
	text := string(data)
	for tag, value := range values {
		var replaceErr error
		text, replaceErr = replaceXMLTagText(text, tag, value)
		if replaceErr != nil {
			return replaceErr
		}
	}
	return writeAtomicFile(path, []byte(text), 0o600)
}

func replaceXMLTagText(document, tag, value string) (string, error) {
	pattern := regexp.MustCompile(`(?s)(<` + regexp.QuoteMeta(tag) + `>).*?(</` + regexp.QuoteMeta(tag) + `>)`)
	if !pattern.MatchString(document) {
		return "", fmt.Errorf("MTA config lacks <%s>", tag)
	}
	var escaped strings.Builder
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		return "", err
	}
	return pattern.ReplaceAllStringFunc(document, func(match string) string {
		parts := pattern.FindStringSubmatch(match)
		return parts[1] + escaped.String() + parts[2]
	}), nil
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

func ensureAdminSecret(cfg Config, family string) (string, error) {
	if cfg.Request.AdminPassword != "" {
		return cfg.Request.AdminPassword, nil
	}
	dir := filepath.Join(cfg.Control, "secrets")
	if err := secureMkdirAll(cfg.Control, dir, 0o700); err != nil {
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
	if len(relative) > 512 || strings.ContainsAny(relative, "\x00\r\n\t\"'`{};#$") {
		return "", fmt.Errorf("WEB_ROOT contains characters that are unsafe in managed server configuration")
	}
	for _, component := range strings.FieldsFunc(filepath.ToSlash(relative), func(character rune) bool { return character == '/' }) {
		if component == "" || component == "." || component == ".." {
			continue
		}
		for _, character := range component {
			if character > 127 || !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '.' && character != '_' && character != '-' {
				return "", fmt.Errorf("WEB_ROOT path components may only contain ASCII letters, digits, dot, underscore and dash")
			}
		}
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

func nginxConfig(managed, root, port, mode, upstream string) string {
	location := "try_files $uri $uri/ =404;"
	if mode == "proxy" {
		location = "proxy_pass \"" + upstream + "\";\n            proxy_set_header Host $host;\n            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;"
	}
	return fmt.Sprintf("worker_processes 1;\npid %s;\nerror_log /dev/stderr info;\nevents { worker_connections 1024; }\nhttp {\n    include /etc/nginx/mime.types;\n    access_log /dev/stdout;\n    client_body_temp_path %s;\n    proxy_temp_path %s;\n    fastcgi_temp_path %s;\n    uwsgi_temp_path %s;\n    scgi_temp_path %s;\n    server {\n        listen 0.0.0.0:%s;\n        root %s;\n        index index.html;\n        location / { %s }\n    }\n}\n", filepath.ToSlash(filepath.Join(managed, "nginx.pid")), filepath.ToSlash(filepath.Join(managed, "tmp", "client")), filepath.ToSlash(filepath.Join(managed, "tmp", "proxy")), filepath.ToSlash(filepath.Join(managed, "tmp", "fastcgi")), filepath.ToSlash(filepath.Join(managed, "tmp", "uwsgi")), filepath.ToSlash(filepath.Join(managed, "tmp", "scgi")), port, filepath.ToSlash(root), location)
}

func apacheConfig(managed, root, port, mode, upstream string) string {
	location := fmt.Sprintf("<Directory %q>\nRequire all granted\n</Directory>\n", root)
	if mode == "proxy" {
		location = fmt.Sprintf("ProxyRequests Off\nProxyPass / %q\nProxyPassReverse / %q\n", upstream, upstream)
	}
	return fmt.Sprintf("ServerRoot /etc/apache2\nDefaultRuntimeDir %s\nPidFile %s\nMutex file:%s default\nListen 0.0.0.0:%s\nServerName localhost\nIncludeOptional /etc/apache2/mods-enabled/*.load\nIncludeOptional /etc/apache2/mods-enabled/*.conf\nTypesConfig /etc/mime.types\nErrorLog /proc/self/fd/2\nLogLevel warn\nLogFormat \"%%h %%l %%u %%t \\\"%%r\\\" %%>s %%b\" combined\nCustomLog /proc/self/fd/1 combined\nDocumentRoot %q\n%s", managed, filepath.Join(managed, "apache.pid"), managed, port, root, location)
}

func caddyConfig(root, port, mode, upstream string) string {
	body := fmt.Sprintf("\troot * %s\n\tfile_server", filepath.ToSlash(root))
	if mode == "proxy" {
		body = "\treverse_proxy \"" + upstream + "\""
	}
	return fmt.Sprintf("{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n\tservers {\n\t\tprotocols h1\n\t}\n}\n\n:%s {\n\tbind 0.0.0.0\n%s\n}\n", port, body)
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
