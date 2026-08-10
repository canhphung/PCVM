package pcvm

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type RuntimeManager struct {
	Catalog Catalog
	Config  Config
	HTTP    *HTTPClient
	Log     *Logger
}

func (m RuntimeManager) Ensure(ctx context.Context, kind, version string) (string, error) {
	if kind == "native" {
		return "", nil
	}
	for _, pack := range m.Catalog.RuntimePacks {
		if pack.Kind != kind || pack.Version != version || pack.Architecture != m.Config.Arch {
			continue
		}
		root := m.runtimeTreeRoot(pack)
		receiptPath := m.runtimeReceiptPath(pack)
		if manifest, err := loadRuntimeTreeReceipt(receiptPath, pack); err == nil {
			if ok, verifyErr := installedRuntimeTreeMatches(root, manifest); verifyErr == nil && ok {
				executable := runtimeExecutable(root, pack.Executable)
				if executable == "" {
					return "", fmt.Errorf("runtime %s receipt has no executable %q", pack.ID, pack.Executable)
				}
				_ = touch(root)
				_ = os.Remove(m.runtimeBlobPath(pack))
				return executable, nil
			}
		}
		if _, err := os.Lstat(root); err == nil {
			if m.Log != nil {
				m.Log.Printf("WARNING: cached %s runtime failed tree verification; reinstalling", pack.ID)
			}
			if err := os.RemoveAll(root); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		_ = os.Remove(receiptPath)
		rawURL := pack.URL
		if m.Config.Policy.RuntimeMirror != "" {
			u, _ := url.Parse(m.Config.Policy.RuntimeMirror)
			m.HTTP.AllowedHosts[strings.ToLower(u.Hostname())] = true
			rawURL = mirrorURL(m.Config.Policy.RuntimeMirror, pack.SHA256)
		}
		archivePath := m.runtimeBlobPath(pack)
		if err := secureMkdirAll(m.Config.Control, filepath.Dir(archivePath), 0o750); err != nil {
			return "", fmt.Errorf("prepare content-addressed runtime blob cache: %w", err)
		}
		if ok, err := regularFileSHA256(archivePath, pack.SHA256); err != nil || !ok {
			if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("remove untrusted runtime archive: %w", err)
			}
			artifact := Artifact{URL: rawURL, FileName: pack.SHA256, SHA256: pack.SHA256}
			if _, err := m.HTTP.Download(ctx, artifact, archivePath); err != nil {
				return "", fmt.Errorf("download runtime %s %s: %w", kind, version, err)
			}
		}
		expectedTree, err := runtimeArchiveManifest(archivePath, pack.Archive)
		if err != nil {
			return "", fmt.Errorf("inspect checksum-pinned runtime %s %s: %w", kind, version, err)
		}
		if pack.Size > 0 {
			info, statErr := os.Lstat(archivePath)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != pack.Size {
				return "", fmt.Errorf("runtime %s %s archive size does not match manifest", kind, version)
			}
		}
		if pack.TreeSHA256 != "" {
			actualTree := runtimeTreeSHA256(expectedTree)
			if !strings.EqualFold(actualTree, pack.TreeSHA256) {
				return "", fmt.Errorf("runtime %s %s extracted-tree checksum mismatch", kind, version)
			}
		}
		treesRoot := filepath.Dir(root)
		if err := secureMkdirAll(m.Config.Control, treesRoot, 0o750); err != nil {
			return "", err
		}
		staging, err := os.MkdirTemp(treesRoot, ".tree-*")
		if err != nil {
			return "", err
		}
		committed := false
		defer func() {
			if !committed {
				_ = os.RemoveAll(staging)
			}
		}()
		if err := extractRuntime(archivePath, staging, pack.Archive); err != nil {
			return "", err
		}
		executable := runtimeExecutable(staging, pack.Executable)
		if executable == "" {
			return "", fmt.Errorf("runtime %s archive did not contain %s", kind, pack.Executable)
		}
		if ok, err := installedRuntimeTreeMatches(staging, expectedTree); err != nil {
			return "", fmt.Errorf("runtime tree integrity verification failed after extraction: %w", err)
		} else if !ok {
			return "", fmt.Errorf("runtime tree checksum mismatch after extraction")
		}
		if err := os.Chmod(executable, 0o750); err != nil {
			return "", fmt.Errorf("runtime executable: %w", err)
		}
		// Executable mode is not part of the content hash. Re-verify contents
		// after chmod, then atomically activate the immutable tree.
		if ok, err := installedRuntimeTreeMatches(staging, expectedTree); err != nil {
			return "", fmt.Errorf("runtime tree changed during activation: %w", err)
		} else if !ok {
			return "", fmt.Errorf("runtime tree changed during activation")
		}
		if err := os.Rename(staging, root); err != nil {
			return "", fmt.Errorf("activate runtime tree: %w", err)
		}
		committed = true
		if err := saveRuntimeTreeReceipt(receiptPath, pack, expectedTree); err != nil {
			_ = os.RemoveAll(root)
			return "", err
		}
		if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove consumed runtime blob: %w", err)
		}
		executable = runtimeExecutable(root, pack.Executable)
		_ = m.Prune(root)
		return executable, nil
	}
	if m.Config.Policy.AllowSystemPath {
		binary := map[string]string{
			"java": "java", "node": "node", "python": "python3", "php-pmmp": "php",
			"caddy": "caddy", "dotnet": "dotnet", "steamcmd": "steamcmd.sh",
			"bun": "bun", "deno": "deno", "go": "go",
		}[kind]
		if path, err := exec.LookPath(binary); err == nil {
			m.Log.Printf("WARNING: using system %s because PCVM_ALLOW_SYSTEM_RUNTIME=1", binary)
			return path, nil
		}
	}
	return "", fmt.Errorf("no checksum-pinned %s runtime %s for %s", kind, version, m.Config.Arch)
}

const runtimeTreeReceiptSchema = 1

type runtimeTreeReceipt struct {
	Schema     int                           `json:"schema"`
	PackID     string                        `json:"pack_id"`
	TreeSHA256 string                        `json:"tree_sha256"`
	Records    map[string]runtimeReceiptFile `json:"records"`
}

type runtimeReceiptFile struct {
	Kind   byte   `json:"kind"`
	Digest string `json:"digest,omitempty"`
	Link   string `json:"link,omitempty"`
}

func saveRuntimeTreeReceipt(path string, pack RuntimePackSpec, manifest map[string]runtimeArchiveRecord) error {
	records := make(map[string]runtimeReceiptFile, len(manifest))
	for name, record := range manifest {
		records[name] = runtimeReceiptFile{Kind: record.kind, Digest: record.digest, Link: record.link}
	}
	return writeJSONAtomic(path, runtimeTreeReceipt{Schema: runtimeTreeReceiptSchema, PackID: pack.ID, TreeSHA256: pack.TreeSHA256, Records: records})
}

func loadRuntimeTreeReceipt(path string, pack RuntimePackSpec) (map[string]runtimeArchiveRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var receipt runtimeTreeReceipt
	if err := decodeStrictJSON(data, &receipt); err != nil {
		return nil, err
	}
	if receipt.Schema != runtimeTreeReceiptSchema || receipt.PackID != pack.ID || !strings.EqualFold(receipt.TreeSHA256, pack.TreeSHA256) {
		return nil, fmt.Errorf("runtime tree receipt identity mismatch")
	}
	manifest := make(map[string]runtimeArchiveRecord, len(receipt.Records))
	for name, record := range receipt.Records {
		if name == "" || (record.Kind != runtimeRecordDirectory && record.Kind != runtimeRecordRegular && record.Kind != runtimeRecordSymlink) {
			return nil, fmt.Errorf("runtime tree receipt has invalid entry")
		}
		manifest[name] = runtimeArchiveRecord{kind: record.Kind, digest: record.Digest, link: record.Link}
	}
	if !strings.EqualFold(runtimeTreeSHA256(manifest), pack.TreeSHA256) {
		return nil, fmt.Errorf("runtime tree receipt does not match embedded tree hash")
	}
	if err := runtimeManifestHasExecutable(manifest, pack.Executable); err != nil {
		return nil, err
	}
	return manifest, nil
}

type runtimeArchiveRecord struct {
	kind   byte
	digest string
	link   string
}

const (
	runtimeRecordDirectory byte = 'd'
	runtimeRecordRegular   byte = 'f'
	runtimeRecordSymlink   byte = 'l'
)

func runtimeArchiveManifest(archive, kind string) (map[string]runtimeArchiveRecord, error) {
	manifest := map[string]runtimeArchiveRecord{}
	budget := newArchiveBudget(defaultArchiveLimits)
	addRegular := func(name string, source io.Reader, expected int64) error {
		if err := budget.add(name, archiveEntryRegular, expected); err != nil {
			return err
		}
		hash := sha256.New()
		written, err := io.Copy(hash, io.LimitReader(source, expected+1))
		if err != nil {
			return err
		}
		if written != expected {
			return fmt.Errorf("runtime archive file %q size mismatch: got %d, expected %d", name, written, expected)
		}
		manifest[name] = runtimeArchiveRecord{kind: runtimeRecordRegular, digest: hex.EncodeToString(hash.Sum(nil))}
		return nil
	}
	normalize := func(name string) (string, error) {
		clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		native := filepath.FromSlash(clean)
		// A number of official archives contain an explicit "./" root
		// directory. It carries no tree identity and is safe to ignore; a
		// regular file resolving to the root remains invalid below.
		if clean == "." {
			return "", nil
		}
		if path.IsAbs(clean) || filepath.IsAbs(native) || filepath.VolumeName(native) != "" || clean == ".." || strings.HasPrefix(clean, "../") {
			return "", fmt.Errorf("unsafe runtime archive path %q", name)
		}
		return clean, nil
	}
	switch kind {
	case "zip":
		reader, err := zip.OpenReader(archive)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		for _, file := range reader.File {
			name, err := normalize(file.Name)
			if err != nil {
				return nil, err
			}
			if name == "" && file.FileInfo().IsDir() {
				continue
			}
			if name == "" {
				return nil, fmt.Errorf("runtime ZIP entry resolves to archive root: %q", file.Name)
			}
			if file.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("runtime ZIP may not contain symlink %q", file.Name)
			}
			if file.FileInfo().IsDir() {
				if err := budget.add(name, archiveEntryDirectory, 0); err != nil {
					return nil, err
				}
				manifest[name] = runtimeArchiveRecord{kind: runtimeRecordDirectory}
				continue
			}
			if !file.Mode().IsRegular() {
				return nil, fmt.Errorf("runtime ZIP contains unsupported entry %q", file.Name)
			}
			if file.UncompressedSize64 > uint64(^uint64(0)>>1) {
				return nil, fmt.Errorf("runtime ZIP file %q is too large", file.Name)
			}
			in, err := file.Open()
			if err != nil {
				return nil, err
			}
			copyErr := addRegular(name, in, int64(file.UncompressedSize64))
			closeErr := in.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	case "tar.gz", "tgz", "":
		file, err := os.Open(archive)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader := tar.NewReader(gz)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name, err := normalize(header.Name)
			if err != nil {
				return nil, err
			}
			if name == "" && header.Typeflag == tar.TypeDir {
				continue
			}
			if name == "" {
				return nil, fmt.Errorf("runtime archive entry resolves to archive root: %q", header.Name)
			}
			switch header.Typeflag {
			case tar.TypeDir:
				if err := budget.add(name, archiveEntryDirectory, 0); err != nil {
					return nil, err
				}
				manifest[name] = runtimeArchiveRecord{kind: runtimeRecordDirectory}
			case tar.TypeReg, tar.TypeRegA:
				if err := addRegular(name, reader, header.Size); err != nil {
					return nil, err
				}
			case tar.TypeSymlink:
				if err := budget.add(name, archiveEntrySymlink, 0); err != nil {
					return nil, err
				}
				link := strings.ReplaceAll(header.Linkname, "\\", "/")
				resolved := path.Clean(path.Join(path.Dir(name), link))
				nativeLink := filepath.FromSlash(link)
				if path.IsAbs(link) || filepath.IsAbs(nativeLink) || filepath.VolumeName(nativeLink) != "" || resolved == ".." || strings.HasPrefix(resolved, "../") {
					return nil, fmt.Errorf("runtime symlink escapes archive: %q", header.Name)
				}
				manifest[name] = runtimeArchiveRecord{kind: runtimeRecordSymlink, link: link}
			case tar.TypeXHeader, tar.TypeXGlobalHeader:
				// archive/tar applies PAX metadata to the following entry.
			default:
				return nil, fmt.Errorf("runtime archive contains unsupported entry %q", header.Name)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported runtime archive %q", kind)
	}
	return manifest, nil
}

func installedRuntimeTreeMatches(root string, manifest map[string]runtimeArchiveRecord) (bool, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return false, fmt.Errorf("runtime root is not a real directory")
	}
	seen := make(map[string]bool, len(manifest))
	err = filepath.Walk(root, func(file string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if file == root {
			return nil
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		record, expected := manifest[name]
		if !expected {
			if info.IsDir() {
				return nil // archives need not contain explicit parent directory entries
			}
			return fmt.Errorf("unexpected runtime cache entry %q", name)
		}
		seen[name] = true
		switch record.kind {
		case runtimeRecordDirectory:
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("runtime directory %q changed type", name)
			}
		case runtimeRecordRegular:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("runtime file %q changed type", name)
			}
			if ok, err := regularFileSHA256(file, record.digest); err != nil {
				return err
			} else if !ok {
				return fmt.Errorf("runtime file %q checksum mismatch", name)
			}
		case runtimeRecordSymlink:
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("runtime symlink %q changed type", name)
			}
			link, err := os.Readlink(file)
			if err != nil {
				return err
			}
			if filepath.ToSlash(link) != record.link {
				return fmt.Errorf("runtime symlink %q changed target", name)
			}
			resolved, err := filepath.EvalSymlinks(file)
			if err != nil {
				return err
			}
			rootClean, resolvedClean := filepath.Clean(root), filepath.Clean(resolved)
			if resolvedClean == rootClean || !strings.HasPrefix(resolvedClean, rootClean+string(filepath.Separator)) {
				return fmt.Errorf("runtime symlink %q escapes cache", name)
			}
		default:
			return fmt.Errorf("runtime manifest has invalid entry %q", name)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for name, record := range manifest {
		if record.kind != runtimeRecordDirectory && !seen[name] {
			return false, fmt.Errorf("runtime cache is missing %q", name)
		}
	}
	return true, nil
}

func regularFileSHA256(file, expected string) (bool, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", file)
	}
	in, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer in.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, in); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected), nil
}

func runtimeExecutable(root, pattern string) string {
	full := filepath.Join(root, filepath.FromSlash(pattern))
	if !strings.ContainsAny(pattern, "*?[") {
		return full
	}
	matches, _ := filepath.Glob(full)
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func mirrorURL(mirror, digest string) string {
	return strings.TrimRight(mirror, "/") + "/sha256/" + strings.ToLower(digest)
}

func extractRuntime(path, dst, kind string) error {
	switch kind {
	case "zip":
		return extractZipSafe(path, dst)
	case "tar.gz", "tgz", "":
		return extractTarGzipSafe(path, dst, true)
	default:
		return fmt.Errorf("unsupported runtime archive %q", kind)
	}
}

func touch(path string) error { now := time.Now(); return os.Chtimes(path, now, now) }

func (m RuntimeManager) Prune(keep string) error {
	cacheRoot, exists, err := realCacheRoot(m.Config.Control)
	if err != nil || !exists {
		return err
	}
	root := filepath.Join(cacheRoot, "trees", "sha256")
	var entries []os.DirEntry
	if info, statErr := os.Lstat(root); os.IsNotExist(statErr) {
		entries = nil
	} else if statErr != nil {
		return statErr
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runtime cache is not a real directory")
	} else if entries, err = os.ReadDir(root); err != nil {
		return err
	}
	type item struct {
		path string
		mod  time.Time
		size int64
	}
	var items []item
	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, _ := entry.Info()
		size := dirSize(path)
		items = append(items, item{path: path, mod: info.ModTime(), size: size})
		total += size
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	if keep != "" {
		keep = filepath.Clean(keep)
	}
	prior := ""
	for _, it := range items {
		if filepath.Clean(it.path) != keep {
			prior = filepath.Clean(it.path)
			break
		}
	}
	for _, it := range items {
		path := filepath.Clean(it.path)
		if path == keep || path == prior {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		total -= it.size
	}

	if err := pruneRuntimeDownloads(filepath.Join(cacheRoot, "blobs", "sha256"), nil); err != nil {
		return err
	}
	if err := m.pruneRuntimeReceipts(keep, prior); err != nil {
		return err
	}

	// CACHE_LIMIT_MB is a limit for the complete PCVM cache, not merely the
	// extracted runtime directory. Consumed artifacts/sources are removed by the
	// app; the only optional LKG entry left here is the previous runtime pair.
	total = dirSize(cacheRoot)
	if m.Config.Policy.CacheLimitBytes > 0 && total > m.Config.Policy.CacheLimitBytes && prior != "" {
		if err := os.RemoveAll(prior); err != nil {
			return err
		}
		_ = m.pruneRuntimeReceipts(keep, "")
		total = dirSize(cacheRoot)
	}
	if m.Config.Policy.CacheLimitBytes > 0 && total > m.Config.Policy.CacheLimitBytes && m.Log != nil {
		m.Log.Printf("WARNING: active runtime cache uses %d MB, above CACHE_LIMIT_MB=%d; active integrity files were retained", (total+1024*1024-1)/(1024*1024), m.Config.Policy.CacheLimitBytes/(1024*1024))
	}
	return nil
}

func (m RuntimeManager) runtimeRoot(kind, version string) string {
	if kind == "" || kind == "native" || version == "" {
		return ""
	}
	for _, pack := range m.Catalog.RuntimePacks {
		if pack.Kind == kind && pack.Version == version && pack.Architecture == m.Config.Arch {
			return m.runtimeTreeRoot(pack)
		}
	}
	return ""
}

func (m RuntimeManager) runtimeTreeRoot(pack RuntimePackSpec) string {
	return filepath.Join(m.Config.Control, "cache", "trees", "sha256", strings.ToLower(pack.TreeSHA256))
}

func (m RuntimeManager) runtimeBlobPath(pack RuntimePackSpec) string {
	return filepath.Join(m.Config.Control, "cache", "blobs", "sha256", strings.ToLower(pack.SHA256))
}

func (m RuntimeManager) runtimeReceiptPath(pack RuntimePackSpec) string {
	return filepath.Join(m.Config.Control, "cache", "runtime-receipts", strings.ToLower(pack.TreeSHA256)+".json")
}

func (m RuntimeManager) pruneRuntimeReceipts(keep, prior string) error {
	root := filepath.Join(m.Config.Control, "cache", "runtime-receipts")
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runtime receipt cache is not a real directory")
	}
	kept := map[string]bool{}
	for _, tree := range []string{keep, prior} {
		if tree != "" {
			kept[strings.ToLower(filepath.Base(filepath.Clean(tree)))+".json"] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if kept[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func pruneRuntimeDownloads(root string, keep map[string]bool) error {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("runtime download cache is not a real directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Clean(filepath.Join(root, entry.Name()))
		if keep[path] {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func dirSize(root string) int64 {
	var size int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}
