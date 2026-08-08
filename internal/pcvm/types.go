package pcvm

import (
	"context"
	"io"
	"time"
)

const (
	StateSchema   = 2
	CatalogSchema = 2
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
	Resolver      string            `json:"resolver"`
	Installer     string            `json:"installer"`
	ReadyPatterns []string          `json:"ready_patterns"`
	StopCommand   string            `json:"stop_command"`
	RequiresEULA  bool              `json:"requires_eula"`
	Options       map[string]string `json:"options,omitempty"`
	MenuPath      []string          `json:"menu_path"`
	Ports         []PortRequirement `json:"ports,omitempty"`
	Readiness     ReadinessSpec     `json:"readiness,omitempty"`
	Control       ControlSpec       `json:"control,omitempty"`
	MinimumMemory int               `json:"minimum_memory_mb,omitempty"`
	MinimumDisk   int               `json:"minimum_disk_mb,omitempty"`
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
}

type RuntimePackSpec struct {
	Kind         string `json:"kind"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	Executable   string `json:"executable"`
	Archive      string `json:"archive"`
}

type Request struct {
	Software       string
	Version        string
	Build          string
	RuntimeVersion string
	AutoUpdate     bool
	UpdateRequest  string
	AcceptEULA     bool
	ResetConfirm   string
	SourceMode     string
	GitURL         string
	GitBranch      string
	EntryFile      string
	AppArgs        string
	AppReady       string
	ServerName     string
	ServerPassword string
	AdminPassword  string
	MaxPlayers     int
	GameMap        string
	GameWorld      string
	GameSeed       string
	GameExtraArgs  string
	SteamGSLT      string
	QueryPort      int
	SteamPort      int
	ReliablePort   int
	GamePort2      int
	GamePort3      int
	RCONPort       int
	TelnetPort     int
	WebMode        string
	WebRoot        string
	UpstreamURL    string
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
}

type State struct {
	Schema            int               `json:"schema"`
	Provider          string            `json:"provider"`
	Family            string            `json:"family"`
	RequestedVersion  string            `json:"requested_version"`
	RequestedBuild    string            `json:"requested_build"`
	ResolvedVersion   string            `json:"resolved_version"`
	ResolvedBuild     string            `json:"resolved_build"`
	RuntimeKind       string            `json:"runtime_kind"`
	RuntimeVersion    string            `json:"runtime_version"`
	RuntimeExecutable string            `json:"runtime_executable"`
	Architecture      string            `json:"architecture"`
	Artifact          Artifact          `json:"artifact"`
	Command           []string          `json:"command"`
	Environment       []string          `json:"environment,omitempty"`
	WorkingDirectory  string            `json:"working_directory"`
	ReadyPatterns     []string          `json:"ready_patterns"`
	StopCommand       string            `json:"stop_command"`
	LastUpdateRequest string            `json:"last_update_request"`
	InstalledAt       time.Time         `json:"installed_at"`
	Metadata          map[string]string `json:"metadata,omitempty"`
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
}

type Artifact struct {
	URL      string            `json:"url"`
	FileName string            `json:"file_name"`
	Kind     string            `json:"kind"`
	SHA256   string            `json:"sha256,omitempty"`
	SHA1     string            `json:"sha1,omitempty"`
	Version  string            `json:"version"`
	Build    string            `json:"build"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type Resolved struct {
	Artifact       Artifact
	RuntimeKind    string
	RuntimeVersion string
	Command        []string
	Environment    []string
	WorkDir        string
	ReadyPatterns  []string
	StopCommand    string
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
}

type Provider interface {
	Spec() ProviderSpec
	Resolve(context.Context, Request, *HTTPClient) (Resolved, error)
	Install(context.Context, InstallContext, Resolved) (Resolved, error)
	BuildProcess(context.Context, Config, State) (ProcessSpec, error)
	CompareVersions(a, b string) int
}

type ProcessSpec struct {
	Command       []string
	Directory     string
	Environment   []string
	ReadyPatterns []string
	StopCommand   string
	ReadyAfter    time.Duration
	ReadyTimeout  time.Duration
	StopTimeout   time.Duration
	Readiness     ReadinessSpec
	Control       ControlSpec
}

type Supervisor interface {
	Run(context.Context, ProcessSpec, io.Reader, io.Writer, io.Writer) error
}
