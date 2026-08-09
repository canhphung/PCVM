package pcvm

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type configSetting struct {
	Key   string
	Value string
}

func (a *App) syncPrimaryAllocation(state LaunchState) (bool, error) {
	port := a.Config.AllocationPort
	if port == 0 {
		return false, nil
	}
	value := strconv.Itoa(port)
	var changed bool
	var err error
	switch state.Provider {
	case "vanilla", "paper", "purpur", "pufferfish", "fabric", "forge", "neoforge":
		changed, err = patchProperties(a.Config.Home, "server.properties", []configSetting{
			{Key: "server-ip", Value: "0.0.0.0"},
			{Key: "server-port", Value: value},
			{Key: "query.port", Value: value},
		})
	case "velocity":
		if err = ensureVelocityConfig(a.Config.Home, state.Command); err == nil {
			changed, err = patchRootTOMLScalar(a.Config.Home, "velocity.toml", "bind", strconv.Quote("0.0.0.0:"+value))
		}
	case "bungeecord":
		if err = ensureBungeeConfig(a.Config.Home, value); err == nil {
			changed, err = patchBungeeListener(a.Config.Home, value)
		}
	case "bedrock":
		changed, err = patchProperties(a.Config.Home, "server.properties", []configSetting{{Key: "server-port", Value: value}})
	case "endstone":
		changed, err = patchProperties(a.Config.Home, "server.properties", []configSetting{{Key: "server-port", Value: value}, {Key: "server-portv6", Value: value}})
	case "pocketmine", "powernukkitx", "cloudburst-nukkit":
		changed, err = patchProperties(a.Config.Home, "server.properties", []configSetting{
			{Key: "server-ip", Value: "0.0.0.0"},
			{Key: "server-port", Value: value},
			{Key: "query.port", Value: value},
		})
	case "lavalink":
		changed, err = patchLavalinkPort(a.Config.Home, value)
	}
	if err != nil {
		return false, fmt.Errorf("configure primary allocation for %s: %w", state.Provider, err)
	}
	return changed, nil
}

func allocationEnvironment(provider string, current []string, port int) []string {
	out := append([]string(nil), current...)
	if port == 0 || provider != "node-bot" && provider != "python-bot" {
		return out
	}
	out = upsertEnvironment(out, "SERVER_PORT", strconv.Itoa(port))
	out = upsertEnvironment(out, "PORT", strconv.Itoa(port))
	out = upsertEnvironment(out, "HOST", "0.0.0.0")
	return out
}

// processUserEnvironment keeps software data inside the mounted Pterodactyl
// server directory. Wings installations can start the container with HOME=/,
// which makes Mono-based servers such as Terraria try to write to /.local.
func processUserEnvironment(provider, home string, current []string) ([]string, error) {
	dataHome := filepath.Join(home, ".local", "share")
	configHome := filepath.Join(home, ".config")
	cacheHome := filepath.Join(home, ".cache")
	directories := []string{dataHome, configHome, cacheHome}
	if provider == "terraria" || provider == "tmodloader" {
		directories = append(directories, filepath.Join(dataHome, "Terraria"))
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create process user directory %s: %w", directory, err)
		}
	}

	out := append([]string(nil), current...)
	out = upsertEnvironment(out, "HOME", home)
	out = upsertEnvironment(out, "XDG_DATA_HOME", dataHome)
	out = upsertEnvironment(out, "XDG_CONFIG_HOME", configHome)
	out = upsertEnvironment(out, "XDG_CACHE_HOME", cacheHome)
	return out, nil
}

func upsertEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	for i, item := range environment {
		if strings.HasPrefix(item, prefix) {
			environment[i] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

func patchProperties(home, name string, settings []configSetting) (bool, error) {
	data, err := readManagedConfig(home, name)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	lines := splitConfigLines(data)
	found := make(map[string]bool, len(settings))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		for _, setting := range settings {
			if propertyKey(trimmed) == setting.Key {
				lines[i] = setting.Key + "=" + setting.Value
				found[setting.Key] = true
				break
			}
		}
	}
	for _, setting := range settings {
		if !found[setting.Key] {
			lines = append(lines, setting.Key+"="+setting.Value)
		}
	}
	return writeManagedConfig(home, name, joinConfigLines(lines))
}

func propertyKey(line string) string {
	if index := strings.IndexAny(line, "=:"); index >= 0 {
		return strings.TrimSpace(line[:index])
	}
	if fields := strings.Fields(line); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func patchRootTOMLScalar(home, name, key, value string) (bool, error) {
	data, err := readManagedConfig(home, name)
	if err != nil {
		return false, err
	}
	lines := splitConfigLines(data)
	found := false
	firstSection := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && firstSection == len(lines) {
			firstSection = i
		}
		if len(line) != len(strings.TrimLeft(line, " \t")) || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if index := strings.Index(trimmed, "="); index >= 0 && strings.TrimSpace(trimmed[:index]) == key {
			lines[i] = key + " = " + value
			found = true
		}
	}
	if !found {
		lines = append(lines, "")
		copy(lines[firstSection+1:], lines[firstSection:])
		lines[firstSection] = key + " = " + value
	}
	return writeManagedConfig(home, name, joinConfigLines(lines))
}

func ensureVelocityConfig(home string, command []string) error {
	if _, err := readManagedConfig(home, "velocity.toml"); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	jar := jarFromCommand(command)
	if jar == "" {
		return fmt.Errorf("velocity.toml is missing and the server jar could not be located")
	}
	return extractJarConfig(home, jar, "default-velocity.toml", "velocity.toml")
}

func ensureBungeeConfig(home, port string) error {
	if _, err := readManagedConfig(home, "config.yml"); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	body := "listeners:\n- host: 0.0.0.0:" + port + "\n  query_port: " + port + "\n"
	_, err := writeManagedConfig(home, "config.yml", []byte(body))
	return err
}

func patchBungeeListener(home, port string) (bool, error) {
	data, err := readManagedConfig(home, "config.yml")
	if err != nil {
		return false, err
	}
	lines := splitConfigLines(data)
	listeners := -1
	for i, line := range lines {
		if len(line) == len(strings.TrimLeft(line, " \t")) && strings.TrimSpace(line) == "listeners:" {
			listeners = i
			break
		}
	}
	if listeners < 0 {
		return false, fmt.Errorf("config.yml has no top-level listeners list")
	}
	start, end := -1, len(lines)
	listIndent := 0
	for i := listeners + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		if start < 0 && strings.HasPrefix(trimmed, "-") {
			start, listIndent = i, indent
			continue
		}
		if start >= 0 && indent <= listIndent {
			end = i
			break
		}
		if start < 0 && indent == 0 {
			break
		}
	}
	if start < 0 {
		return false, fmt.Errorf("config.yml listeners list is empty")
	}
	foundHost, foundQuery := false, false
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		withoutDash := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		prefix := strings.Repeat(" ", len(lines[i])-len(strings.TrimLeft(lines[i], " ")))
		dash := ""
		if strings.HasPrefix(trimmed, "-") {
			dash = "- "
		}
		switch yamlKey(withoutDash) {
		case "host":
			lines[i] = prefix + dash + "host: 0.0.0.0:" + port
			foundHost = true
		case "query_port":
			lines[i] = prefix + dash + "query_port: " + port
			foundQuery = true
		}
	}
	insert := []string{}
	indent := strings.Repeat(" ", listIndent+2)
	if !foundHost {
		insert = append(insert, indent+"host: 0.0.0.0:"+port)
	}
	if !foundQuery {
		insert = append(insert, indent+"query_port: "+port)
	}
	if len(insert) > 0 {
		lines = append(lines, make([]string, len(insert))...)
		copy(lines[end+len(insert):], lines[end:])
		copy(lines[end:], insert)
	}
	return writeManagedConfig(home, "config.yml", joinConfigLines(lines))
}

func yamlKey(line string) string {
	if index := strings.Index(line, ":"); index >= 0 {
		return strings.TrimSpace(line[:index])
	}
	return ""
}

func patchLavalinkPort(home, port string) (bool, error) {
	data, err := readManagedConfig(home, "application.yml")
	if os.IsNotExist(err) {
		body := "server:\n  port: " + port + "\nlavalink:\n  server:\n    password: youshallnotpass\n"
		return writeManagedConfig(home, "application.yml", []byte(body))
	}
	if err != nil {
		return false, err
	}
	lines := splitConfigLines(data)
	server := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(line) == len(strings.TrimLeft(line, " \t")) && strings.HasPrefix(trimmed, "server:") {
			if trimmed != "server:" {
				return false, fmt.Errorf("top-level server configuration must use block YAML")
			}
			server = i
			break
		}
	}
	if server < 0 {
		lines = append([]string{"server:", "  port: " + port}, lines...)
		return writeManagedConfig(home, "application.yml", joinConfigLines(lines))
	}
	for i := server + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		if indent == 0 {
			break
		}
		if yamlKey(trimmed) == "port" {
			lines[i] = strings.Repeat(" ", indent) + "port: " + port
			return writeManagedConfig(home, "application.yml", joinConfigLines(lines))
		}
	}
	lines = append(lines, "")
	copy(lines[server+2:], lines[server+1:])
	lines[server+1] = "  port: " + port
	return writeManagedConfig(home, "application.yml", joinConfigLines(lines))
}

func jarFromCommand(command []string) string {
	for i, arg := range command {
		if arg == "-jar" && i+1 < len(command) {
			return command[i+1]
		}
	}
	return ""
}

func extractJarConfig(home, jar, entry, destination string) error {
	reader, err := zip.OpenReader(jar)
	if err != nil {
		return fmt.Errorf("open server jar: %w", err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != entry {
			continue
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(in, 2<<20))
		in.Close()
		if err != nil {
			return err
		}
		_, err = writeManagedConfig(home, destination, data)
		return err
	}
	return fmt.Errorf("server jar contains no %s", entry)
}

func readManagedConfig(home, name string) ([]byte, error) {
	path, err := managedConfigPath(home, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func writeManagedConfig(home, name string, data []byte) (bool, error) {
	path, err := managedConfigPath(home, name)
	if err != nil {
		return false, err
	}
	if current, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(current, data) {
		return false, nil
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
	}
	mode := os.FileMode(0o640)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".allocation-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

func managedConfigPath(home, name string) (string, error) {
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", fmt.Errorf("invalid managed config name %q", name)
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(homeAbs, 0o750); err != nil {
		return "", err
	}
	homeReal, err := filepath.EvalSymlinks(homeAbs)
	if err != nil {
		return "", err
	}
	path := filepath.Join(homeReal, name)
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("managed config %s may not be a symlink", name)
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	return path, nil
}

func splitConfigLines(data []byte) []string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func joinConfigLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}
