package pcvm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
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
		executable := runtimeExecutable(root, pack.Executable)
		if info, err := os.Stat(executable); err == nil && !info.IsDir() {
			_ = touch(root)
			return executable, nil
		}
		if err := os.RemoveAll(root); err != nil {
			return "", err
		}
		if err := os.MkdirAll(root, 0o750); err != nil {
			return "", err
		}
		rawURL := pack.URL
		if m.Config.Policy.RuntimeMirror != "" {
			u, _ := url.Parse(m.Config.Policy.RuntimeMirror)
			m.HTTP.AllowedHosts[strings.ToLower(u.Hostname())] = true
			rawURL = mirrorURL(m.Config.Policy.RuntimeMirror, pack.URL)
		}
		archivePath := filepath.Join(m.Config.Control, "cache", "downloads", filepath.Base(rawURL))
		artifact := Artifact{URL: rawURL, FileName: filepath.Base(rawURL), SHA256: pack.SHA256}
		if _, err := m.HTTP.Download(ctx, artifact, archivePath); err != nil {
			return "", fmt.Errorf("download runtime %s %s: %w", kind, version, err)
		}
		if err := extractRuntime(archivePath, root, pack.Archive); err != nil {
			return "", err
		}
		executable = runtimeExecutable(root, pack.Executable)
		if executable == "" {
			return "", fmt.Errorf("runtime %s archive did not contain %s", kind, pack.Executable)
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
			name := filepath.Clean(filepath.FromSlash(hdr.Name))
			if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
				return fmt.Errorf("unsafe runtime archive path %q", hdr.Name)
			}
			target := filepath.Join(dst, name)
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(target, 0o750); err != nil {
					return err
				}
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
					return err
				}
				out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
				if err != nil {
					return err
				}
				_, copyErr := io.Copy(out, tr)
				closeErr := out.Close()
				if copyErr != nil {
					return copyErr
				}
				if closeErr != nil {
					return closeErr
				}
			case tar.TypeSymlink:
				if filepath.IsAbs(hdr.Linkname) {
					return fmt.Errorf("absolute runtime symlink %q", hdr.Linkname)
				}
				resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(hdr.Linkname)))
				rootClean := filepath.Clean(dst)
				if resolved != rootClean && !strings.HasPrefix(resolved, rootClean+string(filepath.Separator)) {
					return fmt.Errorf("runtime symlink escapes archive: %q", hdr.Name)
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
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
