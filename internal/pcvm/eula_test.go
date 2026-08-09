package pcvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinecraftEULAFileContract(t *testing.T) {
	home := t.TempDir()
	accepted, err := ensureMinecraftEULA(home, false)
	if err != nil || accepted {
		t.Fatalf("missing eula accepted=%v err=%v", accepted, err)
	}
	if err := os.WriteFile(filepath.Join(home, "eula.txt"), []byte("# generated\neula=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	accepted, err = ensureMinecraftEULA(home, false)
	if err != nil || accepted {
		t.Fatalf("eula=false accepted=%v err=%v", accepted, err)
	}
	if err := os.WriteFile(filepath.Join(home, "eula.txt"), []byte("eula=true"), 0o640); err != nil {
		t.Fatal(err)
	}
	accepted, err = ensureMinecraftEULA(home, false)
	if err != nil || !accepted {
		t.Fatalf("Panel eula=true accepted=%v err=%v", accepted, err)
	}
}

func TestLegacyEULAOverrideMaterializesPanelFile(t *testing.T) {
	home := t.TempDir()
	accepted, err := ensureMinecraftEULA(home, true)
	if err != nil || !accepted {
		t.Fatalf("legacy override accepted=%v err=%v", accepted, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "eula.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "eula=true" {
		t.Fatalf("eula file=%q err=%v", data, err)
	}
}
