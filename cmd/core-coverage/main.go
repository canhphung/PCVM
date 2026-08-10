// Command core-coverage enforces the PCVM core coverage release gate.
//
// The scope is deliberately explicit and stable. It covers the security and
// lifecycle code which turns untrusted startup input into a verified launch:
// catalog/config/state reconciliation, reset transactions and receipts,
// resource planning, proxy/archive/argument hardening, and runtime manifests.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var coreFiles = []string{
	"archive_fs.go",
	"catalog.go",
	"config.go",
	"game_args.go",
	"memory.go",
	"operation.go",
	"receipt.go",
	"reconcile.go",
	"reset.go",
	"runtime_manifest.go",
	"safe_proxy.go",
	"state.go",
}

type coverage struct {
	total   int64
	covered int64
}

func main() {
	profile := flag.String("profile", "coverage.out", "Go cover profile to inspect")
	threshold := flag.Float64("threshold", 80, "minimum aggregate core statement coverage")
	flag.Parse()

	result, err := readCoreCoverage(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "core coverage:", err)
		os.Exit(2)
	}

	var total, covered int64
	for _, name := range coreFiles {
		item := result[name]
		percent := percentage(item)
		fmt.Printf("%-20s %5.1f%% (%d/%d statements)\n", name, percent, item.covered, item.total)
		total += item.total
		covered += item.covered
	}
	overall := percentage(coverage{total: total, covered: covered})
	fmt.Printf("core total           %5.1f%% (%d/%d statements; required %.1f%%)\n", overall, covered, total, *threshold)
	if overall+1e-9 < *threshold {
		os.Exit(1)
	}
}

func readCoreCoverage(profile string) (map[string]coverage, error) {
	file, err := os.Open(profile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	wanted := make(map[string]bool, len(coreFiles))
	result := make(map[string]coverage, len(coreFiles))
	for _, name := range coreFiles {
		wanted[name] = true
	}

	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if line == 1 {
			if !strings.HasPrefix(text, "mode: ") {
				return nil, fmt.Errorf("%s has no Go coverage mode header", profile)
			}
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d has malformed coverage data", profile, line)
		}
		location := fields[0]
		colon := strings.LastIndexByte(location, ':')
		if colon < 0 {
			return nil, fmt.Errorf("%s:%d has malformed source location", profile, line)
		}
		name := filepath.Base(filepath.FromSlash(location[:colon]))
		if !wanted[name] || !strings.Contains(filepath.ToSlash(location[:colon]), "/internal/pcvm/") {
			continue
		}
		statements, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || statements < 0 {
			return nil, fmt.Errorf("%s:%d has invalid statement count", profile, line)
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || count < 0 {
			return nil, fmt.Errorf("%s:%d has invalid execution count", profile, line)
		}
		item := result[name]
		item.total += statements
		if count > 0 {
			item.covered += statements
		}
		result[name] = item
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var missing []string
	for _, name := range coreFiles {
		if result[name].total == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("profile does not contain core files: %s", strings.Join(missing, ", "))
	}
	return result, nil
}

func percentage(item coverage) float64 {
	if item.total == 0 {
		return 0
	}
	return 100 * float64(item.covered) / float64(item.total)
}
