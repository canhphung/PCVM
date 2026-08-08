package pcvm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	control := t.TempDir()
	want := State{Provider: "paper", Family: "bukkit", ResolvedVersion: "1.21.4", InstalledAt: time.Unix(123, 0)}
	if err := SaveState(control, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(control)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != want.Provider || got.Schema != StateSchema {
		t.Fatalf("%+v", got)
	}
}

func TestLoadStateMigratesSchemaOne(t *testing.T) {
	control := t.TempDir()
	path := filepath.Join(control, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"provider":"paper","family":"minecraft-java-bukkit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(control)
	if err != nil {
		t.Fatal(err)
	}
	if state.Schema != StateSchema {
		t.Fatalf("schema=%d", state.Schema)
	}
	var persisted State
	if err := readJSON(path, &persisted); err != nil || persisted.Schema != StateSchema {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}
