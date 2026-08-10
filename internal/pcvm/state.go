package pcvm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const legacyStateCode = "PCVM-E2001 LEGACY_STATE"

type LegacyStateError struct {
	Path   string
	Schema int
}

func (e *LegacyStateError) Error() string {
	if e.Schema > 0 {
		return fmt.Sprintf("%s: %s uses PCVM state schema %d; back up the server and create a fresh PCVM 2.0 server, or set RESET_CONFIRM=REQUEST to begin the guarded reset flow", legacyStateCode, e.Path, e.Schema)
	}
	return fmt.Sprintf("%s: legacy control directory %s exists; back up the server and create a fresh PCVM 2.0 server", legacyStateCode, e.Path)
}

func DetectLegacyControl(home, control string) error {
	legacy := filepath.Join(home, ".multiegg")
	if filepath.Clean(control) == filepath.Join(filepath.Clean(home), ".pcvm") {
		if _, err := os.Lstat(legacy); err == nil {
			return &LegacyStateError{Path: legacy}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func LoadState(control string) (*State, error) {
	path := filepath.Join(control, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if header.Schema >= 1 && header.Schema < StateSchema {
		return nil, &LegacyStateError{Path: path, Schema: header.Schema}
	}
	if header.Schema != StateSchema {
		return nil, fmt.Errorf("unsupported state schema %d", header.Schema)
	}
	var state State
	if err := decodeStrictJSON(data, &state); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if err := validateStateV4(state); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	hydrateStateAliases(&state)
	return &state, nil
}

func SaveState(control string, state State) error {
	state.Schema = StateSchema
	normalizeStateV4(&state)
	if err := validateStateV4(state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return writeJSONAtomic(filepath.Join(control, "state.json"), state)
}

func normalizeStateV4(state *State) {
	if state.Provider == "" {
		return
	}
	if state.Selector.Version == "" {
		state.Selector.Version = state.RequestedVersion
		if state.Selector.Version == "" {
			state.Selector.Version = state.ResolvedVersion
		}
		if state.Selector.Version == "" {
			state.Selector.Version = "latest"
		}
	}
	if state.Selector.Build == "" {
		state.Selector.Build = state.RequestedBuild
		if state.Selector.Build == "" {
			state.Selector.Build = state.ResolvedBuild
		}
		if state.Selector.Build == "" {
			state.Selector.Build = "latest"
		}
	}
	if state.Selector.Runtime == "" {
		state.Selector.Runtime = state.RuntimeVersion
	}
	if state.ArtifactLock.ID == "" {
		state.ArtifactLock = lockArtifact(state.Provider, state.Artifact)
	}
	if state.ArtifactLock.Version == "" {
		state.ArtifactLock.Version = state.Selector.Version
	}
	if state.ArtifactLock.Build == "" {
		state.ArtifactLock.Build = state.Selector.Build
	}
	if state.RuntimePackID == "" && state.RuntimeVersion != "" && state.RuntimeKind != "native" {
		state.RuntimePackID = runtimePackIdentity(state.RuntimeKind, state.RuntimeVersion, state.Architecture)
	}
	if state.InstallFormat == 0 {
		state.InstallFormat = 1
	}
	if state.InstallID == "" {
		state.InstallID = installationID(*state)
	}
	if state.Receipt == "" {
		state.Receipt = state.InstallID + ".json"
	}
	if state.ImmutableConfigHash == "" {
		state.ImmutableConfigHash = sha256Hex(state.Provider)
	}
	if state.UpdateRequestHash == "" && state.LastUpdateRequest != "" {
		state.UpdateRequestHash = hashToken(state.LastUpdateRequest)
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = state.InstalledAt
	}
	hydrateStateAliases(state)
}

func hydrateStateAliases(state *State) {
	state.RequestedVersion = state.Selector.Version
	state.RequestedBuild = state.Selector.Build
	state.ResolvedVersion = state.ArtifactLock.Version
	state.ResolvedBuild = state.ArtifactLock.Build
	state.RuntimeKind, state.RuntimeVersion, _ = parseRuntimePackIdentity(state.RuntimePackID)
	if state.RuntimeVersion == "" {
		state.RuntimeVersion = state.Selector.Runtime
	}
	state.Artifact = Artifact{
		Version: state.ArtifactLock.Version,
		Build:   state.ArtifactLock.Build,
		SHA256:  state.ArtifactLock.Integrity.SHA256,
		SHA512:  state.ArtifactLock.Integrity.SHA512,
		Metadata: map[string]string{
			"artifact_id": state.ArtifactLock.ID,
		},
	}
	if strings.HasPrefix(state.ArtifactLock.ID, "vm:") {
		state.Artifact.Metadata["vm_image_id"] = strings.TrimPrefix(state.ArtifactLock.ID, "vm:")
	}
	if strings.HasPrefix(state.ArtifactLock.ID, "modrinth:") {
		state.Artifact.Metadata["modrinth_project_id"] = strings.TrimPrefix(state.ArtifactLock.ID, "modrinth:")
	}
}

func validateStateV4(state State) error {
	if state.Schema != StateSchema {
		return fmt.Errorf("schema must be %d", StateSchema)
	}
	if !validID.MatchString(state.Provider) {
		return fmt.Errorf("provider is invalid")
	}
	for name, value := range map[string]string{
		"install_id":            state.InstallID,
		"selector.version":      state.Selector.Version,
		"selector.build":        state.Selector.Build,
		"artifact.id":           state.ArtifactLock.ID,
		"artifact.version":      state.ArtifactLock.Version,
		"artifact.build":        state.ArtifactLock.Build,
		"architecture":          state.Architecture,
		"receipt":               state.Receipt,
		"immutable_config_hash": state.ImmutableConfigHash,
	} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	if state.Architecture != "amd64" && state.Architecture != "arm64" {
		return fmt.Errorf("architecture is invalid")
	}
	if state.InstallFormat < 1 {
		return fmt.Errorf("install_format is invalid")
	}
	if filepath.Base(state.Receipt) != state.Receipt || filepath.Ext(state.Receipt) != ".json" {
		return fmt.Errorf("receipt is invalid")
	}
	if !validHexOrEmpty(state.ArtifactLock.Integrity.SHA256, 64) || !validHexOrEmpty(state.ArtifactLock.Integrity.SHA512, 128) {
		return fmt.Errorf("artifact integrity is invalid")
	}
	if !validHexDigest(state.ImmutableConfigHash, 64) || !validHexOrEmpty(state.UpdateRequestHash, 64) {
		return fmt.Errorf("state fingerprints are invalid")
	}
	return nil
}

func validateStateAgainstCatalog(catalog Catalog, spec ProviderSpec, state State, arch string) error {
	if state.Provider != spec.ID || state.InstallFormat != spec.InstallFormat {
		return fmt.Errorf("PCVM-E2004 STATE_MISMATCH: provider installation format does not match the embedded catalog")
	}
	if state.Architecture != arch || installationID(state) != state.InstallID || state.Receipt != state.InstallID+".json" {
		return fmt.Errorf("PCVM-E2004 STATE_MISMATCH: state installation identity is not canonical")
	}
	kind, version, runtimeArch := parseRuntimePackIdentity(state.RuntimePackID)
	if spec.Runtime == "native" {
		if state.RuntimePackID != "" {
			return fmt.Errorf("PCVM-E2004 STATE_MISMATCH: native provider references a runtime pack")
		}
		return nil
	}
	if kind != spec.Runtime || version == "" || runtimeArch != arch || !contains(spec.RuntimePolicy.Allowed, version) || !catalog.HasRuntime(kind, version, arch) {
		return fmt.Errorf("PCVM-E2004 STATE_MISMATCH: runtime pack is not allowed by the embedded catalog")
	}
	return nil
}

func validHexOrEmpty(value string, length int) bool {
	if value == "" {
		return true
	}
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func lockArtifact(provider string, artifact Artifact) ArtifactLock {
	id := ""
	if provider == "paper-geyser" {
		seed := strings.Join([]string{artifact.Version, artifact.Build, artifact.SHA256,
			artifact.Metadata["geyser_version"], artifact.Metadata["geyser_build"], artifact.Metadata["geyser_sha256"],
			artifact.Metadata["floodgate_version"], artifact.Metadata["floodgate_build"], artifact.Metadata["floodgate_sha256"]}, "\x00")
		digest := sha256.Sum256([]byte(seed))
		id = "paper-geyser:" + hex.EncodeToString(digest[:16])
	}
	if id != "" {
		// Composite Paper/Geyser/Floodgate identity is already namespaced.
	} else if imageID := artifact.Metadata["vm_image_id"]; imageID != "" {
		id = "vm:" + imageID
	} else if project := artifact.Metadata["modrinth_project_id"]; project != "" {
		loader := artifact.Metadata["modrinth_loader"]
		if loader == "" {
			loader = "unknown"
		}
		id = "modrinth:" + project + ":" + loader
	} else if commit := artifact.Metadata["source_commit"]; commit != "" {
		id = "git:" + commit
	} else {
		sum := artifact.SHA512
		if sum == "" {
			sum = artifact.SHA256
		}
		if sum == "" {
			sum = artifact.SHA1
		}
		seed := provider + "\x00" + artifact.Version + "\x00" + artifact.Build + "\x00" + sum
		digest := sha256.Sum256([]byte(seed))
		id = provider + ":" + hex.EncodeToString(digest[:16])
	}
	revision := artifact.Metadata["modrinth_order_revision"]
	if revision == "" {
		revision = artifact.Metadata["buildid"]
	}
	if revision == "" {
		revision = artifact.Metadata["source_commit"]
	}
	return ArtifactLock{ID: id, Version: artifact.Version, Build: artifact.Build, Revision: revision,
		Integrity: ArtifactIntegrity{SHA256: strings.ToLower(artifact.SHA256), SHA512: strings.ToLower(artifact.SHA512)}}
}

func runtimePackIdentity(kind, version, arch string) string {
	if kind == "" || kind == "native" || version == "" {
		return ""
	}
	return kind + "/" + version + "/" + arch
}

func parseRuntimePackIdentity(id string) (kind, version, arch string) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func installationID(state State) string {
	seed := strings.Join([]string{state.Provider, state.Architecture, fmt.Sprint(state.InstallFormat), state.ArtifactLock.ID,
		state.ArtifactLock.Version, state.ArtifactLock.Build, state.ArtifactLock.Revision}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:16])
}

func hashToken(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func loadPending(control string) (*PendingSwitch, error) {
	var pending PendingSwitch
	err := readJSON(filepath.Join(control, "pending-switch.json"), &pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pending.Schema != PendingSwitchSchema {
		return nil, fmt.Errorf("unsupported pending reset schema %d", pending.Schema)
	}
	return &pending, nil
}

func savePending(control string, pending PendingSwitch) error {
	pending.Schema = PendingSwitchSchema
	return writeJSONAtomic(filepath.Join(control, "pending-switch.json"), pending)
}

func SaveInstallReceipt(control string, receipt InstallReceipt) (string, error) {
	receipt.Schema = InstallReceiptSchema
	if receipt.ID == "" || receipt.Provider == "" || receipt.InstallFormat < 1 || receipt.ReleaseID == "" {
		return "", fmt.Errorf("install receipt is incomplete")
	}
	name := receipt.ID + ".json"
	path := filepath.Join(control, "receipts", name)
	if err := writeJSONAtomic(path, receipt); err != nil {
		return "", err
	}
	return name, nil
}

func LoadInstallReceipt(control, name string) (InstallReceipt, error) {
	var receipt InstallReceipt
	if filepath.Base(name) != name || filepath.Ext(name) != ".json" {
		return receipt, fmt.Errorf("receipt path is invalid")
	}
	data, err := os.ReadFile(filepath.Join(control, "receipts", name))
	if err != nil {
		return receipt, err
	}
	if err := decodeStrictJSON(data, &receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != InstallReceiptSchema || receipt.ID+".json" != name {
		return receipt, fmt.Errorf("receipt identity is invalid")
	}
	return receipt, nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func newStateFromInstall(spec ProviderSpec, req Request, installed Resolved, arch string, now time.Time) State {
	format := spec.InstallFormat
	if format < 1 {
		format = 1
	}
	state := State{
		Schema: StateSchema, Provider: spec.ID, Selector: Selector{Version: envLatest(req.Version), Build: envLatest(req.Build), Runtime: normalizeRuntimeSelector(req.RuntimeVersion)},
		ArtifactLock: lockArtifact(spec.ID, installed.Artifact), RuntimePackID: runtimePackIdentity(installed.RuntimeKind, installed.RuntimeVersion, arch),
		Architecture: arch, InstallFormat: format, ImmutableConfigHash: immutableConfigFingerprint(spec, req),
		UpdateRequestHash: hashToken(req.UpdateRequest), InstalledAt: now, UpdatedAt: now,
	}
	normalizeStateV4(&state)
	return state
}

func immutableConfigFingerprint(spec ProviderSpec, req Request) string {
	values := []string{spec.ID}
	switch spec.Installer {
	case "qemu-vm":
		values = append(values, req.VMHostname, fmt.Sprint(req.VMDiskGB), normalizeVMCompression(req.VMDiskCompression))
	case "node-app", "python-app", "generic-app":
		values = append(values, req.SourceMode)
		if req.SourceMode == "git" {
			values = append(values, req.GitURL, req.GitBranch)
		}
	case "modrinth":
		values = append(values, req.ModpackMode, req.ModpackProject, req.ModpackFile)
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}
