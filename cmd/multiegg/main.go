package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/canhphung/smart-multiegg/internal/multiegg"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: multiegg [run|version]")
		os.Exit(2)
	}
	cfg, err := multiegg.ConfigFromEnv()
	if err != nil {
		fatal(err)
	}
	manifestPath := os.Getenv("MULTIEGG_RUNTIME_MANIFEST")
	if manifestPath == "" {
		manifestPath = filepath.Join("/opt/multiegg", "runtime-manifest.json")
	}
	manifest, err := multiegg.LoadRuntimeManifest(manifestPath)
	if err != nil {
		fatal(err)
	}
	catalog, err := multiegg.LoadCatalog(manifest)
	if err != nil {
		fatal(err)
	}
	app := multiegg.NewApp(cfg, catalog, os.Stdin, os.Stdout, os.Stderr)
	app.Log.Printf("Smart MultiEgg %s (%s)", version, cfg.Arch)
	if err = app.Run(context.Background()); err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintf(os.Stderr, "[MULTIEGG] ERROR: %v\n", err); os.Exit(1) }
