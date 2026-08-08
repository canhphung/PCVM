package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/canhphung/PCVM/internal/pcvm"
)

var client = &http.Client{Timeout: 60 * time.Second}

func main() {
	out := flag.String("out", "runtime-manifest.json", "output file")
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
	sort.Slice(packs, func(i, j int) bool {
		a, b := packs[i], packs[j]
		return a.Kind+a.Version+a.Architecture < b.Kind+b.Version+b.Architecture
	})
	data, err := json.MarshalIndent(packs, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err = os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d checksum-pinned runtime packs to %s\n", len(packs), *out)
}

func javaPack(major, arch string) (pcvm.RuntimePackSpec, error) {
	apiArch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	endpoint := fmt.Sprintf("https://api.adoptium.net/v3/assets/latest/%s/hotspot?architecture=%s&image_type=jre&os=linux&vendor=eclipse", major, apiArch)
	var data []struct {
		Binary struct {
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
	p := data[0].Binary.Package
	return pcvm.RuntimePackSpec{Kind: "java", Version: major, Architecture: arch, URL: p.Link, SHA256: p.Checksum, Executable: "*/bin/java", Archive: "tar.gz"}, nil
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
	return pcvm.RuntimePackSpec{Kind: "node", Version: major, Architecture: arch, URL: "https://nodejs.org/dist/" + version + "/" + name, SHA256: checksum, Executable: "*/bin/node", Archive: "tar.gz"}, nil
}

type asset struct{ Name, URL, Digest string }

func githubAssets(repo string) ([]asset, error) {
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := getJSON("https://api.github.com/repos/"+repo+"/releases/latest", &release); err != nil {
		return nil, err
	}
	out := make([]asset, 0, len(release.Assets))
	for _, a := range release.Assets {
		out = append(out, asset{a.Name, a.URL, strings.TrimPrefix(a.Digest, "sha256:")})
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
	return pcvm.RuntimePackSpec{Kind: "python", Version: minor, Architecture: arch, URL: a.URL, SHA256: a.Digest, Executable: "python/bin/python3", Archive: "tar.gz"}, nil
}
func phpPack(assets []asset) (pcvm.RuntimePackSpec, error) {
	for _, a := range assets {
		if strings.HasPrefix(a.Name, "PHP-") && strings.Contains(a.Name, "Linux-x86_64") && !strings.Contains(a.Name, "debug") && strings.HasSuffix(a.Name, ".tar.gz") && len(a.Digest) == 64 {
			return pcvm.RuntimePackSpec{Kind: "php-pmmp", Version: "pmmp", Architecture: "amd64", URL: a.URL, SHA256: a.Digest, Executable: "bin/php7/bin/php", Archive: "tar.gz"}, nil
		}
	}
	return pcvm.RuntimePackSpec{}, fmt.Errorf("no digested PocketMine PHP Linux x86_64 asset")
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
