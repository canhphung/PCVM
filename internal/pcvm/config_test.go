package pcvm

import "testing"

func TestConfigPolicyAndGitValidation(t *testing.T) {
	t.Setenv("PCVM_HOME", t.TempDir())
	t.Setenv("ALLOWED_SOFTWARE", "paper,node-bot")
	t.Setenv("CACHE_LIMIT_MB", "128")
	t.Setenv("GIT_ALLOWED_HOSTS", "github.com")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Policy.AllowedSoftware["paper"] || cfg.Policy.AllowedSoftware["bedrock"] {
		t.Fatal("allowlist not applied")
	}
	if cfg.Policy.CacheLimitBytes != 128*1024*1024 {
		t.Fatal("cache limit")
	}
	if err := cfg.ValidateGitURL("https://github.com/acme/bot.git"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://github.com/a/b", "https://token@github.com/a/b", "https://evil.example/a/b", "git@github.com:a/b"} {
		if err := cfg.ValidateGitURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestConfigRejectsUnsafeMirror(t *testing.T) {
	t.Setenv("PCVM_HOME", t.TempDir())
	t.Setenv("RUNTIME_MIRROR_URL", "http://mirror.example")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected HTTPS error")
	}
}

func TestLegacyBrandNameNormalizesToPCVM(t *testing.T) {
	t.Setenv("PCVM_HOME", t.TempDir())
	t.Setenv("BRAND_NAME", "Smart MultiEgg")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.BrandName != "PCVM" {
		t.Fatalf("brand=%q", cfg.Policy.BrandName)
	}
}
