package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCoreCoverageUsesFixedCompleteScope(t *testing.T) {
	var profile strings.Builder
	profile.WriteString("mode: atomic\n")
	for index, name := range coreFiles {
		count := 0
		if index%2 == 0 {
			count = 1
		}
		profile.WriteString("github.com/canhphung/PCVM/internal/pcvm/" + name + ":1.1,2.1 2 " + string(rune('0'+count)) + "\n")
	}
	path := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(path, []byte(profile.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := readCoreCoverage(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != len(coreFiles) {
		t.Fatalf("scope=%d, want %d", len(result), len(coreFiles))
	}
	if result[coreFiles[0]] != (coverage{total: 2, covered: 2}) || result[coreFiles[1]] != (coverage{total: 2}) {
		t.Fatalf("unexpected parsed coverage: %#v", result)
	}
}

func TestReadCoreCoverageRejectsMissingOrMalformedInput(t *testing.T) {
	for name, contents := range map[string]string{
		"header":  "not-a-profile\n",
		"record":  "mode: set\nbroken\n",
		"missing": "mode: set\ngithub.com/example/other.go:1.1,2.1 1 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "coverage.out")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCoreCoverage(path); err == nil {
				t.Fatal("invalid profile was accepted")
			}
		})
	}
}

func TestPercentage(t *testing.T) {
	if percentage(coverage{}) != 0 || percentage(coverage{covered: 4, total: 5}) != 80 {
		t.Fatal("unexpected percentage calculation")
	}
}
