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
// server after the user presses I Accept. PCVM never accepts a startup-variable
// override or writes acceptance on the user's behalf.
func ensureMinecraftEULA(home string) (bool, error) {
	path := filepath.Join(home, "eula.txt")
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
