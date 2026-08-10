package pcvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func isSourceProvider(spec ProviderSpec) bool {
	return spec.Resolver == "local-app"
}

func (p *catalogProvider) installGenericApp(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	source, reused, err := p.materializeGenericSource(ctx, ic, resolved)
	if err != nil {
		return resolved, err
	}
	completed := false
	defer func() {
		if !completed && !reused && ic.Request.SourceMode == "git" {
			_ = os.RemoveAll(source)
		}
	}()
	if ic.Request.SourceMode == "git" {
		resolved.RollbackMode = "staged"
	}
	entry := strings.TrimSpace(ic.Request.EntryFile)
	if entry == "" {
		entry = p.spec.Options.DefaultEntry
	}
	entry, err = cleanRelativeEntry(entry)
	if err != nil {
		return resolved, err
	}
	entryPath := filepath.Join(source, entry)
	info, statErr := os.Stat(entryPath)
	if os.IsNotExist(statErr) && ic.Request.SourceMode != "git" {
		generated, generateErr := generateGenericStarter(p.spec.ID, source, entry)
		if generateErr != nil {
			return resolved, fmt.Errorf("generate starter entry: %w", generateErr)
		}
		if generated && ic.Log != nil {
			ic.Log.Printf("generated Hello World starter %s for %s", entry, p.spec.Name)
		}
		info, statErr = os.Stat(entryPath)
	}
	if statErr != nil {
		return resolved, fmt.Errorf("entry file: %w", statErr)
	}
	if info.IsDir() {
		return resolved, fmt.Errorf("entry file is a directory")
	}
	args, err := SplitArgs(ic.Request.AppArgs)
	if err != nil {
		return resolved, err
	}
	environment, err := processUserEnvironment(p.spec.ID, ic.Home, os.Environ())
	if err != nil {
		return resolved, err
	}
	environment = upsertEnvironment(environment, "PATH", filepath.Dir(ic.Runtime)+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved.WorkDir = source
	switch p.spec.ID {
	case "bun-app":
		if _, err := os.Stat(filepath.Join(source, "package.json")); err == nil && !reused {
			dependencyArgs, err := bunDependencyArgs(p.spec.Options.DependencyPolicy)
			if err != nil {
				return resolved, err
			}
			cmd := exec.CommandContext(ctx, ic.Runtime, dependencyArgs...)
			cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = source, environment, ic.Out, ic.Err
			if err := cmd.Run(); err != nil {
				return resolved, fmt.Errorf("install Bun dependencies: %w", err)
			}
		}
		resolved.Command = append([]string{ic.Runtime, "run", entry}, args...)
	case "deno-app":
		resolved.Command = append([]string{ic.Runtime, "run", "--allow-net", "--allow-read=.", "--allow-env", entry}, args...)
	case "go-app":
		binaryRoot := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
		binary := filepath.Join(binaryRoot, "bin", "app")
		if ic.Request.SourceMode == "git" {
			binaryRoot = source
			binary = filepath.Join(source, ".pcvm-bin", "app")
		}
		if err := secureMkdirAll(binaryRoot, filepath.Dir(binary), 0o750); err != nil {
			return resolved, err
		}
		buildTarget := entry
		if _, err := os.Stat(filepath.Join(source, "go.mod")); err == nil && ic.Request.EntryFile == "" {
			buildTarget = "."
		}
		if reused {
			if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
				return resolved, fmt.Errorf("reused Go release has no compiled application")
			}
			resolved.Command = append([]string{binary}, args...)
			break
		}
		cacheRoot := filepath.Join(ic.ControlDir, "cache")
		if err := secureMkdirAll(ic.ControlDir, cacheRoot, 0o750); err != nil {
			return resolved, err
		}
		cache, err := os.MkdirTemp(cacheRoot, ".go-cache-")
		if err != nil {
			return resolved, fmt.Errorf("create temporary Go cache: %w", err)
		}
		defer os.RemoveAll(cache)
		cmd := exec.CommandContext(ctx, ic.Runtime, "build", "-trimpath", "-buildvcs=false", "-o", binary, buildTarget)
		cmd.Dir, cmd.Stdout, cmd.Stderr = source, ic.Out, ic.Err
		cmd.Env = upsertEnvironment(environment, "GOCACHE", cache)
		if err := cmd.Run(); err != nil {
			return resolved, fmt.Errorf("build Go application: %w", err)
		}
		resolved.Command = append([]string{binary}, args...)
	case "dotnet-app":
		if strings.EqualFold(filepath.Ext(entry), ".dll") {
			resolved.Command = append([]string{ic.Runtime, entry}, args...)
			break
		}
		if !strings.EqualFold(filepath.Ext(entry), ".csproj") {
			return resolved, fmt.Errorf("ENTRY_FILE for dotnet-app must be a published .dll or .csproj")
		}
		if ic.Request.SourceMode == "git" {
			publish := filepath.Join(source, ".pcvm-publish")
			if err := secureMkdirAll(source, publish, 0o750); err != nil {
				return resolved, err
			}
			if !reused {
				cmd := exec.CommandContext(ctx, ic.Runtime, "publish", entry, "--configuration", "Release", "--output", publish, "--nologo")
				cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = source, environment, ic.Out, ic.Err
				if err := cmd.Run(); err != nil {
					return resolved, fmt.Errorf("publish .NET application: %w", err)
				}
			}
			dll, err := findDotnetLaunchDLL(publish)
			if err != nil {
				return resolved, err
			}
			resolved.Command = append([]string{ic.Runtime, dll}, args...)
			break
		}
		publishRoot := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
		if err := secureMkdirAll(ic.ControlDir, publishRoot, 0o750); err != nil {
			return resolved, err
		}
		if err := recoverDotnetPublishActivation(publishRoot); err != nil {
			return resolved, fmt.Errorf("recover .NET publish activation: %w", err)
		}
		candidate, err := os.MkdirTemp(publishRoot, ".publish-candidate-")
		if err != nil {
			return resolved, fmt.Errorf("create .NET publish candidate: %w", err)
		}
		candidateLive := true
		defer func() {
			if candidateLive {
				_ = os.RemoveAll(candidate)
			}
		}()
		cmd := exec.CommandContext(ctx, ic.Runtime, "publish", entry, "--configuration", "Release", "--output", candidate, "--nologo")
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = source, environment, ic.Out, ic.Err
		if err := cmd.Run(); err != nil {
			return resolved, fmt.Errorf("publish .NET application: %w", err)
		}
		candidateDLL, err := findDotnetLaunchDLL(candidate)
		if err != nil {
			return resolved, err
		}
		if err := activateDotnetPublishCandidate(publishRoot, candidate); err != nil {
			return resolved, fmt.Errorf("activate .NET publish candidate: %w", err)
		}
		candidateLive = false
		publishDLL := filepath.Join(publishRoot, "publish", filepath.Base(candidateDLL))
		resolved.Command = append([]string{ic.Runtime, publishDLL}, args...)
	default:
		return resolved, fmt.Errorf("unsupported generic application provider %q", p.spec.ID)
	}
	completed = true
	return resolved, nil
}

func bunDependencyArgs(policy string) ([]string, error) {
	switch strings.TrimSpace(policy) {
	case "", "install --production":
		return []string{"install", "--production"}, nil
	case "install --production --frozen-lockfile":
		return []string{"install", "--production", "--frozen-lockfile"}, nil
	default:
		return nil, fmt.Errorf("unsupported compiled Bun dependency policy %q", policy)
	}
}

func (p *catalogProvider) materializeGenericSource(ctx context.Context, ic InstallContext, resolved Resolved) (string, bool, error) {
	if ic.Request.SourceMode != "git" {
		return ic.Home, false, nil
	}
	if ic.PreparedSource == "" {
		return "", false, fmt.Errorf("immutable prepared Git source is required")
	}
	managed := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managed, 0o750); err != nil {
		return "", false, err
	}
	want, err := gitSourceHead(ctx, ic.PreparedSource)
	if err != nil {
		return "", false, fmt.Errorf("inspect prepared Git source: %w", err)
	}
	lock := lockArtifact(p.spec.ID, resolved.Artifact)
	releaseID := releaseIDFor(p.spec.ID, lock)
	final := releaseRoot(ic.ControlDir, p.spec.ID, releaseID)
	if err := validateRelease(final, p.spec.ID, releaseID, lock); err == nil {
		return filepath.Join(final, "payload"), true, nil
	} else if !os.IsNotExist(err) {
		if _, statErr := os.Lstat(final); statErr == nil {
			return "", false, fmt.Errorf("existing immutable Git release is invalid: %w", err)
		}
	}
	staging, err := os.MkdirTemp(managed, ".source-")
	if err != nil {
		return "", false, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(staging)
		}
	}()
	cmd := exec.CommandContext(ctx, "git", "clone", "--no-local", "--", ic.PreparedSource, staging)
	cmd.Env, cmd.Stdout, cmd.Stderr = sanitizedGitEnvironment(), ic.Out, ic.Err
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("materialize immutable Git source: %w", err)
	}
	got, err := gitSourceHead(ctx, staging)
	if err != nil || got != want {
		return "", false, fmt.Errorf("materialized Git source does not match resolved commit")
	}
	keep = true
	return staging, false, nil
}

func cleanupFailedGenericRelease(control string, spec ProviderSpec, resolved Resolved) {
	if resolved.WorkDir == "" {
		return
	}
	managed := filepath.Join(control, "managed", spec.ID)
	root := filepath.Clean(resolved.WorkDir)
	base := filepath.Base(root)
	if pathWithin(managed, root) && (strings.HasPrefix(base, ".source-") || strings.HasPrefix(base, ".candidate-")) {
		_ = os.RemoveAll(root)
	}
}

func gitSourceHead(ctx context.Context, source string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", source, "rev-parse", "HEAD")
	cmd.Env = sanitizedGitEnvironment()
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	head := strings.ToLower(strings.TrimSpace(string(output)))
	if !validGitCommit(head) {
		return "", fmt.Errorf("invalid Git commit identity")
	}
	return head, nil
}

func generateGenericStarter(providerID, source, entry string) (bool, error) {
	path := filepath.Join(source, entry)
	var content string
	switch providerID {
	case "bun-app":
		content = `const port = Number(Bun.env.SERVER_PORT ?? "3000");
Bun.serve({ hostname: "0.0.0.0", port, fetch: () => new Response("Hello World from PCVM!\n") });
console.log(` + "`" + `Hello World from PCVM! Listening on ${port}` + "`" + `);
`
	case "deno-app":
		content = `const port = Number(Deno.env.get("SERVER_PORT") ?? "3000");
Deno.serve({ hostname: "0.0.0.0", port }, () => new Response("Hello World from PCVM!\n"));
console.log(` + "`" + `Hello World from PCVM! Listening on ${port}` + "`" + `);
`
	case "go-app":
		content = `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("SERVER_PORT")
	if port == "" { port = "3000" }
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "Hello World from PCVM!") })
	fmt.Printf("Hello World from PCVM! Listening on %s\n", port)
	if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil { panic(err) }
}
`
	case "dotnet-app":
		content = `<Project Sdk="Microsoft.NET.Sdk.Web">
  <PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net10.0</TargetFramework><ImplicitUsings>enable</ImplicitUsings></PropertyGroup>
</Project>
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
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	if providerID == "dotnet-app" {
		program := filepath.Join(source, "Program.cs")
		programFile, err := os.OpenFile(program, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err != nil {
			_ = os.Remove(path)
			return false, err
		}
		programBody := `var port = Environment.GetEnvironmentVariable("SERVER_PORT") ?? "3000";
var builder = WebApplication.CreateBuilder(args);
builder.WebHost.UseUrls("http://0.0.0.0:" + port);
var app = builder.Build();
app.MapGet("/", () => "Hello World from PCVM!\n");
Console.WriteLine("Hello World from PCVM! Listening on " + port);
app.Run();
`
		if _, err := programFile.WriteString(programBody); err != nil {
			programFile.Close()
			_ = os.Remove(path)
			_ = os.Remove(program)
			return false, err
		}
		if err := programFile.Close(); err != nil {
			_ = os.Remove(path)
			_ = os.Remove(program)
			return false, err
		}
	}
	return true, nil
}

func findDotnetLaunchDLL(root string) (string, error) {
	runtimeConfigs, err := filepath.Glob(filepath.Join(root, "*.runtimeconfig.json"))
	if err != nil {
		return "", err
	}
	sort.Strings(runtimeConfigs)
	for _, config := range runtimeConfigs {
		base := strings.TrimSuffix(filepath.Base(config), ".runtimeconfig.json")
		dll := filepath.Join(root, base+".dll")
		if info, err := os.Stat(dll); err == nil && info.Mode().IsRegular() {
			return dll, nil
		}
	}
	return "", fmt.Errorf(".NET publish output has no launchable application DLL")
}

func recoverDotnetPublishActivation(root string) error {
	current := filepath.Join(root, "publish")
	previous := filepath.Join(root, "publish.previous")
	currentExists, err := validateDotnetPublishDirectory(current)
	if err != nil {
		return err
	}
	previousExists, err := validateDotnetPublishDirectory(previous)
	if err != nil {
		return err
	}
	if !previousExists {
		return nil
	}
	if currentExists {
		return os.RemoveAll(previous)
	}
	return os.Rename(previous, current)
}

func activateDotnetPublishCandidate(root, candidate string) error {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if filepath.Dir(candidate) != root || !strings.HasPrefix(filepath.Base(candidate), ".publish-candidate-") {
		return fmt.Errorf(".NET publish candidate is outside its managed root")
	}
	exists, err := validateDotnetPublishDirectory(candidate)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf(".NET publish candidate is missing")
	}
	if err := recoverDotnetPublishActivation(root); err != nil {
		return err
	}
	current := filepath.Join(root, "publish")
	previous := filepath.Join(root, "publish.previous")
	currentExists, err := validateDotnetPublishDirectory(current)
	if err != nil {
		return err
	}
	if currentExists {
		if err := os.Rename(current, previous); err != nil {
			return fmt.Errorf("preserve current .NET publish: %w", err)
		}
	}
	if err := os.Rename(candidate, current); err != nil {
		if currentExists {
			_ = os.Rename(previous, current)
		}
		return fmt.Errorf("activate candidate directory: %w", err)
	}
	if currentExists {
		if err := os.RemoveAll(previous); err != nil {
			return fmt.Errorf("remove previous .NET publish: %w", err)
		}
	}
	return nil
}

func validateDotnetPublishDirectory(directory string) (bool, error) {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf(".NET publish path %q is not a real directory", directory)
	}
	return true, nil
}

func rebuildGenericAppCommand(cfg Config, spec ProviderSpec, state State, runtimePath string) ([]string, string, error) {
	source := cfg.Home
	if cfg.Request.SourceMode == "git" {
		// The release ID is derived from canonical state, never from startup
		// input. rewriteStagedLaunchToRelease validates this payload before exec.
		source = releasePayloadRoot(cfg.Control, spec.ID, releaseIDFor(spec.ID, state.ArtifactLock))
	}
	entry := strings.TrimSpace(cfg.Request.EntryFile)
	if entry == "" {
		entry = spec.Options.DefaultEntry
	}
	entry, err := cleanRelativeEntry(entry)
	if err != nil {
		return nil, "", err
	}
	args, err := SplitArgs(cfg.Request.AppArgs)
	if err != nil {
		return nil, "", err
	}
	switch spec.ID {
	case "bun-app":
		return append([]string{runtimePath, "run", entry}, args...), source, nil
	case "deno-app":
		return append([]string{runtimePath, "run", "--allow-net", "--allow-read=.", "--allow-env", entry}, args...), source, nil
	case "go-app":
		binary := filepath.Join(cfg.Control, "managed", spec.ID, "bin", "app")
		if cfg.Request.SourceMode == "git" {
			binary = filepath.Join(source, ".pcvm-bin", "app")
		}
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("compiled Go application is missing")
		}
		return append([]string{binary}, args...), source, nil
	case "dotnet-app":
		if strings.EqualFold(filepath.Ext(entry), ".dll") {
			if info, err := os.Stat(filepath.Join(source, entry)); err != nil || !info.Mode().IsRegular() {
				return nil, "", fmt.Errorf("published .NET entry is missing")
			}
			return append([]string{runtimePath, entry}, args...), source, nil
		}
		publish := filepath.Join(cfg.Control, "managed", spec.ID, "publish")
		if cfg.Request.SourceMode == "git" {
			publish = filepath.Join(source, ".pcvm-publish")
		}
		dll, err := findDotnetLaunchDLL(publish)
		if err != nil {
			return nil, "", err
		}
		return append([]string{runtimePath, dll}, args...), source, nil
	default:
		return nil, "", fmt.Errorf("unsupported generic application provider %q", spec.ID)
	}
}
