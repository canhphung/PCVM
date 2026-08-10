package pcvm

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// archiveLimits cap the expansion work performed after a checksum-pinned
// download. They protect against corrupt upstream archives and decompression
// bombs; individual tests may use smaller limits to exercise each boundary.
type archiveLimits struct {
	MaxEntries    int
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxPathBytes  int
	MaxSymlinks   int
	// MaxCompressionRatio caps expanded/compressed bytes. Zero disables the
	// ratio check for deliberately uncompressed inputs such as plain TAR.
	MaxCompressionRatio int64
}

var defaultArchiveLimits = archiveLimits{
	MaxEntries:          100_000,
	MaxTotalBytes:       4 << 30,
	MaxFileBytes:        2 << 30,
	MaxPathBytes:        4096,
	MaxSymlinks:         10_000,
	MaxCompressionRatio: 200,
}

type archiveBudget struct {
	limits   archiveLimits
	entries  int
	total    int64
	symlinks int
	seen     map[string]byte
}

const (
	archiveEntryDirectory byte = 'd'
	archiveEntryRegular   byte = 'f'
	archiveEntrySymlink   byte = 'l'
)

func newArchiveBudget(limits archiveLimits) *archiveBudget {
	return &archiveBudget{limits: limits, seen: map[string]byte{}}
}

func (b *archiveBudget) add(name string, kind byte, size int64) error {
	if size < 0 {
		return fmt.Errorf("archive entry %q has a negative size", name)
	}
	b.entries++
	if b.limits.MaxEntries > 0 && b.entries > b.limits.MaxEntries {
		return fmt.Errorf("archive contains more than %d entries", b.limits.MaxEntries)
	}
	if b.limits.MaxPathBytes > 0 && len(name) > b.limits.MaxPathBytes {
		return fmt.Errorf("archive path exceeds %d bytes", b.limits.MaxPathBytes)
	}
	if previous, exists := b.seen[name]; exists {
		if previous == archiveEntryDirectory && kind == archiveEntryDirectory {
			return nil
		}
		return fmt.Errorf("archive contains duplicate path %q", name)
	}
	b.seen[name] = kind
	if kind == archiveEntrySymlink {
		b.symlinks++
		if b.limits.MaxSymlinks >= 0 && b.symlinks > b.limits.MaxSymlinks {
			return fmt.Errorf("archive contains more than %d symlinks", b.limits.MaxSymlinks)
		}
	}
	if kind != archiveEntryRegular {
		return nil
	}
	if b.limits.MaxFileBytes > 0 && size > b.limits.MaxFileBytes {
		return fmt.Errorf("archive file %q exceeds the %d byte per-file limit", name, b.limits.MaxFileBytes)
	}
	if b.limits.MaxTotalBytes > 0 && (size > b.limits.MaxTotalBytes || b.total > b.limits.MaxTotalBytes-size) {
		return fmt.Errorf("archive expands beyond the %d byte total limit", b.limits.MaxTotalBytes)
	}
	b.total += size
	return nil
}

func (b *archiveBudget) checkCompression(name string, compressed int64) error {
	if b.limits.MaxCompressionRatio <= 0 || b.total == 0 {
		return nil
	}
	if compressed <= 0 {
		return fmt.Errorf("archive %q has an invalid compressed size", name)
	}
	// Division avoids overflowing when checking total > compressed*ratio.
	ratio := b.limits.MaxCompressionRatio
	if b.total/compressed > ratio || b.total/compressed == ratio && b.total%compressed != 0 {
		return fmt.Errorf("archive %q exceeds the maximum compression ratio of %d:1", name, ratio)
	}
	return nil
}

func archiveTarget(root, name string) (string, string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	clean := path.Clean(normalized)
	native := filepath.FromSlash(clean)
	if clean == "." || path.IsAbs(clean) || filepath.IsAbs(native) || filepath.VolumeName(native) != "" || clean == ".." || strings.HasPrefix(clean, "../") {
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

func openArchiveRoot(root string) (*os.Root, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("archive root %q is not a real directory", absolute)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	samePath := filepath.Clean(resolved) == absolute
	if runtime.GOOS == "windows" {
		samePath = strings.EqualFold(filepath.Clean(resolved), absolute)
	}
	if !samePath {
		return nil, fmt.Errorf("archive root %q contains a symlink component", absolute)
	}
	opened, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	openedInfo, openedErr := opened.Stat(".")
	currentInfo, currentErr := os.Lstat(absolute)
	if openedErr != nil || currentErr != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		_ = opened.Close()
		return nil, fmt.Errorf("archive root %q changed while it was opened", absolute)
	}
	return opened, nil
}

// ensureArchiveDirectory refuses every pre-existing symlink component. os.Root
// additionally anchors every operation to the opened directory descriptor, so
// a concurrent rename cannot redirect extraction outside root.
func ensureArchiveDirectory(root *os.Root, relative string, mode os.FileMode) error {
	relative = filepath.Clean(relative)
	if relative == "." || relative == "" {
		return nil
	}
	current := ""
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			return fmt.Errorf("archive directory escapes extraction root")
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(current, mode.Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("archive path component %q is not a real directory", current)
		}
	}
	return nil
}

func archiveFileMode(mode os.FileMode) os.FileMode {
	permissions := mode.Perm() & 0o755
	if permissions&0o400 == 0 {
		permissions |= 0o400
	}
	if permissions&0o200 == 0 {
		permissions |= 0o200
	}
	return permissions
}

func archiveTempName(directory string) (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return filepath.Join(directory, ".pcvm-extract-"+hex.EncodeToString(bytes)), nil
}

func writeArchiveRegularAt(root *os.Root, relative string, source io.Reader, mode os.FileMode, expected int64) error {
	relative = filepath.Clean(relative)
	if expected < 0 {
		return fmt.Errorf("archive file %q has a negative size", relative)
	}
	if err := ensureArchiveDirectory(root, filepath.Dir(relative), 0o750); err != nil {
		return err
	}
	if info, err := root.Lstat(relative); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular archive target %q", relative)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	temporary, err := archiveTempName(filepath.Dir(relative))
	if err != nil {
		return err
	}
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveFileMode(mode))
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = root.Remove(temporary)
		}
	}()
	written, copyErr := io.CopyN(file, source, expected)
	if copyErr == nil {
		var extra [1]byte
		if count, readErr := source.Read(extra[:]); count != 0 {
			copyErr = fmt.Errorf("archive file %q exceeds its declared size", relative)
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			copyErr = readErr
		}
	}
	if copyErr == nil && written != expected {
		copyErr = fmt.Errorf("archive file %q is truncated", relative)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if _, err := root.Lstat(relative); err == nil {
		if err := root.Remove(relative); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := root.Rename(temporary, relative); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

func writeArchiveRegular(root, target string, source io.Reader, mode os.FileMode, expected int64) error {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive target %q escapes extraction root", target)
	}
	rootFS, err := openArchiveRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	return writeArchiveRegularAt(rootFS, relative, source, mode, expected)
}

func extractZipSafe(archive, destination string) error {
	return extractZipSafeWithLimits(archive, destination, defaultArchiveLimits)
}

func extractZipSafeWithLimits(archive, destination string, limits archiveLimits) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	root, err := openArchiveRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	budget := newArchiveBudget(limits)
	for _, file := range reader.File {
		clean, _, err := archiveTarget(destination, file.Name)
		if err != nil {
			return err
		}
		relative := filepath.FromSlash(clean)
		if file.FileInfo().IsDir() {
			if err := budget.add(clean, archiveEntryDirectory, 0); err != nil {
				return err
			}
			if err := ensureArchiveDirectory(root, relative, 0o750); err != nil {
				return err
			}
			continue
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive may not contain symlink %q", file.Name)
		}
		if !file.Mode().IsRegular() {
			return fmt.Errorf("archive contains unsupported entry %q", file.Name)
		}
		if file.UncompressedSize64 > uint64(^uint64(0)>>1) {
			return fmt.Errorf("archive file %q is too large", file.Name)
		}
		size := int64(file.UncompressedSize64)
		if err := budget.add(clean, archiveEntryRegular, size); err != nil {
			return err
		}
		if file.CompressedSize64 > uint64(^uint64(0)>>1) {
			return fmt.Errorf("archive file %q has an invalid compressed size", file.Name)
		}
		if err := (&archiveBudget{limits: limits, total: size}).checkCompression(clean, int64(file.CompressedSize64)); err != nil {
			return err
		}
		if info, err := root.Lstat(relative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing existing symlink archive target %q", clean)
			}
			if isBedrockConfig(clean) {
				continue
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("archive target parent is not a real directory: %w", err)
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		writeErr := writeArchiveRegularAt(root, relative, input, file.Mode()|0o500, size)
		closeErr := input.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type tarExtractOptions struct {
	Limits          archiveLimits
	StripComponents int
	AllowSymlinks   bool
	// CompressedBytes is the size of the gzip/xz input containing this TAR.
	// Leave zero for a plain, uncompressed TAR stream.
	CompressedBytes int64
}

func stripArchiveComponents(name string, count int) (string, bool, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." {
		return "", false, nil
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("unsafe archive path %q", name)
	}
	parts := strings.Split(clean, "/")
	if len(parts) <= count {
		return "", false, nil
	}
	return strings.Join(parts[count:], "/"), true, nil
}

func extractTarStreamSafe(reader *tar.Reader, destination string, options tarExtractOptions) error {
	root, err := openArchiveRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	budget := newArchiveBudget(options.Limits)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean, include, err := stripArchiveComponents(header.Name, options.StripComponents)
		if err != nil {
			return err
		}
		if !include {
			continue
		}
		relative := filepath.FromSlash(clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := budget.add(clean, archiveEntryDirectory, 0); err != nil {
				return err
			}
			if err := ensureArchiveDirectory(root, relative, 0o750); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := budget.add(clean, archiveEntryRegular, header.Size); err != nil {
				return err
			}
			if options.CompressedBytes > 0 {
				if err := budget.checkCompression("compressed TAR", options.CompressedBytes); err != nil {
					return err
				}
			}
			if err := writeArchiveRegularAt(root, relative, reader, os.FileMode(header.Mode), header.Size); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !options.AllowSymlinks {
				return fmt.Errorf("archive may not contain symlink %q", header.Name)
			}
			if err := budget.add(clean, archiveEntrySymlink, 0); err != nil {
				return err
			}
			link := strings.ReplaceAll(header.Linkname, "\\", "/")
			resolved := path.Clean(path.Join(path.Dir(clean), link))
			nativeLink := filepath.FromSlash(link)
			if path.IsAbs(link) || filepath.IsAbs(nativeLink) || filepath.VolumeName(nativeLink) != "" || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("archive symlink escapes extraction root: %q", header.Name)
			}
			if err := ensureArchiveDirectory(root, filepath.Dir(relative), 0o750); err != nil {
				return err
			}
			if _, err := root.Lstat(relative); err == nil {
				return fmt.Errorf("refusing existing archive symlink target %q", clean)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := root.Symlink(filepath.FromSlash(link), relative); err != nil {
				return err
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// archive/tar applies PAX metadata to the following entry.
		case tar.TypeLink:
			return fmt.Errorf("archive may not contain hard link %q", header.Name)
		default:
			return fmt.Errorf("archive contains unsupported entry %q", header.Name)
		}
	}
}

func extractTarXZSafe(ctx context.Context, archive, destination string, stripComponents int) error {
	info, err := os.Stat(archive)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("XZ archive must be a non-empty regular file")
	}
	command := exec.CommandContext(ctx, "xz", "-dc", "--", archive)
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	command.Stderr = &limitedStringWriter{builder: &stderr, remaining: 64 << 10}
	if err := command.Start(); err != nil {
		return err
	}
	extractErr := extractTarStreamSafe(tar.NewReader(output), destination, tarExtractOptions{
		Limits: defaultArchiveLimits, StripComponents: stripComponents, CompressedBytes: info.Size(),
	})
	if extractErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("decompress xz archive: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractTarGzipSafe(archive, destination string, allowSymlinks bool) error {
	info, err := os.Stat(archive)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("gzip archive must be a non-empty regular file")
	}
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	return extractTarStreamSafe(tar.NewReader(gz), destination, tarExtractOptions{
		Limits: defaultArchiveLimits, AllowSymlinks: allowSymlinks, CompressedBytes: info.Size(),
	})
}

type limitedStringWriter struct {
	builder   *strings.Builder
	remaining int
}

func (w *limitedStringWriter) Write(data []byte) (int, error) {
	written := len(data)
	if w.remaining > 0 {
		keep := len(data)
		if keep > w.remaining {
			keep = w.remaining
		}
		_, _ = w.builder.Write(data[:keep])
		w.remaining -= keep
	}
	return written, nil
}

func isBedrockConfig(name string) bool {
	switch filepath.Base(name) {
	case "server.properties", "allowlist.json", "permissions.json":
		return true
	}
	return false
}
