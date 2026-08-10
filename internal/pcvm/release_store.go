package pcvm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	releaseMetadataSchema = 1
	releaseIndexSchema    = 1
)

type releaseMetadata struct {
	Schema     int          `json:"schema"`
	Provider   string       `json:"provider"`
	ReleaseID  string       `json:"release_id"`
	Artifact   ArtifactLock `json:"artifact"`
	TreeSHA256 string       `json:"tree_sha256"`
	CreatedAt  time.Time    `json:"created_at"`
}

type releaseIndex struct {
	Schema   int    `json:"schema"`
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
}

func releaseStoreSupports(spec ProviderSpec) bool {
	if effectiveRollbackMode(spec) != "staged" {
		return false
	}
	switch spec.Installer {
	case "jar", "phar", "zip", "java-installer", "quilt", "paper-geyser", "modrinth",
		"endstone", "openmp", "mtasa", "terraria", "tmodloader", "tshock", "factorio", "code-server":
		return true
	default:
		return false
	}
}

func releaseStoreSupportsResolved(spec ProviderSpec, resolved Resolved) bool {
	if releaseStoreSupports(spec) {
		return true
	}
	return isSourceInstaller(spec) && resolved.Artifact.Metadata["source_commit"] != ""
}

func releaseStoreSupportsState(spec ProviderSpec, state State) bool {
	if releaseStoreSupports(spec) {
		return true
	}
	return isSourceInstaller(spec) && strings.HasPrefix(state.ArtifactLock.ID, "git:")
}

func isSourceInstaller(spec ProviderSpec) bool {
	return spec.Installer == "node-app" || spec.Installer == "python-app" || spec.Installer == "generic-app"
}

func releaseIDFor(provider string, lock ArtifactLock) string {
	hash := sha256.New()
	for _, value := range []string{provider, lock.ID, lock.Version, lock.Build, lock.Revision, lock.Integrity.SHA256, lock.Integrity.SHA512} {
		_, _ = io.WriteString(hash, value)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func releaseProviderRoot(control, provider string) string {
	return filepath.Join(control, "releases", provider)
}

func releaseRoot(control, provider, releaseID string) string {
	return filepath.Join(releaseProviderRoot(control, provider), releaseID)
}

func releasePayloadRoot(control, provider, releaseID string) string {
	return filepath.Join(releaseRoot(control, provider, releaseID), "payload")
}

func activateStagedRelease(ic InstallContext, spec ProviderSpec, resolved Resolved, now time.Time) (Resolved, error) {
	if !releaseStoreSupportsResolved(spec, resolved) {
		return resolved, nil
	}
	// Modrinth is a staged overlay rather than a relocatable payload. Its
	// candidate/backup transaction is prepared by the installer and finalized
	// only after canonical state activation.
	if spec.Installer == "modrinth" {
		return resolved, nil
	}
	lock := lockArtifact(spec.ID, resolved.Artifact)
	releaseID := releaseIDFor(spec.ID, lock)
	if isSourceInstaller(spec) && filepath.Clean(resolved.WorkDir) == releasePayloadRoot(ic.ControlDir, spec.ID, releaseID) {
		if err := validateRelease(releaseRoot(ic.ControlDir, spec.ID, releaseID), spec.ID, releaseID, lock); err != nil {
			return resolved, err
		}
		return resolved, nil
	}
	providerRoot := releaseProviderRoot(ic.ControlDir, spec.ID)
	if err := secureMkdirAll(ic.ControlDir, providerRoot, 0o750); err != nil {
		return resolved, err
	}
	stage, err := os.MkdirTemp(providerRoot, ".stage-")
	if err != nil {
		return resolved, fmt.Errorf("create release stage: %w", err)
	}
	stageLive := true
	defer func() {
		if stageLive {
			_ = os.RemoveAll(stage)
		}
	}()
	stagedPayload := filepath.Join(stage, "payload")
	fileRelease := spec.Installer == "jar" || spec.Installer == "phar" || spec.Installer == "paper-geyser"
	legacyRoot := ""
	legacyFile := ""
	if fileRelease {
		legacyFile, err = stagedCommandFile(ic.ControlDir, spec, resolved.Command)
		if err != nil {
			return resolved, err
		}
		if err := os.MkdirAll(stagedPayload, 0o750); err != nil {
			return resolved, err
		}
		if err := copyFile(legacyFile, filepath.Join(stagedPayload, filepath.Base(legacyFile)), 0o640); err != nil {
			return resolved, fmt.Errorf("stage release file: %w", err)
		}
	} else {
		legacyRoot, err = stagedReleaseTreeSource(ic.ControlDir, spec, resolved.Command, resolved.Environment, resolved.WorkDir)
		if err != nil {
			return resolved, err
		}
		info, err := os.Lstat(legacyRoot)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return resolved, fmt.Errorf("staged release source is not a real directory: %w", err)
		}
		if err := os.Rename(legacyRoot, stagedPayload); err != nil {
			return resolved, fmt.Errorf("move provider tree into release stage: %w", err)
		}
	}
	treeDigest, err := releaseTreeDigest(stagedPayload)
	if err != nil {
		return resolved, fmt.Errorf("hash staged release tree: %w", err)
	}
	metadata := releaseMetadata{Schema: releaseMetadataSchema, Provider: spec.ID, ReleaseID: releaseID, Artifact: lock, TreeSHA256: treeDigest, CreatedAt: now.UTC()}
	if err := writeJSONAtomic(filepath.Join(stage, ".pcvm-release.json"), metadata); err != nil {
		return resolved, err
	}
	final := releaseRoot(ic.ControlDir, spec.ID, releaseID)
	if _, err := os.Lstat(final); err == nil {
		if err := validateRelease(final, spec.ID, releaseID, lock); err != nil {
			return resolved, fmt.Errorf("existing release conflicts with staged identity: %w", err)
		}
		equal, err := releaseTreesEqual(stagedPayload, filepath.Join(final, "payload"))
		if err != nil {
			return resolved, err
		}
		if !equal {
			return resolved, fmt.Errorf("existing release %s content differs from freshly verified install", releaseID)
		}
		if err := os.RemoveAll(stage); err != nil {
			return resolved, err
		}
		stageLive = false
	} else if !os.IsNotExist(err) {
		return resolved, err
	} else {
		if err := os.Rename(stage, final); err != nil {
			return resolved, fmt.Errorf("atomically activate release: %w", err)
		}
		stageLive = false
	}
	finalPayload := filepath.Join(final, "payload")
	if fileRelease {
		finalFile := filepath.Join(finalPayload, filepath.Base(legacyFile))
		resolved.Command = replaceExactArg(resolved.Command, legacyFile, finalFile)
		if err := os.Remove(legacyFile); err != nil && !os.IsNotExist(err) {
			return resolved, fmt.Errorf("remove legacy release file: %w", err)
		}
	} else {
		resolved.Command = replacePathPrefixArgs(resolved.Command, legacyRoot, finalPayload)
		resolved.Environment = replaceEnvironmentPathPrefix(resolved.Environment, legacyRoot, finalPayload)
		resolved.WorkDir = replacePathPrefix(resolved.WorkDir, legacyRoot, finalPayload)
	}
	return resolved, nil
}

func stagedCommandFile(control string, spec ProviderSpec, command []string) (string, error) {
	managed := filepath.Join(control, "managed", spec.ID)
	for _, argument := range command {
		if !filepath.IsAbs(argument) {
			continue
		}
		clean := filepath.Clean(argument)
		if pathWithin(managed, clean) && clean != managed {
			info, err := os.Lstat(clean)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return "", fmt.Errorf("managed release executable is not a regular file")
			}
			return clean, nil
		}
	}
	return "", fmt.Errorf("staged provider command has no managed release file")
}

func replaceExactArg(command []string, oldPath, newPath string) []string {
	out := append([]string(nil), command...)
	oldPath = filepath.Clean(oldPath)
	for i, argument := range out {
		if filepath.IsAbs(argument) && filepath.Clean(argument) == oldPath {
			out[i] = newPath
		}
	}
	return out
}

func replacePathPrefixArgs(command []string, oldRoot, newRoot string) []string {
	out := append([]string(nil), command...)
	oldRoot = filepath.Clean(oldRoot)
	for i, argument := range out {
		if !filepath.IsAbs(argument) {
			continue
		}
		clean := filepath.Clean(argument)
		if clean == oldRoot || pathWithin(oldRoot, clean) {
			relative, err := filepath.Rel(oldRoot, clean)
			if err == nil {
				out[i] = filepath.Join(newRoot, relative)
			}
		}
	}
	return out
}

func rewriteStagedLaunchToRelease(control string, spec ProviderSpec, state State, launch *LaunchState) error {
	if !releaseStoreSupportsState(spec, state) {
		return nil
	}
	if spec.Installer == "modrinth" {
		return nil
	}
	releaseID := releaseIDFor(spec.ID, state.ArtifactLock)
	root := releaseRoot(control, spec.ID, releaseID)
	if err := validateRelease(root, spec.ID, releaseID, state.ArtifactLock); err != nil {
		return fmt.Errorf("PCVM-E2006 RELEASE_MISSING: %w", err)
	}
	payload := filepath.Join(root, "payload")
	if spec.Installer == "jar" || spec.Installer == "phar" || spec.Installer == "paper-geyser" {
		legacy, err := stagedCommandFileFromLaunch(control, spec, *launch)
		if err != nil {
			return err
		}
		launch.Command = replaceExactArg(launch.Command, legacy, filepath.Join(payload, filepath.Base(legacy)))
		return nil
	}
	legacyRoot, err := stagedReleaseTreeSource(control, spec, launch.Command, launch.Environment, launch.WorkingDirectory)
	if err != nil {
		return err
	}
	launch.Command = replacePathPrefixArgs(launch.Command, legacyRoot, payload)
	launch.Environment = replaceEnvironmentPathPrefix(launch.Environment, legacyRoot, payload)
	launch.WorkingDirectory = replacePathPrefix(launch.WorkingDirectory, legacyRoot, payload)
	return nil
}

func stagedReleaseTreeSource(control string, spec ProviderSpec, command, environment []string, workDir string) (string, error) {
	managed := filepath.Join(control, "managed", spec.ID)
	workDir = filepath.Clean(workDir)
	if workDir != managed && pathWithin(managed, workDir) {
		return workDir, nil
	}
	candidates := append([]string(nil), command...)
	for _, item := range environment {
		if _, value, ok := strings.Cut(item, "="); ok {
			candidates = append(candidates, value)
		}
	}
	for _, argument := range candidates {
		if !filepath.IsAbs(argument) {
			continue
		}
		candidate := filepath.Clean(argument)
		if !pathWithin(managed, candidate) || candidate == managed {
			continue
		}
		relative, err := filepath.Rel(managed, candidate)
		if err != nil {
			continue
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) > 1 && parts[0] != "" && parts[0] != "." && parts[0] != ".." {
			return filepath.Join(managed, parts[0]), nil
		}
	}
	return "", fmt.Errorf("staged release command has no managed candidate tree")
}

func replacePathPrefix(value, oldRoot, newRoot string) string {
	if !filepath.IsAbs(value) {
		return value
	}
	clean := filepath.Clean(value)
	if clean != oldRoot && !pathWithin(oldRoot, clean) {
		return value
	}
	relative, err := filepath.Rel(oldRoot, clean)
	if err != nil {
		return value
	}
	return filepath.Join(newRoot, relative)
}

func replaceEnvironmentPathPrefix(environment []string, oldRoot, newRoot string) []string {
	out := append([]string(nil), environment...)
	for index, item := range out {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		rewritten := replacePathPrefix(value, oldRoot, newRoot)
		if rewritten != value {
			out[index] = key + "=" + rewritten
		}
	}
	return out
}

func stagedCommandFileFromLaunch(control string, spec ProviderSpec, launch LaunchState) (string, error) {
	managed := filepath.Join(control, "managed", spec.ID)
	for _, argument := range launch.Command {
		if filepath.IsAbs(argument) && pathWithin(managed, filepath.Clean(argument)) {
			return filepath.Clean(argument), nil
		}
	}
	return "", fmt.Errorf("trusted staged launch has no managed release file")
}

func validateRelease(root, provider, releaseID string, lock ArtifactLock) error {
	if !validHexDigest(releaseID, 64) || filepath.Base(root) != releaseID {
		return fmt.Errorf("release identity is invalid")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("release root is not a real directory")
	}
	var metadata releaseMetadata
	if err := readJSON(filepath.Join(root, ".pcvm-release.json"), &metadata); err != nil {
		return err
	}
	if metadata.Schema != releaseMetadataSchema || metadata.Provider != provider || metadata.ReleaseID != releaseID || metadata.Artifact != lock || !validHexDigest(metadata.TreeSHA256, 64) {
		return fmt.Errorf("release metadata does not match canonical artifact")
	}
	payload := filepath.Join(root, "payload")
	if info, err := os.Lstat(payload); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("release payload is not a real directory")
	}
	actualTree, err := releaseTreeDigest(payload)
	if err != nil {
		return err
	}
	if !constantTimeHexEqual(actualTree, metadata.TreeSHA256) {
		return fmt.Errorf("release payload tree checksum mismatch")
	}
	return nil
}

func releaseTreesEqual(left, right string) (bool, error) {
	leftDigest, err := releaseTreeDigest(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := releaseTreeDigest(right)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func releaseTreeDigest(root string) (string, error) {
	root = filepath.Clean(root)
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, relative := range paths {
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, relative)
		_, _ = hash.Write([]byte{0})
		_, _ = io.WriteString(hash, info.Mode().String())
		_, _ = hash.Write([]byte{0})
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(hash, target)
		case info.Mode().IsRegular():
			digest, err := hashRegularFile(path)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(hash, digest)
		case info.IsDir():
		default:
			return "", fmt.Errorf("release contains unsupported file %q", relative)
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func recordReleaseActivation(control string, spec ProviderSpec, current State, previous *State) error {
	if !releaseStoreSupportsState(spec, current) {
		return nil
	}
	if spec.Installer == "modrinth" {
		return nil
	}
	currentID := releaseIDFor(spec.ID, current.ArtifactLock)
	previousID := ""
	if previous != nil && previous.Provider == spec.ID {
		previousID = releaseIDFor(spec.ID, previous.ArtifactLock)
		if previousID == currentID {
			previousID = existingPreviousRelease(control, spec, currentID)
		}
	}
	root := releaseProviderRoot(control, spec.ID)
	if err := validateRelease(releaseRoot(control, spec.ID, currentID), spec.ID, currentID, current.ArtifactLock); err != nil {
		return err
	}
	if previousID != "" {
		var validateErr error
		if previous != nil && releaseIDFor(spec.ID, previous.ArtifactLock) == previousID {
			validateErr = validateRelease(releaseRoot(control, spec.ID, previousID), spec.ID, previousID, previous.ArtifactLock)
		} else {
			validateErr = validateReleaseByID(control, spec, previousID)
		}
		if os.IsNotExist(validateErr) {
			previousID = ""
		} else if validateErr != nil {
			return validateErr
		}
	}
	index := releaseIndex{Schema: releaseIndexSchema, Current: currentID, Previous: previousID}
	if err := writeJSONAtomic(filepath.Join(root, "index.json"), index); err != nil {
		return err
	}
	return pruneProviderReleases(control, spec.ID, currentID, previousID)
}

func existingPreviousRelease(control string, spec ProviderSpec, currentID string) string {
	var index releaseIndex
	if err := readJSON(filepath.Join(releaseProviderRoot(control, spec.ID), "index.json"), &index); err != nil || index.Schema != releaseIndexSchema || index.Current != currentID || !validHexDigest(index.Previous, 64) {
		return ""
	}
	if err := validateReleaseByID(control, spec, index.Previous); err != nil {
		return ""
	}
	return index.Previous
}

func validateReleaseByID(control string, spec ProviderSpec, releaseID string) error {
	var metadata releaseMetadata
	root := releaseRoot(control, spec.ID, releaseID)
	if err := readJSON(filepath.Join(root, ".pcvm-release.json"), &metadata); err != nil {
		return err
	}
	if releaseIDFor(spec.ID, metadata.Artifact) != releaseID {
		return fmt.Errorf("release metadata identity is not canonical")
	}
	return validateRelease(root, spec.ID, releaseID, metadata.Artifact)
}

func repairReleaseActivation(control string, spec ProviderSpec, current State) error {
	if !releaseStoreSupportsState(spec, current) {
		return nil
	}
	if spec.Installer == "modrinth" {
		return nil
	}
	currentID := releaseIDFor(spec.ID, current.ArtifactLock)
	previousID := ""
	var index releaseIndex
	if err := readJSON(filepath.Join(releaseProviderRoot(control, spec.ID), "index.json"), &index); err == nil {
		if index.Schema != releaseIndexSchema || !validHexOrEmpty(index.Current, 64) || !validHexOrEmpty(index.Previous, 64) {
			return fmt.Errorf("release index is invalid")
		}
		if index.Current != currentID {
			previousID = index.Current
		} else {
			previousID = index.Previous
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if previousID == currentID {
		previousID = ""
	}
	var previousState *State
	if previousID != "" {
		if err := validateReleaseByID(control, spec, previousID); err != nil {
			return fmt.Errorf("release index previous pointer is invalid: %w", err)
		}
		var metadata releaseMetadata
		if err := readJSON(filepath.Join(releaseRoot(control, spec.ID, previousID), ".pcvm-release.json"), &metadata); err != nil {
			return err
		}
		previousState = &State{Provider: spec.ID, ArtifactLock: metadata.Artifact}
	}
	return recordReleaseActivation(control, spec, current, previousState)
}

func pruneProviderReleases(control, provider, current, previous string) error {
	root := releaseProviderRoot(control, provider)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	keep := map[string]bool{current: true, "index.json": true}
	if previous != "" {
		keep[previous] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] {
			continue
		}
		if strings.HasPrefix(name, ".stage-") || validHexDigest(name, 64) {
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				return err
			}
		}
	}
	return nil
}
