package pcvm

import (
	"fmt"
	"strconv"
	"strings"
)

const memoryMiB int64 = 1024 * 1024

type MemorySnapshot struct {
	Source       string
	LimitMB      int
	CurrentMB    int
	CurrentKnown bool
	OOMKills     uint64
	OOMSource    string
}

type MemoryPlan struct {
	Strategy         string
	Source           string
	LimitMB          int
	CurrentMB        int
	CurrentKnown     bool
	TargetMB         int
	ReserveMB        int
	RecommendedMB    int
	HardMinimumMB    int
	BelowRecommended bool
	UnknownLimit     bool
	OOMKills         uint64
	OOMSource        string
}

func readMemorySnapshot() MemorySnapshot {
	return readMemorySnapshotWith(DefaultDependencies())
}

func readMemorySnapshotWith(dependencies Dependencies) MemorySnapshot {
	dependencies = dependencies.withDefaults()
	readFile := dependencies.ReadFile
	snapshot := MemorySnapshot{}
	if raw, err := readFile("/sys/fs/cgroup/memory.max"); err == nil {
		if current, readErr := readFile("/sys/fs/cgroup/memory.current"); readErr == nil {
			snapshot.CurrentMB = parseCgroupUsage(string(current))
			snapshot.CurrentKnown = validCgroupUsage(string(current))
		}
		if limit := parseCgroupMemory(string(raw)); limit > 0 {
			snapshot.Source, snapshot.LimitMB = "cgroup-v2", limit
		}
	}
	if snapshot.LimitMB == 0 {
		if raw, err := readFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
			if limit := parseCgroupMemory(string(raw)); limit > 0 {
				snapshot.Source, snapshot.LimitMB = "cgroup-v1", limit
				if current, readErr := readFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); readErr == nil {
					snapshot.CurrentMB = parseCgroupUsage(string(current))
					snapshot.CurrentKnown = validCgroupUsage(string(current))
				}
			}
		}
	}
	if events, err := readFile("/sys/fs/cgroup/memory.events"); err == nil {
		snapshot.OOMKills = parseMemoryEvent(string(events), "oom_kill")
		snapshot.OOMSource = "cgroup-v2"
	}
	if snapshot.OOMSource == "" {
		if control, err := readFile("/sys/fs/cgroup/memory/memory.oom_control"); err == nil {
			snapshot.OOMKills = parseMemoryEvent(string(control), "oom_kill")
			snapshot.OOMSource = "cgroup-v1"
		}
	}
	if snapshot.LimitMB == 0 {
		if limit, ok := parseServerMemory(dependencies.Getenv("SERVER_MEMORY")); ok {
			snapshot.Source, snapshot.LimitMB = "SERVER_MEMORY", limit
		}
	}
	if snapshot.Source == "" {
		snapshot.Source = "unknown"
	}
	return snapshot
}

func parseCgroupMemory(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "max" {
		return 0
	}
	bytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || bytes <= 0 || bytes >= 1<<60 {
		return 0
	}
	return int(bytes / memoryMiB)
}

func parseCgroupUsage(raw string) int {
	bytes, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || bytes < 0 {
		return 0
	}
	return int(bytes / memoryMiB)
}

func validCgroupUsage(raw string) bool {
	bytes, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return err == nil && bytes >= 0
}

func parseServerMemory(raw string) (int, bool) {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if raw == "" {
		return 0, false
	}
	multiplier := int64(1)
	if strings.HasSuffix(raw, "M") {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "M"))
	} else if strings.HasSuffix(raw, "G") {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "G"))
		multiplier = 1024
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || value > int64(^uint(0)>>1)/multiplier {
		return 0, false
	}
	return int(value * multiplier), true
}

func parseMemoryEvent(raw, name string) uint64 {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != name {
			continue
		}
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		return value
	}
	return 0
}

func planMemory(spec MemorySpec, req Request, policy Policy, snapshot MemorySnapshot) (MemoryPlan, error) {
	plan := MemoryPlan{
		Strategy: spec.Strategy, Source: snapshot.Source, LimitMB: snapshot.LimitMB, CurrentMB: snapshot.CurrentMB, CurrentKnown: snapshot.CurrentKnown,
		RecommendedMB: spec.RecommendedMB, HardMinimumMB: spec.HardMinimumMB, UnknownLimit: snapshot.LimitMB == 0,
		OOMKills: snapshot.OOMKills, OOMSource: snapshot.OOMSource,
	}
	if plan.Source == "" {
		plan.Source = "unknown"
	}
	if plan.LimitMB > 0 {
		if spec.HardMinimumMB > 0 && plan.LimitMB < spec.HardMinimumMB {
			return MemoryPlan{}, fmt.Errorf("%s requires at least %d MB of container memory; allocation is %d MB", spec.Strategy, spec.HardMinimumMB, plan.LimitMB)
		}
		plan.BelowRecommended = spec.RecommendedMB > 0 && plan.LimitMB < spec.RecommendedMB
	}

	switch spec.Strategy {
	case "jvm-heap":
		if plan.LimitMB == 0 {
			plan.TargetMB = 1024
		} else {
			plan.TargetMB = roundMemory(plan.LimitMB*80/100, 64)
		}
	case "node-heap", "deno-heap", "go-limit", "php-limit", "dotnet-gc":
		if plan.LimitMB > 0 {
			plan.TargetMB = roundMemory(plan.LimitMB*75/100, 64)
		}
	case "qemu-guest":
		memory := 0
		if req.VMMemoryMB == "" || req.VMMemoryMB == "auto" {
			if plan.LimitMB == 0 {
				memory = vmDefaultMemoryMB
			} else {
				memory = roundMemory(plan.LimitMB*75/100, 128)
			}
			if policy.VMMaxMemoryMB > 0 && memory > policy.VMMaxMemoryMB {
				memory = roundMemory(policy.VMMaxMemoryMB, 128)
			}
		} else {
			parsed, err := strconv.Atoi(req.VMMemoryMB)
			if err != nil {
				return MemoryPlan{}, fmt.Errorf("VM_MEMORY_MB must be auto or an integer")
			}
			memory = parsed
		}
		if memory < 768 || policy.VMMaxMemoryMB > 0 && memory > policy.VMMaxMemoryMB {
			return MemoryPlan{}, fmt.Errorf("VM_MEMORY_MB must be at least 768 and no more than VM_MAX_MEMORY_MB (%d)", policy.VMMaxMemoryMB)
		}
		if plan.LimitMB > 0 && memory > plan.LimitMB-vmHostReserveMB {
			return MemoryPlan{}, fmt.Errorf("VM_MEMORY_MB must leave at least %d MB for QEMU and PCVM", vmHostReserveMB)
		}
		plan.TargetMB = memory
	case "cgroup-only":
		if plan.LimitMB > 0 {
			plan.TargetMB = plan.LimitMB
		}
	default:
		return MemoryPlan{}, fmt.Errorf("unsupported memory strategy %q", spec.Strategy)
	}
	if plan.LimitMB > 0 && plan.TargetMB > 0 {
		plan.ReserveMB = plan.LimitMB - plan.TargetMB
	}
	return plan, nil
}

func roundMemory(value, unit int) int {
	if value <= 0 || unit <= 0 {
		return 0
	}
	return value / unit * unit
}

func memoryOOMKilled(before, after MemorySnapshot) bool {
	return before.OOMSource != "" && before.OOMSource == after.OOMSource && after.OOMKills > before.OOMKills
}

func applyMemoryPlan(spec ProviderSpec, process ProcessSpec, plan MemoryPlan) (ProcessSpec, error) {
	if plan.Strategy != spec.Memory.Strategy {
		return ProcessSpec{}, fmt.Errorf("memory plan strategy %q does not match provider strategy %q", plan.Strategy, spec.Memory.Strategy)
	}
	switch plan.Strategy {
	case "jvm-heap":
		if plan.TargetMB <= 0 {
			return ProcessSpec{}, fmt.Errorf("JVM memory plan has no heap target")
		}
		flags := []string{"-Xms128M", fmt.Sprintf("-Xmx%dM", plan.TargetMB)}
		index := -1
		for i, arg := range process.Command {
			if arg == "-jar" || strings.HasPrefix(arg, "@libraries") {
				index = i
				break
			}
		}
		if index < 1 {
			return ProcessSpec{}, fmt.Errorf("cannot locate safe JVM memory argument position")
		}
		process.Command = append(append(append([]string(nil), process.Command[:index]...), flags...), process.Command[index:]...)
	case "node-heap":
		if plan.TargetMB > 0 {
			process.Environment = upsertEnvironment(process.Environment, "NODE_OPTIONS", fmt.Sprintf("--max-old-space-size=%d", plan.TargetMB))
		}
	case "deno-heap":
		if plan.TargetMB > 0 {
			process.Environment = upsertEnvironment(process.Environment, "DENO_V8_FLAGS", fmt.Sprintf("--max-old-space-size=%d", plan.TargetMB))
		}
	case "go-limit":
		if plan.TargetMB > 0 {
			process.Environment = upsertEnvironment(process.Environment, "GOMEMLIMIT", fmt.Sprintf("%dMiB", plan.TargetMB))
		}
	case "php-limit":
		if plan.TargetMB > 0 {
			if len(process.Command) < 2 {
				return ProcessSpec{}, fmt.Errorf("PHP process command is incomplete")
			}
			args := []string{"-d", fmt.Sprintf("memory_limit=%dM", plan.TargetMB)}
			process.Command = append(append(append([]string(nil), process.Command[:1]...), args...), process.Command[1:]...)
		}
	case "dotnet-gc":
		if plan.TargetMB > 0 {
			process.Environment = upsertEnvironment(process.Environment, "DOTNET_GCHeapHardLimit", fmt.Sprintf("0x%X", int64(plan.TargetMB)*memoryMiB))
		}
	case "qemu-guest", "cgroup-only":
	default:
		return ProcessSpec{}, fmt.Errorf("unsupported memory strategy %q", plan.Strategy)
	}
	return process, nil
}
