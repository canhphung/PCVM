package pcvm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewMinecraftProvidersSynchronizePrimaryAllocation(t *testing.T) {
	for _, provider := range []string{"folia", "canvas", "quilt", "paper-geyser", "modrinth-modpack"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			properties := "# user comment\nmotd=Unchanged\nserver-port=25565\nquery.port=25565\n"
			if err := os.WriteFile(filepath.Join(home, "server.properties"), []byte(properties), 0o640); err != nil {
				t.Fatal(err)
			}
			if provider == "paper-geyser" {
				config := "# retained\nbedrock:\n  address: 127.0.0.1\n  port: 19132\n  custom-option: keep\nremote:\n  address: auto\n  port: 25565\n  auth-type: online\nmetrics:\n  enabled: false\n"
				path := filepath.Join(home, filepath.FromSlash(geyserConfigRelative))
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(config), 0o640); err != nil {
					t.Fatal(err)
				}
			}

			app := &App{Config: Config{Home: home, AllocationPort: 30131}}
			changed, err := app.syncPrimaryAllocation(LaunchState{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("first allocation reconciliation reported no change")
			}
			data, err := os.ReadFile(filepath.Join(home, "server.properties"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"# user comment", "motd=Unchanged", "server-ip=0.0.0.0", "server-port=30131", "query.port=30131"} {
				if !strings.Contains(string(data), want) {
					t.Fatalf("%s server.properties missing %q:\n%s", provider, want, data)
				}
			}
			if provider == "paper-geyser" {
				config, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(geyserConfigRelative)))
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"# retained", "port: 30131", "custom-option: keep", "metrics:", "enabled: false", "auth-type: floodgate"} {
					if !strings.Contains(string(config), want) {
						t.Fatalf("Geyser config missing %q:\n%s", want, config)
					}
				}
			}
			changed, err = app.syncPrimaryAllocation(LaunchState{Provider: provider})
			if err != nil || changed {
				t.Fatalf("second allocation reconciliation changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestGeyserYAMLPatcherRejectsAmbiguousManagedKeys(t *testing.T) {
	for _, config := range []string{
		"bedrock: { port: 19132 }\n",
		"bedrock:\n  port: 19132\n  port: 19133\n",
		"bedrock:\n\tport: 19132\n",
	} {
		if _, err := patchGeyserYAML([]byte(config), 30131); err == nil {
			t.Fatalf("ambiguous config was accepted:\n%s", config)
		}
	}
}

func TestCanvasDecoderSupportsDocumentedArrayAndLegacyEnvelope(t *testing.T) {
	build := `{"buildNumber":9,"downloadUrl":"https://jenkins.canvasmc.io/job/Canvas/9/artifact/canvas.jar","channelVersion":"1.21.8","isExperimental":false}`
	for name, payload := range map[string]string{
		"documented-array": `[` + build + `]`,
		"legacy-envelope":  `{"project":"canvas","builds":[` + build + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var response canvasBuildResponse
			if err := json.Unmarshal([]byte(payload), &response); err != nil {
				t.Fatal(err)
			}
			if response.Project != "canvas" || len(response.Builds) != 1 || response.Builds[0].BuildNumber != 9 {
				t.Fatalf("decoded response=%+v", response)
			}
		})
	}

	endpoint := "https://canvasmc.io/api/v2/builds?project=canvas&experimental=false"
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtureTransport{endpoint: `[` + build + `]`}}
	artifact, err := resolveCanvas(context.Background(), Request{Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.21.8" || artifact.Build != "9" {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestPaperGeyserFamilyTransitionRemovesOnlyOwnedJarsAndRestores(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	plugins := filepath.Join(home, "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "Geyser-Spigot"), 0o750); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"plugins/Geyser-Spigot.jar":        "owned-geyser",
		"plugins/floodgate-spigot.jar":     "owned-floodgate",
		"plugins/UserPlugin.jar":           "user-plugin",
		"plugins/Geyser-Spigot/config.yml": "bedrock:\n  port: 19132\n  user-option: preserved\nremote:\n  port: 25565\n",
	}
	for relative, body := range paths {
		path := filepath.Join(home, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	geyserSpec := catalogSpec(t, "paper-geyser")
	geyserResolved := Resolved{Artifact: Artifact{Version: "1.21.8", Build: "12", SHA256: strings.Repeat("a", 64), Metadata: map[string]string{
		"geyser_version": "2.11.1", "geyser_build": "1212", "geyser_sha256": strings.Repeat("b", 64),
		"floodgate_version": "2.2.5", "floodgate_build": "140", "floodgate_sha256": strings.Repeat("c", 64),
	}}, RuntimeKind: "java", RuntimeVersion: "21"}
	request := Request{Version: "latest", Build: "latest", RuntimeVersion: "auto", Architecture: "amd64"}
	state := newStateFromInstall(geyserSpec, request, geyserResolved, "amd64", time.Now())
	var files []ReceiptFile
	for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
		if err := appendSealedReceiptFile(home, filepath.Join(home, filepath.FromSlash(relative)), &files); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	receipt := InstallReceipt{Schema: InstallReceiptSchema, ID: state.InstallID, Provider: state.Provider, InstallFormat: state.InstallFormat,
		ReleaseID: state.ArtifactLock.ID, RollbackMode: "staged", RootSHA256: receiptRoot(files), Files: files, Artifact: state.ArtifactLock, CreatedAt: time.Now()}
	name, err := SaveInstallReceipt(control, receipt)
	if err != nil {
		t.Fatal(err)
	}
	state.Receipt = name
	if err := SaveState(control, state); err != nil {
		t.Fatal(err)
	}

	target := catalogSpec(t, "paper")
	targetResolved := Resolved{Artifact: Artifact{Version: "1.21.8", Build: "13", SHA256: strings.Repeat("d", 64)}, RuntimeKind: "java", RuntimeVersion: "21"}
	ic := InstallContext{Home: home, ControlDir: control, Request: request}
	if err := stagePaperGeyserRemoval(ic, target, targetResolved); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("owned plugin %s was not removed: %v", relative, err)
		}
	}
	for _, relative := range []string{"plugins/UserPlugin.jar", "plugins/Geyser-Spigot/config.yml"} {
		data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(relative)))
		if err != nil || string(data) != paths[relative] {
			t.Fatalf("user path %s changed: %q err=%v", relative, data, err)
		}
	}
	if err := rollbackPendingInstallOverlay(home, control, target.ID); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
		data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(relative)))
		if err != nil || string(data) != paths[relative] {
			t.Fatalf("rollback did not restore %s: %q err=%v", relative, data, err)
		}
	}
}

func TestPaperGeyserReverseTransitionUsesCandidateConfig(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	configPath := filepath.Join(home, filepath.FromSlash(geyserConfigRelative))
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		t.Fatal(err)
	}
	original := "bedrock:\n  port: 19132\n  passthrough-motd: true\nremote:\n  port: 25565\nmetrics:\n  enabled: false\n"
	if err := os.WriteFile(configPath, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	userPlugin := filepath.Join(home, "plugins", "UserPlugin.jar")
	if err := os.WriteFile(userPlugin, []byte("user"), 0o640); err != nil {
		t.Fatal(err)
	}
	paperArtifact := filepath.Join(home, "paper.jar")
	if err := os.WriteFile(paperArtifact, []byte("paper"), 0o640); err != nil {
		t.Fatal(err)
	}
	body := "plugin-body"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	fixtures := fixtureTransport{
		"https://download.geysermc.org/v2/projects/geyser/versions/2.11.1/builds/1212/downloads/spigot":  body,
		"https://download.geysermc.org/v2/projects/floodgate/versions/2.2.5/builds/140/downloads/spigot": body,
	}
	httpClient := NewHTTPClient()
	httpClient.Client = &http.Client{Transport: fixtures}
	request := Request{Version: "latest", Build: "latest", RuntimeVersion: "auto", Architecture: "amd64"}
	resolved := Resolved{Artifact: Artifact{Version: "1.21.8", Build: "12", SHA256: strings.Repeat("a", 64), Metadata: map[string]string{
		"geyser_version": "2.11.1", "geyser_build": "1212", "geyser_name": "Geyser-Spigot.jar", "geyser_sha256": digest,
		"floodgate_version": "2.2.5", "floodgate_build": "140", "floodgate_name": "floodgate-spigot.jar", "floodgate_sha256": digest,
	}}, RuntimeKind: "java", RuntimeVersion: "21"}
	provider := NewProvider(catalogSpec(t, "paper-geyser")).(*catalogProvider)
	installed, err := provider.installPaperGeyser(context.Background(), InstallContext{Home: home, ControlDir: control, AllocationPort: 30132,
		Artifact: paperArtifact, Runtime: "/runtime/java", Request: request, HTTP: httpClient}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if installed.RollbackMode != "staged" {
		t.Fatalf("rollback mode=%q", installed.RollbackMode)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"port: 30132", "passthrough-motd: true", "metrics:", "enabled: false"} {
		if !strings.Contains(string(config), want) {
			t.Fatalf("candidate config lost %q:\n%s", want, config)
		}
	}
	if data, err := os.ReadFile(userPlugin); err != nil || string(data) != "user" {
		t.Fatalf("user plugin changed: %q err=%v", data, err)
	}
	for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
		if data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(relative))); err != nil || string(data) != body {
			t.Fatalf("managed plugin %s not restored: %q err=%v", relative, data, err)
		}
	}
	if err := rollbackPendingInstallOverlay(home, control, "paper-geyser"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(configPath); err != nil || string(data) != original {
		t.Fatalf("rollback did not restore exact config: %q err=%v", data, err)
	}
}

func TestMinecraftJavaRuntimePatchBoundaries(t *testing.T) {
	for version, want := range map[string]string{
		"1.12.2": "8",
		"1.13.0": "11",
		"1.16.5": "11",
		"1.17.1": "17",
		"1.20.4": "17",
		"1.20.5": "21",
		"1.20.6": "21",
		"1.21.0": "21",
		"26.1":   "25",
	} {
		if got := JavaVersionFor(version); got != want {
			t.Errorf("JavaVersionFor(%q)=%s, want %s", version, got, want)
		}
	}
}

func TestModrinthMutableServerPropertiesFollowLiveConfig(t *testing.T) {
	home := t.TempDir()
	candidate := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "server.properties"), []byte("server-port=30133\nmotd=user\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "server.properties"), []byte("server-port=25565\nmotd=pack\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := preserveModrinthMutableFiles(home, candidate); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(candidate, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "server-port=30133\nmotd=user\n" {
		t.Fatalf("candidate did not preserve live properties: %q", data)
	}
	managed, err := modrinthCandidateDigests(candidate, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range modrinthMutableFiles {
		delete(managed, relative)
	}
	if _, ok := managed["server.properties"]; ok {
		t.Fatal("server.properties remained a conflict-checked Modrinth artifact")
	}
}
