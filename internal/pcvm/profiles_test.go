package pcvm

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestImageProfileProviderMatrix(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{ImageProfileCore: 21, ImageProfileGames: 38, ImageProfileVM: 26, ImageProfileFull: 43}
	for profile, expected := range want {
		count := 0
		for _, spec := range catalog.Providers {
			if ImageProfileSupports(profile, spec) {
				count++
			}
		}
		if count != expected {
			t.Fatalf("profile %s exposes %d providers, want %d", profile, count, expected)
		}
	}
	for _, test := range []struct {
		profile, provider string
		want              bool
	}{
		{ImageProfileCore, "paper", true},
		{ImageProfileCore, "rust", false},
		{ImageProfileCore, "vm-debian", false},
		{ImageProfileGames, "rust", true},
		{ImageProfileGames, "vm-debian", false},
		{ImageProfileVM, "paper", true},
		{ImageProfileVM, "vm-debian", true},
		{ImageProfileVM, "rust", false},
		{ImageProfileFull, "rust", true},
		{ImageProfileFull, "vm-debian", true},
	} {
		spec, ok := catalog.Provider(test.provider)
		if !ok {
			t.Fatal(test.provider)
		}
		if got := ImageProfileSupports(test.profile, spec); got != test.want {
			t.Errorf("profile=%s provider=%s got=%v want=%v", test.profile, test.provider, got, test.want)
		}
	}
}

func TestImageProfileCannotBeOverriddenByEnvironment(t *testing.T) {
	t.Setenv("PCVM_IMAGE_PROFILE", ImageProfileVM)
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImageProfile != ImageProfileFull {
		t.Fatalf("environment changed embedded profile to %q", cfg.ImageProfile)
	}
}

func TestDirectProviderSelectionRejectsMissingImageCapability(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Home: t.TempDir(), Arch: "amd64", ImageProfile: ImageProfileCore,
		Request: Request{Software: "rust", Version: "latest", Build: "latest", Architecture: "amd64"},
		Policy:  Policy{AllowedSoftware: map[string]bool{"rust": true}},
	}
	cfg.Control = cfg.Home + "/.pcvm"
	app := NewApp(cfg, catalog, bytes.NewReader(nil), io.Discard, io.Discard)
	err = app.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires the games image capability") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeImageProfile(t *testing.T) {
	for _, profile := range []string{"", "CORE", " games ", "vm", "full"} {
		if _, err := NormalizeImageProfile(profile); err != nil {
			t.Errorf("profile %q: %v", profile, err)
		}
	}
	if _, err := NormalizeImageProfile("custom"); err == nil {
		t.Fatal("accepted unknown profile")
	}
}
