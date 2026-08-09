package pcvm

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func archiveTarget(root, name string) (string, string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	clean := path.Clean(normalized)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("unsafe archive path %q", name)
	}
	rootClean := filepath.Clean(root)
	target := filepath.Join(rootClean, filepath.FromSlash(clean))
	if target == rootClean || !strings.HasPrefix(target, rootClean+string(filepath.Separator)) {
		return "", "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, target, nil
}

func secureMkdirAll(root, directory string, mode os.FileMode) error {
	rootClean := filepath.Clean(root)
	directory = filepath.Clean(directory)
	if directory != rootClean && !strings.HasPrefix(directory, rootClean+string(filepath.Separator)) {
		return fmt.Errorf("directory escapes extraction root")
	}
	if err := ensureRealDirectory(rootClean, mode); err != nil {
		return err
	}
	relative, err := filepath.Rel(rootClean, directory)
	if err != nil {
		return err
	}
	current := rootClean
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if err := ensureRealDirectory(current, mode); err != nil {
			return err
		}
	}
	return nil
}

func ensureRealDirectory(directory string, mode os.FileMode) error {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		if err := os.Mkdir(directory, mode); err != nil && !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("archive path component %q is not a real directory", directory)
	}
	return nil
}

func writeArchiveRegular(root, target string, source io.Reader, mode os.FileMode) error {
	if err := secureMkdirAll(root, filepath.Dir(target), 0o750); err != nil {
		return err
	}
	targetExists := false
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return fmt.Errorf("refusing to replace non-regular archive target %q", target)
		}
		targetExists = true
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".pcvm-extract-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode.Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, source); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if targetExists {
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	// Rename replaces a final symlink itself instead of following it. Parent
	// components were checked above, so pre-existing links cannot redirect writes.
	return os.Rename(name, target)
}
