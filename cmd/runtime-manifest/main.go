package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/canhphung/PCVM/internal/pcvm"
)

var client = &http.Client{Timeout: 10 * time.Minute}

func main() {
	out := flag.String("out", "runtime-manifest.json", "output file")
	cache := flag.String("cache", "", "archive cache used while finalizing packs (temporary when empty)")
	flag.Parse()
	var packs []pcvm.RuntimePackSpec
	var err error
	for _, major := range []string{"8", "11", "17", "21", "25"} {
		for _, arch := range []string{"amd64", "arm64"} {
			var p pcvm.RuntimePackSpec
			p, err = javaPack(major, arch)
			if err != nil {
				fatal(err)
			}
			packs = append(packs, p)
		}
	}
	for _, major := range []string{"22", "24"} {
		for _, arch := range []string{"amd64", "arm64"} {
			var p pcvm.RuntimePackSpec
			p, err = nodePack(major, arch)
			if err != nil {
				fatal(err)
			}
			packs = append(packs, p)
		}
	}
	assets, err := githubAssets("astral-sh/python-build-standalone")
	if err != nil {
		fatal(err)
	}
	for _, minor := range []string{"3.11", "3.12", "3.13", "3.14"} {
		for _, arch := range []string{"amd64", "arm64"} {
			p, e := pythonPack(minor, arch, assets)
			if e != nil {
				fatal(e)
			}
			packs = append(packs, p)
		}
	}
	phpAssets, err := githubAssets("pmmp/PHP-Binaries")
	if err != nil {
		fatal(err)
	}
	p, err := phpPack(phpAssets)
	if err != nil {
		fatal(err)
	}
	packs = append(packs, p)
	for _, arch := range []string{"amd64", "arm64"} {
		p, err := caddyPack(arch)
		if err != nil {
			fatal(err)
		}
		packs = append(packs, p)
	}
	for _, major := range []string{"8", "9"} {
		for _, arch := range []string{"amd64", "arm64"} {
			p, err := dotnetPack(major, arch, false)
			if err != nil {
				fatal(err)
			}
			packs = append(packs, p)
		}
	}
	p, err = steamCMDPack()
	if err != nil {
		fatal(err)
	}
	packs = append(packs, p)
	for _, arch := range []string{"amd64", "arm64"} {
		for _, source := range []func(string) (pcvm.RuntimePackSpec, error){bunPack, denoPack, goPack} {
			p, err := source(arch)
			if err != nil {
				fatal(err)
			}
			packs = append(packs, p)
		}
		p, err := dotnetPack("10", arch, true)
		if err != nil {
			fatal(err)
		}
		packs = append(packs, p)
	}
	cleanup := func() {}
	if *cache == "" {
		*cache, err = os.MkdirTemp("", "pcvm-runtime-manifest-")
		if err != nil {
			fatal(err)
		}
		cleanup = func() { _ = os.RemoveAll(*cache) }
	}
	defer cleanup()
	if err := os.MkdirAll(*cache, 0o750); err != nil {
		fatal(err)
	}
	for i := range packs {
		packs[i], err = finalizePack(packs[i], *cache)
		if err != nil {
			fatal(fmt.Errorf("finalize %s/%s/%s: %w", packs[i].Kind, packs[i].Version, packs[i].Architecture, err))
		}
		fmt.Printf("finalized %s (%d bytes)\n", packs[i].ID, packs[i].Size)
	}
	sort.Slice(packs, func(i, j int) bool {
		a, b := packs[i], packs[j]
		return a.Kind+a.Version+a.Architecture < b.Kind+b.Version+b.Architecture
	})
	manifest := pcvm.RuntimeManifest{
		Schema: pcvm.RuntimeManifestSchema, Release: "2.0.0",
		Compatibility: "pcvm>=2.0.0", Packs: packs,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err = os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d checksum-pinned runtime packs to %s\n", len(packs), *out)
}

func finalizePack(pack pcvm.RuntimePackSpec, cache string) (pcvm.RuntimePackSpec, error) {
	urlHash := sha256.Sum256([]byte(pack.URL))
	archive := filepath.Join(cache, fmt.Sprintf("%x-%s", urlHash[:8], filepath.Base(mustURLPath(pack.URL))))
	if finalized, err := pcvm.FinalizeRuntimePack(archive, pack); err == nil {
		return finalized, nil
	}
	if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
		return pcvm.RuntimePackSpec{}, err
	}
	if err := downloadFile(pack.URL, archive, 1<<30); err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	finalized, err := pcvm.FinalizeRuntimePack(archive, pack)
	if err != nil {
		_ = os.Remove(archive)
		return pcvm.RuntimePackSpec{}, err
	}
	return finalized, nil
}

func mustURLPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || filepath.Base(u.Path) == "." || filepath.Base(u.Path) == string(filepath.Separator) {
		fatal(fmt.Errorf("invalid runtime URL %q", raw))
	}
	return u.Path
}

func downloadFile(raw, destination string, limit int64) error {
	body, err := request(raw)
	if err != nil {
		return err
	}
	defer body.Close()
	temporary := destination + ".part"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		if removeErr := os.Remove(temporary); removeErr != nil {
			return removeErr
		}
		file, err = os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(body, limit+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > limit {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return fmt.Errorf("download exceeds %d bytes", limit)
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func javaPack(major, arch string) (pcvm.RuntimePackSpec, error) {
	apiArch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	endpoint := fmt.Sprintf("https://api.adoptium.net/v3/assets/latest/%s/hotspot?architecture=%s&image_type=jre&os=linux&vendor=eclipse", major, apiArch)
	var data []struct {
		ReleaseName string `json:"release_name"`
		Binary      struct {
			Package struct {
				Name     string `json:"name"`
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binary"`
	}
	if err := getJSON(endpoint, &data); err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	if len(data) == 0 {
		return pcvm.RuntimePackSpec{}, fmt.Errorf("no Temurin Java %s/%s", major, arch)
	}
	if strings.TrimSpace(data[0].ReleaseName) == "" {
		return pcvm.RuntimePackSpec{}, fmt.Errorf("Temurin Java %s/%s has no exact release name", major, arch)
	}
	p := data[0].Binary.Package
	return pcvm.RuntimePackSpec{Kind: "java", Version: major, UpstreamVersion: data[0].ReleaseName, Architecture: arch, URL: p.Link, SHA256: p.Checksum, Executable: "*/bin/java", Archive: "tar.gz"}, nil
}

func nodePack(major, arch string) (pcvm.RuntimePackSpec, error) {
	var releases []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := getJSON("https://nodejs.org/dist/index.json", &releases); err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	version := ""
	for _, r := range releases {
		if strings.HasPrefix(r.Version, "v"+major+".") {
			version = r.Version
			break
		}
	}
	if version == "" {
		return pcvm.RuntimePackSpec{}, fmt.Errorf("no Node.js %s", major)
	}
	nodeArch := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]
	name := fmt.Sprintf("node-%s-linux-%s.tar.gz", version, nodeArch)
	sums, err := getText("https://nodejs.org/dist/" + version + "/SHASUMS256.txt")
	if err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	checksum := ""
	scan := bufio.NewScanner(strings.NewReader(sums))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 2 && fields[1] == name {
			checksum = fields[0]
			break
		}
	}
	if checksum == "" {
		return pcvm.RuntimePackSpec{}, fmt.Errorf("Node checksum absent for %s", name)
	}
	return pcvm.RuntimePackSpec{Kind: "node", Version: major, UpstreamVersion: version, Architecture: arch, URL: "https://nodejs.org/dist/" + version + "/" + name, SHA256: checksum, Executable: "*/bin/node", Archive: "tar.gz"}, nil
}

type asset struct{ Name, URL, Digest, UpstreamVersion string }

func githubAssets(repo string) ([]asset, error) {
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := getJSON("https://api.github.com/repos/"+repo+"/releases/latest", &release); err != nil {
		return nil, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return nil, fmt.Errorf("GitHub release %s has no exact tag", repo)
	}
	out := make([]asset, 0, len(release.Assets))
	for _, a := range release.Assets {
		out = append(out, asset{Name: a.Name, URL: a.URL, Digest: strings.TrimPrefix(a.Digest, "sha256:"), UpstreamVersion: release.TagName})
	}
	return out, nil
}
func pythonPack(minor, arch string, assets []asset) (pcvm.RuntimePackSpec, error) {
	platform := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch] + "-unknown-linux-gnu-install_only.tar.gz"
	var matches []asset
	for _, a := range assets {
		if strings.HasPrefix(a.Name, "cpython-"+minor+".") && strings.HasSuffix(a.Name, platform) && !strings.Contains(a.Name, "stripped") {
			matches = append(matches, a)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name > matches[j].Name })
	if len(matches) == 0 || len(matches[0].Digest) != 64 {
		return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested Python %s/%s asset", minor, arch)
	}
	a := matches[0]
	upstreamVersion := strings.TrimSuffix(strings.TrimPrefix(a.Name, "cpython-"), "-"+platform)
	return pcvm.RuntimePackSpec{Kind: "python", Version: minor, UpstreamVersion: upstreamVersion, Architecture: arch, URL: a.URL, SHA256: a.Digest, Executable: "python/bin/python3", Archive: "tar.gz"}, nil
}
func phpPack(assets []asset) (pcvm.RuntimePackSpec, error) {
	for _, a := range assets {
		if strings.HasPrefix(a.Name, "PHP-") && strings.Contains(a.Name, "Linux-x86_64") && !strings.Contains(a.Name, "debug") && strings.HasSuffix(a.Name, ".tar.gz") && len(a.Digest) == 64 {
			return pcvm.RuntimePackSpec{Kind: "php-pmmp", Version: "pmmp", UpstreamVersion: a.UpstreamVersion, Architecture: "amd64", URL: a.URL, SHA256: a.Digest, Executable: "bin/php7/bin/php", Archive: "tar.gz"}, nil
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested PocketMine PHP Linux x86_64 asset")
}

func caddyPack(arch string) (pcvm.RuntimePackSpec, error) {
	assets, err := githubAssets("caddyserver/caddy")
	if err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	assetArch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[arch]
	pattern := "_linux_" + assetArch + ".tar.gz"
	for _, a := range assets {
		if strings.HasSuffix(a.Name, pattern) && len(a.Digest) == 64 {
			return pcvm.RuntimePackSpec{Kind: "caddy", Version: "2", UpstreamVersion: a.UpstreamVersion, Architecture: arch, URL: a.URL, SHA256: a.Digest, Executable: "caddy", Archive: "tar.gz"}, nil
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested Caddy Linux %s asset", arch)
}

func dotnetPack(major, arch string, sdk bool) (pcvm.RuntimePackSpec, error) {
	var metadata struct {
		LatestRuntime string `json:"latest-runtime"`
		LatestSDK     string `json:"latest-sdk"`
		Releases      []struct {
			Runtime struct {
				Version string `json:"version"`
				Files   []struct {
					Name, RID, URL, Hash string
				} `json:"files"`
			} `json:"runtime"`
			SDK struct {
				Version string `json:"version"`
				Files   []struct {
					Name, RID, URL, Hash string
				} `json:"files"`
			} `json:"sdk"`
		} `json:"releases"`
	}
	if err := getJSON("https://dotnetcli.blob.core.windows.net/dotnet/release-metadata/"+major+".0/releases.json", &metadata); err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	rid := map[string]string{"amd64": "linux-x64", "arm64": "linux-arm64"}[arch]
	for _, release := range metadata.Releases {
		version := release.Runtime.Version
		files := release.Runtime.Files
		label := "runtime"
		if sdk {
			version, files, label = release.SDK.Version, release.SDK.Files, "SDK"
			if version != metadata.LatestSDK {
				continue
			}
		} else if version != metadata.LatestRuntime {
			continue
		}
		for _, file := range files {
			if file.RID == rid && strings.HasSuffix(file.Name, ".tar.gz") {
				checksum, err := sha256URL(file.URL, 512<<20)
				if err != nil {
					return pcvm.RuntimePackSpec{}, fmt.Errorf("hash .NET %s %s: %w", major, label, err)
				}
				return pcvm.RuntimePackSpec{Kind: "dotnet", Version: major, UpstreamVersion: version, Architecture: arch, URL: file.URL, SHA256: checksum, Executable: "dotnet", Archive: "tar.gz"}, nil
			}
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no .NET %s pack for %s", major, arch)
}

func bunPack(arch string) (pcvm.RuntimePackSpec, error) {
	assets, err := githubAssets("oven-sh/bun")
	if err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	assetArch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	wanted := "bun-linux-" + assetArch + ".zip"
	for _, asset := range assets {
		if asset.Name == wanted && len(asset.Digest) == 64 {
			return pcvm.RuntimePackSpec{Kind: "bun", Version: "1", UpstreamVersion: asset.UpstreamVersion, Architecture: arch, URL: asset.URL, SHA256: asset.Digest, Executable: "bun-linux-" + assetArch + "/bun", Archive: "zip"}, nil
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested Bun Linux %s asset", arch)
}

func denoPack(arch string) (pcvm.RuntimePackSpec, error) {
	assets, err := githubAssets("denoland/deno")
	if err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	assetArch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	wanted := "deno-" + assetArch + "-unknown-linux-gnu.zip"
	for _, asset := range assets {
		if asset.Name == wanted && len(asset.Digest) == 64 {
			return pcvm.RuntimePackSpec{Kind: "deno", Version: "2", UpstreamVersion: asset.UpstreamVersion, Architecture: arch, URL: asset.URL, SHA256: asset.Digest, Executable: "deno", Archive: "zip"}, nil
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested Deno Linux %s asset", arch)
}

func goPack(arch string) (pcvm.RuntimePackSpec, error) {
	var releases []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			Version  string `json:"version"`
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	if err := getJSON("https://go.dev/dl/?mode=json", &releases); err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	for _, release := range releases {
		if !release.Stable || !strings.HasPrefix(release.Version, "go1.26.") {
			continue
		}
		for _, file := range release.Files {
			if file.OS == "linux" && file.Arch == arch && file.Kind == "archive" && len(file.SHA256) == 64 {
				return pcvm.RuntimePackSpec{Kind: "go", Version: "1.26", UpstreamVersion: release.Version, Architecture: arch, URL: "https://go.dev/dl/" + file.Filename, SHA256: file.SHA256, Executable: "go/bin/go", Archive: "tar.gz", Size: file.Size}, nil
			}
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no stable Go 1.26 Linux %s archive", arch)
}

func steamCMDPack() (pcvm.RuntimePackSpec, error) {
	raw := "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
	checksum, err := sha256URL(raw, 128<<20)
	if err != nil {
		return pcvm.RuntimePackSpec{}, err
	}
	// Valve does not publish a versioned SteamCMD bootstrap URL. Its archive
	// digest is therefore the only exact upstream release identifier.
	return pcvm.RuntimePackSpec{Kind: "steamcmd", Version: "1", UpstreamVersion: "sha256:" + checksum, Architecture: "amd64", URL: raw, SHA256: checksum, Executable: "steamcmd.sh", Archive: "tar.gz"}, nil
}

func sha256URL(raw string, limit int64) (string, error) {
	body, err := request(raw)
	if err != nil {
		return "", err
	}
	defer body.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(body, limit+1))
	if err != nil {
		return "", err
	}
	if written > limit {
		return "", fmt.Errorf("download exceeds %d bytes", limit)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func getJSON(raw string, value any) error {
	body, err := request(raw)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(value)
}
func getText(raw string) (string, error) {
	body, err := request(raw)
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 8<<20))
	return string(data), err
}
func request(raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("invalid URL")
	}
	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	req.Header.Set("User-Agent", "PCVM-runtime-manifest")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && u.Hostname() == "api.github.com" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, raw)
	}
	return resp.Body, nil
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
