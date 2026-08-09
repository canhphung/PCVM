package pcvm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fixtureTransport map[string]string

func (f fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, ok := f[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("missing fixture")), Header: make(http.Header), Request: req}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
}

func TestPaperResolverContractFixture(t *testing.T) {
	fixtures := fixtureTransport{
		"https://fill.papermc.io/v3/projects/paper":                        `{"versions":{"1.21":["1.21.4"],"1.20":["1.20.6"]}}`,
		"https://fill.papermc.io/v3/projects/paper/versions/1.21.4/builds": `[{"id":12,"channel":"STABLE","downloads":{"server:default":{"name":"paper.jar","url":"https://fill-data.papermc.io/paper.jar","checksums":{"sha256":"` + strings.Repeat("a", 64) + `"}}}}]`,
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	spec := ProviderSpec{ID: "paper", Name: "Paper", Family: "bukkit", Architectures: []string{"amd64"}, Runtime: "java", Resolver: "papermc", Installer: "jar", Options: map[string]string{"project": "paper"}}
	r, err := NewProvider(spec).Resolve(context.Background(), Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if r.Artifact.Version != "1.21.4" || r.Artifact.Build != "12" || !strings.Contains(r.Artifact.URL, "paper.jar") {
		t.Fatalf("%+v", r.Artifact)
	}
	if r.RuntimeVersion != "21" {
		t.Fatalf("runtime=%s", r.RuntimeVersion)
	}
}

func TestMojangResolverContractFixture(t *testing.T) {
	detail := "https://piston-meta.mojang.com/v/1.21.4.json"
	fixtures := fixtureTransport{
		"https://piston-meta.mojang.com/mc/game/version_manifest_v2.json": fmt.Sprintf(`{"latest":{"release":"1.21.4"},"versions":[{"id":"1.21.4","url":%q}]}`, detail),
		detail: `{"downloads":{"server":{"url":"https://piston-data.mojang.com/server.jar","sha1":"abcd","size":4}}}`,
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	spec := ProviderSpec{ID: "vanilla", Name: "Vanilla", Family: "vanilla", Architectures: []string{"amd64"}, Runtime: "java", Resolver: "mojang", Installer: "jar"}
	r, err := NewProvider(spec).Resolve(context.Background(), Request{Version: "latest", RuntimeVersion: "auto"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if r.Artifact.Version != "1.21.4" || r.Artifact.SHA1 != "abcd" {
		t.Fatalf("%+v", r.Artifact)
	}
}

func TestBedrockResolverContractFixture(t *testing.T) {
	fixtures := fixtureTransport{"https://net-secondary.web.minecraft-services.net/api/v1.0/download/links": `{"result":{"links":[{"downloadType":"serverBedrockLinux","downloadUrl":"https://www.minecraft.net/bedrockdedicatedserver/bin-linux/bedrock-server-1.26.36.1.zip"}]}}`}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	artifact, err := resolveBedrock(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.26.36.1" || !strings.HasSuffix(artifact.URL, ".zip") {
		t.Fatalf("%+v", artifact)
	}
}

func TestBedrockExtensionResolverFixtures(t *testing.T) {
	t.Run("PowerNukkitX", func(t *testing.T) {
		fixtures := fixtureTransport{
			"https://api.github.com/repos/PowerNukkitX/PowerNukkitX/releases/latest": `{"tag_name":"3.0.2","assets":[{"name":"powernukkitx.jar","browser_download_url":"https://github.com/PowerNukkitX/PowerNukkitX/releases/download/3.0.2/powernukkitx.jar","digest":"sha256:` + strings.Repeat("a", 64) + `"}]}`,
		}
		h := NewHTTPClient()
		h.Client = &http.Client{Transport: fixtures}
		resolved, err := NewProvider(catalogSpec(t, "powernukkitx")).Resolve(context.Background(), Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, h)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Artifact.Version != "3.0.2" || resolved.RuntimeVersion != "21" || len(resolved.Artifact.SHA256) != 64 {
			t.Fatalf("resolved=%+v", resolved)
		}
	})

	t.Run("CloudburstNukkit", func(t *testing.T) {
		const base = "https://repo.opencollab.dev/maven-snapshots/cn/nukkit/nukkit/1.0-SNAPSHOT/"
		fixtures := fixtureTransport{
			base + "maven-metadata.xml":                         `<?xml version="1.0"?><metadata><versioning><snapshot><buildNumber>1241</buildNumber></snapshot><snapshotVersions><snapshotVersion><classifier>dev</classifier><extension>jar</extension><value>1.0-build-dev</value></snapshotVersion><snapshotVersion><extension>jar</extension><value>1.0-20260806.230407-1241</value></snapshotVersion></snapshotVersions></versioning></metadata>`,
			base + "nukkit-1.0-20260806.230407-1241.jar.sha256": strings.Repeat("b", 64),
		}
		h := NewHTTPClient()
		h.Client = &http.Client{Transport: fixtures}
		resolved, err := NewProvider(catalogSpec(t, "cloudburst-nukkit")).Resolve(context.Background(), Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, h)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Artifact.Build != "1241" || resolved.RuntimeVersion != "8" || !strings.HasSuffix(resolved.Artifact.URL, "1241.jar") {
			t.Fatalf("resolved=%+v", resolved)
		}
	})

	t.Run("Endstone", func(t *testing.T) {
		fixtures := fixtureTransport{
			"https://pypi.org/pypi/endstone/json": `{"info":{"version":"0.11.8"},"urls":[{"filename":"endstone-0.11.8-cp313-cp313-manylinux_2_31_x86_64.whl","packagetype":"bdist_wheel","url":"https://files.pythonhosted.org/endstone.whl","digests":{"sha256":"` + strings.Repeat("c", 64) + `"}}]}`,
		}
		h := NewHTTPClient()
		h.Client = &http.Client{Transport: fixtures}
		resolved, err := NewProvider(catalogSpec(t, "endstone")).Resolve(context.Background(), Request{Version: "latest", RuntimeVersion: "auto"}, h)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Artifact.Version != "0.11.8" || resolved.RuntimeVersion != "3.13" || resolved.Artifact.Kind != "wheel" {
			t.Fatalf("resolved=%+v", resolved)
		}
	})
}

func TestOpenMPAndCodeServerResolverFixtures(t *testing.T) {
	tests := []struct {
		name, provider, endpoint, tag, asset, wantSHA string
		request                                       Request
	}{
		{
			name: "openmp", provider: "samp", endpoint: "https://api.github.com/repos/openmultiplayer/open.mp/releases/latest",
			tag: "v1.5.8.3079", asset: "open.mp-linux-x86.tar.gz", wantSHA: strings.Repeat("a", 64),
			request: Request{Version: "latest", Build: "latest", Architecture: "amd64"},
		},
		{
			name: "code-server-amd64", provider: "code-server", endpoint: "https://api.github.com/repos/coder/code-server/releases/latest",
			tag: "v4.131.0", asset: "code-server-4.131.0-linux-amd64.tar.gz", wantSHA: strings.Repeat("b", 64),
			request: Request{Version: "latest", Build: "latest", Architecture: "amd64"},
		},
		{
			name: "code-server-arm64", provider: "code-server", endpoint: "https://api.github.com/repos/coder/code-server/releases/latest",
			tag: "v4.131.0", asset: "code-server-4.131.0-linux-arm64.tar.gz", wantSHA: strings.Repeat("c", 64),
			request: Request{Version: "latest", Build: "latest", Architecture: "arm64"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := `{"tag_name":"` + test.tag + `","assets":[{"name":"` + test.asset + `","browser_download_url":"https://github.com/example/` + test.asset + `","digest":"sha256:` + test.wantSHA + `"}]}`
			h := NewHTTPClient()
			h.Client = &http.Client{Transport: fixtureTransport{test.endpoint: fixture}}
			resolved, err := NewProvider(catalogSpec(t, test.provider)).Resolve(context.Background(), test.request, h)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Artifact.Version != test.tag || resolved.Artifact.FileName != test.asset || resolved.Artifact.SHA256 != test.wantSHA {
				t.Fatalf("artifact=%+v", resolved.Artifact)
			}
		})
	}
}

func TestMTAResolverUsesPinnedBundleIdentity(t *testing.T) {
	resolved, err := NewProvider(catalogSpec(t, "mtasa")).Resolve(context.Background(), Request{Version: "latest", Build: "latest", Architecture: "amd64"}, NewHTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Artifact.Version != "1.6.0" || resolved.Artifact.Build != "23312" || len(resolved.Artifact.SHA256) != 64 {
		t.Fatalf("artifact=%+v", resolved.Artifact)
	}
	if _, err := NewProvider(catalogSpec(t, "mtasa")).Resolve(context.Background(), Request{Version: "9.9.9", Architecture: "amd64"}, NewHTTPClient()); err == nil {
		t.Fatal("unpinned MTA version was accepted")
	}
}

func TestBedrockJavaProcessArguments(t *testing.T) {
	state := LaunchState{Command: []string{"java", "-jar", "server.jar"}, WorkingDirectory: "/home/container"}
	cfg := Config{AllocationPort: 19140, Request: Request{ServerName: "PCVM Bedrock"}}
	power, err := NewProvider(catalogSpec(t, "powernukkitx")).BuildProcess(context.Background(), cfg, state, catalogMemoryPlan(t, "powernukkitx"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(power.Command, " ")
	for _, want := range []string{"--skip-setup", "--accept-license", "--server-name PCVM Bedrock", "--port 19140"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("PowerNukkitX command missing %q: %v", want, power.Command)
		}
	}
	cloudburst, err := NewProvider(catalogSpec(t, "cloudburst-nukkit")).BuildProcess(context.Background(), cfg, state, catalogMemoryPlan(t, "cloudburst-nukkit"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cloudburst.Command, " "), "--language eng") {
		t.Fatalf("Cloudburst command=%v", cloudburst.Command)
	}
}

func TestPufferfishResolverUsesJenkinsArtifactFixture(t *testing.T) {
	fixtures := fixtureTransport{"https://ci.pufferfish.host/job/Pufferfish-1.21/api/json": `{"lastSuccessfulBuild":{"number":39,"url":"https://ci.pufferfish.host/job/Pufferfish-1.21/39/"}}`, "https://ci.pufferfish.host/job/Pufferfish-1.21/39/api/json": `{"artifacts":[{"fileName":"pufferfish-paperclip-1.21.10-R0.1-SNAPSHOT-mojmap.jar","relativePath":"pufferfish-server/build/libs/pufferfish-paperclip-1.21.10-R0.1-SNAPSHOT-mojmap.jar"}]}`}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	artifact, err := resolvePufferfish(context.Background(), Request{Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.21.10" || !strings.Contains(artifact.URL, "pufferfish-server/build/libs") {
		t.Fatalf("%+v", artifact)
	}
}

func TestGenerateStarterEntries(t *testing.T) {
	tests := []struct {
		provider string
		entry    string
		want     string
	}{
		{provider: "node-bot", entry: "index.js", want: `console.log("Hello World from PCVM!")`},
		{provider: "python-bot", entry: "main.py", want: `print("Hello World from PCVM!", flush=True)`},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.entry)
			generated, err := generateStarterEntry(test.provider, path)
			if err != nil || !generated {
				t.Fatalf("generated=%v err=%v", generated, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), test.want) {
				t.Fatalf("starter does not contain %q:\n%s", test.want, data)
			}
			if generated, err = generateStarterEntry(test.provider, path); err != nil || generated {
				t.Fatalf("existing entry was overwritten: generated=%v err=%v", generated, err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || string(unchanged) != string(data) {
				t.Fatalf("existing entry changed: err=%v", err)
			}
		})
	}
}

func TestUploadBotInstallGeneratesDefaultEntry(t *testing.T) {
	tests := []struct {
		provider       string
		entry          string
		runtimeVersion string
	}{
		{provider: "node-bot", entry: "index.js", runtimeVersion: "24"},
		{provider: "python-bot", entry: "main.py", runtimeVersion: "3.13"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			home := t.TempDir()
			control := filepath.Join(home, ".pcvm")
			runtimePath := filepath.Join(control, "runtime", test.provider)
			if err := os.MkdirAll(filepath.Dir(runtimePath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(runtimePath, []byte("fixture"), 0o750); err != nil {
				t.Fatal(err)
			}
			if test.provider == "python-bot" {
				venvPython := filepath.Join(control, "managed", test.provider, "venv-"+test.runtimeVersion, "bin", "python3")
				if err := os.MkdirAll(filepath.Dir(venvPython), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(venvPython, []byte("fixture"), 0o750); err != nil {
					t.Fatal(err)
				}
			}

			installed, err := NewProvider(catalogSpec(t, test.provider)).Install(context.Background(), InstallContext{
				Home: home, ControlDir: control, Runtime: runtimePath,
				Request: Request{SourceMode: "upload"}, Log: NewLogger(io.Discard), Out: io.Discard, Err: io.Discard,
			}, Resolved{RuntimeVersion: test.runtimeVersion})
			if err != nil {
				t.Fatal(err)
			}
			if len(installed.Command) < 2 || installed.Command[1] != test.entry {
				t.Fatalf("command=%v", installed.Command)
			}
			if _, err := os.Stat(filepath.Join(home, test.entry)); err != nil {
				t.Fatalf("default entry was not generated: %v", err)
			}
		})
	}
}

func TestEndstoneInstallUsesStagedVirtualenvAndManagedPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX virtualenv shim")
	}
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	runtimePath := filepath.Join(home, "python-shim")
	shim := `#!/bin/sh
set -eu
if [ "${1:-}" = "-m" ] && [ "${2:-}" = "venv" ]; then
  mkdir -p "$3/bin"
  cp "$0" "$3/bin/python3"
  chmod 0750 "$3/bin/python3"
  exit 0
fi
if [ "${1:-}" = "-m" ] && [ "${2:-}" = "pip" ]; then
  exit 0
fi
exit 9
`
	if err := os.WriteFile(runtimePath, []byte(shim), 0o750); err != nil {
		t.Fatal(err)
	}
	wheel := filepath.Join(home, "endstone.whl")
	if err := os.WriteFile(wheel, []byte("fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	installed, err := NewProvider(catalogSpec(t, "endstone")).Install(context.Background(), InstallContext{
		Home: home, ControlDir: control, AllocationPort: 19145, Artifact: wheel, Runtime: runtimePath,
		Request: Request{AcceptEULA: true, ServerName: "PCVM\nBedrock", MaxPlayers: 20}, Out: io.Discard, Err: io.Discard,
	}, Resolved{Artifact: Artifact{Version: "0.11.8"}, RuntimeVersion: "3.13"})
	if err != nil {
		t.Fatal(err)
	}
	if len(installed.Command) < 6 || installed.Command[1] != "-m" || installed.Command[2] != "endstone" || installed.Command[4] != home {
		t.Fatalf("command=%v", installed.Command)
	}
	if _, err := os.Stat(installed.Command[0]); err != nil {
		t.Fatalf("activated virtualenv is missing: %v", err)
	}
	properties, err := os.ReadFile(filepath.Join(home, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server-name=PCVM Bedrock", "max-players=20", "server-port=19145", "server-portv6=19145"} {
		if !strings.Contains(string(properties), want) {
			t.Fatalf("server.properties missing %q:\n%s", want, properties)
		}
	}
}
