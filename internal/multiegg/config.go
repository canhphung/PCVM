package multiegg

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Config struct {
	Home           string
	Control        string
	Arch           string
	AllocationPort int
	Request        Request
	Policy         Policy
}

func ConfigFromEnv() (Config, error) {
	home := strings.TrimSpace(os.Getenv("MULTIEGG_HOME"))
	if home == "" {
		home = "/home/container"
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return Config{}, fmt.Errorf("resolve home: %w", err)
	}
	home = filepath.Clean(abs)
	limit := int64(2048)
	if raw := strings.TrimSpace(os.Getenv("CACHE_LIMIT_MB")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 64 {
			return Config{}, fmt.Errorf("CACHE_LIMIT_MB must be at least 64")
		}
		limit = parsed
	}
	allowed := csvSet(envDefault("ALLOWED_SOFTWARE", "vanilla,paper,purpur,pufferfish,fabric,forge,neoforge,velocity,bungeecord,bedrock,pocketmine,node-bot,python-bot,lavalink"))
	gitHosts := csvSet(envDefault("GIT_ALLOWED_HOSTS", "github.com,gitlab.com,codeberg.org"))
	request := Request{
		Software: envDefault("SOFTWARE", "interactive"), Version: envDefault("SOFTWARE_VERSION", "latest"),
		Build: envDefault("SOFTWARE_BUILD", "latest"), RuntimeVersion: envDefault("RUNTIME_VERSION", "auto"),
		AutoUpdate: envBool("AUTO_UPDATE", false), UpdateRequest: os.Getenv("UPDATE_REQUEST"),
		AcceptEULA: envBool("ACCEPT_MINECRAFT_EULA", false), ResetConfirm: os.Getenv("RESET_CONFIRM"),
		SourceMode: envDefault("SOURCE_MODE", "upload"), GitURL: os.Getenv("GIT_URL"),
		GitBranch: envDefault("GIT_BRANCH", "main"), EntryFile: os.Getenv("ENTRY_FILE"),
		AppArgs: os.Getenv("APP_ARGS"), AppReady: os.Getenv("APP_READY_PATTERN"),
	}
	policy := Policy{AllowedSoftware: allowed, AllowUserReset: envBool("ALLOW_USER_RESET", true),
		BrandName: envDefault("BRAND_NAME", "Smart MultiEgg"), SupportURL: os.Getenv("SUPPORT_URL"),
		RuntimeMirror: os.Getenv("RUNTIME_MIRROR_URL"), AllowedGitHosts: gitHosts,
		CacheLimitBytes: limit * 1024 * 1024, AllowSystemPath: envBool("MULTIEGG_ALLOW_SYSTEM_RUNTIME", false)}
	if policy.RuntimeMirror != "" {
		u, err := url.Parse(policy.RuntimeMirror)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return Config{}, fmt.Errorf("RUNTIME_MIRROR_URL must be HTTPS")
		}
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return Config{}, fmt.Errorf("unsupported architecture %q", arch)
	}
	allocationPort := 0
	if raw := strings.TrimSpace(os.Getenv("SERVER_PORT")); raw != "" {
		allocationPort, err = strconv.Atoi(raw)
		if err != nil || allocationPort < 1 || allocationPort > 65535 {
			return Config{}, fmt.Errorf("SERVER_PORT must be an integer between 1 and 65535")
		}
	}
	return Config{Home: home, Control: filepath.Join(home, ".multiegg"), Arch: arch, AllocationPort: allocationPort, Request: request, Policy: policy}, nil
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
func csvSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			out[item] = true
		}
	}
	return out
}

func (c Config) ValidateGitURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("GIT_URL must be a public HTTPS URL without credentials")
	}
	if !c.Policy.AllowedGitHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("Git host %q is not allowed", u.Hostname())
	}
	return nil
}
