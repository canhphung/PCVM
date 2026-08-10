package pcvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMinecraftEULAFileContract(t *testing.T) {
	home := t.TempDir()
	accepted, err := ensureMinecraftEULA(home)
	if err != nil || accepted {
		t.Fatalf("missing eula accepted=%v err=%v", accepted, err)
	}
	if err := os.WriteFile(filepath.Join(home, "eula.txt"), []byte("# generated\neula=false\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	accepted, err = ensureMinecraftEULA(home)
	if err != nil || accepted {
		t.Fatalf("eula=false accepted=%v err=%v", accepted, err)
	}
	if err := os.WriteFile(filepath.Join(home, "eula.txt"), []byte("eula=true"), 0o640); err != nil {
		t.Fatal(err)
	}
	accepted, err = ensureMinecraftEULA(home)
	if err != nil || !accepted {
		t.Fatalf("Panel eula=true accepted=%v err=%v", accepted, err)
	}
}
