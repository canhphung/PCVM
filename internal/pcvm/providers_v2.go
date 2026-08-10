package pcvm

import (
	"archive/tar"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// canvasBuild is the stable, documented response shape of CanvasMC's v2
// downloads API. PCVM deliberately ignores experimental builds.
type canvasBuild struct {
	BuildNumber    int    `json:"buildNumber"`
	DownloadURL    string `json:"downloadUrl"`
	ChannelVersion string `json:"channelVersion"`
	Experimental   bool   `json:"isExperimental"`
}

type canvasBuildResponse struct {
	Project string        `json:"project"`
	Builds  []canvasBuild `json:"builds"`
}

// UnmarshalJSON prefers the documented Canvas v2 response (a top-level build
// array), while retaining compatibility with the short-lived object envelope
// previously returned by the service. Supporting both shapes avoids treating
// a harmless upstream rollout/rollback as an empty catalog.
func (response *canvasBuildResponse) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var builds []canvasBuild
		if err := json.Unmarshal(data, &builds); err != nil {
			return err
		}
		response.Project = "canvas"
		response.Builds = builds
		return nil
	}
	type envelope canvasBuildResponse
	var decoded envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*response = canvasBuildResponse(decoded)
	return nil
}

func resolveCanvas(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	endpoint := "https://canvasmc.io/api/v2/builds?project=canvas&experimental=false"
	if req.Version != "" && req.Version != "latest" {
		endpoint += "&channel=" + url.QueryEscape(req.Version)
	}
	var response canvasBuildResponse
	if err := h.JSON(ctx, endpoint, &response); err != nil {
		return Artifact{}, fmt.Errorf("Canvas downloads API: %w", err)
	}
	if response.Project != "canvas" {
		return Artifact{}, fmt.Errorf("Canvas downloads API returned unexpected project %q", response.Project)
	}
	builds := response.Builds
	filtered := builds[:0]
	for _, build := range builds {
		if build.Experimental || build.BuildNumber < 1 || build.ChannelVersion == "" || build.DownloadURL == "" {
			continue
		}
		if req.Version != "" && req.Version != "latest" && build.ChannelVersion != req.Version {
			continue
		}
		if req.Build != "" && req.Build != "latest" && strconv.Itoa(build.BuildNumber) != req.Build {
			continue
		}
		filtered = append(filtered, build)
	}
	if len(filtered) == 0 {
		return Artifact{}, fmt.Errorf("Canvas has no stable build for version=%q build=%q", envLatest(req.Version), envLatest(req.Build))
	}
	sort.Slice(filtered, func(i, j int) bool {
		versionOrder := CompareVersions(filtered[i].ChannelVersion, filtered[j].ChannelVersion)
		if versionOrder != 0 {
			return versionOrder > 0
		}
		return filtered[i].BuildNumber > filtered[j].BuildNumber
	})
	chosen := filtered[0]
	parsed, err := url.Parse(chosen.DownloadURL)
	if err != nil || parsed.Scheme != "https" || strings.ToLower(parsed.Hostname()) != "jenkins.canvasmc.io" || parsed.User != nil {
		return Artifact{}, fmt.Errorf("Canvas returned an unapproved download URL")
	}
	return Artifact{URL: chosen.DownloadURL, FileName: filepath.Base(parsed.Path), Kind: "jar", Version: chosen.ChannelVersion, Build: strconv.Itoa(chosen.BuildNumber)}, nil
}

type quiltGameVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

type quiltLoaderVersion struct {
	Loader struct {
		Version string `json:"version"`
	} `json:"loader"`
}

type quiltInstallerMetadata struct {
	Versioning struct {
		Release string `xml:"release"`
	} `xml:"versioning"`
}

func resolveQuilt(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var games []quiltGameVersion
	if err := h.JSON(ctx, "https://meta.quiltmc.org/v3/versions/game", &games); err != nil {
		return Artifact{}, err
	}
	game := strings.TrimSpace(req.Version)
	if game == "" || game == "latest" {
		game = ""
		for _, candidate := range games {
			if candidate.Stable && (game == "" || CompareVersions(candidate.Version, game) > 0) {
				game = candidate.Version
			}
		}
	}
	if game == "" || game == "latest" {
		return Artifact{}, fmt.Errorf("Quilt returned no stable Minecraft version")
	}
	known := false
	for _, candidate := range games {
		if candidate.Version == game && candidate.Stable {
			known = true
			break
		}
	}
	if !known {
		return Artifact{}, fmt.Errorf("Quilt does not support stable Minecraft %q", game)
	}
	var loaders []quiltLoaderVersion
	if err := h.JSON(ctx, "https://meta.quiltmc.org/v3/versions/loader/"+url.PathEscape(game), &loaders); err != nil {
		return Artifact{}, err
	}
	loader := strings.TrimSpace(req.Build)
	if loader == "" || loader == "latest" {
		loader = ""
		for _, candidate := range loaders {
			// A release loader has no SemVer pre-release suffix.
			if candidate.Loader.Version != "" && !strings.Contains(candidate.Loader.Version, "-") && (loader == "" || CompareVersions(candidate.Loader.Version, loader) > 0) {
				loader = candidate.Loader.Version
			}
		}
	} else {
		found := false
		for _, candidate := range loaders {
			if candidate.Loader.Version == loader {
				found = true
				break
			}
		}
		if !found {
			return Artifact{}, fmt.Errorf("Quilt loader %q is unavailable for Minecraft %s", loader, game)
		}
	}
	if loader == "" {
		return Artifact{}, fmt.Errorf("Quilt returned no release loader for Minecraft %s", game)
	}
	metadataRaw, err := h.Text(ctx, "https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/maven-metadata.xml", 1<<20)
	if err != nil {
		return Artifact{}, err
	}
	var metadata quiltInstallerMetadata
	if err := xml.Unmarshal(metadataRaw, &metadata); err != nil || metadata.Versioning.Release == "" {
		return Artifact{}, fmt.Errorf("Quilt installer metadata is invalid")
	}
	installer := metadata.Versioning.Release
	base := "https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/" + url.PathEscape(installer) + "/quilt-installer-" + url.PathEscape(installer) + ".jar"
	digestRaw, err := h.Text(ctx, base+".sha256", 1024)
	if err != nil {
		return Artifact{}, fmt.Errorf("Quilt installer checksum: %w", err)
	}
	digest := strings.Fields(string(digestRaw))
	if len(digest) == 0 || !validHexDigest(digest[0], 64) {
		return Artifact{}, fmt.Errorf("Quilt installer has no valid SHA-256")
	}
	return Artifact{URL: base, FileName: "quilt-installer-" + installer + ".jar", Kind: "quilt-installer", SHA256: strings.ToLower(digest[0]), Version: game, Build: loader, Metadata: map[string]string{"installer": installer}}, nil
}

func (p *catalogProvider) installQuilt(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	root := filepath.Join(ic.ControlDir, "managed", p.spec.ID, resolved.Artifact.Version+"-"+resolved.Artifact.Build)
	if err := secureMkdirAll(ic.ControlDir, root, 0o750); err != nil {
		return resolved, err
	}
	launcher := filepath.Join(root, "quilt-server-launch.jar")
	if _, err := os.Stat(launcher); os.IsNotExist(err) {
		cmd := exec.CommandContext(ctx, ic.Runtime, "-jar", ic.Artifact, "install", "server", resolved.Artifact.Version, resolved.Artifact.Build, "--download-server", "--install-dir="+root)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, ic.Out, ic.Err
		if err := cmd.Run(); err != nil {
			return resolved, fmt.Errorf("Quilt installer: %w", err)
		}
	}
	if info, err := os.Stat(launcher); err != nil || !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("Quilt installer did not create quilt-server-launch.jar")
	}
	if err := linkMutableData(ic.Home, root, []string{"world", "world_nether", "world_the_end", "mods", "config"}, []string{"server.properties", "eula.txt", "ops.json", "whitelist.json", "banned-ips.json", "banned-players.json"}); err != nil {
		return resolved, err
	}
	resolved.WorkDir = root
	resolved.Command = []string{ic.Runtime, "-jar", launcher, "nogui"}
	return resolved, nil
}

type geyserBuild struct {
	Version   string `json:"version"`
	Build     int    `json:"build"`
	Downloads map[string]struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"downloads"`
}

func resolvePaperGeyser(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	paper, err := resolvePaper(ctx, "paper", req, h)
	if err != nil {
		return Artifact{}, err
	}
	var geyser, floodgate geyserBuild
	if err := h.JSON(ctx, "https://download.geysermc.org/v2/projects/geyser/versions/latest/builds/latest", &geyser); err != nil {
		return Artifact{}, fmt.Errorf("resolve Geyser: %w", err)
	}
	if err := h.JSON(ctx, "https://download.geysermc.org/v2/projects/floodgate/versions/latest/builds/latest", &floodgate); err != nil {
		return Artifact{}, fmt.Errorf("resolve Floodgate: %w", err)
	}
	g, gok := geyser.Downloads["spigot"]
	f, fok := floodgate.Downloads["spigot"]
	if !gok || !fok || !validHexDigest(g.SHA256, 64) || !validHexDigest(f.SHA256, 64) || geyser.Version == "" || floodgate.Version == "" {
		return Artifact{}, fmt.Errorf("Geyser download metadata is incomplete")
	}
	paper.Metadata = map[string]string{
		"geyser_version": geyser.Version, "geyser_build": strconv.Itoa(geyser.Build), "geyser_name": g.Name, "geyser_sha256": strings.ToLower(g.SHA256),
		"floodgate_version": floodgate.Version, "floodgate_build": strconv.Itoa(floodgate.Build), "floodgate_name": f.Name, "floodgate_sha256": strings.ToLower(f.SHA256),
	}
	return paper, nil
}

func (p *catalogProvider) installPaperGeyser(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	server := filepath.Join(managed, resolved.Artifact.Version+"-"+resolved.Artifact.Build+"-server.jar")
	if err := copyFile(ic.Artifact, server, 0o640); err != nil {
		return resolved, err
	}
	transactionRoot := installOverlayRoot(ic.ControlDir, p.spec.ID)
	if _, err := os.Lstat(transactionRoot); err == nil {
		return resolved, fmt.Errorf("a pending Paper-Geyser overlay must be recovered before installation")
	} else if !os.IsNotExist(err) {
		return resolved, err
	}
	if err := secureMkdirAll(ic.ControlDir, transactionRoot, 0o750); err != nil {
		return resolved, err
	}
	candidateHome := installOverlayNewRoot(ic.ControlDir, p.spec.ID)
	candidatePlugins := filepath.Join(candidateHome, "plugins")
	if err := secureMkdirAll(transactionRoot, candidatePlugins, 0o750); err != nil {
		_ = os.RemoveAll(transactionRoot)
		return resolved, err
	}
	transactionApplied := false
	defer func() {
		if !transactionApplied {
			_ = os.RemoveAll(transactionRoot)
		}
	}()
	for _, plugin := range []struct{ project, prefix, target string }{{"geyser", "geyser", "Geyser-Spigot.jar"}, {"floodgate", "floodgate", "floodgate-spigot.jar"}} {
		name, checksum := resolved.Artifact.Metadata[plugin.prefix+"_name"], resolved.Artifact.Metadata[plugin.prefix+"_sha256"]
		version, build := resolved.Artifact.Metadata[plugin.prefix+"_version"], resolved.Artifact.Metadata[plugin.prefix+"_build"]
		if name == "" || version == "" || build == "" || !validHexDigest(checksum, 64) {
			return resolved, fmt.Errorf("%s artifact identity is incomplete", plugin.project)
		}
		cache := filepath.Join(ic.ControlDir, "cache", "artifacts", plugin.project+"-"+version+"-"+build+"-"+name)
		if err := secureMkdirAll(ic.ControlDir, filepath.Dir(cache), 0o750); err != nil {
			return resolved, err
		}
		artifact := Artifact{URL: "https://download.geysermc.org/v2/projects/" + plugin.project + "/versions/" + url.PathEscape(version) + "/builds/" + url.PathEscape(build) + "/downloads/spigot", FileName: name, SHA256: checksum}
		if _, err := ic.HTTP.Download(ctx, artifact, cache); err != nil {
			return resolved, fmt.Errorf("download %s: %w", plugin.project, err)
		}
		if err := copyFile(cache, filepath.Join(candidatePlugins, plugin.target), 0o640); err != nil {
			return resolved, err
		}
	}
	configData, configMode, err := readNestedManagedConfig(ic.Home, geyserConfigRelative)
	if err != nil && !os.IsNotExist(err) {
		return resolved, err
	}
	configData, err = patchGeyserYAML(configData, ic.AllocationPort)
	if err != nil {
		return resolved, err
	}
	if configMode == 0 {
		configMode = 0o640
	}
	configCandidate := filepath.Join(candidateHome, filepath.FromSlash(geyserConfigRelative))
	if err := secureMkdirAll(transactionRoot, filepath.Dir(configCandidate), 0o750); err != nil {
		return resolved, err
	}
	if err := writeAtomicFile(configCandidate, configData, configMode); err != nil {
		return resolved, err
	}
	resolved.WorkDir = ic.Home
	resolved.Command = []string{ic.Runtime, "-jar", server, "nogui"}
	resolved.RollbackMode = "staged"
	previousManaged, err := paperGeyserPreviousManaged(ic)
	if err != nil {
		return resolved, err
	}
	candidateState := newStateFromInstall(p.spec, ic.Request, resolved, ic.Request.Architecture, time.Now())
	if err := applyInstallOverlay(ic.Home, ic.ControlDir, p.spec.ID, candidateState.InstallID, previousManaged); err != nil {
		return resolved, err
	}
	transactionApplied = true
	return resolved, nil
}

func paperGeyserPreviousManaged(ic InstallContext) (map[string]string, error) {
	managed, err := paperGeyserOwnedPlugins(ic)
	if err != nil {
		return nil, err
	}
	// Geyser's config is mutable user-facing YAML. It is candidate-owned for
	// atomic updates, but deliberately not sealed into the executable receipt:
	// users may edit unrelated settings and PCVM only reconciles network keys.
	if _, _, err := readNestedManagedConfig(ic.Home, geyserConfigRelative); err == nil {
		managed[geyserConfigRelative] = "mutable-config"
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return managed, nil
}

func paperGeyserOwnedPlugins(ic InstallContext) (map[string]string, error) {
	managed := map[string]string{}
	state, err := LoadState(ic.ControlDir)
	if err != nil {
		return nil, err
	}
	if state != nil && state.Provider == "paper-geyser" {
		receipt, err := LoadInstallReceipt(ic.ControlDir, state.Receipt)
		if err != nil {
			return nil, err
		}
		if err := verifyInstallReceipt(ic.Home, *state, receipt); err != nil {
			return nil, err
		}
		for _, file := range receipt.Files {
			if file.Path == "plugins/Geyser-Spigot.jar" || file.Path == "plugins/floodgate-spigot.jar" {
				managed[file.Path] = file.SHA256
			}
		}
		for _, required := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
			if _, ok := managed[required]; !ok {
				return nil, fmt.Errorf("Paper-Geyser receipt does not own required plugin %s", required)
			}
		}
	}
	return managed, nil
}

// stagePaperGeyserRemoval makes the same-family transition back to a plain
// Paper fork exact and reversible. Only plugin JARs sealed by the active
// Paper-Geyser receipt are removed; user plugins and the mutable Geyser config
// remain untouched. The generic install transaction commits or rolls this
// overlay back together with target state activation.
func stagePaperGeyserRemoval(ic InstallContext, target ProviderSpec, resolved Resolved) error {
	if target.ID != "paper" && target.ID != "purpur" && target.ID != "pufferfish" {
		return nil
	}
	owned, err := paperGeyserOwnedPlugins(ic)
	if err != nil || len(owned) == 0 {
		return err
	}
	transactionRoot := installOverlayRoot(ic.ControlDir, target.ID)
	if _, err := os.Lstat(transactionRoot); err == nil {
		return fmt.Errorf("a pending %s overlay must be recovered before installation", target.ID)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := secureMkdirAll(ic.ControlDir, installOverlayNewRoot(ic.ControlDir, target.ID), 0o750); err != nil {
		return err
	}
	candidateState := newStateFromInstall(target, ic.Request, resolved, ic.Request.Architecture, time.Now())
	if err := applyInstallOverlay(ic.Home, ic.ControlDir, target.ID, candidateState.InstallID, owned); err != nil {
		_ = os.RemoveAll(transactionRoot)
		return err
	}
	return nil
}

func (p *catalogProvider) installTShock(ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	archiveStage, err := os.MkdirTemp(managed, ".tshock-archive-")
	if err != nil {
		return resolved, err
	}
	defer os.RemoveAll(archiveStage)
	root, err := os.MkdirTemp(managed, ".candidate-")
	if err != nil {
		return resolved, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(root)
		}
	}()
	if err := extractZipSafe(ic.Artifact, archiveStage); err != nil {
		return resolved, err
	}
	archives, err := filepath.Glob(filepath.Join(archiveStage, "*.tar"))
	if err != nil || len(archives) != 1 {
		return resolved, fmt.Errorf("TShock release must contain exactly one TAR payload")
	}
	if err := extractTShockTar(archives[0], root); err != nil {
		return resolved, err
	}
	entry := filepath.Join(root, filepath.FromSlash(p.spec.Options.Executable))
	info, err := os.Lstat(entry)
	if err != nil {
		return resolved, fmt.Errorf("TShock managed assembly: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("TShock managed assembly must be a regular non-symlink file")
	}
	// Archive modes are not represented consistently on Windows development
	// hosts. Set a known executable mode after the no-symlink regular-file
	// check; Linux containers will then execute the apphost directly.
	if err := os.Chmod(entry, 0o750); err != nil {
		return resolved, fmt.Errorf("make TShock managed apphost executable: %w", err)
	}
	if err := linkMutableData(ic.Home, root, []string{"world", "tshock", "ServerPlugins"}, nil); err != nil {
		return resolved, err
	}
	resolved.WorkDir = root
	// TShock 6 is published as a framework-dependent single-file apphost, not
	// as TShock.Server.dll. Execute that sealed apphost directly and bind it to
	// the runtime pack selected by PCVM. This keeps host-installed runtimes out
	// of resolution and works for both x64 and arm64 release assets.
	resolved.Command = []string{entry}
	resolved.Environment = append(resolved.Environment,
		"DOTNET_ROOT="+filepath.Dir(ic.Runtime),
		"DOTNET_MULTILEVEL_LOOKUP=0",
	)
	complete = true
	return resolved, nil
}

func extractTShockTar(archive, root string) error {
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	return extractTarStreamSafe(tar.NewReader(in), root, tarExtractOptions{Limits: archiveLimits{
		MaxEntries: 100_000, MaxTotalBytes: 2 << 30, MaxFileBytes: 1 << 30,
		MaxPathBytes: 4096, MaxSymlinks: 0,
	}})
}
