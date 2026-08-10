package pcvm

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// rebuildLaunchState treats state.json as an untrusted installation index. All
// fields that can influence exec, environment, readiness, or shutdown are
// derived from the embedded catalog and PCVM's fixed installation layout.
func (a *App) rebuildLaunchState(ctx context.Context, spec ProviderSpec, state State) (LaunchState, error) {
	empty := LaunchState{}
	if err := validateStatePathToken("resolved version", state.ResolvedVersion); err != nil {
		return empty, err
	}
	if err := validateStatePathToken("resolved build", state.ResolvedBuild); err != nil {
		return empty, err
	}

	runtimePath := ""
	if spec.Runtime != "native" && spec.Runtime != "steamcmd" {
		if err := validateStatePathToken("runtime version", state.RuntimeVersion); err != nil {
			return empty, err
		}
		manager := RuntimeManager{Catalog: a.Catalog, Config: a.Config, HTTP: a.HTTP, Log: a.Log}
		var err error
		runtimePath, err = manager.Ensure(ctx, spec.Runtime, state.RuntimeVersion)
		if err != nil {
			return empty, err
		}
	}

	launch := LaunchState{Provider: spec.ID, ResolvedVersion: state.ResolvedVersion, ResolvedBuild: state.ResolvedBuild, RuntimeVersion: state.RuntimeVersion,
		Readiness: spec.Readiness, Control: spec.Control}
	launch.Readiness.Patterns = append([]string(nil), spec.Readiness.Patterns...)
	if a.Config.Request.AppReady != "" && (spec.Installer == "node-app" || spec.Installer == "python-app" || spec.Installer == "generic-app") {
		launch.Readiness.Mode = "regex"
		launch.Readiness.Patterns = []string{a.Config.Request.AppReady}
	}
	if _, err := compileReadyPatterns(launch.Readiness.Patterns); err != nil {
		return empty, fmt.Errorf("catalog or APP_READY_PATTERN: %w", err)
	}

	managed := filepath.Join(a.Config.Control, "managed", spec.ID)
	version, build := state.ResolvedVersion, state.ResolvedBuild
	switch spec.Installer {
	case "jar":
		target := filepath.Join(managed, version+"-"+build+"-server.jar")
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{runtimePath, "-jar", target}
		if strings.HasPrefix(spec.Family, "minecraft-java-") {
			launch.Command = append(launch.Command, "nogui")
		}
	case "phar":
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{runtimePath, filepath.Join(managed, version+"-PocketMine-MP.phar"), "--no-wizard"}
		launch.Environment = []string{"PHPRC="}
	case "zip":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, "bedrock_server")}
		launch.Environment = []string{"LD_LIBRARY_PATH=."}
	case "java-installer":
		launch.WorkingDirectory = filepath.Join(managed, version)
		argsFile := filepath.Join("@libraries", "net", "minecraftforge", "forge", version, "unix_args.txt")
		if spec.ID == "neoforge" {
			argsFile = filepath.Join("@libraries", "net", "neoforged", "neoforge", version, "unix_args.txt")
		}
		launch.Command = []string{runtimePath, "@user_jvm_args.txt", argsFile, "nogui"}
	case "quilt":
		launch.WorkingDirectory = filepath.Join(managed, version+"-"+build)
		launch.Command = []string{runtimePath, "-jar", filepath.Join(launch.WorkingDirectory, "quilt-server-launch.jar"), "nogui"}
	case "paper-geyser":
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{runtimePath, "-jar", filepath.Join(managed, version+"-"+build+"-server.jar"), "nogui"}
	case "modrinth":
		command, workDir, err := rebuildModrinthCommand(a.Config, runtimePath)
		if err != nil {
			return empty, err
		}
		launch.Command, launch.WorkingDirectory = command, workDir
	case "node-app", "python-app":
		command, workDir, err := a.rebuildAppCommand(spec, state, runtimePath)
		if err != nil {
			return empty, err
		}
		launch.Command, launch.WorkingDirectory = command, workDir
		if spec.ID == "python-bot" {
			launch.Environment = []string{"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1"}
			if a.Config.Request.SourceMode == "git" {
				launch.Environment = append(launch.Environment, "PYTHONPATH="+filepath.Join(workDir, ".pcvm-site-packages"))
			}
		}
	case "generic-app":
		command, workDir, err := rebuildGenericAppCommand(a.Config, spec, state, runtimePath)
		if err != nil {
			return empty, err
		}
		launch.Command, launch.WorkingDirectory = command, workDir
	case "web":
		launch.WorkingDirectory = a.Config.Home
		if spec.ID == "caddy" {
			launch.Command = []string{runtimePath}
		}
	case "steamcmd":
		launch.WorkingDirectory = filepath.Join(a.Config.Home, "game")
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, filepath.FromSlash(spec.Options.Executable))}
	case "terraria", "factorio":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, filepath.FromSlash(spec.Options.Executable))}
	case "tmodloader":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{runtimePath, filepath.Join(launch.WorkingDirectory, filepath.FromSlash(spec.Options.Executable))}
	case "tshock":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{runtimePath, filepath.Join(launch.WorkingDirectory, filepath.FromSlash(spec.Options.Executable))}
	case "endstone":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{runtimePath, "-m", "endstone", "--server-folder", a.Config.Home, "--yes", "--remote", "https://raw.githubusercontent.com/EndstoneMC/bedrock-server-data/v2"}
		launch.Environment = []string{"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1", "PYTHONPATH=" + filepath.Join(launch.WorkingDirectory, "site-packages")}
	case "openmp":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, "omp-server")}
	case "mtasa":
		launch.WorkingDirectory = filepath.Join(managed, version)
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, "mta-server64")}
	case "code-server":
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{filepath.Join(managed, version, "bin", "code-server")}
	case "qemu-vm":
		image, ok := findVMImageForArtifact(spec, state.Artifact, a.Config.Arch)
		if !ok {
			return empty, fmt.Errorf("VM state does not reference an image pinned by the embedded catalog")
		}
		compression, err := validateVMCompression(a.Config.Request.VMDiskCompression)
		if err != nil {
			return empty, fmt.Errorf("VM disk compression is invalid: %w", err)
		}
		launch.WorkingDirectory = a.Config.Home
		launch.VMImageID = image.ID
		launch.VMImageVariant = image.Variant
		launch.VMImageChecksum = pinnedVMImageChecksum(image)
		launch.VMDiskCompression = compression
	default:
		return empty, fmt.Errorf("unsupported installer %q", spec.Installer)
	}
	if state.Schema == StateSchema {
		if err := rewriteStagedLaunchToRelease(a.Config.Control, spec, state, &launch); err != nil {
			return empty, err
		}
	}
	if spec.Installer != "web" && spec.Installer != "qemu-vm" && len(launch.Command) == 0 {
		return empty, fmt.Errorf("catalog produced no command")
	}
	return launch, nil
}

func (a *App) rebuildAppCommand(spec ProviderSpec, state State, runtimePath string) ([]string, string, error) {
	source := a.Config.Home
	if a.Config.Request.SourceMode == "git" {
		source = releasePayloadRoot(a.Config.Control, spec.ID, releaseIDFor(spec.ID, state.ArtifactLock))
	}
	entry := a.Config.Request.EntryFile
	if entry == "" {
		if spec.ID == "node-bot" {
			entry = "index.js"
		} else {
			entry = "main.py"
		}
	}
	entry, err := cleanRelativeEntry(entry)
	if err != nil {
		return nil, "", err
	}
	entryPath := filepath.Join(source, entry)
	info, err := os.Stat(entryPath)
	if err != nil {
		return nil, "", fmt.Errorf("entry file: %w", err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("entry file is a directory")
	}
	args, err := SplitArgs(a.Config.Request.AppArgs)
	if err != nil {
		return nil, "", err
	}
	if spec.ID == "python-bot" {
		if a.Config.Request.SourceMode == "git" {
			// Git dependencies are installed with pip --target so the immutable
			// release remains relocatable across atomic activation.
		} else {
			runtimePath = filepath.Join(a.Config.Control, "managed", spec.ID, "venv-"+state.RuntimeVersion, "bin", "python3")
		}
	}
	return append([]string{runtimePath, entry}, args...), source, nil
}

func cleanRelativeEntry(entry string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(entry, "\\", "/"))
	local := filepath.FromSlash(clean)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(local) {
		return "", fmt.Errorf("ENTRY_FILE must stay inside source")
	}
	return local, nil
}

func validateStatePathToken(name, value string) error {
	if value == "" || value == "." || value == ".." || len(value) > 128 || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("state %s is invalid", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("state %s is invalid", name)
		}
	}
	return nil
}
