package pcvm

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzArchiveTargetStaysWithinRoot(f *testing.F) {
	for _, seed := range []string{
		"payload.txt", "dir/payload.txt", "../escape", "/absolute", `C:\escape`,
		`dir\..\escape`, "./nested/file", "", ".", "..",
	} {
		f.Add(seed)
	}
	root := filepath.Join(f.TempDir(), "root")
	f.Fuzz(func(t *testing.T, name string) {
		if len(name) > 16<<10 {
			t.Skip()
		}
		_, target, err := archiveTarget(root, name)
		if err != nil {
			return
		}
		relative, err := filepath.Rel(root, target)
		if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("archiveTarget(%q) escaped root: target=%q relative=%q err=%v", name, target, relative, err)
		}
	})
}

func FuzzGitURLValidation(f *testing.F) {
	for _, seed := range []string{
		"https://github.com/acme/project.git", "http://github.com/a/b", "https://token@github.com/a/b",
		"https://127.0.0.1/repo", "git@github.com:a/b", "https://github.com.evil.invalid/a/b",
	} {
		f.Add(seed)
	}
	cfg := Config{Policy: Policy{AllowedGitHosts: map[string]bool{"github.com": true}}}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 8<<10 {
			t.Skip()
		}
		if err := cfg.ValidateGitURL(raw); err != nil {
			return
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.User != nil {
			t.Fatalf("ValidateGitURL accepted unsafe URL %q: parsed=%#v err=%v", raw, parsed, err)
		}
	})
}

func FuzzStateParserIsReadOnly(f *testing.F) {
	for _, seed := range [][]byte{
		{}, []byte(`{"schema":1}`), []byte(`{"schema":4}`), []byte(`{"schema":999}`),
		[]byte(`{"schema":4,"provider":"paper","unknown":true}`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		control := t.TempDir()
		path := filepath.Join(control, "state.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = LoadState(control)
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("LoadState removed its input: %v", err)
		}
		if !bytes.Equal(after, data) {
			t.Fatal("LoadState modified untrusted state input")
		}
	})
}
