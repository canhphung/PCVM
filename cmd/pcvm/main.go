package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/canhphung/PCVM/internal/pcvm"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: pcvm [run|version]")
		os.Exit(2)
	}
	cfg, err := pcvm.ConfigFromEnv()
	if err != nil {
		fatal(err)
	}
	manifestPath := os.Getenv("PCVM_RUNTIME_MANIFEST")
	if manifestPath == "" {
		manifestPath = filepath.Join("/opt/pcvm", "runtime-manifest.json")
	}
	manifest, err := pcvm.LoadRuntimeManifest(manifestPath)
	if err != nil {
		fatal(err)
	}
	catalog, err := pcvm.LoadCatalog(manifest)
	if err != nil {
		fatal(err)
	}
	app := pcvm.NewApp(cfg, catalog, os.Stdin, os.Stdout, os.Stderr)
	app.Log.Printf("PCVM %s (%s)", version, cfg.Arch)
	if err = app.Run(context.Background()); err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintf(os.Stderr, "[PCVM] ERROR: %v\n", err); os.Exit(1) }
