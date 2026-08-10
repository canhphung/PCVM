package pcvm

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const installOverlaySchema = 1

type installOverlayEntry struct {
	Path   string      `json:"path"`
	HadOld bool        `json:"had_old"`
	Mode   os.FileMode `json:"mode,omitempty"`
}

type installOverlayMetadata struct {
	Schema             int                   `json:"schema"`
	Provider           string                `json:"provider"`
	CandidateInstallID string                `json:"candidate_install_id"`
	Phase              string                `json:"phase"`
	Entries            []installOverlayEntry `json:"entries"`
	CreatedAt          time.Time             `json:"created_at"`
}

func installOverlayRoot(control, provider string) string {
	return filepath.Join(control, "transactions", provider)
}

func installOverlayNewRoot(control, provider string) string {
	return filepath.Join(installOverlayRoot(control, provider), "new")
}

func installOverlayBackupRoot(control, provider string) string {
	return filepath.Join(installOverlayRoot(control, provider), "backup")
}

func installOverlayMetadataPath(control, provider string) string {
	return filepath.Join(installOverlayRoot(control, provider), "transaction.json")
}

// applyInstallOverlay copies a fully validated candidate tree into the live
// server root. Every target is backed up before the first write. The backup is
// retained until canonical state activation, so a later receipt/state failure
// can restore the exact previous tree.
func applyInstallOverlay(home, control, provider, candidateInstallID string, previousManaged map[string]string) error {
	return applyInstallOverlayWithWriter(home, control, provider, candidateInstallID, previousManaged, writeArchiveRegularAt)
}

type installOverlayWriter func(*os.Root, string, io.Reader, os.FileMode, int64) error

func applyInstallOverlayWithWriter(home, control, provider, candidateInstallID string, previousManaged map[string]string, writer installOverlayWriter) error {
	newRoot := installOverlayNewRoot(control, provider)
	backupRoot := installOverlayBackupRoot(control, provider)
	if !validID.MatchString(provider) || !validHexDigest(candidateInstallID, 32) {
		return fmt.Errorf("invalid install overlay identity")
	}
	if info, err := os.Lstat(newRoot); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("install overlay candidate is not a real directory: %w", err)
	}

	newPaths, err := regularTreePaths(newRoot)
	if err != nil {
		return err
	}
	union := make(map[string]bool, len(newPaths)+len(previousManaged))
	for _, relative := range newPaths {
		union[relative] = true
	}
	for relative := range previousManaged {
		clean, _, err := archiveTarget(home, relative)
		if err != nil || clean != strings.ReplaceAll(relative, "\\", "/") {
			return fmt.Errorf("previous managed path %q is invalid", relative)
		}
		union[clean] = true
	}
	paths := make([]string, 0, len(union))
	for relative := range union {
		paths = append(paths, relative)
	}
	sort.Strings(paths)

	if err := secureMkdirAll(control, backupRoot, 0o750); err != nil {
		return err
	}
	homeFS, err := openArchiveRoot(home)
	if err != nil {
		return err
	}
	defer homeFS.Close()
	entries := make([]installOverlayEntry, 0, len(paths))
	for _, relative := range paths {
		native := filepath.FromSlash(relative)
		info, statErr := homeFS.Lstat(native)
		hadOld := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if hadOld && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
			return fmt.Errorf("managed target %q is not a regular file", relative)
		}
		if hadOld && !hasManagedPath(previousManaged, relative) {
			return fmt.Errorf("candidate path %q collides with a user-owned file; update aborted", relative)
		}
		entry := installOverlayEntry{Path: relative, HadOld: hadOld}
		if hadOld {
			entry.Mode = info.Mode().Perm()
			input, openErr := homeFS.Open(native)
			if openErr != nil {
				return openErr
			}
			_, backup, targetErr := archiveTarget(backupRoot, relative)
			if targetErr == nil {
				targetErr = writeArchiveRegular(backupRoot, backup, input, info.Mode(), info.Size())
			}
			closeErr := input.Close()
			if targetErr != nil {
				return targetErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		entries = append(entries, entry)
	}
	metadata := installOverlayMetadata{Schema: installOverlaySchema, Provider: provider, CandidateInstallID: candidateInstallID, Phase: "prepared", Entries: entries, CreatedAt: time.Now().UTC()}
	if err := writeJSONAtomic(installOverlayMetadataPath(control, provider), metadata); err != nil {
		return err
	}

	if err := writeOverlayCandidate(homeFS, newRoot, entries, writer); err != nil {
		_ = restoreInstallOverlay(home, control, metadata)
		return err
	}
	metadata.Phase = "applied"
	if err := writeJSONAtomic(installOverlayMetadataPath(control, provider), metadata); err != nil {
		_ = restoreInstallOverlay(home, control, metadata)
		return err
	}
	return nil
}

func writeOverlayCandidate(homeFS *os.Root, newRoot string, entries []installOverlayEntry, writer installOverlayWriter) error {
	for _, entry := range entries {
		relative := filepath.FromSlash(entry.Path)
		candidate := filepath.Join(newRoot, relative)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			if removeErr := homeFS.Remove(relative); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("remove stale managed file %q: %w", entry.Path, removeErr)
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("candidate file %q is not regular", entry.Path)
		}
		input, err := os.Open(candidate)
		if err != nil {
			return err
		}
		err = writer(homeFS, relative, input, info.Mode(), info.Size())
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func restoreInstallOverlay(home, control string, metadata installOverlayMetadata) error {
	homeFS, err := openArchiveRoot(home)
	if err != nil {
		return err
	}
	defer homeFS.Close()
	backupRoot := installOverlayBackupRoot(control, metadata.Provider)
	for _, entry := range metadata.Entries {
		relative := filepath.FromSlash(entry.Path)
		if !entry.HadOld {
			if err := homeFS.Remove(relative); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		_, backup, err := archiveTarget(backupRoot, entry.Path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(backup)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("install overlay backup %q is invalid: %w", entry.Path, err)
		}
		input, err := os.Open(backup)
		if err != nil {
			return err
		}
		err = writeArchiveRegularAt(homeFS, relative, input, entry.Mode, info.Size())
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return os.RemoveAll(installOverlayRoot(control, metadata.Provider))
}

func rollbackPendingInstallOverlay(home, control, provider string) error {
	var metadata installOverlayMetadata
	err := readJSON(installOverlayMetadataPath(control, provider), &metadata)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateInstallOverlayMetadata(metadata, provider); err != nil {
		return err
	}
	return restoreInstallOverlay(home, control, metadata)
}

func commitPendingInstallOverlay(control, provider, installID string) error {
	var metadata installOverlayMetadata
	err := readJSON(installOverlayMetadataPath(control, provider), &metadata)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateInstallOverlayMetadata(metadata, provider); err != nil {
		return err
	}
	if metadata.Phase != "applied" || metadata.CandidateInstallID != installID {
		return fmt.Errorf("install overlay does not match activated state")
	}
	return os.RemoveAll(installOverlayRoot(control, provider))
}

func recoverPendingInstallOverlays(home, control string, state *State) error {
	root := filepath.Join(control, "transactions")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID.MatchString(entry.Name()) {
			continue
		}
		provider := entry.Name()
		var metadata installOverlayMetadata
		if err := readJSON(installOverlayMetadataPath(control, provider), &metadata); os.IsNotExist(err) {
			// No live write occurs before transaction.json is durable.
			if err := os.RemoveAll(installOverlayRoot(control, provider)); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		if err := validateInstallOverlayMetadata(metadata, provider); err != nil {
			return err
		}
		if metadata.Phase == "applied" && state != nil && state.Provider == provider && state.InstallID == metadata.CandidateInstallID {
			if err := commitPendingInstallOverlay(control, provider, state.InstallID); err != nil {
				return err
			}
			continue
		}
		if err := restoreInstallOverlay(home, control, metadata); err != nil {
			return err
		}
	}
	return nil
}

func validateInstallOverlayMetadata(metadata installOverlayMetadata, provider string) error {
	if metadata.Schema != installOverlaySchema || metadata.Provider != provider || !validHexDigest(metadata.CandidateInstallID, 32) || metadata.Phase != "prepared" && metadata.Phase != "applied" {
		return fmt.Errorf("install overlay metadata is invalid")
	}
	seen := map[string]bool{}
	for _, entry := range metadata.Entries {
		clean := strings.ReplaceAll(entry.Path, "\\", "/")
		if clean == "" || clean != entry.Path || path.Clean(clean) != clean || seen[clean] || path.IsAbs(clean) || filepath.IsAbs(filepath.FromSlash(clean)) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("install overlay path is invalid")
		}
		seen[clean] = true
	}
	return nil
}

func regularTreePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("install overlay candidate contains unsupported entry %q", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func hasManagedPath(managed map[string]string, relative string) bool {
	_, ok := managed[filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))]
	return ok
}

func copyRegularIntoTree(root, relative, source string) error {
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("overlay source %q is not a regular file: %w", relative, err)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	_, target, err := archiveTarget(root, relative)
	if err != nil {
		return err
	}
	return writeArchiveRegular(root, target, io.LimitReader(input, info.Size()), info.Mode(), info.Size())
}
