package pcvm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const diskPlannerReserveBytes int64 = 128 << 20

// DiskPlan is a conservative peak-space estimate. Available space remains an
// observation (Wings is the quota authority), while RequiredBytes accounts
// for both the installed tree and temporary downloads used during activation.
type DiskPlan struct {
	Source         string
	AvailableBytes int64
	RequiredBytes  int64
	InstallBytes   int64
	DownloadBytes  int64
	Known          bool
}

func planDisk(spec ProviderSpec, artifactBytes, runtimeBytes int64, fresh bool) DiskPlan {
	install := int64(spec.MinimumDisk) << 20
	download := nonNegative(artifactBytes) + nonNegative(runtimeBytes)
	required := download
	if fresh || effectiveRollbackMode(spec) == "staged" {
		required += install
	} else {
		// In-place installers cannot promise double-space rollback, but still
		// need room for a patch workspace. Use half the declared installed
		// footprint when upstream does not publish a download size.
		workspace := install / 2
		if required < workspace {
			required = workspace
		}
	}
	if required > 0 {
		required += diskPlannerReserveBytes
	}
	return DiskPlan{RequiredBytes: required, InstallBytes: install, DownloadBytes: download}
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (a *App) preflightDisk(ctx context.Context, spec ProviderSpec, resolved Resolved, fresh bool) error {
	artifactBytes := a.estimateArtifactBytes(ctx, resolved.Artifact)
	runtimeBytes := runtimePackDownloadSize(a.Catalog, resolved.RuntimeKind, resolved.RuntimeVersion, a.Config.Arch)
	plan := planDisk(diskPlanningSpec(spec, a.Config.Request), artifactBytes, runtimeBytes, fresh)
	available, err := platformDiskAvailable(a.Config.Home)
	if err != nil || available < 0 {
		a.Log.Printf("WARNING: DISK source=unknown required=%dMB install=%dMB download=%dMB", bytesToMiB(plan.RequiredBytes), bytesToMiB(plan.InstallBytes), bytesToMiB(plan.DownloadBytes))
		return nil
	}
	plan.Source, plan.AvailableBytes, plan.Known = "filesystem", available, true
	a.Log.Printf("DISK source=%s available=%dMB required=%dMB install=%dMB download=%dMB", plan.Source, bytesToMiB(available), bytesToMiB(plan.RequiredBytes), bytesToMiB(plan.InstallBytes), bytesToMiB(plan.DownloadBytes))
	if available < plan.RequiredBytes {
		return fmt.Errorf("PCVM-E4007 STORAGE_PRECHECK: provider %s needs an estimated %d MB free at peak, but only %d MB is available", spec.ID, bytesToMiB(plan.RequiredBytes), bytesToMiB(available))
	}
	return nil
}

func diskPlanningSpec(spec ProviderSpec, req Request) ProviderSpec {
	// Public Git is cloned and built in a complete immutable release before
	// activation, even though upload mode for the same provider is in-place.
	// Account for that second tree in peak space before the source is cloned.
	if isSourceProvider(spec) && req.SourceMode == "git" {
		spec.RollbackMode = "staged"
	}
	return spec
}

func (a *App) estimateArtifactBytes(ctx context.Context, artifact Artifact) int64 {
	if artifact.Metadata != nil {
		if raw := strings.TrimSpace(artifact.Metadata["size_bytes"]); raw != "" {
			if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
				return value
			}
		}
	}
	if artifact.Kind == "mrpack-upload" {
		clean, err := cleanRelativeEntry(artifact.FileName)
		if err == nil {
			if info, statErr := os.Stat(filepath.Join(a.Config.Home, clean)); statErr == nil && info.Mode().IsRegular() {
				return info.Size()
			}
		}
	}
	if artifact.URL == "" || a.HTTP == nil || a.HTTP.Client == nil {
		return 0
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, artifact.URL, nil)
	if err != nil || a.HTTP.validate(artifact.URL) != nil {
		return 0
	}
	request.Header.Set("User-Agent", "PCVM/2")
	response, err := a.HTTP.Client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 || response.ContentLength < 0 {
		return 0
	}
	return response.ContentLength
}

func runtimePackDownloadSize(catalog Catalog, kind, version, architecture string) int64 {
	if kind == "" || kind == "native" {
		return 0
	}
	for _, pack := range catalog.RuntimePacks {
		if pack.Kind == kind && pack.Version == version && pack.Architecture == architecture {
			return pack.Size
		}
	}
	return 0
}

func bytesToMiB(value int64) int64 {
	if value <= 0 {
		return 0
	}
	return (value + (1 << 20) - 1) >> 20
}
