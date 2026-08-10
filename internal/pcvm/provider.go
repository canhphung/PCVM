package pcvm

import (
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
	"time"
)

type catalogProvider struct {
	spec    ProviderSpec
	drivers providerDrivers
	initErr error
}

func NewProvider(spec ProviderSpec) Provider {
	drivers, err := compiledProviderDrivers(spec)
	return &catalogProvider{spec: spec, drivers: drivers, initErr: err}
}
func (p *catalogProvider) Spec() ProviderSpec { return p.spec }
func (p *catalogProvider) CompareVersions(a, b string) int {
	if p.drivers.Comparator == nil {
		return CompareVersions(a, b)
	}
	return p.drivers.Comparator.Compare(a, b)
}

func (p *catalogProvider) BuildProcess(ctx context.Context, cfg Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
	if p.initErr != nil {
		return ProcessSpec{}, p.initErr
	}
	if err := p.drivers.Validator.ValidateConfig(p, cfg); err != nil {
		return ProcessSpec{}, err
	}
	configured, err := p.drivers.Configurator.Configure(ctx, p, cfg, state)
	if err != nil {
		return ProcessSpec{}, fmt.Errorf("configure provider %s: %w", p.spec.ID, err)
	}
	process, err := p.drivers.Process.Build(ctx, p, cfg, configured, memory)
	if err != nil {
		return ProcessSpec{}, err
	}
	return p.drivers.Control.Apply(p, cfg, configured, process)
}

func (p *catalogProvider) Resolve(ctx context.Context, req Request, httpc *HTTPClient) (Resolved, error) {
	if p.initErr != nil {
		return Resolved{}, p.initErr
	}
	if err := p.drivers.Validator.ValidateRequest(p, req); err != nil {
		return Resolved{}, err
	}
	artifact, err := p.drivers.Resolver.Resolve(ctx, p, req, httpc)
	if err != nil {
		return Resolved{}, err
	}
	if (p.spec.Installer == "openmp" || p.spec.Installer == "code-server") && !validHexDigest(artifact.SHA256, 64) {
		return Resolved{}, fmt.Errorf("%s release asset has no upstream SHA-256 digest", p.spec.Name)
	}
	runtimeVersion, err := resolveRuntimeVersion(p.spec, req.RuntimeVersion, artifact)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Artifact: artifact, RuntimeKind: p.spec.Runtime, RuntimeVersion: runtimeVersion}, nil
}

func envLatest(value string) string {
	if value == "" {
		return "latest"
	}
	return value
}

func resolveTerraria(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var names []string
	if err := h.JSON(ctx, "https://terraria.org/api/get/dedicated-servers-names", &names); err != nil {
		return Artifact{}, err
	}
	if len(names) == 0 {
		return Artifact{}, fmt.Errorf("Terraria returned no dedicated server releases")
	}
	name := names[0]
	if req.Version != "" && req.Version != "latest" {
		digits := strings.ReplaceAll(req.Version, ".", "")
		name = "terraria-server-" + digits + ".zip"
	}
	version := strings.TrimSuffix(strings.TrimPrefix(name, "terraria-server-"), ".zip")
	return Artifact{URL: "https://terraria.org/api/download/pc-dedicated-server/" + name, FileName: name, Kind: "zip", Version: version, Build: "release"}, nil
}

func resolveFactorio(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	var releases struct {
		Stable struct {
			Headless string `json:"headless"`
		} `json:"stable"`
		Experimental struct {
			Headless string `json:"headless"`
		} `json:"experimental"`
	}
	if err := h.JSON(ctx, "https://factorio.com/api/latest-releases", &releases); err != nil {
		return Artifact{}, err
	}
	version := req.Version
	if version == "" || version == "latest" {
		version = releases.Stable.Headless
	} else if version == "experimental" {
		version = releases.Experimental.Headless
	}
	if version == "" {
		return Artifact{}, fmt.Errorf("Factorio returned no headless release")
	}
	return Artifact{URL: "https://www.factorio.com/get-download/" + url.PathEscape(version) + "/headless/linux64", FileName: "factorio-" + version + ".tar.xz", Kind: "tar.xz", Version: version, Build: "release"}, nil
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

func resolveCloudburstNukkit(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	version := req.Version
	if version == "" || version == "latest" {
		version = "1.0-SNAPSHOT"
	}
	if version != "1.0-SNAPSHOT" {
		return Artifact{}, fmt.Errorf("Cloudburst Nukkit supports SOFTWARE_VERSION=latest or 1.0-SNAPSHOT")
	}
	const base = "https://repo.opencollab.dev/maven-snapshots/cn/nukkit/nukkit/1.0-SNAPSHOT/"
	var metadata struct {
		Versioning struct {
			Snapshot struct {
				BuildNumber string `xml:"buildNumber"`
			} `xml:"snapshot"`
			Versions []struct {
				Classifier string `xml:"classifier"`
				Extension  string `xml:"extension"`
				Value      string `xml:"value"`
			} `xml:"snapshotVersions>snapshotVersion"`
		} `xml:"versioning"`
	}
	data, err := h.Text(ctx, base+"maven-metadata.xml", 1<<20)
	if err != nil {
		return Artifact{}, err
	}
	if err := xml.Unmarshal(data, &metadata); err != nil {
		return Artifact{}, fmt.Errorf("decode Cloudburst Nukkit metadata: %w", err)
	}
	value := ""
	for _, candidate := range metadata.Versioning.Versions {
		if candidate.Extension == "jar" && candidate.Classifier == "" {
			value = candidate.Value
			break
		}
	}
	if value == "" || metadata.Versioning.Snapshot.BuildNumber == "" {
		return Artifact{}, fmt.Errorf("Cloudburst Nukkit metadata contains no server JAR")
	}
	build := metadata.Versioning.Snapshot.BuildNumber
	if req.Build != "" && req.Build != "latest" && req.Build != build {
		return Artifact{}, fmt.Errorf("Cloudburst Nukkit build %s is unavailable; latest is %s", req.Build, build)
	}
	name := "nukkit-" + value + ".jar"
	checksum, err := h.Text(ctx, base+name+".sha256", 1024)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve Cloudburst Nukkit checksum: %w", err)
	}
	sha256 := strings.TrimSpace(string(checksum))
	if !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(sha256) {
		return Artifact{}, fmt.Errorf("Cloudburst Nukkit returned an invalid SHA-256")
	}
	return Artifact{URL: base + name, FileName: "nukkit.jar", Kind: "jar", SHA256: strings.ToLower(sha256), Version: version, Build: build}, nil
}

func resolveEndstone(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	pythonVersion := req.RuntimeVersion
	if pythonVersion == "" || pythonVersion == "auto" {
		pythonVersion = "3.13"
	}
	if pythonVersion != "3.11" && pythonVersion != "3.12" && pythonVersion != "3.13" && pythonVersion != "3.14" {
		return Artifact{}, fmt.Errorf("Endstone supports PCVM Python runtimes 3.11 through 3.14")
	}
	pythonTag := "cp" + strings.ReplaceAll(pythonVersion, ".", "")
	version := strings.TrimPrefix(req.Version, "v")
	endpoint := "https://pypi.org/pypi/endstone/json"
	if version != "" && version != "latest" {
		endpoint = "https://pypi.org/pypi/endstone/" + url.PathEscape(version) + "/json"
	}
	var release struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		URLs []struct {
			Filename    string `json:"filename"`
			PackageType string `json:"packagetype"`
			URL         string `json:"url"`
			Digests     struct {
				SHA256 string `json:"sha256"`
			} `json:"digests"`
		} `json:"urls"`
	}
	if err := h.JSON(ctx, endpoint, &release); err != nil {
		return Artifact{}, err
	}
	for _, file := range release.URLs {
		if file.PackageType == "bdist_wheel" && strings.Contains(file.Filename, "-"+pythonTag+"-"+pythonTag+"-manylinux_") && strings.HasSuffix(file.Filename, "_x86_64.whl") {
			if !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(file.Digests.SHA256) {
				return Artifact{}, fmt.Errorf("Endstone wheel lacks a valid SHA-256")
			}
			return Artifact{URL: file.URL, FileName: file.Filename, Kind: "wheel", SHA256: strings.ToLower(file.Digests.SHA256), Version: release.Info.Version, Build: "release"}, nil
		}
	}
	return Artifact{}, fmt.Errorf("Endstone release %s has no CPython %s Linux x86_64 wheel", release.Info.Version, pythonVersion)
}

func resolveGitHub(ctx context.Context, req Request, h *HTTPClient, repo, assetPattern string) (Artifact, error) {
	var release struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
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
			return Artifact{URL: a.URL, FileName: a.Name, Kind: kind, SHA256: strings.TrimPrefix(a.Digest, "sha256:"), Version: release.Tag, Build: "release"}, nil
		}
	}
	return Artifact{}, fmt.Errorf("release contains no matching artifact")
}

func resolvePinnedMTA(req Request, options DriverOptions) (Artifact, error) {
	version, build := options.Version, options.Build
	if version == "" || build == "" || options.MainURL == "" || !validHexDigest(options.MainSHA256, 64) {
		return Artifact{}, fmt.Errorf("MTA catalog entry is incomplete")
	}
	if req.Version != "" && req.Version != "latest" && req.Version != version {
		return Artifact{}, fmt.Errorf("MTA version %q is not pinned by this PCVM release", req.Version)
	}
	if req.Build != "" && req.Build != "latest" && req.Build != build {
		return Artifact{}, fmt.Errorf("MTA build %q is not pinned by this PCVM release", req.Build)
	}
	return Artifact{
		URL: options.MainURL, FileName: "multitheftauto-linux-" + version + "-" + build + ".tar.gz",
		Kind: "tar.gz", SHA256: options.MainSHA256, Version: version, Build: build,
	}, nil
}

func (p *catalogProvider) Install(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	return p.installWithDriver(ctx, ic, resolved, false)
}

// Update exposes the updater half of the compiled provider contract without
// widening the legacy Provider interface used by tests and orchestration. New
// reconciliation code can type-assert this capability while old callers retain
// behavior parity through Install.
func (p *catalogProvider) Update(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	return p.installWithDriver(ctx, ic, resolved, true)
}

func (p *catalogProvider) installWithDriver(ctx context.Context, ic InstallContext, resolved Resolved, update bool) (Resolved, error) {
	if p.initErr != nil {
		return resolved, p.initErr
	}
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return resolved, err
	}
	var installed Resolved
	var err error
	if update {
		installed, err = p.drivers.Updater.Update(ctx, p, ic, resolved)
	} else {
		installed, err = p.drivers.Installer.Install(ctx, p, ic, resolved)
	}
	if err != nil {
		return installed, err
	}
	installed, err = activateStagedRelease(ic, p.spec, installed, time.Now())
	if err != nil {
		cleanupFailedGenericRelease(ic.ControlDir, p.spec, installed)
		return installed, fmt.Errorf("activate staged release: %w", err)
	}
	return installed, nil
}

func (p *catalogProvider) installEndstone(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
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
	sitePackages := filepath.Join(candidate, "site-packages")
	environment, envErr := processUserEnvironment(p.spec.ID, ic.Home, os.Environ())
	if envErr != nil {
		return resolved, envErr
	}
	environment = upsertEnvironment(environment, "PATH", filepath.Dir(ic.Runtime)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.CommandContext(ctx, ic.Runtime, "-m", "pip", "install", "--disable-pip-version-check", "--no-cache-dir", "--only-binary=:all:", "--index-url", "https://pypi.org/simple", "--target", sitePackages, ic.Artifact)
	cmd.Stdout, cmd.Stderr = ic.Out, ic.Err
	cmd.Env = upsertEnvironment(environment, "PIP_CONFIG_FILE", os.DevNull)
	cmd.Env = upsertEnvironment(cmd.Env, "PIP_EXTRA_INDEX_URL", "")
	cmd.Env = upsertEnvironment(cmd.Env, "PIP_TRUSTED_HOST", "")
	cmd.Env = upsertEnvironment(cmd.Env, "PIP_NO_INPUT", "1")
	if err := cmd.Run(); err != nil {
		return resolved, fmt.Errorf("install Endstone wheel: %w", err)
	}
	if info, err := os.Lstat(filepath.Join(sitePackages, "endstone")); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return resolved, fmt.Errorf("Endstone wheel did not install a regular package tree")
	}
	if err := ensureEndstoneProperties(ic); err != nil {
		return resolved, err
	}
	resolved.WorkDir = candidate
	resolved.Command = []string{ic.Runtime, "-m", "endstone", "--server-folder", ic.Home, "--yes", "--remote", "https://raw.githubusercontent.com/EndstoneMC/bedrock-server-data/v2"}
	resolved.Environment = upsertEnvironment(resolved.Environment, "PYTHONUNBUFFERED", "1")
	resolved.Environment = upsertEnvironment(resolved.Environment, "PYTHONDONTWRITEBYTECODE", "1")
	resolved.Environment = upsertEnvironment(resolved.Environment, "PYTHONPATH", sitePackages)
	candidateLive = false
	return resolved, nil
}

func ensureEndstoneProperties(ic InstallContext) error {
	path := filepath.Join(ic.Home, "server.properties")
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return fmt.Errorf("server.properties is a directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	name := iniQuoted(ic.Request.ServerName)
	data := fmt.Sprintf("server-name=%s\ngamemode=survival\nforce-gamemode=false\ndifficulty=easy\nallow-cheats=false\nmax-players=%d\nonline-mode=true\nallow-list=false\nserver-port=%d\nserver-portv6=%d\nenable-lan-visibility=true\nview-distance=32\ntick-distance=4\nplayer-idle-timeout=30\nlevel-name=Bedrock level\ndefault-player-permission-level=member\ntexturepack-required=false\n", name, ic.Request.MaxPlayers, ic.AllocationPort, ic.AllocationPort)
	return writeAtomicFile(path, []byte(data), 0o640)
}

func (p *catalogProvider) installApp(ctx context.Context, ic InstallContext, r Resolved) (Resolved, error) {
	source, reused, err := p.materializeGenericSource(ctx, ic, r)
	if err != nil {
		return r, err
	}
	completed := false
	defer func() {
		if !completed && !reused && ic.Request.SourceMode == "git" {
			_ = os.RemoveAll(source)
		}
	}()
	if ic.Request.SourceMode == "git" {
		r.RollbackMode = "staged"
	}
	entry := ic.Request.EntryFile
	if entry == "" {
		if p.spec.ID == "node-bot" {
			entry = "index.js"
		} else {
			entry = "main.py"
		}
	}
	entry, err = cleanRelativeEntry(entry)
	if err != nil {
		return r, err
	}
	entryPath := filepath.Join(source, entry)
	info, err := os.Stat(entryPath)
	if os.IsNotExist(err) && ic.Request.SourceMode != "git" {
		generated, generateErr := generateStarterEntry(p.spec.ID, entryPath)
		if generateErr != nil {
			return r, fmt.Errorf("generate starter entry: %w", generateErr)
		}
		if generated && ic.Log != nil {
			ic.Log.Printf("generated Hello World starter %s for %s", entry, p.spec.Name)
		}
		info, err = os.Stat(entryPath)
	}
	if err != nil {
		return r, fmt.Errorf("entry file: %w", err)
	}
	if info.IsDir() {
		return r, fmt.Errorf("entry file is a directory")
	}
	args, err := SplitArgs(ic.Request.AppArgs)
	if err != nil {
		return r, err
	}
	runtimePath := ic.Runtime
	if p.spec.ID == "node-bot" {
		if _, err := os.Stat(filepath.Join(source, "package.json")); err == nil && !reused {
			npm := filepath.Join(filepath.Dir(ic.Runtime), "npm")
			npmArgs := []string{"install", "--omit=dev", "--no-audit", "--no-fund"}
			if _, err := os.Stat(filepath.Join(source, "package-lock.json")); err == nil {
				npmArgs[0] = "ci"
			}
			cacheRoot := filepath.Join(ic.ControlDir, "cache")
			if err := secureMkdirAll(ic.ControlDir, cacheRoot, 0o750); err != nil {
				return r, fmt.Errorf("prepare temporary npm cache: %w", err)
			}
			cache, err := os.MkdirTemp(cacheRoot, ".npm-cache-")
			if err != nil {
				return r, fmt.Errorf("create temporary npm cache: %w", err)
			}
			defer os.RemoveAll(cache)
			npmArgs = append(npmArgs, "--cache", cache)
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
		if ic.Request.SourceMode == "git" {
			sitePackages := filepath.Join(source, ".pcvm-site-packages")
			if _, err := os.Stat(filepath.Join(source, "requirements.txt")); err == nil && !reused {
				if err := secureMkdirAll(source, sitePackages, 0o750); err != nil {
					return r, err
				}
				cmd := exec.CommandContext(ctx, ic.Runtime, "-m", "pip", "install", "--disable-pip-version-check", "--no-cache-dir", "--target", sitePackages, "-r", "requirements.txt")
				cmd.Dir = source
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return r, fmt.Errorf("install Python dependencies: %w", err)
				}
			}
			runtimePath = ic.Runtime
			r.Environment = upsertEnvironment(r.Environment, "PYTHONPATH", sitePackages)
		} else {
			venvRoot := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
			venv := filepath.Join(venvRoot, "venv-"+r.RuntimeVersion)
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
				cmd := exec.CommandContext(ctx, venvPython, "-m", "pip", "install", "--disable-pip-version-check", "--no-cache-dir", "-r", "requirements.txt")
				cmd.Dir = source
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if err := cmd.Run(); err != nil {
					return r, fmt.Errorf("install Python dependencies: %w", err)
				}
			}
			runtimePath = venvPython
		}
	}
	r.WorkDir = source
	r.Command = append([]string{runtimePath, entry}, args...)
	completed = true
	return r, nil
}

func generateStarterEntry(providerID, path string) (bool, error) {
	var content string
	switch providerID {
	case "node-bot":
		content = `console.log("Hello World from PCVM!");

// Keep the starter process alive until Pterodactyl stops the server.
setInterval(() => {}, 60 * 60 * 1000);
`
	case "python-bot":
		content = `import time

print("Hello World from PCVM!", flush=True)

# Keep the starter process alive until Pterodactyl stops the server.
while True:
    time.sleep(3600)
`
	default:
		return false, fmt.Errorf("unsupported starter provider %q", providerID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	created = false
	return true, nil
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

var _ = json.Valid
