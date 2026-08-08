package multiegg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMirrorURL(t *testing.T) {
	got := mirrorURL("https://mirror.example/runtimes/", "https://github.com/acme/release/node.tar.gz")
	if got != "https://mirror.example/runtimes/node.tar.gz" {
		t.Fatal(got)
	}
}

func TestCachePruningKeepsCurrentAndOnePrevious(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".multiegg", "cache", "runtimes")
	names := []string{"old", "previous", "current"}
	for i, name := range names {
		path := filepath.Join(root, name)
		mustWrite(t, filepath.Join(path, "payload"))
		when := time.Unix(int64(100+i), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	m := RuntimeManager{Config: Config{Control: filepath.Join(home, ".multiegg"), Policy: Policy{CacheLimitBytes: 1024}}}
	if err := m.Prune(filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Fatal("old runtime retained")
	}
	for _, name := range []string{"previous", "current"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("%s removed: %v", name, err)
		}
	}
}

func TestRuntimeArchiveAllowsInternalSymlinkAndRejectsEscape(t *testing.T) {
	makeArchive := func(link string) string {
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: "runtime/lib.so.1", Mode: 0o644, Size: 1})
		_, _ = tw.Write([]byte("x"))
		_ = tw.WriteHeader(&tar.Header{Name: "runtime/lib.so", Typeflag: tar.TypeSymlink, Linkname: link, Mode: 0o777})
		_ = tw.Close()
		_ = gz.Close()
		path := filepath.Join(t.TempDir(), "runtime.tar.gz")
		if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	dst := t.TempDir()
	if err := extractRuntime(makeArchive("lib.so.1"), dst, "tar.gz"); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "runtime", "lib.so")); err != nil || target != "lib.so.1" {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if err := extractRuntime(makeArchive("../../../outside"), t.TempDir(), "tar.gz"); err == nil {
		t.Fatal("accepted escaping symlink")
	}
}
