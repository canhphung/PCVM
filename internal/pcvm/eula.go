package pcvm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const minecraftEULATrigger = "You need to agree to the EULA in order to run the server. Go to eula.txt for more info."

// ensureMinecraftEULA implements the Pterodactyl EULA feature contract. The
// Panel writes eula=true to /home/container/eula.txt and restarts an offline
// server after the user presses I Accept. ACCEPT_MINECRAFT_EULA remains a
// backwards-compatible non-UI override and materializes the same file.
func ensureMinecraftEULA(home string, legacyAccepted bool) (bool, error) {
	path := filepath.Join(home, "eula.txt")
	if legacyAccepted {
		if err := writeEULAFile(path); err != nil {
			return false, err
		}
		return true, nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect eula.txt: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("eula.txt must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read eula.txt: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "eula") {
			return strings.EqualFold(strings.TrimSpace(parts[1]), "true"), nil
		}
	}
	return false, nil
}

func writeEULAFile(path string) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("eula.txt must be a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".eula-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString("eula=true\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("write eula.txt: %w", err)
	}
	return nil
}
