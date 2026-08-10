package pcvm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func canonicalJavaStateFixture(t *testing.T) (Catalog, ProviderSpec, State) {
	t.Helper()
	spec := ProviderSpec{
		ID: "paper", Family: "paper", Runtime: "java", Installer: "jar", InstallFormat: 3,
		RuntimePolicy: RuntimePolicySpec{Default: "auto", Allowed: []string{"21"}},
	}
	resolved := Resolved{
		Artifact:    Artifact{Version: "1.21.4", Build: "100", SHA256: strings.Repeat("a", 64)},
		RuntimeKind: "java", RuntimeVersion: "21",
	}
	state := newStateFromInstall(spec, Request{Version: "1.21.4", Build: "100", RuntimeVersion: "21"}, resolved, "amd64", time.Unix(1, 0).UTC())
	catalog := Catalog{RuntimePacks: []RuntimePackSpec{{ID: "java/21/amd64", Kind: "java", Version: "21", Architecture: "amd64"}}}
	return catalog, spec, state
}

func TestValidateStateAgainstCatalogMatrix(t *testing.T) {
	catalog, spec, valid := canonicalJavaStateFixture(t)
	if err := validateStateAgainstCatalog(catalog, spec, valid, "amd64"); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	tests := map[string]func(*Catalog, *ProviderSpec, *State, *string){
		"provider":        func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.Provider = "purpur" },
		"format":          func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.InstallFormat++ },
		"architecture":    func(_ *Catalog, _ *ProviderSpec, _ *State, arch *string) { *arch = "arm64" },
		"install id":      func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.InstallID = strings.Repeat("0", 32) },
		"receipt":         func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.Receipt = "other.json" },
		"runtime kind":    func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.RuntimePackID = "node/21/amd64" },
		"runtime version": func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.RuntimePackID = "java/17/amd64" },
		"runtime arch":    func(_ *Catalog, _ *ProviderSpec, state *State, _ *string) { state.RuntimePackID = "java/21/arm64" },
		"runtime missing": func(catalog *Catalog, _ *ProviderSpec, _ *State, _ *string) { catalog.RuntimePacks = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			copyCatalog, copySpec, copyState, arch := catalog, spec, valid, "amd64"
			mutate(&copyCatalog, &copySpec, &copyState, &arch)
			if err := validateStateAgainstCatalog(copyCatalog, copySpec, copyState, arch); err == nil {
				t.Fatal("invalid state was accepted")
			}
		})
	}

	nativeSpec := spec
	nativeSpec.Runtime = "native"
	native := valid
	native.Provider, nativeSpec.ID = "nginx", "nginx"
	native.RuntimePackID = ""
	native.InstallID = installationID(native)
	native.Receipt = native.InstallID + ".json"
	if err := validateStateAgainstCatalog(catalog, nativeSpec, native, "amd64"); err != nil {
		t.Fatalf("valid native state rejected: %v", err)
	}
	native.RuntimePackID = "java/21/amd64"
	if err := validateStateAgainstCatalog(catalog, nativeSpec, native, "amd64"); err == nil {
		t.Fatal("native state with runtime was accepted")
	}
}

func TestPendingPersistenceAndBindingMatrix(t *testing.T) {
	control := t.TempDir()
	if pending, err := loadPending(control); err != nil || pending != nil {
		t.Fatalf("missing pending result = %+v, %v", pending, err)
	}
	now := time.Unix(100, 0).UTC()
	pending, err := NewPending(State{Provider: "paper", InstallID: "install", ResolvedVersion: "1"}, ProviderSpec{ID: "node-bot"}, "2", "family", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := savePending(control, pending); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadPending(control)
	if err != nil || loaded.Nonce != pending.Nonce || loaded.Schema != PendingSwitchSchema {
		t.Fatalf("pending did not round-trip: %+v, %v", loaded, err)
	}
	confirm := "DELETE:" + loaded.Nonce
	if err := ValidateResetBinding(*loaded, confirm, loaded.SourceHash, loaded.TargetHash, now); err != nil {
		t.Fatal(err)
	}
	for name, hashes := range map[string][2]string{
		"source": {strings.Repeat("0", 64), loaded.TargetHash},
		"target": {loaded.SourceHash, strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateResetBinding(*loaded, confirm, hashes[0], hashes[1], now); err == nil {
				t.Fatal("mismatched reset binding was accepted")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(control, "pending-switch.json"), []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPending(control); err == nil {
		t.Fatal("obsolete pending schema was accepted")
	}
}

func TestReceiptValidationFailureMatrix(t *testing.T) {
	home := t.TempDir()
	_, spec, state := canonicalJavaStateFixture(t)
	releaseID := releaseIDFor(spec.ID, state.ArtifactLock)
	release := filepath.Join(home, ".pcvm", "releases", spec.ID, releaseID)
	managed := filepath.Join(release, "payload", "server.jar")
	if err := os.MkdirAll(filepath.Dir(managed), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("jar"), 0o640); err != nil {
		t.Fatal(err)
	}
	treeDigest, err := releaseTreeDigest(filepath.Join(release, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := releaseMetadata{Schema: releaseMetadataSchema, Provider: spec.ID, ReleaseID: releaseID, Artifact: state.ArtifactLock, TreeSHA256: treeDigest, CreatedAt: time.Unix(1, 0).UTC()}
	if err := writeJSONAtomic(filepath.Join(release, ".pcvm-release.json"), metadata); err != nil {
		t.Fatal(err)
	}
	resolved := Resolved{Artifact: Artifact{Version: state.ArtifactLock.Version, Build: state.ArtifactLock.Build, SHA256: state.ArtifactLock.Integrity.SHA256}, RuntimeKind: "java", RuntimeVersion: "21", Command: []string{"/java", "-jar", managed}}
	receipt, err := buildInstallReceipt(home, spec, state, resolved, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallReceipt(home, state, receipt); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	mutations := map[string]func(*InstallReceipt){
		"schema":   func(value *InstallReceipt) { value.Schema-- },
		"provider": func(value *InstallReceipt) { value.Provider = "purpur" },
		"rollback": func(value *InstallReceipt) { value.RollbackMode = "magic" },
		"root":     func(value *InstallReceipt) { value.RootSHA256 = strings.Repeat("0", 64) },
		"absolute path": func(value *InstallReceipt) {
			value.Files[0].Path = managed
			value.RootSHA256 = receiptRoot(value.Files)
		},
		"backslash": func(value *InstallReceipt) {
			value.Files[0].Path = `.pcvm\\server.jar`
			value.RootSHA256 = receiptRoot(value.Files)
		},
		"bad mode": func(value *InstallReceipt) { value.Files[0].Mode = 0o600; value.RootSHA256 = receiptRoot(value.Files) },
		"bad digest": func(value *InstallReceipt) {
			value.Files[0].SHA256 = strings.Repeat("0", 64)
			value.RootSHA256 = receiptRoot(value.Files)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copyReceipt := receipt
			copyReceipt.Files = append([]ReceiptFile(nil), receipt.Files...)
			mutate(&copyReceipt)
			if err := verifyInstallReceipt(home, state, copyReceipt); err == nil {
				t.Fatal("invalid receipt was accepted")
			}
		})
	}
}

func TestReconcileActionMatrix(t *testing.T) {
	state := reconcileState()
	spec := ProviderSpec{ID: "paper", Family: "paper", Installer: "jar", InstallFormat: 1, VersionDomain: "minecraft"}
	base := Request{Version: "1.21.1", Build: "10", RuntimeVersion: "21"}
	if got := Reconcile(nil, spec, base, nil); got.Kind != ActionInstall || !got.RequiresResolve {
		t.Fatalf("fresh unresolved = %+v", got)
	}
	resolved := Resolved{Artifact: Artifact{Version: "1.21.1", Build: "10"}}
	if got := Reconcile(nil, spec, base, &resolved); got.Kind != ActionInstall || got.RequiresResolve {
		t.Fatalf("fresh resolved = %+v", got)
	}
	auto := base
	auto.AutoUpdate = true
	if got := Reconcile(state, spec, auto, nil); got.Kind != ActionUpdate || got.Reason != "AUTO_UPDATE enabled" {
		t.Fatalf("auto update = %+v", got)
	}
	incompatible := spec
	incompatible.ID, incompatible.Family = "node-bot", "node"
	if got := Reconcile(state, incompatible, base, &resolved); got.Kind != ActionReset || got.Reason != "incompatible provider family" {
		t.Fatalf("incompatible = %+v", got)
	}
	buildDowngrade := Resolved{Artifact: Artifact{Version: state.ResolvedVersion, Build: "9"}}
	if got := Reconcile(state, spec, base, &buildDowngrade); got.Kind != ActionReset || got.Reason != "downgrade requires reset" {
		t.Fatalf("build downgrade = %+v", got)
	}
	if got := CompareVersionsForDomain("opaque", "b", "a"); got != 1 {
		t.Fatalf("opaque comparison = %d", got)
	}
	if got := CompareVersionsForDomain("none", "same", "same"); got != 0 {
		t.Fatalf("opaque equality = %d", got)
	}
}

func TestStateAndReceiptStrictValidationMatrix(t *testing.T) {
	_, _, valid := canonicalJavaStateFixture(t)
	if err := validateStateV4(valid); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	mutations := map[string]func(*State){
		"schema":       func(state *State) { state.Schema = 99 },
		"provider":     func(state *State) { state.Provider = "BAD" },
		"empty field":  func(state *State) { state.ArtifactLock.Build = "" },
		"newline":      func(state *State) { state.Selector.Version = "bad\nvalue" },
		"architecture": func(state *State) { state.Architecture = "386" },
		"format":       func(state *State) { state.InstallFormat = 0 },
		"receipt path": func(state *State) { state.Receipt = "../receipt.json" },
		"integrity":    func(state *State) { state.ArtifactLock.Integrity.SHA256 = "zz" },
		"fingerprint":  func(state *State) { state.ImmutableConfigHash = "short" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			state := valid
			mutate(&state)
			if err := validateStateV4(state); err == nil {
				t.Fatal("invalid state was accepted")
			}
		})
	}

	control := t.TempDir()
	if _, err := SaveInstallReceipt(control, InstallReceipt{}); err == nil {
		t.Fatal("incomplete receipt was saved")
	}
	receipt := InstallReceipt{ID: valid.InstallID, Provider: valid.Provider, InstallFormat: valid.InstallFormat, ReleaseID: valid.ArtifactLock.ID}
	name, err := SaveInstallReceipt(control, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstallReceipt(control, "../"+name); err == nil {
		t.Fatal("receipt traversal was accepted")
	}
	if err := os.WriteFile(filepath.Join(control, "receipts", name), []byte(`{"schema":3,"id":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstallReceipt(control, name); err == nil {
		t.Fatal("receipt identity mismatch was accepted")
	}
	if err := decodeStrictJSON([]byte(`{"value":1,"unknown":2}`), &struct {
		Value int `json:"value"`
	}{}); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
	if err := decodeStrictJSON([]byte(`{"value":1} {"value":2}`), &struct {
		Value int `json:"value"`
	}{}); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
}

func TestLoadCatalogRuntimeManifestEnvelopeMatrix(t *testing.T) {
	if _, err := LoadCatalog(nil); err != nil {
		t.Fatalf("embedded catalog rejected: %v", err)
	}
	pack := RuntimePackSpec{
		ID: "java/21/amd64", Kind: "java", Version: "21", UpstreamVersion: "jdk-21.0.12+8", Architecture: "amd64",
		URL: "https://example.com/java.tar.gz", SHA256: strings.Repeat("a", 64), TreeSHA256: strings.Repeat("b", 64),
		Executable: "bin/java", Archive: "tar.gz", Size: 1,
	}
	valid := RuntimeManifest{Schema: RuntimeManifestSchema, Release: "2.0.0", Compatibility: "pcvm>=2.0.0", Packs: []RuntimePackSpec{pack}}
	encode := func(value RuntimeManifest) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if catalog, err := LoadCatalog(encode(valid)); err != nil || len(catalog.RuntimePacks) != 1 {
		t.Fatalf("valid manifest rejected: packs=%d err=%v", len(catalog.RuntimePacks), err)
	}
	for name, mutate := range map[string]func(*RuntimeManifest){
		"schema":           func(value *RuntimeManifest) { value.Schema++ },
		"release":          func(value *RuntimeManifest) { value.Release = "2.0.1" },
		"compatibility":    func(value *RuntimeManifest) { value.Compatibility = "any" },
		"pack":             func(value *RuntimeManifest) { value.Packs[0].TreeSHA256 = "" },
		"upstream-empty":   func(value *RuntimeManifest) { value.Packs[0].UpstreamVersion = "" },
		"upstream-control": func(value *RuntimeManifest) { value.Packs[0].UpstreamVersion = "21\nmalformed" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Packs = append([]RuntimePackSpec(nil), valid.Packs...)
			mutate(&candidate)
			if _, err := LoadCatalog(encode(candidate)); err == nil {
				t.Fatal("invalid runtime manifest was accepted")
			}
		})
	}
	if _, err := LoadCatalog([]byte(`[]`)); err == nil {
		t.Fatal("legacy runtime array was accepted")
	}
}

func TestResetRootAndQuarantineCommitContracts(t *testing.T) {
	home := t.TempDir()
	if got, err := validateResetRoot(home, home); err != nil || got != filepath.Clean(home) {
		t.Fatalf("valid reset root = %q, %v", got, err)
	}
	if _, err := validateResetRoot(home, filepath.Join(home, "other")); err == nil {
		t.Fatal("unexpected reset root was accepted")
	}
	if _, err := validateResetRoot(string(filepath.Separator), string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root was accepted")
	}

	control := filepath.Join(home, ".pcvm")
	if err := os.MkdirAll(control, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "world.dat"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}
	quarantine, err := beginQuarantineAt(home, control, "commit-contract", home)
	if err != nil {
		t.Fatal(err)
	}
	if relative := quarantine.Relative(); relative == "" || !strings.Contains(relative, "quarantine") {
		t.Fatalf("unexpected quarantine relative path %q", relative)
	}
	if err := quarantine.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(quarantine.Root); !os.IsNotExist(err) {
		t.Fatalf("committed quarantine still exists: %v", err)
	}
}
