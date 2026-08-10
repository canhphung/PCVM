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

func buildInstallReceipt(home string, spec ProviderSpec, state State, installed Resolved, now time.Time) (InstallReceipt, error) {
	files, root, err := receiptFilesForInstall(home, spec, state, installed.Command, installed.WorkDir)
	if err != nil {
		return InstallReceipt{}, err
	}
	rollback := effectiveRollbackModeForResolved(spec, installed)
	receipt := InstallReceipt{
		Schema: InstallReceiptSchema, ID: state.InstallID, Provider: spec.ID, InstallFormat: state.InstallFormat,
		ReleaseID: state.ArtifactLock.ID, RollbackMode: rollback, RootSHA256: root, Files: files,
		Artifact: state.ArtifactLock, CreatedAt: now,
	}
	if installed.Artifact.Metadata != nil {
		receipt.SourceCommit = installed.Artifact.Metadata["source_commit"]
	}
	return receipt, nil
}

func receiptFilesForInstall(home string, spec ProviderSpec, state State, command []string, workDir string) ([]ReceiptFile, string, error) {
	files, _, err := receiptFiles(home, command)
	if err != nil {
		return nil, "", err
	}
	if spec.ID == "paper-geyser" {
		for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
			if err := appendSealedReceiptFile(home, filepath.Join(home, filepath.FromSlash(relative)), &files); err != nil {
				return nil, "", fmt.Errorf("seal managed overlay %s: %w", relative, err)
			}
		}
	}
	if spec.ID == "modrinth-modpack" {
		managedReceipt := filepath.Join(home, ".pcvm", "managed", spec.ID, "install.json")
		if err := appendSealedReceiptFile(home, managedReceipt, &files); err != nil {
			return nil, "", fmt.Errorf("seal Modrinth install metadata: %w", err)
		}
		// Forge and NeoForge launch through relative @argument files. They are
		// executable launch inputs just like a binary, so seal them even though
		// they are not represented by an absolute argv element.
		for _, argument := range command {
			if !strings.HasPrefix(argument, "@") || strings.HasPrefix(argument, "@@") {
				continue
			}
			relative := strings.TrimPrefix(argument, "@")
			if relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) {
				return nil, "", fmt.Errorf("seal Modrinth argument file: invalid path %q", argument)
			}
			if err := appendSealedReceiptFile(home, filepath.Join(workDir, filepath.FromSlash(relative)), &files); err != nil {
				return nil, "", fmt.Errorf("seal Modrinth argument file %s: %w", relative, err)
			}
		}
		var install modrinthInstallReceipt
		if err := readJSON(managedReceipt, &install); err != nil {
			return nil, "", fmt.Errorf("read sealed Modrinth install metadata: %w", err)
		}
		if install.Loader == "quilt" {
			if err := appendSealedReceiptFile(home, filepath.Join(home, "quilt-server-launch.jar"), &files); err != nil {
				return nil, "", fmt.Errorf("seal Quilt launch jar: %w", err)
			}
		}
	}
	if releaseStoreSupportsState(spec, state) && spec.Installer != "modrinth" {
		metadata := filepath.Join(home, ".pcvm", "releases", spec.ID, releaseIDFor(spec.ID, state.ArtifactLock), ".pcvm-release.json")
		if err := appendSealedReceiptFile(home, metadata, &files); err != nil {
			return nil, "", fmt.Errorf("seal staged release metadata: %w", err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, receiptRoot(files), nil
}

func appendSealedReceiptFile(home, path string, files *[]ReceiptFile) error {
	home, path = filepath.Clean(home), filepath.Clean(path)
	if !pathWithin(home, path) {
		return fmt.Errorf("managed path escapes server root")
	}
	relative, err := filepath.Rel(home, path)
	if err != nil {
		return err
	}
	relative = filepath.ToSlash(relative)
	for _, file := range *files {
		if file.Path == relative {
			return nil
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular non-symlink file")
	}
	digest, err := hashRegularFile(path)
	if err != nil {
		return err
	}
	*files = append(*files, ReceiptFile{Path: relative, SHA256: digest, Mode: uint32(info.Mode().Perm())})
	return nil
}

func effectiveRollbackMode(spec ProviderSpec) string {
	if spec.RollbackMode != "" {
		return spec.RollbackMode
	}
	switch spec.Installer {
	case "steamcmd":
		return "none"
	case "web", "qemu-vm":
		return "in-place"
	default:
		return "staged"
	}
}

func effectiveRollbackModeForResolved(spec ProviderSpec, resolved Resolved) string {
	if spec.Installer == "modrinth" && resolved.RollbackMode == "staged" {
		return "staged"
	}
	if isSourceInstaller(spec) && resolved.Artifact.Metadata["source_commit"] != "" {
		return "staged"
	}
	return effectiveRollbackMode(spec)
}

func effectiveRollbackModeForState(spec ProviderSpec, state State) string {
	if isSourceInstaller(spec) && strings.HasPrefix(state.ArtifactLock.ID, "git:") {
		return "staged"
	}
	return effectiveRollbackMode(spec)
}

func receiptFiles(home string, command []string) ([]ReceiptFile, string, error) {
	home = filepath.Clean(home)
	seen := map[string]bool{}
	var files []ReceiptFile
	for _, candidate := range command {
		if !filepath.IsAbs(candidate) {
			continue
		}
		candidate = filepath.Clean(candidate)
		if !pathWithin(home, candidate) {
			continue
		}
		rel, err := filepath.Rel(home, candidate)
		if err != nil {
			return nil, "", err
		}
		rel = filepath.ToSlash(rel)
		// Uploaded/Git application source is intentionally user mutable. Only
		// launcher-managed releases, runtime executables, and game binaries are
		// sealed into the install receipt.
		if !strings.HasPrefix(rel, ".pcvm/") && !strings.HasPrefix(rel, "game/") {
			continue
		}
		if seen[rel] {
			continue
		}
		info, err := os.Lstat(candidate)
		if err != nil {
			return nil, "", fmt.Errorf("seal managed executable %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("managed executable %s must be a regular non-symlink file", rel)
		}
		digest, err := hashRegularFile(candidate)
		if err != nil {
			return nil, "", err
		}
		files = append(files, ReceiptFile{Path: rel, SHA256: digest, Mode: uint32(info.Mode().Perm())})
		seen[rel] = true
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, receiptRoot(files), nil
}

func verifyInstallReceipt(home string, state State, receipt InstallReceipt) error {
	if receipt.Schema != InstallReceiptSchema || receipt.ID != state.InstallID || receipt.Provider != state.Provider ||
		receipt.InstallFormat != state.InstallFormat || receipt.ReleaseID != state.ArtifactLock.ID || receipt.Artifact != state.ArtifactLock {
		return fmt.Errorf("PCVM-E2004 RECEIPT_MISMATCH: install receipt does not match canonical state")
	}
	if receipt.RollbackMode != "staged" && receipt.RollbackMode != "in-place" && receipt.RollbackMode != "none" {
		return fmt.Errorf("PCVM-E2004 RECEIPT_MISMATCH: invalid rollback mode")
	}
	if receipt.RootSHA256 != receiptRoot(receipt.Files) {
		return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: receipt root hash is invalid")
	}
	for _, file := range receipt.Files {
		// Receipt modes are serialized permission bits, not an os.FileMode. Reject
		// type/special bits explicitly instead of relying on the comparison with
		// the local filesystem to happen to catch them.
		if file.Mode&^uint32(0o777) != 0 {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: invalid managed file mode")
		}
		if file.Path == "" || filepath.IsAbs(filepath.FromSlash(file.Path)) || strings.Contains(file.Path, "\\") {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: invalid managed path")
		}
		path := filepath.Join(home, filepath.FromSlash(file.Path))
		if !pathWithin(home, path) {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: managed path escapes server root")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("PCVM-E2006 RELEASE_MISSING: %s: %w", file.Path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || uint32(info.Mode().Perm()) != file.Mode {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: managed file %s changed type or mode", file.Path)
		}
		digest, err := hashRegularFile(path)
		if err != nil {
			return err
		}
		if !constantTimeHexEqual(digest, file.SHA256) {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: managed file %s checksum mismatch", file.Path)
		}
	}
	return nil
}

// verifyLaunchReceiptCompleteness binds the command rebuilt from trusted
// catalog drivers to the complete managed-file set sealed at installation.
// A receipt with an omitted executable must never turn verification into an
// allowlist chosen by the user-writable receipt itself.
func verifyLaunchReceiptCompleteness(home string, receipt InstallReceipt, launch LaunchState) error {
	expected, _, err := receiptFiles(home, launch.Command)
	if err != nil {
		return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: derive trusted launch files: %w", err)
	}
	if receipt.Provider == "paper-geyser" {
		for _, relative := range []string{"plugins/Geyser-Spigot.jar", "plugins/floodgate-spigot.jar"} {
			if err := appendSealedReceiptFile(home, filepath.Join(home, filepath.FromSlash(relative)), &expected); err != nil {
				return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: derive trusted Paper-Geyser plugin %s: %w", relative, err)
			}
		}
	}
	received := make(map[string]ReceiptFile, len(receipt.Files))
	for _, file := range receipt.Files {
		received[file.Path] = file
	}
	for _, file := range expected {
		if got, ok := received[file.Path]; !ok || got != file {
			return fmt.Errorf("PCVM-E2005 RECEIPT_TAMPERED: trusted launch file %q does not match the receipt", file.Path)
		}
	}
	return nil
}

func receiptRoot(files []ReceiptFile) string {
	h := sha256.New()
	for _, file := range files {
		_, _ = io.WriteString(h, file.Path)
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, strings.ToLower(file.SHA256))
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, fmt.Sprintf("%o", file.Mode))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashRegularFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
