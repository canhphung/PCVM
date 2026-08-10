package pcvm

import (
	"context"
	"io"
	"time"
)

const (
	StateSchema           = 4
	CatalogSchema         = 6
	InstallReceiptSchema  = 3
	PendingSwitchSchema   = 2
	RuntimeManifestSchema = 1

	ImageProfileMinecraft = "minecraft"
	ImageProfileGames     = "games"
	ImageProfileApps      = "apps"
	ImageProfileVM        = "vm"
	ImageProfileFull      = "full"
)

type Catalog struct {
	Schema       int               `json:"schema"`
	Version      string            `json:"version"`
	Providers    []ProviderSpec    `json:"providers"`
	RuntimePacks []RuntimePackSpec `json:"runtime_packs"`
}

type ProviderSpec struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Family        string            `json:"family"`
	Architectures []string          `json:"architectures"`
	Runtime       string            `json:"runtime"`
	RuntimePolicy RuntimePolicySpec `json:"runtime_policy"`
	Resolver      string            `json:"resolver"`
	Installer     string            `json:"installer"`
	RequiresEULA  bool              `json:"requires_eula"`
	Options       DriverOptions     `json:"options,omitempty"`
	MenuPath      []string          `json:"menu_path"`
	Ports         []PortRequirement `json:"ports,omitempty"`
	Readiness     ReadinessSpec     `json:"readiness,omitempty"`
	Control       ControlSpec       `json:"control,omitempty"`
	Memory        MemorySpec        `json:"memory"`
	MinimumDisk   int               `json:"minimum_disk_mb,omitempty"`
	VMImages      []VMImageSpec     `json:"vm_images,omitempty"`
	SupportTier   string            `json:"support_tier,omitempty"`
	Upstream      string            `json:"upstream,omitempty"`
	License       string            `json:"license,omitempty"`
	Capabilities  []string          `json:"capabilities,omitempty"`
	Variables     []string          `json:"variables,omitempty"`
	InstallFormat int               `json:"install_format,omitempty"`
	RollbackMode  string            `json:"rollback_mode,omitempty"`
	VersionDomain string            `json:"version_domain,omitempty"`
	Preservation  PreservationSpec  `json:"preservation,omitempty"`
}

type RuntimePolicySpec struct {
	Default string   `json:"default"`
	Allowed []string `json:"allowed"`
}

// DriverOptions is the closed, declarative union of parameters accepted by
// compiled provider drivers. Strict catalog decoding rejects every key not
// represented here; unlike the 1.x map, it cannot grow hidden behavior at
// runtime.
type DriverOptions struct {
	Project          string `json:"project,omitempty"`
	Repository       string `json:"repository,omitempty"`
	AssetRegex       string `json:"asset_regex,omitempty"`
	AssetRegexAMD64  string `json:"asset_regex_amd64,omitempty"`
	AssetRegexARM64  string `json:"asset_regex_arm64,omitempty"`
	AppID            string `json:"appid,omitempty"`
	Executable       string `json:"executable,omitempty"`
	Overlay          string `json:"overlay,omitempty"`
	GameRoot         string `json:"game_root,omitempty"`
	Version          string `json:"version,omitempty"`
	Build            string `json:"build,omitempty"`
	MainURL          string `json:"main_url,omitempty"`
	MainSHA256       string `json:"main_sha256,omitempty"`
	BaseURL          string `json:"base_url,omitempty"`
	BaseSHA256       string `json:"base_sha256,omitempty"`
	ResourcesURL     string `json:"resources_url,omitempty"`
	ResourcesSHA256  string `json:"resources_sha256,omitempty"`
	DefaultEntry     string `json:"default_entry,omitempty"`
	DependencyPolicy string `json:"dependency_command,omitempty"`
}

func (o DriverOptions) AssetRegexForArchitecture(architecture string) string {
	if architecture == "amd64" {
		return o.AssetRegexAMD64
	}
	if architecture == "arm64" {
		return o.AssetRegexARM64
	}
	return ""
}

func (o DriverOptions) PinnedArtifact(name string) (rawURL, checksum string) {
	switch name {
	case "main":
		return o.MainURL, o.MainSHA256
	case "base":
		return o.BaseURL, o.BaseSHA256
	case "resources":
		return o.ResourcesURL, o.ResourcesSHA256
	default:
		return "", ""
	}
}

type PreservationSpec struct {
	Paths        []string `json:"paths,omitempty"`
	ManagedPaths []string `json:"managed_paths,omitempty"`
}

type MemorySpec struct {
	Strategy      string `json:"strategy"`
	RecommendedMB int    `json:"recommended_mb"`
	HardMinimumMB int    `json:"hard_minimum_mb"`
}

type VMImageSpec struct {
	ID           string `json:"image_id"`
	Variant      string `json:"variant"`
	Deprecated   bool   `json:"deprecated,omitempty"`
	Version      string `json:"version"`
	Build        string `json:"build"`
	Architecture string `json:"architecture"`
	URL          string `json:"url"`
	Format       string `json:"format"`
	SHA256       string `json:"sha256,omitempty"`
	SHA512       string `json:"sha512,omitempty"`
}

type PortRequirement struct {
	Variable string `json:"variable"`
	Offset   int    `json:"offset,omitempty"`
	Internal bool   `json:"internal,omitempty"`
}

type ReadinessSpec struct {
	Mode           string   `json:"mode,omitempty"`
	Patterns       []string `json:"patterns,omitempty"`
	PortVariable   string   `json:"port_variable,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type ControlSpec struct {
	Mode         string `json:"mode,omitempty"`
	StopCommand  string `json:"stop_command,omitempty"`
	PortVariable string `json:"port_variable,omitempty"`
	Password     string `json:"password,omitempty"`
	SocketPath   string `json:"socket_path,omitempty"`
}

type RuntimePackSpec struct {
	ID              string `json:"id,omitempty"`
	Kind            string `json:"kind"`
	Version         string `json:"version"`
	UpstreamVersion string `json:"upstream_version"`
	Architecture    string `json:"architecture"`
	URL             string `json:"url"`
	SHA256          string `json:"sha256"`
	Executable      string `json:"executable"`
	Archive         string `json:"archive"`
	Size            int64  `json:"size,omitempty"`
	TreeSHA256      string `json:"tree_sha256,omitempty"`
}

type RuntimeManifest struct {
	Schema        int               `json:"schema"`
	Release       string            `json:"release"`
	Compatibility string            `json:"compatibility"`
	Packs         []RuntimePackSpec `json:"packs"`
}

type Request struct {
	Software           string
	Version            string
	Build              string
	RuntimeVersion     string
	AutoUpdate         bool
	UpdateRequest      string
	ResetConfirm       string
	SourceMode         string
	GitURL             string
	GitBranch          string
	EntryFile          string
	AppArgs            string
	AppReady           string
	CodeServerPassword string
	ServerName         string
	ServerPassword     string
	AdminPassword      string
	MaxPlayers         int
	GameMap            string
	GameWorld          string
	GameSeed           string
	GameExtraArgs      string
	SteamGSLT          string
	QueryPort          int
	SteamPort          int
	ReliablePort       int
	GamePort2          int
	GamePort3          int
	RCONPort           int
	TelnetPort         int
	WebMode            string
	WebRoot            string
	UpstreamURL        string
	Architecture       string
	VMMemoryMB         string
	VMCPUs             string
	VMDiskGB           int
	VMDiskCompression  string
	VMHostname         string
	ModpackMode        string
	ModpackProject     string
	ModpackFile        string
}

type Policy struct {
	AllowedSoftware map[string]bool
	AllowUserReset  bool
	BrandName       string
	SupportURL      string
	RuntimeMirror   string
	AllowedGitHosts map[string]bool
	CacheLimitBytes int64
	AllowSystemPath bool
	ClearConsole    bool
	VMMaxMemoryMB   int
	VMMaxCPUs       int
	VMMaxDiskGB     int
}

type Selector struct {
	Version string `json:"version"`
	Build   string `json:"build"`
	Runtime string `json:"runtime"`
}

type ArtifactIntegrity struct {
	SHA256 string `json:"sha256,omitempty"`
	SHA512 string `json:"sha512,omitempty"`
}

type ArtifactLock struct {
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	Build     string            `json:"build"`
	Revision  string            `json:"revision,omitempty"`
	Integrity ArtifactIntegrity `json:"integrity"`
}

// State is an untrusted installation index. Only the fields above the
// compatibility divider are serialized. The aliases below it keep the
// launcher internals readable while v2 drivers move to ArtifactLock and
// Selector; they can never be supplied through state.json.
type State struct {
	Schema              int          `json:"schema"`
	Provider            string       `json:"provider"`
	InstallID           string       `json:"install_id"`
	Selector            Selector     `json:"selector"`
	ArtifactLock        ArtifactLock `json:"artifact"`
	RuntimePackID       string       `json:"runtime_pack_id,omitempty"`
	Architecture        string       `json:"architecture"`
	InstallFormat       int          `json:"install_format"`
	Receipt             string       `json:"receipt"`
	ImmutableConfigHash string       `json:"immutable_config_hash"`
	UpdateRequestHash   string       `json:"update_request_hash,omitempty"`
	InstalledAt         time.Time    `json:"installed_at"`
	UpdatedAt           time.Time    `json:"updated_at"`

	Family            string            `json:"-"`
	RequestedVersion  string            `json:"-"`
	RequestedBuild    string            `json:"-"`
	ResolvedVersion   string            `json:"-"`
	ResolvedBuild     string            `json:"-"`
	RuntimeKind       string            `json:"-"`
	RuntimeVersion    string            `json:"-"`
	Artifact          Artifact          `json:"-"`
	LastUpdateRequest string            `json:"-"`
	Metadata          map[string]string `json:"-"`
}

// LaunchState is constructed exclusively from the embedded catalog, fixed
// installation paths, and validated startup variables. It cannot be decoded
// from the user-writable persisted State.
type LaunchState struct {
	Provider          string
	ResolvedVersion   string
	ResolvedBuild     string
	RuntimeVersion    string
	VMImageID         string
	VMImageVariant    string
	VMImageChecksum   string
	VMDiskCompression string
	Command           []string
	Environment       []string
	WorkingDirectory  string
	Readiness         ReadinessSpec
	Control           ControlSpec
}

type PendingSwitch struct {
	Schema       int       `json:"schema"`
	FromProvider string    `json:"from_provider"`
	ToProvider   string    `json:"to_provider"`
	FromVersion  string    `json:"from_version"`
	ToVersion    string    `json:"to_version"`
	Nonce        string    `json:"nonce"`
	ExpiresAt    time.Time `json:"expires_at"`
	Reason       string    `json:"reason"`
	SourceHash   string    `json:"source_hash,omitempty"`
	TargetHash   string    `json:"target_hash,omitempty"`
}

type InstallReceipt struct {
	Schema        int           `json:"schema"`
	ID            string        `json:"id"`
	Provider      string        `json:"provider"`
	InstallFormat int           `json:"install_format"`
	ReleaseID     string        `json:"release_id"`
	RollbackMode  string        `json:"rollback_mode"`
	RootSHA256    string        `json:"root_sha256,omitempty"`
	Files         []ReceiptFile `json:"files,omitempty"`
	ManagedPaths  []string      `json:"managed_paths,omitempty"`
	SourceCommit  string        `json:"source_commit,omitempty"`
	Artifact      ArtifactLock  `json:"artifact"`
	CreatedAt     time.Time     `json:"created_at"`
}

type ReceiptFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type Artifact struct {
	URL      string            `json:"url"`
	FileName string            `json:"file_name"`
	Kind     string            `json:"kind"`
	SHA256   string            `json:"sha256,omitempty"`
	SHA1     string            `json:"sha1,omitempty"`
	SHA512   string            `json:"sha512,omitempty"`
	Version  string            `json:"version"`
	Build    string            `json:"build"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Resolved struct {
	Artifact       Artifact
	RuntimeKind    string
	RuntimeVersion string
	// RollbackMode is an ephemeral installer decision. It is used when one
	// provider supports both user-mutable installs (upload) and immutable,
	// release-backed installs (Git). It is never accepted from persisted state.
	RollbackMode string
	// PreparedArtifact is an ephemeral, verified local path used between
	// identity reconciliation and installation. It is never persisted in state.
	PreparedArtifact string
	Command          []string
	Environment      []string
	WorkDir          string
}

type InstallContext struct {
	Home           string
	ControlDir     string
	AllocationPort int
	Artifact       string
	Runtime        string
	PreparedSource string
	Request        Request
	Log            *Logger
	HTTP           *HTTPClient
	Out            io.Writer
	Err            io.Writer
	Dependencies   Dependencies
}

type Provider interface {
	Spec() ProviderSpec
	Resolve(context.Context, Request, *HTTPClient) (Resolved, error)
	Install(context.Context, InstallContext, Resolved) (Resolved, error)
	BuildProcess(context.Context, Config, LaunchState, MemoryPlan) (ProcessSpec, error)
	CompareVersions(a, b string) int
}

type ProcessSpec struct {
	Command         []string
	Directory       string
	Environment     []string
	ReadyAfter      time.Duration
	ReadyTimeout    time.Duration
	StopTimeout     time.Duration
	Readiness       ReadinessSpec
	Control         ControlSpec
	RawOutput       bool
	RepeatReadiness bool
	BeforeStart     func(context.Context) (func() error, error)
}

// LaunchPlan is the trusted, fully rebuilt process contract handed to the
// supervisor. ProcessSpec remains as a compatibility name for the supervisor
// implementation; neither form is serialized or accepted from state.json.
type LaunchPlan = ProcessSpec

type Supervisor interface {
	Run(context.Context, ProcessSpec, io.Reader, io.Writer, io.Writer) error
}
