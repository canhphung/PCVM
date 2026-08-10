package pcvm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMirrorURL(t *testing.T) {
	digest := strings.Repeat("a", 64)
	got := mirrorURL("https://mirror.example/runtimes/", digest)
	if got != "https://mirror.example/runtimes/sha256/"+digest {
		t.Fatal(got)
	}
}

func TestCachePruningKeepsCurrentAndOnePrevious(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".pcvm", "cache", "trees", "sha256")
	names := []string{"old", "previous", "current"}
	for i, name := range names {
		path := filepath.Join(root, name)
		mustWrite(t, filepath.Join(path, "payload"))
		when := time.Unix(int64(100+i), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	m := RuntimeManager{Config: Config{Control: filepath.Join(home, ".pcvm"), Policy: Policy{CacheLimitBytes: 1024}}}
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

func TestCachePruningPairsRuntimeArchivesAndUsesGlobalLimit(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	root := filepath.Join(control, "cache", "trees", "sha256")
	trees := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)}
	for i, tree := range trees {
		runtimeRoot := filepath.Join(root, tree)
		if err := os.MkdirAll(runtimeRoot, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeRoot, "payload"), bytes.Repeat([]byte("x"), 8), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(100+i), 0)
		if err := os.Chtimes(runtimeRoot, when, when); err != nil {
			t.Fatal(err)
		}
		receipt := filepath.Join(control, "cache", "runtime-receipts", tree+".json")
		if err := os.MkdirAll(filepath.Dir(receipt), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	blob := filepath.Join(control, "cache", "blobs", "sha256", strings.Repeat("a", 64))
	if err := os.MkdirAll(filepath.Dir(blob), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, bytes.Repeat([]byte("y"), 8), 0o600); err != nil {
		t.Fatal(err)
	}
	m := RuntimeManager{Config: Config{
		Control: control, Arch: "amd64", Policy: Policy{CacheLimitBytes: 100},
	}}
	current := filepath.Join(root, trees[2])
	if err := m.Prune(current); err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{
		current, filepath.Join(control, "cache", "runtime-receipts", trees[2]+".json"),
		filepath.Join(root, trees[1]), filepath.Join(control, "cache", "runtime-receipts", trees[1]+".json"),
	} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatalf("active/previous runtime tree or receipt removed: %s: %v", retained, err)
		}
	}
	for _, removed := range []string{
		filepath.Join(root, trees[0]), filepath.Join(control, "cache", "runtime-receipts", trees[0]+".json"), blob,
	} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("expired cache retained: %s: %v", removed, err)
		}
	}
	m.Config.Policy.CacheLimitBytes = 10
	if err := m.Prune(current); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{
		filepath.Join(root, trees[1]), filepath.Join(control, "cache", "runtime-receipts", trees[1]+".json"),
	} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("previous runtime exceeded global quota but was retained: %s: %v", removed, err)
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
