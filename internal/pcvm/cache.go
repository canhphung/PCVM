package pcvm

import (
	"fmt"
	"os"
	"path/filepath"
)

// cleanupConsumedInstallCache removes inputs that have already been copied or
// extracted into their committed installation directories. Runtime trees and
// their signed-manifest-derived receipts are deliberately handled by
// RuntimeManager; consumed runtime archive blobs are removed after activation.
func cleanupConsumedInstallCache(control string) error {
	for _, name := range []string{"artifacts", "sources"} {
		if err := removeCacheCategory(control, name); err != nil {
			return err
		}
	}
	return nil
}

func removeCacheCategory(control, name string) error {
	cacheRoot, exists, err := realCacheRoot(control)
	if err != nil || !exists {
		return err
	}
	target := filepath.Join(cacheRoot, name)
	if err := pathInside(cacheRoot, target); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// RemoveAll removes a symlink itself rather than following it. Avoid calling
	// ReadDir first so a user-created category symlink can never redirect cleanup.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return os.Remove(target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(target, entry.Name())
		if err := pathInside(target, child); err != nil {
			return err
		}
		if err := os.RemoveAll(child); err != nil {
			return err
		}
	}
	return nil
}

func realCacheRoot(control string) (string, bool, error) {
	control = filepath.Clean(control)
	info, err := os.Lstat(control)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, fmt.Errorf("PCVM control path is not a real directory")
	}
	root := filepath.Join(control, "cache")
	info, err = os.Lstat(root)
	if os.IsNotExist(err) {
		return root, false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false, fmt.Errorf("PCVM cache path is not a real directory")
	}
	return root, true, nil
}

func pathInside(root, target string) error {
	root, target = filepath.Clean(root), filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("cache path %q escapes %q", target, root)
	}
	return nil
}
