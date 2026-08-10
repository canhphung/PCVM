package pcvm

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type StatusReport struct {
	Schema       int               `json:"schema"`
	Installed    bool              `json:"installed"`
	Provider     string            `json:"provider,omitempty"`
	InstallID    string            `json:"install_id,omitempty"`
	Selector     Selector          `json:"selector,omitempty"`
	Artifact     ArtifactLock      `json:"artifact,omitempty"`
	RuntimePack  string            `json:"runtime_pack_id,omitempty"`
	Architecture string            `json:"architecture,omitempty"`
	Receipt      string            `json:"receipt,omitempty"`
	Verified     bool              `json:"verified"`
	Operation    *OperationJournal `json:"operation,omitempty"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type DoctorReport struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

type CacheReport struct {
	Exists     bool             `json:"exists"`
	Files      int              `json:"files"`
	Bytes      int64            `json:"bytes"`
	Categories map[string]int64 `json:"categories"`
}

func InspectStatus(cfg Config, catalog Catalog) (StatusReport, error) {
	report := StatusReport{Schema: StateSchema}
	if err := DetectLegacyControl(cfg.Home, cfg.Control); err != nil {
		return report, err
	}
	state, err := LoadState(cfg.Control)
	if err != nil {
		return report, err
	}
	report.Operation, err = LoadOperation(cfg.Control)
	if err != nil {
		return report, err
	}
	if state == nil {
		return report, nil
	}
	spec, ok := catalog.Provider(state.Provider)
	if !ok {
		return report, fmt.Errorf("PCVM-E2003 UNKNOWN_STATE_PROVIDER: %q", state.Provider)
	}
	if err := validateStateAgainstCatalog(catalog, spec, *state, cfg.Arch); err != nil {
		return report, err
	}
	report.Installed = true
	report.Provider = state.Provider
	report.InstallID = state.InstallID
	report.Selector = state.Selector
	report.Artifact = state.ArtifactLock
	report.RuntimePack = state.RuntimePackID
	report.Architecture = state.Architecture
	report.Receipt = state.Receipt
	receipt, err := LoadInstallReceipt(cfg.Control, state.Receipt)
	if err != nil {
		return report, fmt.Errorf("load install receipt: %w", err)
	}
	if err := verifyInstallReceipt(cfg.Home, *state, receipt); err != nil {
		return report, err
	}
	if receipt.RollbackMode != effectiveRollbackModeForState(spec, *state) {
		return report, fmt.Errorf("PCVM-E2004 RECEIPT_MISMATCH: receipt rollback mode does not match the compiled provider")
	}
	app := NewApp(cfg, catalog, strings.NewReader(""), io.Discard, io.Discard)
	launch, err := app.rebuildLaunchState(context.Background(), spec, *state)
	if err != nil {
		return report, fmt.Errorf("rebuild trusted launch plan: %w", err)
	}
	if err := verifyLaunchReceiptCompleteness(cfg.Home, receipt, launch); err != nil {
		return report, err
	}
	report.Verified = true
	return report, nil
}

func RunDoctor(cfg Config, catalog Catalog) DoctorReport {
	report := DoctorReport{OK: true}
	add := func(name string, err error, success string) {
		check := DoctorCheck{Name: name, Status: "ok", Message: success}
		if err != nil {
			check.Status, check.Message, report.OK = "error", redactDiagnostic(err.Error()), false
		}
		report.Checks = append(report.Checks, check)
	}
	add("catalog", catalog.Validate(), fmt.Sprintf("schema=%d providers=%d", catalog.Schema, len(catalog.Providers)))
	_, statusErr := InspectStatus(cfg, catalog)
	add("installation", statusErr, "state and receipt verified")
	memory := readMemorySnapshotWith(cfg.Dependencies)
	memoryMessage := "limit=unknown"
	if memory.LimitMB > 0 {
		memoryMessage = fmt.Sprintf("source=%s limit=%dMB", memory.Source, memory.LimitMB)
	}
	add("memory", nil, memoryMessage)
	cache, cacheErr := InspectCache(cfg.Control)
	add("cache", cacheErr, fmt.Sprintf("files=%d bytes=%d", cache.Files, cache.Bytes))
	if info, err := os.Lstat(cfg.Home); err != nil {
		add("server-root", err, "")
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		add("server-root", fmt.Errorf("server root is not a real directory"), "")
	} else {
		add("server-root", nil, filepath.Clean(cfg.Home))
	}
	return report
}

func InspectCache(control string) (CacheReport, error) {
	report := CacheReport{Categories: map[string]int64{}}
	root, exists, err := realCacheRoot(control)
	if err != nil || !exists {
		return report, err
	}
	report.Exists = true
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		category := strings.Split(filepath.ToSlash(rel), "/")[0]
		report.Files++
		report.Bytes += info.Size()
		report.Categories[category] += info.Size()
		return nil
	})
	return report, err
}

func PruneCache(cfg Config, catalog Catalog, log *Logger) (CacheReport, error) {
	if err := cleanupConsumedInstallCache(cfg.Control); err != nil {
		return CacheReport{}, err
	}
	state, err := LoadState(cfg.Control)
	if err != nil {
		return CacheReport{}, err
	}
	if state != nil {
		spec, ok := catalog.Provider(state.Provider)
		if !ok {
			return CacheReport{}, fmt.Errorf("state references unknown provider %q", state.Provider)
		}
		manager := RuntimeManager{Catalog: catalog, Config: cfg, HTTP: NewHTTPClient(), Log: log}
		if err := manager.Prune(manager.runtimeRoot(spec.Runtime, state.RuntimeVersion)); err != nil {
			return CacheReport{}, err
		}
	}
	return InspectCache(cfg.Control)
}

func CreateSupportBundle(cfg Config, catalog Catalog, now time.Time) (string, error) {
	status, statusErr := InspectStatus(cfg, catalog)
	doctor := RunDoctor(cfg, catalog)
	cache, cacheErr := InspectCache(cfg.Control)
	providers := make([]map[string]any, 0, len(catalog.Providers))
	for _, provider := range catalog.Providers {
		providers = append(providers, map[string]any{"id": provider.ID, "support_tier": provider.SupportTier, "architectures": provider.Architectures})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i]["id"].(string) < providers[j]["id"].(string) })
	bundle := map[string]any{
		"generated_at": now.UTC(), "catalog_schema": catalog.Schema, "catalog_version": catalog.Version,
		"status": status, "doctor": doctor, "cache": cache, "providers": providers,
	}
	if statusErr != nil {
		bundle["status_error"] = redactDiagnostic(statusErr.Error())
	}
	if cacheErr != nil {
		bundle["cache_error"] = redactDiagnostic(cacheErr.Error())
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("pcvm-support-%s.zip", now.UTC().Format("20060102T150405Z"))
	path := filepath.Join(cfg.Home, name)
	tmp, err := os.CreateTemp(cfg.Home, ".pcvm-support-*.zip")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	archive := zip.NewWriter(tmp)
	entry, err := archive.Create("report.json")
	if err == nil {
		_, err = entry.Write(append(data, '\n'))
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return path, nil
}

func redactDiagnostic(value string) string {
	value = Redact(value)
	for _, marker := range []string{"PASSWORD=", "TOKEN=", "SECRET=", "STEAM_GSLT="} {
		if strings.Contains(strings.ToUpper(value), marker) {
			return "[REDACTED DIAGNOSTIC]"
		}
	}
	return value
}

func statusExitError(report StatusReport, err error) error {
	if err == nil {
		return nil
	}
	var legacy *LegacyStateError
	if errors.As(err, &legacy) {
		return legacy
	}
	return err
}
