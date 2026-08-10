package pcvm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV2ProviderCatalogMatrix(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		family, installer, runtime string
	}{
		"modrinth-modpack": {"minecraft-java-modrinth", "modrinth", "java"},
		"folia":            {"minecraft-java-regionized", "jar", "java"},
		"canvas":           {"minecraft-java-regionized", "jar", "java"},
		"quilt":            {"minecraft-java-quilt", "quilt", "java"},
		"paper-geyser":     {"minecraft-java-bukkit", "paper-geyser", "java"},
		"tshock":           {"game-tshock", "tshock", "dotnet"},
		"bun-app":          {"app-bun", "generic-app", "bun"},
		"deno-app":         {"app-deno", "generic-app", "deno"},
		"go-app":           {"app-go", "generic-app", "go"},
		"dotnet-app":       {"app-dotnet", "generic-app", "dotnet"},
	}
	for id, expected := range want {
		spec, ok := catalog.Provider(id)
		if !ok {
			t.Errorf("provider %s is missing", id)
			continue
		}
		if spec.Family != expected.family || spec.Installer != expected.installer || spec.Runtime != expected.runtime || spec.SupportTier != "stable" {
			t.Errorf("provider %s metadata=%+v", id, spec)
		}
	}
}

func TestCanvasResolverContract(t *testing.T) {
	endpoint := "https://canvasmc.io/api/v2/builds?project=canvas&experimental=false"
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtureTransport{endpoint: `{"project":"canvas","builds":[
      {"buildNumber":8,"downloadUrl":"https://jenkins.canvasmc.io/job/Canvas/8/artifact/canvas.jar","channelVersion":"1.21.8","isExperimental":false},
      {"buildNumber":9,"downloadUrl":"https://jenkins.canvasmc.io/job/Canvas/9/artifact/canvas.jar","channelVersion":"1.21.8","isExperimental":false}
    ]}`}}
	artifact, err := resolveCanvas(context.Background(), Request{Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.21.8" || artifact.Build != "9" || artifact.FileName != "canvas.jar" {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestQuiltResolverContract(t *testing.T) {
	digest := strings.Repeat("a", 64)
	fixtures := fixtureTransport{
		"https://meta.quiltmc.org/v3/versions/game":                                                                         `[{"version":"1.21.8","stable":true}]`,
		"https://meta.quiltmc.org/v3/versions/loader/1.21.8":                                                                `[{"loader":{"version":"0.30.0"}},{"loader":{"version":"0.30.1-beta.1"}}]`,
		"https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/maven-metadata.xml":                       `<metadata><versioning><release>0.15.1</release></versioning></metadata>`,
		"https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-installer/0.15.1/quilt-installer-0.15.1.jar.sha256": digest + "  quilt-installer.jar\n",
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	artifact, err := resolveQuilt(context.Background(), Request{Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.21.8" || artifact.Build != "0.30.0" || artifact.SHA256 != digest || artifact.Metadata["installer"] != "0.15.1" {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestPaperGeyserCompositeResolver(t *testing.T) {
	digest := strings.Repeat("b", 64)
	fixtures := fixtureTransport{
		"https://fill.papermc.io/v3/projects/paper":                                         `{"versions":{"1.21":["1.21.8"]}}`,
		"https://fill.papermc.io/v3/projects/paper/versions/1.21.8/builds":                  `[{"id":12,"channel":"STABLE","downloads":{"server:default":{"url":"https://fill-data.papermc.io/paper.jar","checksums":{"sha256":"` + digest + `"}}}}]`,
		"https://download.geysermc.org/v2/projects/geyser/versions/latest/builds/latest":    `{"version":"2.11.1","build":1212,"downloads":{"spigot":{"name":"Geyser-Spigot.jar","sha256":"` + digest + `"}}}`,
		"https://download.geysermc.org/v2/projects/floodgate/versions/latest/builds/latest": `{"version":"2.2.5","build":140,"downloads":{"spigot":{"name":"floodgate-spigot.jar","sha256":"` + digest + `"}}}`,
	}
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtures}
	artifact, err := resolvePaperGeyser(context.Background(), Request{Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.21.8" || artifact.Metadata["geyser_build"] != "1212" || artifact.Metadata["floodgate_build"] != "140" {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestModrinthResolverAndPackApplication(t *testing.T) {
	packDigest := strings.Repeat("c", 128)
	versionURL := "https://api.modrinth.com/v2/project/demo-pack/version"
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtureTransport{versionURL: `[{"id":"version-id","project_id":"project-id","version_number":"1.2.3","date_published":"2026-08-01T12:34:56Z","files":[{"filename":"demo.mrpack","url":"https://cdn.modrinth.com/data/demo.mrpack","primary":true,"hashes":{"sha512":"` + packDigest + `"}}]}]`}}
	artifact, err := resolveModrinth(context.Background(), Request{ModpackMode: "project", ModpackProject: "demo-pack", Version: "latest", Build: "latest"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "1.2.3" || artifact.Build != "version-id" || artifact.Metadata["modrinth_project_id"] != "project-id" {
		t.Fatalf("artifact=%+v", artifact)
	}

	modBody := []byte("mod")
	modHash := sha512.Sum512(modBody)
	index := fmt.Sprintf(`{"formatVersion":1,"game":"minecraft","name":"Fixture Pack","versionId":"fixture","dependencies":{"minecraft":"1.21.8","fabric-loader":"0.16.0"},"files":[{"path":"mods/a.jar","hashes":{"sha512":"%x"},"downloads":["https://cdn.modrinth.com/data/a.jar"],"fileSize":3}]}`, modHash)
	pack := filepath.Join(t.TempDir(), "fixture.mrpack")
	file, err := os.Create(pack)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entry, _ := zw.Create("modrinth.index.json")
	_, _ = entry.Write([]byte(index))
	entry, _ = zw.Create("overrides/config/value.txt")
	_, _ = entry.Write([]byte("client"))
	entry, _ = zw.Create("server-overrides/config/value.txt")
	_, _ = entry.Write([]byte("server"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	packBytes, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	verifiedDigest := sha512.Sum512(packBytes)
	identityHome := t.TempDir()
	identityApp := &App{Config: Config{Home: identityHome, Control: filepath.Join(identityHome, ".pcvm")}, HTTP: NewHTTPClient()}
	identityApp.HTTP.Client = &http.Client{Transport: fixtureTransport{"https://cdn.modrinth.com/data/demo.mrpack": string(packBytes)}}
	spec := catalogSpec(t, "modrinth-modpack")
	bound, err := identityApp.prepareModrinthIdentity(context.Background(), spec, Request{RuntimeVersion: "auto"}, Resolved{
		Artifact:    Artifact{URL: "https://cdn.modrinth.com/data/demo.mrpack", FileName: "demo.mrpack", Kind: "mrpack", SHA512: fmt.Sprintf("%x", verifiedDigest), Version: "1.2.3", Build: "version-id", Metadata: map[string]string{"modrinth_project_id": "project-id"}},
		RuntimeKind: "java", RuntimeVersion: "25",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.PreparedArtifact == "" || bound.Artifact.Metadata["modrinth_loader"] != "fabric" || bound.RuntimeVersion != "21" {
		t.Fatalf("Modrinth identity was not bound before reconciliation: %+v", bound)
	}
	targetLock := lockArtifact(spec.ID, bound.Artifact)
	if targetLock.ID != "modrinth:project-id:fabric" {
		t.Fatalf("artifact lock=%q", targetLock.ID)
	}
	current := &State{Provider: spec.ID, Family: spec.Family, ResolvedVersion: bound.Artifact.Version, ResolvedBuild: bound.Artifact.Build, ArtifactLock: targetLock}
	if plan := Reconcile(current, spec, Request{}, &bound); plan.Kind != ActionUpdate {
		t.Fatalf("same project/loader identity unexpectedly requires reset: %+v", plan)
	}
	parsed, reader, err := readMRPack(pack)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	download := NewHTTPClient()
	download.Client = &http.Client{Transport: fixtureTransport{"https://cdn.modrinth.com/data/a.jar": string(modBody)}}
	staging := t.TempDir()
	if err := stageMRPackFiles(context.Background(), download, reader, parsed, staging); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(staging, "config", "value.txt")); err != nil || string(data) != "server" {
		t.Fatalf("server override precedence data=%q err=%v", data, err)
	}
	home := t.TempDir()
	managed, err := applyMRPackStage(staging, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedModrinthFiles(home, managed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mods", "a.jar"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := verifyManagedModrinthFiles(home, managed); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampered managed file was accepted: %v", err)
	}
}

func TestModrinthRejectsDeclaredSizeMismatchAndOverrideBomb(t *testing.T) {
	body := []byte("three")
	digest := sha512.Sum512(body)
	index := mrpackIndex{FormatVersion: 1, Game: "minecraft", Name: "Fixture Pack", VersionID: "fixture", Dependencies: map[string]string{"minecraft": "1.21.8"}}
	index.Files = append(index.Files, struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		Env       map[string]string `json:"env"`
		Downloads []string          `json:"downloads"`
		FileSize  int64             `json:"fileSize"`
	}{Path: "mods/a.jar", Hashes: map[string]string{"sha512": fmt.Sprintf("%x", digest)}, Downloads: []string{"https://cdn.modrinth.com/data/a.jar"}, FileSize: 4})
	h := NewHTTPClient()
	h.Client = &http.Client{Transport: fixtureTransport{"https://cdn.modrinth.com/data/a.jar": string(body)}}
	if err := stageMRPackFiles(context.Background(), h, &zip.ReadCloser{}, index, t.TempDir()); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("declared Modrinth size mismatch was accepted: %v", err)
	}

	pack := filepath.Join(t.TempDir(), "bomb.mrpack")
	file, err := os.Create(pack)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, _ := writer.Create("modrinth.index.json")
	_, _ = entry.Write([]byte(`{"formatVersion":1,"game":"minecraft","name":"Bomb Pack","versionId":"bomb","dependencies":{"minecraft":"1.21.8"},"files":[]}`))
	entry, _ = writer.Create("overrides/config/bomb.txt")
	_, _ = entry.Write(bytes.Repeat([]byte("A"), 1<<20))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, reader, err := readMRPack(pack)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := stageMRPackFiles(context.Background(), NewHTTPClient(), reader, parsed, t.TempDir()); err == nil || !strings.Contains(err.Error(), "compression ratio") {
		t.Fatalf("Modrinth override compression bomb was accepted: %v", err)
	}
}

func TestBunDependencyPolicyIsFrozenAndNotShellParsed(t *testing.T) {
	args, err := bunDependencyArgs("install --production --frozen-lockfile")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, "|") != "install|--production|--frozen-lockfile" {
		t.Fatalf("Bun dependency argv=%v", args)
	}
	if _, err := bunDependencyArgs("install --production; touch /tmp/pwned"); err == nil {
		t.Fatal("accepted arbitrary Bun dependency command")
	}
}

func TestGenericAppStarterFiles(t *testing.T) {
	for _, test := range []struct {
		provider, entry, contains string
	}{
		{"bun-app", "index.ts", "Bun.serve"},
		{"deno-app", "main.ts", "Deno.serve"},
		{"go-app", "main.go", "ListenAndServe"},
		{"dotnet-app", "PCVM.csproj", "net10.0"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			root := t.TempDir()
			created, err := generateGenericStarter(test.provider, root, test.entry)
			if err != nil || !created {
				t.Fatalf("created=%v err=%v", created, err)
			}
			data, err := os.ReadFile(filepath.Join(root, test.entry))
			if err != nil || !bytes.Contains(data, []byte(test.contains)) {
				t.Fatalf("starter=%q err=%v", data, err)
			}
			if test.provider == "dotnet-app" {
				if _, err := os.Stat(filepath.Join(root, "Program.cs")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTShockTarExtractionRejectsLinks(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	_ = writer.WriteHeader(&tar.Header{Name: "TShock.Server", Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
	_, _ = writer.Write([]byte("bin"))
	_ = writer.Close()
	archive := filepath.Join(t.TempDir(), "tshock.tar")
	if err := os.WriteFile(archive, body.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractTShockTar(archive, destination); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "TShock.Server")); err != nil || string(data) != "bin" {
		t.Fatalf("data=%q err=%v", data, err)
	}

	body.Reset()
	writer = tar.NewWriter(&body)
	_ = writer.WriteHeader(&tar.Header{Name: "escape", Linkname: "../outside", Typeflag: tar.TypeSymlink})
	_ = writer.Close()
	if err := os.WriteFile(archive, body.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := extractTShockTar(archive, t.TempDir()); err == nil {
		t.Fatal("TShock TAR symlink was accepted")
	}
}

func TestTShockInstallUsesPinnedDotnetRuntime(t *testing.T) {
	var tarBody bytes.Buffer
	tarWriter := tar.NewWriter(&tarBody)
	assembly := []byte("managed TShock assembly")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "TShock.Server.dll", Mode: 0o644, Size: int64(len(assembly)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(assembly); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	artifact := filepath.Join(t.TempDir(), "tshock.zip")
	file, err := os.Create(artifact)
	if err != nil {
		t.Fatal(err)
	}
	zipWriter := zip.NewWriter(file)
	payload, err := zipWriter.Create("TShock-release.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payload.Write(tarBody.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	runtime := filepath.Join(home, ".pcvm", "cache", "runtime", "dotnet")
	provider := NewProvider(catalogSpec(t, "tshock")).(*catalogProvider)
	resolved, err := provider.installTShock(InstallContext{
		Home: home, ControlDir: filepath.Join(home, ".pcvm"), Artifact: artifact, Runtime: runtime,
	}, Resolved{RuntimeKind: "dotnet", RuntimeVersion: "9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Command) != 2 || resolved.Command[0] != runtime || filepath.Base(resolved.Command[1]) != "TShock.Server.dll" {
		t.Fatalf("TShock command is not bound to the pinned runtime and assembly: %v", resolved.Command)
	}
}
