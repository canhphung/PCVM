package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/canhphung/PCVM/internal/pcvm"
)

var (
	version      = "dev"
	imageProfile = pcvm.ImageProfileFull
)

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, in io.Reader, out, errOut io.Writer) int {
	command := "run"
	if len(args) > 1 {
		command = args[1]
	}
	if command == "version" {
		fmt.Fprintln(out, version)
		return 0
	}
	if command == "profile" {
		fmt.Fprintln(out, imageProfile)
		return 0
	}
	if command != "run" {
		return runCommand(command, args[2:], out, errOut)
	}
	if len(args) > 2 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	clearConsole, err := envBool("CLEAR_CONSOLE", true)
	if err != nil {
		return fatal(errOut, err)
	}
	if err := pcvm.ClearConsole(out, clearConsole); err != nil {
		return fatal(errOut, err)
	}
	cfg, err := pcvm.ConfigFromEnv()
	if err != nil {
		return fatal(errOut, err)
	}
	cfg.ImageProfile, err = pcvm.NormalizeImageProfile(imageProfile)
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
	app.Log.Printf("PCVM %s (%s, %s profile)", version, cfg.Arch, cfg.ImageProfile)
	if err = app.Run(context.Background()); err != nil {
		return fatal(errOut, err)
	}
	return 0
}

const usage = "usage: pcvm [run|version|profile|status [--json]|doctor [--json]|catalog list [--json]|verify [--json]|cache status|cache prune|support-bundle]"

func runCommand(command string, args []string, out, errOut io.Writer) int {
	jsonOutput := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonOutput = true
		} else {
			filtered = append(filtered, arg)
		}
	}
	cfg, catalog, err := loadConfigCatalog()
	if err != nil {
		return fatal(errOut, err)
	}
	switch command {
	case "status":
		if len(filtered) != 0 {
			break
		}
		report, err := pcvm.InspectStatus(cfg, catalog)
		if err != nil {
			return fatal(errOut, err)
		}
		if jsonOutput {
			return writeJSON(out, errOut, report)
		}
		if !report.Installed {
			fmt.Fprintln(out, "PCVM is not installed")
		} else {
			fmt.Fprintf(out, "provider=%s install=%s artifact=%s runtime=%s verified=%t\n", report.Provider, report.InstallID, report.Artifact.ID, report.RuntimePack, report.Verified)
		}
		return 0
	case "doctor":
		if len(filtered) != 0 {
			break
		}
		report := pcvm.RunDoctor(cfg, catalog)
		if jsonOutput {
			code := writeJSON(out, errOut, report)
			if code == 0 && !report.OK {
				return 1
			}
			return code
		}
		for _, check := range report.Checks {
			fmt.Fprintf(out, "%-16s %-5s %s\n", check.Name, check.Status, check.Message)
		}
		if !report.OK {
			return 1
		}
		return 0
	case "catalog":
		if len(filtered) != 1 || filtered[0] != "list" {
			break
		}
		providers := catalog.Providers
		if jsonOutput {
			return writeJSON(out, errOut, providers)
		}
		for _, provider := range providers {
			fmt.Fprintf(out, "%-22s %-8s %s\n", provider.ID, provider.SupportTier, provider.Name)
		}
		return 0
	case "verify":
		if len(filtered) != 0 {
			break
		}
		report, err := pcvm.InspectStatus(cfg, catalog)
		if err != nil {
			return fatal(errOut, err)
		}
		if !report.Installed {
			return fatal(errOut, fmt.Errorf("no PCVM installation to verify"))
		}
		if jsonOutput {
			return writeJSON(out, errOut, report)
		}
		fmt.Fprintf(out, "verified provider=%s install=%s\n", report.Provider, report.InstallID)
		return 0
	case "cache":
		if len(filtered) != 1 || (filtered[0] != "status" && filtered[0] != "prune") {
			break
		}
		var report pcvm.CacheReport
		if filtered[0] == "prune" {
			report, err = pcvm.PruneCache(cfg, catalog, pcvm.NewLogger(out))
		} else {
			report, err = pcvm.InspectCache(cfg.Control)
		}
		if err != nil {
			return fatal(errOut, err)
		}
		if jsonOutput {
			return writeJSON(out, errOut, report)
		}
		fmt.Fprintf(out, "cache files=%d bytes=%d\n", report.Files, report.Bytes)
		return 0
	case "support-bundle":
		if len(filtered) != 0 || jsonOutput {
			break
		}
		path, err := pcvm.CreateSupportBundle(cfg, catalog, time.Now())
		if err != nil {
			return fatal(errOut, err)
		}
		fmt.Fprintln(out, path)
		return 0
	}
	fmt.Fprintln(errOut, usage)
	return 2
}

func loadConfigCatalog() (pcvm.Config, pcvm.Catalog, error) {
	cfg, err := pcvm.ConfigFromEnv()
	if err != nil {
		return pcvm.Config{}, pcvm.Catalog{}, err
	}
	cfg.ImageProfile, err = pcvm.NormalizeImageProfile(imageProfile)
	if err != nil {
		return pcvm.Config{}, pcvm.Catalog{}, err
	}
	manifestPath := os.Getenv("PCVM_RUNTIME_MANIFEST")
	if manifestPath == "" {
		manifestPath = filepath.Join("/opt/pcvm", "runtime-manifest.json")
	}
	manifest, err := pcvm.LoadRuntimeManifest(manifestPath)
	if err != nil {
		return pcvm.Config{}, pcvm.Catalog{}, err
	}
	catalog, err := pcvm.LoadCatalog(manifest)
	return cfg, catalog, err
}

func writeJSON(out, errOut io.Writer, value any) int {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fatal(errOut, err)
	}
	return 0
}

func fatal(out io.Writer, err error) int { fmt.Fprintf(out, "[PCVM] ERROR: %v\n", err); return 1 }

func envBool(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (0/1 or true/false)", key)
	}
	return parsed, nil
}
