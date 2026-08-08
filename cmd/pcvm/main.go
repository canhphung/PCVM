package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/canhphung/PCVM/internal/pcvm"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) > 1 && args[1] == "version" {
		fmt.Fprintln(out, version)
		return 0
	}
	if len(args) > 1 && args[1] != "run" {
		fmt.Fprintln(errOut, "usage: pcvm [run|version]")
		return 2
	}
	if err := pcvm.ClearConsole(out, envBool("CLEAR_CONSOLE", true)); err != nil {
		return fatal(errOut, err)
	}
	cfg, err := pcvm.ConfigFromEnv()
	if err != nil {
		return fatal(errOut, err)
	}
	manifestPath := os.Getenv("PCVM_RUNTIME_MANIFEST")
	if manifestPath == "" {
		manifestPath = filepath.Join("/opt/pcvm", "runtime-manifest.json")
	}
	manifest, err := pcvm.LoadRuntimeManifest(manifestPath)
	if err != nil {
		return fatal(errOut, err)
	}
	catalog, err := pcvm.LoadCatalog(manifest)
	if err != nil {
		return fatal(errOut, err)
	}
	app := pcvm.NewApp(cfg, catalog, in, out, errOut)
	app.Log.Printf("PCVM %s (%s)", version, cfg.Arch)
	if err = app.Run(context.Background()); err != nil {
		return fatal(errOut, err)
	}
	return 0
}

func fatal(out io.Writer, err error) int { fmt.Fprintf(out, "[PCVM] ERROR: %v\n", err); return 1 }

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value != "0" && value != "false" && value != "FALSE"
}
