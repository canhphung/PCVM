package pcvm

import "testing"

func TestGameExtraArgsProviderAllowlist(t *testing.T) {
	for _, test := range []struct {
		provider string
		raw      string
		valid    bool
	}{
		{"cs2", `-tickrate=128 +game_mode 1`, true},
		{"valheim", `-crossplay -saveinterval 1200`, true},
		{"factorio", `--non-blocking-saving --autosave-slots 5`, true},
		{"rust", `+server.description "friendly server"`, true},
		{"rust", `+server.description @/home/container/args.txt`, false},
		{"rust", `+server.description=@args.txt`, false},
		{"cs2", `+sv_cheats 1`, false},
		{"cs2", `-tickrate +game_mode 1`, false},
		{"valheim", `-crossplay=true`, false},
		{"palworld", `-port 9999`, false},
		{"nginx", `--anything`, false},
	} {
		_, err := safeGameExtraArgs(test.provider, test.raw)
		if (err == nil) != test.valid {
			t.Errorf("provider=%s raw=%q valid=%v err=%v", test.provider, test.raw, test.valid, err)
		}
	}
}

func TestGameExtraArgsRejectsControlCharactersAndLimits(t *testing.T) {
	if _, err := safeGameExtraArgs("rust", "+server.description \"line\nfeed\""); err == nil {
		t.Fatal("newline accepted")
	}
	tooLong := "+server.description "
	for range 300 {
		tooLong += "x"
	}
	if _, err := safeGameExtraArgs("rust", tooLong); err == nil {
		t.Fatal("oversized token accepted")
	}
}
