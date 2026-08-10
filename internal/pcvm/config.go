package pcvm

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
	ImageProfile   string
	AllocationPort int
	Request        Request
	Policy         Policy
	Dependencies   Dependencies
}

func ConfigFromEnv() (Config, error) {
	autoUpdate, err := envStrictBool("AUTO_UPDATE", false)
	if err != nil {
		return Config{}, err
	}
	allowUserReset, err := envStrictBool("ALLOW_USER_RESET", true)
	if err != nil {
		return Config{}, err
	}
	allowSystemPath, err := envStrictBool("PCVM_ALLOW_SYSTEM_RUNTIME", false)
	if err != nil {
		return Config{}, err
	}
	clearConsole, err := envStrictBool("CLEAR_CONSOLE", true)
	if err != nil {
		return Config{}, err
	}
	sourceMode, err := envStrictEnum("SOURCE_MODE", "upload", "upload", "git")
	if err != nil {
		return Config{}, err
	}
	webMode, err := envStrictEnum("WEB_MODE", "static", "static", "proxy")
	if err != nil {
		return Config{}, err
	}
	diskCompression, err := envStrictEnum("VM_DISK_COMPRESSION", "off", "off", "zstd")
	if err != nil {
		return Config{}, err
	}
	modpackMode, err := envStrictEnum("MODPACK_MODE", "project", "project", "upload")
	if err != nil {
		return Config{}, err
	}
	home := strings.TrimSpace(os.Getenv("PCVM_HOME"))
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
	allowed := csvSet(envDefault("ALLOWED_SOFTWARE", "vanilla,paper,purpur,pufferfish,folia,canvas,fabric,quilt,forge,neoforge,paper-geyser,modrinth-modpack,velocity,bungeecord,bedrock,pocketmine,powernukkitx,cloudburst-nukkit,endstone,cs2,gmod,l4d2,samp,mtasa,palworld,rust,rust-umod,project-zomboid,valheim,valheim-bepinex,7dtd,unturned,terraria,tmodloader,tshock,satisfactory,factorio,nginx,apache,caddy,node-bot,python-bot,bun-app,deno-app,go-app,dotnet-app,lavalink,code-server,vm-ubuntu,vm-debian,vm-almalinux,vm-rocky,vm-alpine"))
	gitHosts := csvSet(envDefault("GIT_ALLOWED_HOSTS", "github.com,gitlab.com,codeberg.org"))
	request := Request{
		Software: envDefault("SOFTWARE", "interactive"), Version: envDefault("SOFTWARE_VERSION", "latest"),
		Build: envDefault("SOFTWARE_BUILD", "latest"), RuntimeVersion: envDefault("RUNTIME_VERSION", "auto"),
		AutoUpdate: autoUpdate, UpdateRequest: os.Getenv("UPDATE_REQUEST"), ResetConfirm: os.Getenv("RESET_CONFIRM"),
		SourceMode: sourceMode, GitURL: os.Getenv("GIT_URL"),
		GitBranch: envDefault("GIT_BRANCH", "main"), EntryFile: os.Getenv("ENTRY_FILE"),
		AppArgs: os.Getenv("APP_ARGS"), AppReady: os.Getenv("APP_READY_PATTERN"),
		CodeServerPassword: os.Getenv("CODE_SERVER_PASSWORD"),
		ServerName:         envDefault("SERVER_NAME", "PCVM Server"), ServerPassword: os.Getenv("SERVER_PASSWORD"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"), MaxPlayers: envInt("MAX_PLAYERS", 16),
		GameMap: os.Getenv("GAME_MAP"), GameWorld: envDefault("GAME_WORLD", "Dedicated"),
		GameSeed: os.Getenv("GAME_SEED"), GameExtraArgs: os.Getenv("GAME_EXTRA_ARGS"), SteamGSLT: os.Getenv("STEAM_GSLT"),
		QueryPort: envPort("QUERY_PORT"), SteamPort: envPort("STEAM_PORT"), ReliablePort: envPort("RELIABLE_PORT"),
		GamePort2: envPort("GAME_PORT_2"), GamePort3: envPort("GAME_PORT_3"), RCONPort: envPortDefault("RCON_PORT", 25575),
		TelnetPort: envPortDefault("TELNET_PORT", 8081), WebMode: webMode,
		WebRoot: envDefault("WEB_ROOT", "public"), UpstreamURL: os.Getenv("UPSTREAM_URL"),
		VMMemoryMB: envDefault("VM_MEMORY_MB", "auto"), VMCPUs: envDefault("VM_CPUS", "auto"),
		VMDiskGB: envInt("VM_DISK_GB", 10), VMDiskCompression: diskCompression,
		VMHostname:  envDefault("VM_HOSTNAME", "pcvm"),
		ModpackMode: modpackMode, ModpackProject: os.Getenv("MODPACK_PROJECT"),
		ModpackFile: envDefault("MODPACK_FILE", "server.mrpack"),
	}
	brandName := envDefault("BRAND_NAME", "PCVM")
	if strings.EqualFold(strings.ReplaceAll(brandName, " ", ""), "smartmultiegg") {
		brandName = "PCVM"
	}
	policy := Policy{AllowedSoftware: allowed, AllowUserReset: allowUserReset,
		BrandName: brandName, SupportURL: os.Getenv("SUPPORT_URL"),
		RuntimeMirror: os.Getenv("RUNTIME_MIRROR_URL"), AllowedGitHosts: gitHosts,
		CacheLimitBytes: limit * 1024 * 1024, AllowSystemPath: allowSystemPath,
		ClearConsole: clearConsole, VMMaxMemoryMB: envInt("VM_MAX_MEMORY_MB", 16384),
		VMMaxCPUs: envInt("VM_MAX_CPUS", 8), VMMaxDiskGB: envInt("VM_MAX_DISK_GB", 64)}
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
	request.Architecture = arch
	if policy.VMMaxMemoryMB < 768 || policy.VMMaxCPUs < 1 || policy.VMMaxCPUs > 8 || policy.VMMaxDiskGB < 8 {
		return Config{}, fmt.Errorf("VM admin caps are invalid")
	}
	allocationPort := 0
	if raw := strings.TrimSpace(os.Getenv("SERVER_PORT")); raw != "" {
		allocationPort, err = strconv.Atoi(raw)
		if err != nil || allocationPort < 1 || allocationPort > 65535 {
			return Config{}, fmt.Errorf("SERVER_PORT must be an integer between 1 and 65535")
		}
	}
	return Config{Home: home, Control: filepath.Join(home, ".pcvm"), Arch: arch, ImageProfile: ImageProfileFull, AllocationPort: allocationPort, Request: request, Policy: policy, Dependencies: DefaultDependencies()}, nil
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func envStrictBool(key string, fallback bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (0/1 or true/false)", key)
	}
	return b, nil
}

func envStrictEnum(key, fallback string, allowed ...string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		value = fallback
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s must be one of: %s", key, strings.Join(allowed, ", "))
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return value
}

func envPort(key string) int { return envPortDefault(key, 0) }

func envPortDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 65535 {
		return -1
	}
	return value
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
