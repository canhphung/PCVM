package pcvm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	control := t.TempDir()
	want := State{Provider: "paper", Family: "bukkit", ResolvedVersion: "1.21.4", ResolvedBuild: "release", Architecture: "amd64", InstalledAt: time.Unix(123, 0)}
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

func TestLoadStateRefusesLegacySchemasWithoutWrites(t *testing.T) {
	for _, schema := range []int{1, 2, 3} {
		t.Run(string(rune('0'+schema)), func(t *testing.T) {
			control := t.TempDir()
			path := filepath.Join(control, "state.json")
			legacy := []byte(`{"schema":` + string(rune('0'+schema)) + `,"provider":"bedrock","command":["/bin/sh","-c","id"]}`)
			if err := os.WriteFile(path, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if state, err := LoadState(control); state != nil || err == nil || !strings.Contains(err.Error(), legacyStateCode) {
				t.Fatalf("legacy schema was not refused: state=%+v err=%v", state, err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, legacy) || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("legacy state was modified: err=%v", err)
			}
		})
	}
}
