package pcvm

import (
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
