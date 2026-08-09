package pcvm

import (
	"bytes"
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
	data, err := os.ReadFile(filepath.Join(control, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte(`"command"`), []byte(`"environment"`), []byte(`"working_directory"`), []byte(`"ready_patterns"`), []byte(`"stop_command"`), []byte(`/bin/sh`)} {
		if bytes.Contains(data, forbidden) {
			t.Fatalf("state persisted process metadata %q: %s", forbidden, data)
		}
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

func TestLoadStateMigratesSchemaTwoAndDropsProcessMetadata(t *testing.T) {
	control := t.TempDir()
	path := filepath.Join(control, "state.json")
	legacy := `{"schema":2,"provider":"bedrock","resolved_version":"1","resolved_build":"release","command":["/bin/sh","-c","id"],"environment":["LD_PRELOAD=evil"],"working_directory":"/tmp"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(control)
	if err != nil {
		t.Fatal(err)
	}
	if state.Schema != StateSchema {
		t.Fatalf("unsafe schema-2 process metadata survived migration: %+v", state)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(`/bin/sh`)) || bytes.Contains(data, []byte(`LD_PRELOAD`)) {
		t.Fatalf("unsafe process metadata remained on disk: %s", data)
	}
}
