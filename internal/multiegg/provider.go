package multiegg

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type catalogProvider struct{ spec ProviderSpec }

func NewProvider(spec ProviderSpec) Provider               { return &catalogProvider{spec: spec} }
func (p *catalogProvider) Spec() ProviderSpec              { return p.spec }
func (p *catalogProvider) CompareVersions(a, b string) int { return CompareVersions(a, b) }

func (p *catalogProvider) Resolve(ctx context.Context, req Request, httpc *HTTPClient) (Resolved, error) {
	var artifact Artifact
	var err error
	switch p.spec.Resolver {
	case "mojang":
		artifact, err = resolveMojang(ctx, req, httpc)
	case "papermc":
		artifact, err = resolvePaper(ctx, p.spec.Options["project"], req, httpc)
	case "purpur":
		artifact, err = resolvePurpur(ctx, req, httpc)
	case "pufferfish":
		artifact, err = resolvePufferfish(ctx, req, httpc)
	case "fabric":
		artifact, err = resolveFabric(ctx, req, httpc)
	case "forge":
		artifact, err = resolveMaven(ctx, req, httpc, "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml", "https://maven.minecraftforge.net/net/minecraftforge/forge/%s/forge-%s-installer.jar")
	case "neoforge":
		artifact, err = resolveMaven(ctx, req, httpc, "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml", "https://maven.neoforged.net/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar")
	case "bungeecord":
		artifact, err = resolveBungee(ctx, req, httpc)
	case "bedrock":
		artifact, err = resolveBedrock(ctx, httpc)
	case "pocketmine":
		artifact, err = resolveGitHub(ctx, req, httpc, "pmmp/PocketMine-MP", `(?i)\.phar$`)
	case "github-release":
		artifact, err = resolveGitHub(ctx, req, httpc, p.spec.Options["repository"], p.spec.Options["asset_regex"])
	case "local-app":
		artifact = Artifact{Kind: "source", Version: req.Version, Build: req.Build}
	default:
		err = fmt.Errorf("unsupported resolver %q", p.spec.Resolver)
	}
	if err != nil {
		return Resolved{}, err
	}
	runtimeVersion := req.RuntimeVersion
	if runtimeVersion == "" || runtimeVersion == "auto" {
		switch p.spec.Runtime {
		case "java":
			switch p.spec.ID {
			case "velocity":
				runtimeVersion = "21"
			case "lavalink":
				runtimeVersion = "17"
			default:
				runtimeVersion = JavaVersionFor(artifact.Version)
			}
		case "node":
			runtimeVersion = "24"
		case "python":
			runtimeVersion = "3.13"
		case "php-pmmp":
			runtimeVersion = "pmmp"
		default:
			runtimeVersion = "native"
		}
	}
	patterns := append([]string(nil), p.spec.ReadyPatterns...)
	if req.AppReady != "" {
		patterns = []string{req.AppReady}
	}
	return Resolved{Artifact: artifact, RuntimeKind: p.spec.Runtime, RuntimeVersion: runtimeVersion, ReadyPatterns: patterns, StopCommand: p.spec.StopCommand}, nil
}

func latestOr(want string, values []string) (string, error) {
	if len(values) == 0 {
		return "", fmt.Errorf("upstream returned no versions")
	}
	if want != "" && want != "latest" {
		for _, v := range values {
			if v == want {
				return v, nil
			}
		}
		return "", fmt.Errorf("version %q not found", want)
	}
	sort.Slice(values, func(i, j int) bool { return CompareVersions(values[i], values[j]) > 0 })
	return values[0], nil
}

func resolveMojang(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var manifest struct {
		Latest struct {
			Release string `json:"release"`
		} `json:"latest"`
		Versions []struct{ ID, URL string } `json:"versions"`
	}
	if err := h.JSON(ctx, "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json", &manifest); err != nil {
		return Artifact{}, err
	}
	version := req.Version
	if version == "" || version == "latest" {
		version = manifest.Latest.Release
	}
	var detailURL string
	for _, v := range manifest.Versions {
		if v.ID == version {
			detailURL = v.URL
			break
		}
	}
	if detailURL == "" {
		return Artifact{}, fmt.Errorf("Minecraft version %q not found", version)
	}
	var detail struct {
		Downloads struct {
			Server struct {
				URL, SHA1 string
				Size      int64
			} `json:"server"`
		} `json:"downloads"`
	}
	if err := h.JSON(ctx, detailURL, &detail); err != nil {
		return Artifact{}, err
	}
	if detail.Downloads.Server.URL == "" {
		return Artifact{}, fmt.Errorf("Minecraft %s has no server artifact", version)
	}
	return Artifact{URL: detail.Downloads.Server.URL, FileName: "server.jar", Kind: "jar", SHA1: detail.Downloads.Server.SHA1, Version: version, Build: "release"}, nil
}

func resolvePaper(ctx context.Context, project string, req Request, h *HTTPClient) (Artifact, error) {
	var pv struct {
		Versions map[string][]string `json:"versions"`
	}
	base := "https://fill.papermc.io/v3/projects/" + project
	if err := h.JSON(ctx, base, &pv); err != nil {
		return Artifact{}, err
	}
	var versions []string
	for _, group := range pv.Versions {
		versions = append(versions, group...)
	}
	type build struct {
		ID        int    `json:"id"`
		Channel   string `json:"channel"`
		Downloads map[string]struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			Checksums struct {
				SHA256 string `json:"sha256"`
			} `json:"checksums"`
		} `json:"downloads"`
	}
	wantLatest := req.Version == "" || req.Version == "latest"
	if !wantLatest {
		found := false
		for _, v := range versions {
			if v == req.Version {
				versions = []string{v}
				found = true
				break
			}
		}
		if !found {
			return Artifact{}, fmt.Errorf("version %q not found", req.Version)
		}
	} else {
		sort.Slice(versions, func(i, j int) bool { return CompareVersions(versions[i], versions[j]) > 0 })
	}
	for _, version := range versions {
		var builds []build
		if err := h.JSON(ctx, base+"/versions/"+version+"/builds", &builds); err != nil {
			if wantLatest {
				continue
			}
			return Artifact{}, err
		}
		if len(builds) == 0 {
			continue
		}
		var selected *build
		if req.Build != "" && req.Build != "latest" {
			for i := range builds {
				if fmt.Sprint(builds[i].ID) == req.Build {
					selected = &builds[i]
					break
				}
			}
			if selected == nil {
				return Artifact{}, fmt.Errorf("build %q not found", req.Build)
			}
		} else {
			for i := range builds {
				if builds[i].Channel == "STABLE" {
					selected = &builds[i]
					break
				}
			}
			if selected == nil && !wantLatest {
				selected = &builds[0]
			}
		}
		if selected == nil {
			continue
		}
		dl, ok := selected.Downloads["server:default"]
		if !ok {
			continue
		}
		return Artifact{URL: dl.URL, FileName: "server.jar", Kind: "jar", SHA256: dl.Checksums.SHA256, Version: version, Build: fmt.Sprint(selected.ID)}, nil
	}
	return Artifact{}, fmt.Errorf("no suitable stable build for %s", project)
}

func resolvePurpur(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var root struct {
		Versions []string `json:"versions"`
	}
	if err := h.JSON(ctx, "https://api.purpurmc.org/v2/purpur", &root); err != nil {
		return Artifact{}, err
	}
	version, err := latestOr(req.Version, root.Versions)
	if err != nil {
		return Artifact{}, err
	}
	build := req.Build
	if build == "" || build == "latest" {
		var v struct {
			Builds struct {
				Latest string `json:"latest"`
			} `json:"builds"`
		}
		if err := h.JSON(ctx, "https://api.purpurmc.org/v2/purpur/"+version, &v); err != nil {
			return Artifact{}, err
		}
		build = v.Builds.Latest
	}
	return Artifact{URL: fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/%s/download", version, build), FileName: "server.jar", Kind: "jar", Version: version, Build: build}, nil
}

func resolvePufferfish(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	version := req.Version
	if version == "" || version == "latest" {
		version = "1.21"
	}
	job := "Pufferfish-" + version
	var data struct {
		LastSuccessfulBuild struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		} `json:"lastSuccessfulBuild"`
	}
	if err := h.JSON(ctx, "https://ci.pufferfish.host/job/"+job+"/api/json", &data); err != nil {
		return Artifact{}, err
	}
	if data.LastSuccessfulBuild.Number == 0 {
		return Artifact{}, fmt.Errorf("no successful Pufferfish build")
	}
	build := fmt.Sprint(data.LastSuccessfulBuild.Number)
	var detail struct {
		Artifacts []struct {
			FileName     string `json:"fileName"`
			RelativePath string `json:"relativePath"`
		} `json:"artifacts"`
	}
	if err := h.JSON(ctx, fmt.Sprintf("https://ci.pufferfish.host/job/%s/%s/api/json", job, build), &detail); err != nil {
		return Artifact{}, err
	}
	for _, item := range detail.Artifacts {
		if strings.Contains(item.FileName, "paperclip") && strings.HasSuffix(item.FileName, ".jar") {
			resolvedVersion := version
			if match := regexp.MustCompile(`paperclip-([0-9.]+)-R`).FindStringSubmatch(item.FileName); len(match) == 2 {
				resolvedVersion = match[1]
			}
			return Artifact{URL: fmt.Sprintf("https://ci.pufferfish.host/job/%s/%s/artifact/%s", job, build, item.RelativePath), FileName: "server.jar", Kind: "jar", Version: resolvedVersion, Build: build}, nil
		}
	}
	return Artifact{}, fmt.Errorf("Pufferfish build %s has no paperclip artifact", build)
}

func resolveFabric(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var games []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := h.JSON(ctx, "https://meta.fabricmc.net/v2/versions/game", &games); err != nil {
		return Artifact{}, err
	}
	var versions []string
	for _, g := range games {
		if g.Stable {
			versions = append(versions, g.Version)
		}
	}
	game, err := latestOr(req.Version, versions)
	if err != nil {
		return Artifact{}, err
	}
	var loaders, installers []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err = h.JSON(ctx, "https://meta.fabricmc.net/v2/versions/loader", &loaders); err != nil {
		return Artifact{}, err
	}
	if err = h.JSON(ctx, "https://meta.fabricmc.net/v2/versions/installer", &installers); err != nil {
		return Artifact{}, err
	}
	if len(loaders) == 0 || len(installers) == 0 {
		return Artifact{}, fmt.Errorf("Fabric metadata empty")
	}
	lv, iv := loaders[0].Version, installers[0].Version
	return Artifact{URL: fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s/%s/server/jar", game, lv, iv), FileName: "server.jar", Kind: "jar", Version: game, Build: lv, Metadata: map[string]string{"installer": iv}}, nil
}

func resolveMaven(ctx context.Context, req Request, h *HTTPClient, metadataURL, pattern string) (Artifact, error) {
	data, err := h.Text(ctx, metadataURL, 4<<20)
	if err != nil {
		return Artifact{}, err
	}
	var meta struct {
		Versioning struct {
			Release  string   `xml:"release"`
			Versions []string `xml:"versions>version"`
		} `xml:"versioning"`
	}
	if err = xml.Unmarshal(data, &meta); err != nil {
		return Artifact{}, err
	}
	want := req.Version
	var candidates []string
	for _, v := range meta.Versioning.Versions {
		if want == "" || want == "latest" || v == want || strings.HasPrefix(v, want+"-") {
			candidates = append(candidates, v)
		}
	}
	version, err := latestOr("latest", candidates)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{URL: fmt.Sprintf(pattern, version, version), FileName: "installer.jar", Kind: "jar", Version: version, Build: "release"}, nil
}

func resolveBungee(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var data struct {
		LastSuccessfulBuild struct {
			Number int `json:"number"`
		} `json:"lastSuccessfulBuild"`
	}
	if err := h.JSON(ctx, "https://ci.md-5.net/job/BungeeCord/api/json", &data); err != nil {
		return Artifact{}, err
	}
	build := fmt.Sprint(data.LastSuccessfulBuild.Number)
	return Artifact{URL: "https://ci.md-5.net/job/BungeeCord/" + build + "/artifact/bootstrap/target/BungeeCord.jar", FileName: "server.jar", Kind: "jar", Version: "latest", Build: build}, nil
}

func resolveBedrock(ctx context.Context, h *HTTPClient) (Artifact, error) {
	var service struct {
		Result struct {
			Links []struct {
				DownloadType string `json:"downloadType"`
				DownloadURL  string `json:"downloadUrl"`
			} `json:"links"`
		} `json:"result"`
	}
	if err := h.JSON(ctx, "https://net-secondary.web.minecraft-services.net/api/v1.0/download/links", &service); err == nil {
		for _, link := range service.Result.Links {
			if link.DownloadType != "serverBedrockLinux" {
				continue
			}
			re := regexp.MustCompile(`bedrock-server-([0-9.]+)\.zip$`)
			match := re.FindStringSubmatch(link.DownloadURL)
			if len(match) == 2 {
				return Artifact{URL: link.DownloadURL, FileName: "bedrock.zip", Kind: "zip", Version: match[1], Build: "release"}, nil
			}
		}
	}
	data, err := h.Text(ctx, "https://www.minecraft.net/en-us/download/server/bedrock", 8<<20)
	if err != nil {
		return Artifact{}, err
	}
	normalized := strings.ReplaceAll(string(data), `\/`, "/")
	normalized = strings.ReplaceAll(normalized, `\u002F`, "/")
	normalized = html.UnescapeString(normalized)
	if decoded, decodeErr := url.QueryUnescape(normalized); decodeErr == nil {
		normalized = decoded
	}
	re := regexp.MustCompile(`https://(?:(?:www\.)?minecraft\.net/bedrockdedicatedserver|minecraft\.azureedge\.net)/bin-linux/bedrock-server-([0-9.]+)\.zip`)
	m := re.FindStringSubmatch(normalized)
	if len(m) != 2 {
		return Artifact{}, fmt.Errorf("Bedrock download link not found")
	}
	return Artifact{URL: m[0], FileName: "bedrock.zip", Kind: "zip", Version: m[1], Build: "release"}, nil
}

func resolveGitHub(ctx context.Context, req Request, h *HTTPClient, repo, assetPattern string) (Artifact, error) {
	var release struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	endpoint := "https://api.github.com/repos/" + repo + "/releases/latest"
	if req.Version != "" && req.Version != "latest" {
		endpoint = "https://api.github.com/repos/" + repo + "/releases/tags/" + req.Version
	}
	if err := h.JSON(ctx, endpoint, &release); err != nil {
		return Artifact{}, err
	}
	re, err := regexp.Compile(assetPattern)
	if err != nil {
		return Artifact{}, err
	}
	for _, a := range release.Assets {
		if re.MatchString(a.Name) {
			kind := "file"
			if strings.HasSuffix(strings.ToLower(a.Name), ".phar") {
				kind = "phar"
			}
			return Artifact{URL: a.URL, FileName: a.Name, Kind: kind, Version: release.Tag, Build: "release"}, nil
		}
	}
	return Artifact{}, fmt.Errorf("release contains no matching artifact")
}

func (p *catalogProvider) Install(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := os.MkdirAll(managed, 0o750); err != nil {
		return resolved, err
	}
	switch p.spec.Installer {
	case "jar":
		target := filepath.Join(managed, resolved.Artifact.Version+"-"+resolved.Artifact.Build+"-server.jar")
		if err := copyFile(ic.Artifact, target, 0o640); err != nil {
			return resolved, err
		}
		resolved.WorkDir = ic.Home
		resolved.Command = []string{ic.Runtime, "-Xms128M", "-Xmx" + serverMemory(), "-jar", target}
		if strings.HasPrefix(p.spec.Family, "minecraft-java-") {
			resolved.Command = append(resolved.Command, "nogui")
		}
		if p.spec.ID == "lavalink" {
			config := filepath.Join(ic.Home, "application.yml")
			if _, err := os.Stat(config); os.IsNotExist(err) {
				port := envDefault("SERVER_PORT", "2333")
				body := fmt.Sprintf("server:\n  port: %s\nlavalink:\n  server:\n    password: youshallnotpass\n", port)
				if err := os.WriteFile(config, []byte(body), 0o640); err != nil {
					return resolved, err
				}
			}
		}
	case "phar":
		target := filepath.Join(managed, resolved.Artifact.Version+"-PocketMine-MP.phar")
		if err := copyFile(ic.Artifact, target, 0o640); err != nil {
			return resolved, err
		}
		resolved.WorkDir = ic.Home
		resolved.Command = []string{ic.Runtime, target, "--no-wizard"}
		resolved.Environment = []string{"PHPRC="}
	case "zip":
		versionRoot := filepath.Join(managed, resolved.Artifact.Version)
		if err := os.MkdirAll(versionRoot, 0o750); err != nil {
			return resolved, err
		}
		if err := extractZipSafe(ic.Artifact, versionRoot); err != nil {
			return resolved, err
		}
		if err := linkMutableData(ic.Home, versionRoot, []string{"worlds"}, []string{"server.properties", "allowlist.json", "permissions.json"}); err != nil {
			return resolved, err
		}
		resolved.WorkDir = versionRoot
		resolved.Command = []string{filepath.Join(versionRoot, "bedrock_server")}
		resolved.Environment = []string{"LD_LIBRARY_PATH=."}
	case "java-installer":
		managed = filepath.Join(managed, resolved.Artifact.Version)
		if err := os.MkdirAll(managed, 0o750); err != nil {
			return resolved, err
		}
		cmd := exec.CommandContext(ctx, ic.Runtime, "-jar", ic.Artifact, "--installServer")
		cmd.Dir = managed
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return resolved, fmt.Errorf("installer: %w", err)
		}
		resolved.WorkDir = managed
		resolved.Command = []string{ic.Runtime, "@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/" + resolved.Artifact.Version + "/unix_args.txt", "nogui"}
		if p.spec.ID == "neoforge" {
			resolved.Command = []string{ic.Runtime, "@user_jvm_args.txt", "@libraries/net/neoforged/neoforge/" + resolved.Artifact.Version + "/unix_args.txt", "nogui"}
		}
		if err := linkMutableData(ic.Home, managed, []string{"world", "world_nether", "world_the_end", "mods", "config", "defaultconfigs"}, []string{"server.properties", "eula.txt", "ops.json", "whitelist.json", "banned-ips.json", "banned-players.json"}); err != nil {
			return resolved, err
		}
	case "node-app", "python-app":
		return p.installApp(ctx, ic, resolved)
	default:
		return resolved, fmt.Errorf("unsupported installer %q", p.spec.Installer)
	}
	if p.spec.RequiresEULA && ic.Request.AcceptEULA {
		if err := os.WriteFile(filepath.Join(resolved.WorkDir, "eula.txt"), []byte("eula=true\n"), 0o640); err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}

func (p *catalogProvider) installApp(ctx context.Context, ic InstallContext, r Resolved) (Resolved, error) {
	source := filepath.Join(ic.Home, "app")
	if ic.Request.SourceMode == "git" {
		if ic.Request.GitURL == "" {
			return r, fmt.Errorf("GIT_URL is required")
		}
		if _, err := os.Stat(filepath.Join(source, ".git")); os.IsNotExist(err) {
			cloneFrom := ic.Request.GitURL
			cloneArgs := []string{"clone", "--depth", "1", "--branch", ic.Request.GitBranch, "--", cloneFrom, source}
			if ic.PreparedSource != "" {
				cloneFrom = ic.PreparedSource
				cloneArgs = []string{"clone", "--no-local", "--", cloneFrom, source}
			}
			cmd := exec.CommandContext(ctx, "git", cloneArgs...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return r, err
			}
			if ic.PreparedSource != "" {
				cmd = exec.CommandContext(ctx, "git", "-C", source, "remote", "set-url", "origin", ic.Request.GitURL)
				if err := cmd.Run(); err != nil {
					return r, err
				}
			}
		} else {
			cmd := exec.CommandContext(ctx, "git", "-C", source, "pull", "--ff-only", "origin", ic.Request.GitBranch)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return r, fmt.Errorf("update Git source: %w", err)
			}
		}
	} else {
		source = ic.Home
	}
	entry := ic.Request.EntryFile
	if entry == "" {
		if p.spec.ID == "node-bot" {
			entry = "index.js"
		} else {
			entry = "main.py"
		}
	}
	entry = filepath.Clean(entry)
	if filepath.IsAbs(entry) || strings.HasPrefix(entry, ".."+string(filepath.Separator)) {
		return r, fmt.Errorf("ENTRY_FILE must stay inside source")
	}
	if _, err := os.Stat(filepath.Join(source, entry)); err != nil {
		return r, fmt.Errorf("entry file: %w", err)
	}
	args, err := SplitArgs(ic.Request.AppArgs)
	if err != nil {
		return r, err
	}
	runtimePath := ic.Runtime
	if p.spec.ID == "node-bot" {
		if _, err := os.Stat(filepath.Join(source, "package.json")); err == nil {
			npm := filepath.Join(filepath.Dir(ic.Runtime), "npm")
			npmArgs := []string{"install", "--omit=dev", "--no-audit", "--no-fund"}
			if _, err := os.Stat(filepath.Join(source, "package-lock.json")); err == nil {
				npmArgs[0] = "ci"
			}
			cmd := exec.CommandContext(ctx, npm, npmArgs...)
			cmd.Dir = source
			cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(ic.Runtime)+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return r, fmt.Errorf("install Node.js dependencies: %w", err)
			}
		}
	} else {
		venv := filepath.Join(ic.ControlDir, "managed", p.spec.ID, "venv-"+r.RuntimeVersion)
		venvPython := filepath.Join(venv, "bin", "python3")
		if _, err := os.Stat(venvPython); os.IsNotExist(err) {
			cmd := exec.CommandContext(ctx, ic.Runtime, "-m", "venv", venv)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return r, fmt.Errorf("create Python virtualenv: %w", err)
			}
		}
		if _, err := os.Stat(filepath.Join(source, "requirements.txt")); err == nil {
			cmd := exec.CommandContext(ctx, venvPython, "-m", "pip", "install", "--disable-pip-version-check", "-r", "requirements.txt")
			cmd.Dir = source
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return r, fmt.Errorf("install Python dependencies: %w", err)
			}
		}
		runtimePath = venvPython
	}
	r.WorkDir = source
	r.Command = append([]string{runtimePath, entry}, args...)
	return r, nil
}

func SplitArgs(raw string) ([]string, error) {
	var out []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, ch := range raw {
		if escaped {
			b.WriteRune(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				b.WriteRune(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch == ' ' || ch == '\t' {
			flush()
		} else {
			b.WriteRune(ch)
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("invalid APP_ARGS quoting")
	}
	flush()
	return out, nil
}
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
func extractZipSafe(path, dst string) error {
	z, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer z.Close()
	for _, f := range z.File {
		clean := filepath.Clean(f.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", f.Name)
		}
		target := filepath.Join(dst, clean)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive may not contain symlink %q", f.Name)
		}
		if _, err := os.Stat(target); err == nil && isBedrockConfig(clean) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode()|0o500)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}
func isBedrockConfig(name string) bool {
	switch filepath.Base(name) {
	case "server.properties", "allowlist.json", "permissions.json":
		return true
	}
	return false
}

func linkMutableData(home, work string, dirs, files []string) error {
	for _, name := range append(dirs, files...) {
		shared, local := filepath.Join(home, name), filepath.Join(work, name)
		isDir := contains(dirs, name)
		if _, err := os.Lstat(shared); os.IsNotExist(err) {
			if _, localErr := os.Lstat(local); localErr == nil {
				if err := os.Rename(local, shared); err != nil {
					return err
				}
			} else if isDir {
				if err := os.MkdirAll(shared, 0o750); err != nil {
					return err
				}
			}
		}
		if _, err := os.Lstat(local); err == nil {
			if err := os.RemoveAll(local); err != nil {
				return err
			}
		}
		if err := os.Symlink(shared, local); err != nil {
			return fmt.Errorf("link persistent %s: %w", name, err)
		}
	}
	return nil
}

func serverMemory() string {
	value := envDefault("SERVER_MEMORY", "1024")
	if matched, _ := regexp.MatchString(`^[0-9]+$`, value); matched {
		return value + "M"
	}
	return value
}

var _ = json.Valid
