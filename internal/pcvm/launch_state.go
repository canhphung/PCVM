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

	launch := LaunchState{Provider: spec.ID, ResolvedVersion: state.ResolvedVersion, ResolvedBuild: state.ResolvedBuild, RuntimeVersion: state.RuntimeVersion}
	launch.ReadyPatterns = append([]string(nil), spec.ReadyPatterns...)
	if len(spec.Readiness.Patterns) > 0 {
		launch.ReadyPatterns = append([]string(nil), spec.Readiness.Patterns...)
	}
	if a.Config.Request.AppReady != "" && (spec.Installer == "node-app" || spec.Installer == "python-app") {
		launch.ReadyPatterns = []string{a.Config.Request.AppReady}
	}
	if _, err := compileReadyPatterns(launch.ReadyPatterns); err != nil {
		return empty, fmt.Errorf("catalog or APP_READY_PATTERN: %w", err)
	}
	launch.StopCommand = spec.StopCommand
	if spec.Control.StopCommand != "" {
		launch.StopCommand = spec.Control.StopCommand
	}

	managed := filepath.Join(a.Config.Control, "managed", spec.ID)
	version, build := state.ResolvedVersion, state.ResolvedBuild
	switch spec.Installer {
	case "jar":
		target := filepath.Join(managed, version+"-"+build+"-server.jar")
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{runtimePath, "-Xms128M", "-Xmx" + serverMemory(), "-jar", target}
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
	case "node-app", "python-app":
		command, workDir, err := a.rebuildAppCommand(spec, state, runtimePath)
		if err != nil {
			return empty, err
		}
		launch.Command, launch.WorkingDirectory = command, workDir
		if spec.ID == "python-bot" {
			launch.Environment = []string{"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1"}
		}
	case "web":
		launch.WorkingDirectory = a.Config.Home
		if spec.ID == "caddy" {
			launch.Command = []string{runtimePath}
		}
	case "steamcmd", "terraria", "factorio":
		launch.WorkingDirectory = filepath.Join(a.Config.Home, "game")
		launch.Command = []string{filepath.Join(launch.WorkingDirectory, filepath.FromSlash(spec.Options["executable"]))}
	case "tmodloader":
		launch.WorkingDirectory = filepath.Join(a.Config.Home, "game")
		dll, err := findFile(launch.WorkingDirectory, spec.Options["executable"])
		if err != nil {
			return empty, err
		}
		launch.Command = []string{runtimePath, dll}
	case "endstone":
		venv := filepath.Join(managed, "venv-"+version+"-py"+state.RuntimeVersion)
		launch.WorkingDirectory = a.Config.Home
		launch.Command = []string{filepath.Join(venv, "bin", "python3"), "-m", "endstone", "--server-folder", a.Config.Home, "--yes", "--remote", "https://raw.githubusercontent.com/EndstoneMC/bedrock-server-data/v2"}
		launch.Environment = []string{"PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1"}
	case "qemu-vm":
		launch.WorkingDirectory = a.Config.Home
	default:
		return empty, fmt.Errorf("unsupported installer %q", spec.Installer)
	}
	if spec.Installer != "web" && spec.Installer != "qemu-vm" && len(launch.Command) == 0 {
		return empty, fmt.Errorf("catalog produced no command")
	}
	return launch, nil
}

func (a *App) rebuildAppCommand(spec ProviderSpec, state State, runtimePath string) ([]string, string, error) {
	source := a.Config.Home
	if a.Config.Request.SourceMode == "git" {
		source = filepath.Join(a.Config.Home, "app")
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
		runtimePath = filepath.Join(a.Config.Control, "managed", spec.ID, "venv-"+state.RuntimeVersion, "bin", "python3")
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
