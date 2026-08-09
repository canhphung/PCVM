package pcvm

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func vmTestPolicy() Policy {
	return Policy{VMMaxMemoryMB: 16384, VMMaxCPUs: 8, VMMaxDiskGB: 64}
}

func TestResolveVMImageLatestAndArchitecture(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := c.Provider("vm-ubuntu")
	artifact, err := resolveVMImage(spec, Request{Version: "latest", Build: "latest", Architecture: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "26.04" || artifact.Build != "20260723" || artifact.Metadata["architecture"] != "arm64" ||
		artifact.Metadata["vm_image_variant"] != "minimal" || artifact.Metadata["vm_image_id"] == "" {
		t.Fatalf("unexpected latest artifact: %#v", artifact)
	}
	artifact, err = resolveVMImage(spec, Request{Version: "24.04", Build: "20260801", Architecture: "amd64"})
	if err != nil || artifact.Version != "24.04" {
		t.Fatalf("pinned resolve=%#v err=%v", artifact, err)
	}
	if _, err := resolveVMImage(spec, Request{Version: "24.04", Build: "missing", Architecture: "amd64"}); err == nil {
		t.Fatal("accepted unknown VM build")
	}
	alpine, _ := c.Provider("vm-alpine")
	artifact, err = resolveVMImage(alpine, Request{Version: "latest", Build: "latest", Architecture: "amd64"})
	if err != nil || artifact.Version != "3.24" || artifact.Build != "3.24.1-r0" || artifact.Metadata["vm_image_variant"] != "cloudinit" {
		t.Fatalf("unexpected latest Alpine artifact: %#v err=%v", artifact, err)
	}
}

func TestLegacyUbuntuImageCanonicalizesButIsNotResolved(t *testing.T) {
	c, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := c.Provider("vm-ubuntu")
	var legacy VMImageSpec
	for _, image := range spec.VMImages {
		if image.Deprecated && image.Architecture == "amd64" && image.Version == "24.04" {
			legacy = image
			break
		}
	}
	if legacy.ID == "" {
		t.Fatal("embedded catalog has no legacy Ubuntu Standard image")
	}
	artifact := Artifact{URL: legacy.URL, Version: legacy.Version, Build: legacy.Build, SHA256: legacy.SHA256}
	canonical, ok := findVMImageForArtifact(spec, artifact, "amd64")
	if !ok || canonical.ID != legacy.ID || canonical.Variant != "standard" {
		t.Fatalf("legacy Ubuntu image was not canonicalized: %#v", canonical)
	}
	fresh, err := resolveVMImage(spec, Request{Version: "24.04", Build: "latest", Architecture: "amd64"})
	if err != nil || fresh.Metadata["vm_image_variant"] != "minimal" || fresh.URL == legacy.URL {
		t.Fatalf("fresh Ubuntu resolution selected legacy image: %#v err=%v", fresh, err)
	}

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "vm"), 0o750); err != nil {
		t.Fatal(err)
	}
	oldMeta := vmInstallMetadata{Schema: 1, Provider: spec.ID, Version: legacy.Version, Build: legacy.Build,
		Architecture: "amd64", Checksum: legacy.SHA256, DiskGB: 10, Hostname: "pcvm"}
	if err := writeJSONAtomic(filepath.Join(home, "vm", "install.json"), oldMeta); err != nil {
		t.Fatal(err)
	}
	state := State{Provider: spec.ID, ResolvedVersion: legacy.Version, ResolvedBuild: legacy.Build, Artifact: artifact}
	if migrated, err := repairLegacyVMInstallMetadata(home, spec, state, "amd64"); err != nil || !migrated {
		t.Fatalf("legacy Ubuntu metadata migration=%v err=%v", migrated, err)
	}
	var migrated vmInstallMetadata
	if err := readJSON(filepath.Join(home, "vm", "install.json"), &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Schema != vmInstallSchema || migrated.ImageID != legacy.ID || migrated.Variant != "standard" || migrated.Compression != "off" {
		t.Fatalf("legacy Ubuntu metadata changed incorrectly: %+v", migrated)
	}
	app := &App{Config: Config{Home: home, Arch: "amd64"}, Catalog: c}
	launch, err := app.rebuildLaunchState(context.Background(), spec, state)
	if err != nil || launch.VMImageID != legacy.ID || launch.VMImageVariant != "standard" || launch.VMDiskCompression != "off" {
		t.Fatalf("legacy Ubuntu launch state=%+v err=%v", launch, err)
	}
}

func TestVMResourceCalculation(t *testing.T) {
	tests := []struct {
		name   string
		req    Request
		limits cgroupLimits
		want   vmResources
		bad    bool
	}{
		{"finite auto", Request{VMMemoryMB: "auto", VMCPUs: "auto"}, cgroupLimits{MemoryLimitMB: 2048, CPUQuota: 1.5}, vmResources{MemoryMB: 1536, CPUs: 2}, false},
		{"unlimited auto", Request{VMMemoryMB: "auto", VMCPUs: "auto"}, cgroupLimits{}, vmResources{MemoryMB: 1024, CPUs: 2}, false},
		{"manual", Request{VMMemoryMB: "2048", VMCPUs: "3"}, cgroupLimits{MemoryLimitMB: 4096, CPUQuota: 2.1}, vmResources{MemoryMB: 2048, CPUs: 3}, false},
		{"container too small", Request{VMMemoryMB: "auto", VMCPUs: "auto"}, cgroupLimits{MemoryLimitMB: 1400, CPUQuota: 2}, vmResources{}, true},
		{"no host reserve", Request{VMMemoryMB: "1800", VMCPUs: "1"}, cgroupLimits{MemoryLimitMB: 2048, CPUQuota: 2}, vmResources{}, true},
		{"over cpu quota", Request{VMMemoryMB: "1024", VMCPUs: "3"}, cgroupLimits{MemoryLimitMB: 2048, CPUQuota: 2}, vmResources{}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := calculateVMResources(test.req, vmTestPolicy(), test.limits)
			if (err != nil) != test.bad || !test.bad && got != test.want {
				t.Fatalf("resources=%#v err=%v", got, err)
			}
		})
	}
	if got := parseCgroupMemory("max\n"); got != 0 {
		t.Fatalf("max parsed as %d", got)
	}
	if got := parseCgroupMemory("1610612736\n"); got != 1536 {
		t.Fatalf("memory parsed as %d", got)
	}
	if got := parseCgroupMemory("malformed"); got != 0 {
		t.Fatalf("malformed memory parsed as %d", got)
	}
	if got := parseCgroupV2CPU("150000 100000\n"); got != 1.5 {
		t.Fatalf("cgroup v2 CPU parsed as %f", got)
	}
	if got := parseCgroupV2CPU("max 100000\n"); got != 0 {
		t.Fatalf("unlimited cgroup v2 CPU parsed as %f", got)
	}
	if got := parseCgroupV1CPU("250000", "100000"); got != 2.5 {
		t.Fatalf("cgroup v1 CPU parsed as %f", got)
	}
	if got := parseCgroupV1CPU("-1", "100000"); got != 0 {
		t.Fatalf("unlimited cgroup v1 CPU parsed as %f", got)
	}
	capped := vmTestPolicy()
	capped.VMMaxMemoryMB, capped.VMMaxCPUs = 1024, 1
	if got, err := calculateVMResources(Request{VMMemoryMB: "auto", VMCPUs: "auto"}, capped, cgroupLimits{MemoryLimitMB: 4096, CPUQuota: 8}); err != nil || got != (vmResources{MemoryMB: 1024, CPUs: 1}) {
		t.Fatalf("admin caps resources=%#v err=%v", got, err)
	}
}

func TestVMValidationAndCloudInit(t *testing.T) {
	spec := ProviderSpec{ID: "vm-ubuntu", MinimumDisk: 8192}
	cfg := Config{Arch: "amd64", Request: Request{Architecture: "amd64", VMMemoryMB: "auto", VMCPUs: "auto", VMDiskGB: 10, VMHostname: "lab-vm"}, Policy: vmTestPolicy()}
	if err := validateVMRequest(spec, cfg); err != nil && !strings.Contains(err.Error(), "container memory limit") {
		t.Fatal(err)
	}
	data := cloudInitUserData("lab-vm", "amd64")
	for _, want := range []string{"hostname: lab-vm", "name: pcvm", "NOPASSWD", "serial-getty@ttyS0", "serial-getty@ttyAMA0", "pcvm-ready.service"} {
		if !strings.Contains(data, want) {
			t.Fatalf("cloud-init missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(data), "password:") {
		t.Fatal("cloud-init persisted a password")
	}
	if strings.Contains(data, "%!") || !strings.Contains(data, "restart, serial-getty@ttyS0.service") {
		t.Fatalf("invalid AMD64 cloud-init formatting: %s", data)
	}
	if arm := cloudInitUserData("lab-vm", "arm64"); !strings.Contains(arm, "restart, serial-getty@ttyAMA0.service") {
		t.Fatalf("ARM64 cloud-init targets the wrong serial console: %s", arm)
	}
	cfg.Request.VMHostname = "bad/name"
	if err := validateVMRequest(spec, cfg); err == nil {
		t.Fatal("accepted invalid hostname")
	}
	cfg.Request.VMHostname = "lab"
	cfg.Request.AutoUpdate = true
	if err := validateVMRequest(spec, cfg); err == nil {
		t.Fatal("accepted VM auto update")
	}
	cfg.Request.AutoUpdate = false
	cfg.Request.VMHostname = "lab"
	cfg.Request.VMDiskCompression = "gzip"
	if err := validateVMRequest(spec, cfg); err == nil {
		t.Fatal("accepted unsupported VM compression")
	}
	cfg.Request.VMDiskCompression = "off"
	cfg.Request.VMDiskGB = 2
	if err := validateVMRequest(spec, cfg); err == nil {
		t.Fatal("accepted undersized Ubuntu disk")
	}
	alpine := ProviderSpec{ID: "vm-alpine", MinimumDisk: 2048}
	if err := validateVMRequest(alpine, cfg); err != nil && !strings.Contains(err.Error(), "container memory limit") {
		t.Fatalf("Alpine 2 GiB disk was rejected: %v", err)
	}
}

func TestAlpineCloudInitUsesOpenRCAndSerialAutologin(t *testing.T) {
	data := alpineCloudInitUserData("tiny-vm", "arm64")
	for _, want := range []string{"hostname: tiny-vm", "shell: /bin/ash", "/etc/doas.d/pcvm.conf", "/etc/local.d/pcvm-ready.start", "/usr/local/bin/sudo"} {
		if !strings.Contains(data, want) {
			t.Fatalf("Alpine cloud-init missing %q: %s", want, data)
		}
	}
	var decoded strings.Builder
	for _, line := range strings.Split(data, "\n") {
		value := strings.TrimPrefix(strings.TrimSpace(line), "content: ")
		if value == strings.TrimSpace(line) {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
			decoded.Write(raw)
		}
	}
	for _, script := range []string{"ttyAMA0", "rc-update add local default", "[PCVM-GUEST] READY", `exec /usr/bin/doas -s "$@"`} {
		if !strings.Contains(decoded.String(), script) {
			t.Fatalf("Alpine cloud-init missing encoded %q", script)
		}
	}
	if strings.Contains(data, "packages:") || strings.Contains(strings.ToLower(data), "password:") {
		t.Fatal("Alpine provisioning downloads packages or persists a password")
	}
}

func TestVMTransitionAlwaysResetsOnImageChange(t *testing.T) {
	spec := ProviderSpec{ID: "vm-debian", Family: "vm-debian", Installer: "qemu-vm"}
	state := &State{Provider: spec.ID, Family: spec.Family, ResolvedVersion: "12", ResolvedBuild: "old"}
	if reset, _ := EvaluateTransition(state, NewProvider(spec), Resolved{Artifact: Artifact{Version: "12", Build: "new"}}); !reset {
		t.Fatal("VM build change did not require reset")
	}
	if reset, _ := EvaluateTransition(state, NewProvider(spec), Resolved{Artifact: Artifact{Version: "12", Build: "old"}}); reset {
		t.Fatal("unchanged VM image required reset")
	}
	state.Artifact = Artifact{URL: "https://images.example/standard.qcow2", Metadata: map[string]string{"vm_image_id": "ubuntu-standard", "disk_compression": "off"}}
	variant := Resolved{Artifact: Artifact{Version: "12", Build: "old", URL: "https://images.example/minimal.qcow2", Metadata: map[string]string{"vm_image_id": "ubuntu-minimal", "disk_compression": "off"}}}
	if reset, reason := EvaluateTransition(state, NewProvider(spec), variant); !reset || reason != "changing a VM image variant requires reset" {
		t.Fatalf("variant transition reset=%v reason=%q", reset, reason)
	}
	state.Artifact.Metadata = map[string]string{"disk_compression": "off"}
	state.Artifact.URL = ""
	resolved := Resolved{Artifact: Artifact{Version: "12", Build: "old", Metadata: map[string]string{"disk_compression": "zstd"}}}
	if reset, reason := EvaluateTransition(state, NewProvider(spec), resolved); !reset || reason != "changing VM disk compression requires reset" {
		t.Fatalf("compression transition reset=%v reason=%q", reset, reason)
	}
}

func TestQEMUArgumentsCannotBeOverriddenByUserInput(t *testing.T) {
	cfg := Config{Home: "/home/container", Arch: "amd64", Request: Request{GameExtraArgs: "-accel kvm -drive file=/etc/passwd -netdev tap"}}
	args := qemuArguments(cfg, vmResources{MemoryMB: 1024, CPUs: 2}, "/usr/share/OVMF/OVMF_CODE.fd", "/home/container/vm/qmp.sock")
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{" kvm", "file=/etc/passwd", "netdev tap", "hostfwd", "-virtfs", "/dev/kvm"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("QEMU argv accepted forbidden user override %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"tcg,thread=multi", "q35", "unix:/home/container/vm/qmp.sock", "user,id=net0", "disk.qcow2"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("QEMU argv missing %q: %s", required, joined)
		}
	}
	if strings.Count(joined, "-cpu max") != 1 {
		t.Fatalf("AMD64 TCG must use exactly one max CPU model: %s", joined)
	}
	if !strings.Contains(joined, "virtio-blk-pci,drive=osdisk,bootindex=1") || !strings.Contains(joined, "scsi-cd,drive=seed,bootindex=99") {
		t.Fatalf("QEMU argv does not boot the OS disk before the NoCloud seed: %s", joined)
	}
}

func TestVMInstallMetadataMatching(t *testing.T) {
	dir := t.TempDir()
	meta := vmInstallMetadata{Schema: vmInstallSchema, ImageID: "debian", Variant: "genericcloud", Compression: "off", Provider: "vm-debian", Version: "13", Build: "build", Architecture: "amd64", Checksum: "sum", DiskGB: 10, Hostname: "pcvm"}
	if err := writeJSONAtomic(filepath.Join(dir, "install.json"), meta); err != nil {
		t.Fatal(err)
	}
	exists, matches, err := vmDirectoryStatus(dir, meta)
	if err != nil || !exists || !matches {
		t.Fatalf("exists=%v matches=%v err=%v", exists, matches, err)
	}
	meta.Build = "other"
	_, matches, _ = vmDirectoryStatus(dir, meta)
	if matches {
		t.Fatal("mismatched staging metadata accepted")
	}
	meta.Build = "build"
	meta.Compression = "zstd"
	_, matches, _ = vmDirectoryStatus(dir, meta)
	if matches {
		t.Fatal("staging metadata with different disk compression was accepted")
	}
}

func TestVMStagedInstallAndResume(t *testing.T) {
	home := t.TempDir()
	control := filepath.Join(home, ".pcvm")
	firmware := filepath.Join(t.TempDir(), "vars.fd")
	if err := os.WriteFile(firmware, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalRunner, originalFirmware := vmRunTool, vmFirmwareResolver
	t.Cleanup(func() { vmRunTool, vmFirmwareResolver = originalRunner, originalFirmware })
	vmFirmwareResolver = func(string) (string, string, error) { return firmware, firmware, nil }
	vmRunTool = func(_ context.Context, _, _ anyWriter, name string, args ...string) error {
		switch name {
		case "qemu-img":
			switch args[0] {
			case "convert":
				return os.WriteFile(args[len(args)-1], []byte("independent qcow2"), 0o600)
			case "resize":
				return nil
			case "check":
				_, err := os.Stat(args[len(args)-1])
				return err
			}
		case "genisoimage":
			for i := range args {
				if args[i] == "-output" && i+1 < len(args) {
					return os.WriteFile(args[i+1], []byte("seed"), 0o600)
				}
			}
		}
		return fmt.Errorf("unexpected tool call %s %v", name, args)
	}
	source := filepath.Join(control, "cache", "base.qcow2")
	if err := os.MkdirAll(filepath.Dir(source), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	pinnedSHA512 := strings.Repeat("a", 128)
	downloadedSHA256 := strings.Repeat("b", 64)
	provider := &catalogProvider{spec: ProviderSpec{ID: "vm-debian", Installer: "qemu-vm", VMImages: []VMImageSpec{{
		ID: "debian-test", Variant: "genericcloud", Version: "13", Build: "build", Architecture: "amd64", URL: "https://cloud.debian.org/test.qcow2", SHA512: pinnedSHA512,
	}}}}
	request := Request{Architecture: "amd64", VMDiskGB: 10, VMDiskCompression: "off", VMHostname: "pcvm"}
	resolved := Resolved{Artifact: Artifact{URL: "https://cloud.debian.org/test.qcow2", Version: "13", Build: "build", SHA256: downloadedSHA256, SHA512: pinnedSHA512}}
	ic := InstallContext{Home: home, ControlDir: control, Artifact: source, Request: request, Out: io.Discard, Err: io.Discard}
	installed, err := provider.installVM(context.Background(), ic, resolved)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk.qcow2", "seed.iso", "uefi-vars.fd", "install.json"} {
		if _, err := os.Stat(filepath.Join(home, "vm", name)); err != nil {
			t.Fatalf("missing committed %s: %v", name, err)
		}
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("base image cache was not removed: %v", err)
	}
	if installed.Command[0] != qemuBinary("amd64") {
		t.Fatalf("command=%v", installed.Command)
	}
	var meta vmInstallMetadata
	if err := readJSON(filepath.Join(home, "vm", "install.json"), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Schema != vmInstallSchema || meta.ImageID != "debian-test" || meta.Variant != "genericcloud" || meta.Compression != "off" || meta.Checksum != pinnedSHA512 {
		t.Fatalf("install metadata used downloaded SHA-256 instead of pinned SHA-512: %s", meta.Checksum)
	}
	if _, err := provider.installVM(context.Background(), ic, resolved); err != nil {
		t.Fatalf("matching committed install did not resume: %v", err)
	}
}

func TestRepairLegacyDebianVMInstallMetadata(t *testing.T) {
	home := t.TempDir()
	vmDir := filepath.Join(home, "vm")
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		t.Fatal(err)
	}
	pinnedSHA512 := strings.Repeat("a", 128)
	legacySHA256 := strings.Repeat("b", 64)
	image := VMImageSpec{ID: "debian-test", Variant: "genericcloud", Version: "13", Build: "build", Architecture: "amd64", URL: "https://cloud.debian.org/debian.qcow2", SHA512: pinnedSHA512}
	spec := ProviderSpec{ID: "vm-debian", Installer: "qemu-vm", VMImages: []VMImageSpec{image}}
	state := State{Provider: spec.ID, ResolvedVersion: image.Version, ResolvedBuild: image.Build, Artifact: Artifact{
		URL: image.URL, Version: image.Version, Build: image.Build, SHA256: legacySHA256, SHA512: pinnedSHA512,
	}}
	legacy := vmInstallMetadata{Schema: 1, Provider: spec.ID, Version: image.Version, Build: image.Build,
		Architecture: image.Architecture, Checksum: legacySHA256, DiskGB: 10, Hostname: "pcvm"}
	if err := writeJSONAtomic(filepath.Join(vmDir, "install.json"), legacy); err != nil {
		t.Fatal(err)
	}
	repaired, err := repairLegacyVMInstallMetadata(home, spec, state, "amd64")
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var got vmInstallMetadata
	if err := readJSON(filepath.Join(vmDir, "install.json"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != vmInstallSchema || got.ImageID != image.ID || got.Variant != image.Variant || got.Compression != "off" || got.Checksum != pinnedSHA512 || got.DiskGB != legacy.DiskGB || got.Hostname != legacy.Hostname {
		t.Fatalf("unexpected repaired metadata: %+v", got)
	}

	legacy.Checksum = legacySHA256
	if err := writeJSONAtomic(filepath.Join(vmDir, "install.json"), legacy); err != nil {
		t.Fatal(err)
	}
	state.Artifact.SHA512 = strings.Repeat("c", 128)
	if repaired, err := repairLegacyVMInstallMetadata(home, spec, state, "amd64"); err != nil || repaired {
		t.Fatalf("untrusted state repaired metadata: repaired=%v err=%v", repaired, err)
	}
}

func TestRepairInterruptedLegacyVMStagingMetadata(t *testing.T) {
	dir := t.TempDir()
	pinnedSHA512 := strings.Repeat("a", 128)
	legacySHA256 := strings.Repeat("b", 64)
	want := vmInstallMetadata{Schema: vmInstallSchema, ImageID: "debian-test", Variant: "genericcloud", Compression: "off", Provider: "vm-debian", Version: "13", Build: "build",
		Architecture: "amd64", Checksum: pinnedSHA512, DiskGB: 10, Hostname: "pcvm"}
	legacy := want
	legacy.Schema = 1
	legacy.ImageID, legacy.Variant, legacy.Compression = "", "", ""
	legacy.Checksum = legacySHA256
	path := filepath.Join(dir, "install.json")
	disk := filepath.Join(dir, "disk.qcow2")
	if err := writeJSONAtomic(path, legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(disk, []byte("existing disk must not change"), 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := migrateLegacyVMMetadataFile(path, want, legacySHA256)
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var got vmInstallMetadata
	if err := readJSON(path, &got); err != nil || got != want {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if data, err := os.ReadFile(disk); err != nil || string(data) != "existing disk must not change" {
		t.Fatalf("legacy metadata migration changed disk: data=%q err=%v", data, err)
	}

	legacy.Hostname = "different"
	if err := writeJSONAtomic(path, legacy); err != nil {
		t.Fatal(err)
	}
	if repaired, err := migrateLegacyVMMetadataFile(path, want, legacySHA256); err != nil || repaired {
		t.Fatalf("mismatched install was repaired: repaired=%v err=%v", repaired, err)
	}
}

func TestVMFailedConvertCleansPartialDisk(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	original := vmRunTool
	t.Cleanup(func() { vmRunTool = original })
	vmRunTool = func(_ context.Context, _, _ anyWriter, name string, args ...string) error {
		if name == "qemu-img" && args[0] == "convert" {
			_ = os.WriteFile(args[len(args)-1], []byte("partial"), 0o600)
			return fmt.Errorf("simulated interruption")
		}
		return nil
	}
	if err := ensureVMStandaloneDisk(context.Background(), filepath.Join(dir, "base"), disk, 10, "off", io.Discard, io.Discard); err == nil {
		t.Fatal("expected convert failure")
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatalf("partial disk was not removed: %v", err)
	}
}

func TestVMZstdConvertUsesDirectQEMUArguments(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	original := vmRunTool
	t.Cleanup(func() { vmRunTool = original })
	var convert []string
	vmRunTool = func(_ context.Context, _, _ anyWriter, name string, args ...string) error {
		if name != "qemu-img" {
			return fmt.Errorf("unexpected tool %s", name)
		}
		switch args[0] {
		case "convert":
			convert = append([]string(nil), args...)
			return os.WriteFile(args[len(args)-1], []byte("zstd qcow2"), 0o600)
		case "resize", "check":
			return nil
		default:
			return fmt.Errorf("unexpected qemu-img args %v", args)
		}
	}
	if err := ensureVMStandaloneDisk(context.Background(), filepath.Join(dir, "base.qcow2"), disk, 2, "zstd", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"convert", "-f", "qcow2", "-O", "qcow2", "-c", "-o", "compat=1.1,compression_type=zstd"}
	if len(convert) < len(want)+2 || strings.Join(convert[:len(want)], " ") != strings.Join(want, " ") {
		t.Fatalf("unexpected Zstd conversion argv: %v", convert)
	}
}

func TestQMPPowerdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix QMP socket is exercised on Linux CI")
	}
	socket := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 3)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		encoder := json.NewEncoder(connection)
		decoder := json.NewDecoder(bufio.NewReader(connection))
		_ = encoder.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}}})
		for range 3 {
			var request struct {
				Execute string `json:"execute"`
			}
			if decoder.Decode(&request) != nil {
				return
			}
			commands <- request.Execute
			_ = encoder.Encode(map[string]any{"return": map[string]any{}})
		}
	}()
	if err := qmpPowerdown(socket); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"qmp_capabilities", "query-status", "system_powerdown"} {
		select {
		case got := <-commands:
			if got != want {
				t.Fatalf("QMP command=%q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing QMP command %q", want)
		}
	}
}
