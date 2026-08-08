package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunClearsBeforeConfigurationErrors(t *testing.T) {
	t.Setenv("CLEAR_CONSOLE", "1")
	t.Setenv("CACHE_LIMIT_MB", "invalid")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pcvm", "run"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if stdout.String() != "\x1b[2J\x1b[H\x1b[3J" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "CACHE_LIMIT_MB") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestVersionAndInvalidSubcommandsDoNotClear(t *testing.T) {
	t.Setenv("CLEAR_CONSOLE", "1")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pcvm", "version"}, strings.NewReader(""), &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "\x1b[2J") {
		t.Fatalf("version code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"pcvm", "unknown"}, strings.NewReader(""), &stdout, &stderr); code != 2 || stdout.Len() != 0 {
		t.Fatalf("invalid code=%d stdout=%q", code, stdout.String())
	}
}
