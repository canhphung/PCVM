package pcvm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
)

// FinalizeRuntimePack binds a runtime-pack descriptor to the exact archive on
// disk. The resulting tree hash is independent of archive ordering and
// metadata such as timestamps, owners, and modes; it only commits to the entry
// type, path, file contents, and symlink target that PCVM extracts.
func FinalizeRuntimePack(archivePath string, pack RuntimePackSpec) (RuntimePackSpec, error) {
	if !validRuntimeUpstreamVersion(pack.UpstreamVersion) {
		return RuntimePackSpec{}, fmt.Errorf("runtime upstream version must be non-empty printable text")
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		return RuntimePackSpec{}, fmt.Errorf("inspect runtime archive: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return RuntimePackSpec{}, fmt.Errorf("runtime archive must be a non-empty regular file")
	}
	digest, err := regularFileDigest(archivePath)
	if err != nil {
		return RuntimePackSpec{}, err
	}
	if pack.SHA256 != "" && !strings.EqualFold(pack.SHA256, digest) {
		return RuntimePackSpec{}, fmt.Errorf("runtime archive checksum mismatch: got %s, expected %s", digest, pack.SHA256)
	}
	manifest, err := runtimeArchiveManifest(archivePath, pack.Archive)
	if err != nil {
		return RuntimePackSpec{}, fmt.Errorf("inspect runtime archive tree: %w", err)
	}
	if err := runtimeManifestHasExecutable(manifest, pack.Executable); err != nil {
		return RuntimePackSpec{}, err
	}
	pack.ID = runtimePackIdentity(pack.Kind, pack.Version, pack.Architecture)
	pack.SHA256 = digest
	pack.Size = info.Size()
	pack.TreeSHA256 = runtimeTreeSHA256(manifest)
	return pack, nil
}

func validRuntimeUpstreamVersion(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if !unicode.IsPrint(character) {
			return false
		}
	}
	return true
}

func regularFileDigest(file string) (string, error) {
	in, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("open runtime archive: %w", err)
	}
	defer in.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, in); err != nil {
		return "", fmt.Errorf("hash runtime archive: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func runtimeTreeSHA256(manifest map[string]runtimeArchiveRecord) string {
	names := make([]string, 0, len(manifest))
	for name := range manifest {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		record := manifest[name]
		_, _ = hash.Write([]byte{record.kind, 0})
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(record.digest))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(record.link))
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func runtimeManifestHasExecutable(manifest map[string]runtimeArchiveRecord, executable string) error {
	if executable == "" {
		return fmt.Errorf("runtime executable is empty")
	}
	matches := 0
	for name, record := range manifest {
		if record.kind != runtimeRecordRegular && record.kind != runtimeRecordSymlink {
			continue
		}
		matched := name == executable
		if strings.ContainsAny(executable, "*?[") {
			matched, _ = path.Match(executable, name)
		}
		if matched {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("runtime archive contains %d matches for executable %q", matches, executable)
	}
	return nil
}
