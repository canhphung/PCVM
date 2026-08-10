package pcvm

import (
	"strings"
	"testing"
)

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

func TestConfigReadsVMCompressionAndAllowsAlpineByDefault(t *testing.T) {
	t.Setenv("PCVM_HOME", t.TempDir())
	t.Setenv("VM_DISK_COMPRESSION", "ZSTD")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Request.VMDiskCompression != "zstd" || !cfg.Policy.AllowedSoftware["vm-alpine"] || !cfg.Policy.AllowedSoftware["samp"] || !cfg.Policy.AllowedSoftware["mtasa"] || !cfg.Policy.AllowedSoftware["code-server"] {
		t.Fatalf("unexpected VM config: compression=%q alpine=%v", cfg.Request.VMDiskCompression, cfg.Policy.AllowedSoftware["vm-alpine"])
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

func TestInvalidMaxPlayersIsNotSilentlyDefaulted(t *testing.T) {
	t.Setenv("PCVM_HOME", t.TempDir())
	t.Setenv("MAX_PLAYERS", "many")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProviderRequest(catalogSpec(t, "rust"), cfg); err == nil {
		t.Fatal("invalid MAX_PLAYERS was accepted")
	}
}

func TestConfigRejectsInvalidBooleans(t *testing.T) {
	for _, key := range []string{"AUTO_UPDATE", "ALLOW_USER_RESET", "PCVM_ALLOW_SYSTEM_RUNTIME", "CLEAR_CONSOLE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("PCVM_HOME", t.TempDir())
			t.Setenv(key, "sometimes")
			if _, err := ConfigFromEnv(); err == nil {
				t.Fatalf("invalid %s was silently accepted", key)
			}
		})
	}
}

func TestConfigRejectsInvalidPublicEnums(t *testing.T) {
	for key, value := range map[string]string{
		"SOURCE_MODE":         "auto",
		"WEB_MODE":            "cgi",
		"VM_DISK_COMPRESSION": "gzip",
		"MODPACK_MODE":        "remote",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("PCVM_HOME", t.TempDir())
			t.Setenv(key, value)
			if _, err := ConfigFromEnv(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("invalid %s=%q was accepted: %v", key, value, err)
			}
		})
	}
}
