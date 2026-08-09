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
		root := filepath.Join(m.Config.Control, "cache", "runtimes", kind+"-"+version+"-"+m.Config.Arch)
		rawURL := pack.URL
		if m.Config.Policy.RuntimeMirror != "" {
			u, _ := url.Parse(m.Config.Policy.RuntimeMirror)
			m.HTTP.AllowedHosts[strings.ToLower(u.Hostname())] = true
			rawURL = mirrorURL(m.Config.Policy.RuntimeMirror, pack.URL)
		}
		archivePath := filepath.Join(m.Config.Control, "cache", "downloads", filepath.Base(rawURL))
		if err := secureMkdirAll(m.Config.Control, filepath.Dir(archivePath), 0o750); err != nil {
			return "", fmt.Errorf("prepare runtime download cache: %w", err)
		}
		if ok, err := regularFileSHA256(archivePath, pack.SHA256); err != nil || !ok {
			if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("remove untrusted runtime archive: %w", err)
			}
			artifact := Artifact{URL: rawURL, FileName: filepath.Base(rawURL), SHA256: pack.SHA256}
			if _, err := m.HTTP.Download(ctx, artifact, archivePath); err != nil {
				return "", fmt.Errorf("download runtime %s %s: %w", kind, version, err)
			}
		}
		expectedTree, err := runtimeArchiveManifest(archivePath, pack.Archive)
		if err != nil {
			return "", fmt.Errorf("inspect checksum-pinned runtime %s %s: %w", kind, version, err)
		}
		executable := runtimeExecutable(root, pack.Executable)
		if ok, err := installedRuntimeTreeMatches(root, expectedTree); err == nil && ok {
			_ = touch(root)
			return executable, nil
		}
		if executable != "" && m.Log != nil {
			m.Log.Printf("WARNING: cached %s %s runtime failed integrity verification; reinstalling", kind, version)
		}
		if err := os.RemoveAll(root); err != nil {
			return "", err
		}
		if err := secureMkdirAll(filepath.Join(m.Config.Control, "cache"), root, 0o750); err != nil {
			return "", err
		}
		if err := extractRuntime(archivePath, root, pack.Archive); err != nil {
			return "", err
		}
		executable = runtimeExecutable(root, pack.Executable)
		if executable == "" {
			return "", fmt.Errorf("runtime %s archive did not contain %s", kind, pack.Executable)
		}
		if ok, err := installedRuntimeTreeMatches(root, expectedTree); err != nil {
			return "", fmt.Errorf("runtime tree integrity verification failed after extraction: %w", err)
		} else if !ok {
			return "", fmt.Errorf("runtime tree checksum mismatch after extraction")
		}
		if err := os.Chmod(executable, 0o750); err != nil {
			return "", fmt.Errorf("runtime executable: %w", err)
		}
		_ = m.Prune(root)
		return executable, nil
	}
	if m.Config.Policy.AllowSystemPath {
		binary := map[string]string{
			"java": "java", "node": "node", "python": "python3", "php-pmmp": "php",
			"caddy": "caddy", "dotnet": "dotnet", "steamcmd": "steamcmd.sh",
		}[kind]
		if path, err := exec.LookPath(binary); err == nil {
			m.Log.Printf("WARNING: using system %s because PCVM_ALLOW_SYSTEM_RUNTIME=1", binary)
			return path, nil
		}
	}
	return "", fmt.Errorf("no checksum-pinned %s runtime %s for %s", kind, version, m.Config.Arch)
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
	addRegular := func(name string, source io.Reader) error {
		hash := sha256.New()
		if _, err := io.Copy(hash, source); err != nil {
			return err
		}
		manifest[name] = runtimeArchiveRecord{kind: runtimeRecordRegular, digest: hex.EncodeToString(hash.Sum(nil))}
		return nil
	}
	normalize := func(name string) (string, error) {
		clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
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
			if file.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("runtime ZIP may not contain symlink %q", file.Name)
			}
			if file.FileInfo().IsDir() {
				manifest[name] = runtimeArchiveRecord{kind: runtimeRecordDirectory}
				continue
			}
			if !file.Mode().IsRegular() {
				return nil, fmt.Errorf("runtime ZIP contains unsupported entry %q", file.Name)
			}
			in, err := file.Open()
			if err != nil {
				return nil, err
			}
			copyErr := addRegular(name, in)
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
			switch header.Typeflag {
			case tar.TypeDir:
				manifest[name] = runtimeArchiveRecord{kind: runtimeRecordDirectory}
			case tar.TypeReg, tar.TypeRegA:
				if err := addRegular(name, reader); err != nil {
					return nil, err
				}
			case tar.TypeSymlink:
				link := strings.ReplaceAll(header.Linkname, "\\", "/")
				resolved := path.Clean(path.Join(path.Dir(name), link))
				if path.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, "../") {
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

func mirrorURL(mirror, upstream string) string {
	u, _ := url.Parse(upstream)
	return strings.TrimRight(mirror, "/") + "/" + filepath.Base(u.Path)
}

func extractRuntime(path, dst, kind string) error {
	switch kind {
	case "zip":
		return extractZipSafe(path, dst)
	case "tar.gz", "tgz", "":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		gz, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			_, target, pathErr := archiveTarget(dst, hdr.Name)
			if pathErr != nil {
				return pathErr
			}
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := secureMkdirAll(dst, target, 0o750); err != nil {
					return err
				}
			case tar.TypeReg, tar.TypeRegA:
				if err := writeArchiveRegular(dst, target, tr, os.FileMode(hdr.Mode)&0o777); err != nil {
					return err
				}
			case tar.TypeSymlink:
				if filepath.IsAbs(hdr.Linkname) {
					return fmt.Errorf("absolute runtime symlink %q", hdr.Linkname)
				}
				resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(strings.ReplaceAll(hdr.Linkname, "\\", "/"))))
				rootClean := filepath.Clean(dst)
				if resolved != rootClean && !strings.HasPrefix(resolved, rootClean+string(filepath.Separator)) {
					return fmt.Errorf("runtime symlink escapes archive: %q", hdr.Name)
				}
				if err := secureMkdirAll(dst, filepath.Dir(target), 0o750); err != nil {
					return err
				}
				if _, err := os.Lstat(target); err == nil {
					return fmt.Errorf("refusing existing runtime symlink target %q", hdr.Name)
				} else if !os.IsNotExist(err) {
					return err
				}
				if err := os.Symlink(filepath.FromSlash(hdr.Linkname), target); err != nil {
					return err
				}
			case tar.TypeLink:
				return fmt.Errorf("runtime archives may not contain hard links")
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported runtime archive %q", kind)
	}
}

func touch(path string) error { now := time.Now(); return os.Chtimes(path, now, now) }

func (m RuntimeManager) Prune(keep string) error {
	root := filepath.Join(m.Config.Control, "cache", "runtimes")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
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
	keep = filepath.Clean(keep)
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
	if total > m.Config.Policy.CacheLimitBytes && prior != "" {
		if err := os.RemoveAll(prior); err != nil {
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
