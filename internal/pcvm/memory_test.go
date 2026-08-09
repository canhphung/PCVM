package pcvm

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func withMemoryFiles(t *testing.T, files map[string]string) {
	t.Helper()
	original := memoryReadFile
	memoryReadFile = func(path string) ([]byte, error) {
		if value, ok := files[path]; ok {
			return []byte(value), nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { memoryReadFile = original })
}

func TestMemorySnapshotPrecedenceAndParsing(t *testing.T) {
	t.Setenv("SERVER_MEMORY", "8G")
	withMemoryFiles(t, map[string]string{
		"/sys/fs/cgroup/memory.max":     "4294967296\n",
		"/sys/fs/cgroup/memory.current": "536870912\n",
		"/sys/fs/cgroup/memory.events":  "low 0\noom 2\noom_kill 3\n",
	})
	snapshot := readMemorySnapshot()
	if snapshot.Source != "cgroup-v2" || snapshot.LimitMB != 4096 || snapshot.CurrentMB != 512 || !snapshot.CurrentKnown || snapshot.OOMKills != 3 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestMemorySnapshotV1AndServerMemoryFallback(t *testing.T) {
	t.Run("v1", func(t *testing.T) {
		t.Setenv("SERVER_MEMORY", "8G")
		withMemoryFiles(t, map[string]string{
			"/sys/fs/cgroup/memory.max":                   "max\n",
			"/sys/fs/cgroup/memory/memory.limit_in_bytes": "2147483648\n",
			"/sys/fs/cgroup/memory/memory.usage_in_bytes": "268435456\n",
			"/sys/fs/cgroup/memory/memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\noom_kill 4\n",
		})
		snapshot := readMemorySnapshot()
		if snapshot.Source != "cgroup-v1" || snapshot.LimitMB != 2048 || snapshot.CurrentMB != 256 || !snapshot.CurrentKnown || snapshot.OOMKills != 4 || snapshot.OOMSource != "cgroup-v1" {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("SERVER_MEMORY", "2G")
		withMemoryFiles(t, nil)
		snapshot := readMemorySnapshot()
		if snapshot.Source != "SERVER_MEMORY" || snapshot.LimitMB != 2048 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Setenv("SERVER_MEMORY", "invalid")
		withMemoryFiles(t, nil)
		if snapshot := readMemorySnapshot(); snapshot.Source != "unknown" || snapshot.LimitMB != 0 {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	t.Run("malformed cgroups", func(t *testing.T) {
		t.Setenv("SERVER_MEMORY", "512M")
		withMemoryFiles(t, map[string]string{
			"/sys/fs/cgroup/memory.max":                   "not-a-number\n",
			"/sys/fs/cgroup/memory.current":               "also-invalid\n",
			"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
		})
		snapshot := readMemorySnapshot()
		if snapshot.Source != "SERVER_MEMORY" || snapshot.LimitMB != 512 || snapshot.CurrentKnown {
			t.Fatalf("snapshot=%+v", snapshot)
		}
	})
	for raw, want := range map[string]int{"1024": 1024, "512M": 512, "2g": 2048} {
		if got, ok := parseServerMemory(raw); !ok || got != want {
			t.Fatalf("parseServerMemory(%q)=%d,%v", raw, got, ok)
		}
	}
}

func TestMemoryPlanLogFormat(t *testing.T) {
	var output bytes.Buffer
	app := &App{Log: NewLogger(&output)}
	app.logMemoryPlan(MemoryPlan{
		Strategy: "jvm-heap", Source: "cgroup-v2", LimitMB: 4096, CurrentMB: 512, CurrentKnown: true,
		TargetMB: 3264, ReserveMB: 832, RecommendedMB: 1024, OOMKills: 2, OOMSource: "cgroup-v2",
	})
	want := "[PCVM] MEMORY source=cgroup-v2 limit=4096MB target=3264MB reserve=832MB strategy=jvm-heap recommended=1024MB\n"
	if !strings.Contains(output.String(), want) || !strings.Contains(output.String(), "[PCVM] MEMORY diagnostics current=512MB oom_kill=2") {
		t.Fatalf("memory log=%q", output.String())
	}
}

func TestBalancedMemoryPlans(t *testing.T) {
	policy := vmTestPolicy()
	tests := []struct {
		name            string
		spec            MemorySpec
		snapshot        MemorySnapshot
		req             Request
		target, reserve int
		unknown, below  bool
	}{
		{"jvm", MemorySpec{Strategy: "jvm-heap", RecommendedMB: 1024, HardMinimumMB: 384}, MemorySnapshot{Source: "cgroup-v2", LimitMB: 4096}, Request{}, 3264, 832, false, false},
		{"node", MemorySpec{Strategy: "node-heap", RecommendedMB: 512, HardMinimumMB: 256}, MemorySnapshot{Source: "cgroup-v2", LimitMB: 4096}, Request{}, 3072, 1024, false, false},
		{"native warning", MemorySpec{Strategy: "cgroup-only", RecommendedMB: 2048}, MemorySnapshot{Source: "cgroup-v2", LimitMB: 1024}, Request{}, 1024, 0, false, true},
		{"unknown jvm", MemorySpec{Strategy: "jvm-heap", RecommendedMB: 1024, HardMinimumMB: 384}, MemorySnapshot{Source: "unknown"}, Request{}, 1024, 0, true, false},
		{"unknown node", MemorySpec{Strategy: "node-heap", RecommendedMB: 256, HardMinimumMB: 256}, MemorySnapshot{Source: "unknown"}, Request{}, 0, 0, true, false},
		{"qemu auto", MemorySpec{Strategy: "qemu-guest", RecommendedMB: 1536, HardMinimumMB: 1536}, MemorySnapshot{Source: "cgroup-v2", LimitMB: 2048}, Request{VMMemoryMB: "auto"}, 1536, 512, false, false},
		{"qemu manual", MemorySpec{Strategy: "qemu-guest", RecommendedMB: 1536, HardMinimumMB: 1536}, MemorySnapshot{Source: "cgroup-v2", LimitMB: 4096}, Request{VMMemoryMB: "2048"}, 2048, 2048, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := planMemory(test.spec, test.req, policy, test.snapshot)
			if err != nil || plan.TargetMB != test.target || plan.ReserveMB != test.reserve || plan.UnknownLimit != test.unknown || plan.BelowRecommended != test.below {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
		})
	}
	if _, err := planMemory(MemorySpec{Strategy: "jvm-heap", RecommendedMB: 1024, HardMinimumMB: 384}, Request{}, policy, MemorySnapshot{LimitMB: 256}); err == nil {
		t.Fatal("hard minimum was not enforced")
	}
}

func TestApplyMemoryPlanUsesSafeRuntimeInterfaces(t *testing.T) {
	jvmSpec := ProviderSpec{Memory: MemorySpec{Strategy: "jvm-heap"}}
	jvmPlan := MemoryPlan{Strategy: "jvm-heap", TargetMB: 3264}
	jar, err := applyMemoryPlan(jvmSpec, ProcessSpec{Command: []string{"java", "-jar", "server.jar"}}, jvmPlan)
	if err != nil || !reflect.DeepEqual(jar.Command, []string{"java", "-Xms128M", "-Xmx3264M", "-jar", "server.jar"}) {
		t.Fatalf("jar=%v err=%v", jar.Command, err)
	}
	forge, err := applyMemoryPlan(jvmSpec, ProcessSpec{Command: []string{"java", "@user_jvm_args.txt", "@libraries/forge/unix_args.txt", "nogui"}}, jvmPlan)
	if err != nil || strings.Join(forge.Command, " ") != "java @user_jvm_args.txt -Xms128M -Xmx3264M @libraries/forge/unix_args.txt nogui" {
		t.Fatalf("forge=%v err=%v", forge.Command, err)
	}

	tests := []struct {
		strategy string
		process  ProcessSpec
		plan     MemoryPlan
		want     string
	}{
		{"node-heap", ProcessSpec{Command: []string{"node", "index.js"}}, MemoryPlan{Strategy: "node-heap", TargetMB: 768}, "NODE_OPTIONS=--max-old-space-size=768"},
		{"php-limit", ProcessSpec{Command: []string{"php", "server.phar"}}, MemoryPlan{Strategy: "php-limit", TargetMB: 384}, "php -d memory_limit=384M server.phar"},
		{"dotnet-gc", ProcessSpec{Command: []string{"dotnet", "server.dll"}}, MemoryPlan{Strategy: "dotnet-gc", TargetMB: 768}, "DOTNET_GCHeapHardLimit=0x30000000"},
	}
	for _, test := range tests {
		t.Run(test.strategy, func(t *testing.T) {
			spec := ProviderSpec{Memory: MemorySpec{Strategy: test.strategy}}
			got, err := applyMemoryPlan(spec, test.process, test.plan)
			if err != nil || !strings.Contains(strings.Join(append(got.Command, got.Environment...), " "), test.want) {
				t.Fatalf("process=%+v err=%v", got, err)
			}
		})
	}
	native := ProcessSpec{Command: []string{"server", "--port", "1234"}, Environment: []string{"A=B"}}
	got, err := applyMemoryPlan(ProviderSpec{Memory: MemorySpec{Strategy: "cgroup-only"}}, native, MemoryPlan{Strategy: "cgroup-only", TargetMB: 1024})
	if err != nil || !reflect.DeepEqual(got, native) {
		t.Fatalf("cgroup-only process changed: %+v err=%v", got, err)
	}
}

func TestMemoryOOMKillDetection(t *testing.T) {
	before := MemorySnapshot{OOMSource: "cgroup-v2", OOMKills: 2}
	if !memoryOOMKilled(before, MemorySnapshot{OOMSource: "cgroup-v2", OOMKills: 3}) {
		t.Fatal("OOM kill increment was not detected")
	}
	if memoryOOMKilled(before, MemorySnapshot{OOMSource: "cgroup-v1", OOMKills: 3}) || memoryOOMKilled(before, MemorySnapshot{OOMSource: "cgroup-v2", OOMKills: 2}) {
		t.Fatal("false OOM kill detection")
	}
}

func TestMemoryFileReaderUsesMissingFileErrors(t *testing.T) {
	withMemoryFiles(t, nil)
	if _, err := memoryReadFile("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected missing-file error: %v", err)
	}
}
