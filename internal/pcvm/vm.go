package pcvm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	vmMinimumContainerMB = 1536
	vmHostReserveMB      = 384
	vmDefaultMemoryMB    = 1024
)

var vmHostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
var vmRunTool = runVMTool
var vmFirmwareResolver = vmFirmware

type vmInstallMetadata struct {
	Schema       int    `json:"schema"`
	Provider     string `json:"provider"`
	Version      string `json:"version"`
	Build        string `json:"build"`
	Architecture string `json:"architecture"`
	Checksum     string `json:"checksum"`
	DiskGB       int    `json:"disk_gb"`
	Hostname     string `json:"hostname"`
}

type vmResources struct {
	MemoryMB int
	CPUs     int
}

func resolveVMImage(spec ProviderSpec, req Request) (Artifact, error) {
	images := make([]VMImageSpec, 0, len(spec.VMImages))
	for _, image := range spec.VMImages {
		if image.Architecture == req.Architecture && (req.Version == "" || req.Version == "latest" || image.Version == req.Version) {
			images = append(images, image)
		}
	}
	if len(images) == 0 {
		return Artifact{}, fmt.Errorf("no %s VM image for version %q on %s", spec.Name, req.Version, req.Architecture)
	}
	sort.Slice(images, func(i, j int) bool {
		if compared := CompareVersions(images[i].Version, images[j].Version); compared != 0 {
			return compared > 0
		}
		return CompareVersions(images[i].Build, images[j].Build) > 0
	})
	selected := images[0]
	if req.Build != "" && req.Build != "latest" {
		found := false
		for _, image := range images {
			if image.Build == req.Build {
				selected, found = image, true
				break
			}
		}
		if !found {
			return Artifact{}, fmt.Errorf("VM image build %q not found for %s %s on %s", req.Build, spec.Name, selected.Version, req.Architecture)
		}
	}
	return Artifact{
		URL: selected.URL, FileName: filepath.Base(selected.URL), Kind: "qcow2", SHA256: selected.SHA256,
		SHA512: selected.SHA512, Version: selected.Version, Build: selected.Build,
		Metadata: map[string]string{"architecture": selected.Architecture, "format": selected.Format},
	}, nil
}

func validateVMRequest(cfg Config) error {
	if cfg.Request.AutoUpdate || strings.TrimSpace(cfg.Request.UpdateRequest) != "" {
		return fmt.Errorf("VM providers do not support AUTO_UPDATE or UPDATE_REQUEST; update packages inside the guest OS")
	}
	if !vmHostnamePattern.MatchString(cfg.Request.VMHostname) {
		return fmt.Errorf("VM_HOSTNAME must be a valid single-label Linux hostname")
	}
	if cfg.Request.VMDiskGB < 8 || cfg.Request.VMDiskGB > cfg.Policy.VMMaxDiskGB {
		return fmt.Errorf("VM_DISK_GB must be between 8 and VM_MAX_DISK_GB (%d)", cfg.Policy.VMMaxDiskGB)
	}
	_, err := calculateVMResources(cfg.Request, cfg.Policy, readHostCgroupLimits())
	return err
}

type cgroupLimits struct {
	MemoryLimitMB int
	CPUQuota      float64
}

func readHostCgroupLimits() cgroupLimits {
	limits := cgroupLimits{}
	if raw, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		limits.MemoryLimitMB = parseCgroupMemory(string(raw))
	} else if raw, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		limits.MemoryLimitMB = parseCgroupMemory(string(raw))
	}
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		limits.CPUQuota = parseCgroupV2CPU(string(raw))
	} else {
		quotaRaw, quotaErr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		periodRaw, periodErr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		if quotaErr == nil && periodErr == nil {
			limits.CPUQuota = parseCgroupV1CPU(string(quotaRaw), string(periodRaw))
		}
	}
	return limits
}

func parseCgroupV2CPU(raw string) float64 {
	fields := strings.Fields(raw)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	return parseCgroupV1CPU(fields[0], fields[1])
}

func parseCgroupV1CPU(quotaRaw, periodRaw string) float64 {
	quota, qErr := strconv.ParseFloat(strings.TrimSpace(quotaRaw), 64)
	period, pErr := strconv.ParseFloat(strings.TrimSpace(periodRaw), 64)
	if qErr != nil || pErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quota / period
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
	return int(bytes / (1024 * 1024))
}

func calculateVMResources(req Request, policy Policy, limits cgroupLimits) (vmResources, error) {
	if limits.MemoryLimitMB > 0 && limits.MemoryLimitMB < vmMinimumContainerMB {
		return vmResources{}, fmt.Errorf("VM requires a container memory limit of at least %d MB", vmMinimumContainerMB)
	}
	memory := 0
	if req.VMMemoryMB == "" || req.VMMemoryMB == "auto" {
		if limits.MemoryLimitMB == 0 {
			memory = vmDefaultMemoryMB
		} else {
			memory = (limits.MemoryLimitMB * 75 / 100 / 128) * 128
		}
		if memory > policy.VMMaxMemoryMB {
			memory = policy.VMMaxMemoryMB / 128 * 128
		}
	} else {
		parsed, err := strconv.Atoi(req.VMMemoryMB)
		if err != nil {
			return vmResources{}, fmt.Errorf("VM_MEMORY_MB must be auto or an integer")
		}
		memory = parsed
	}
	if memory < 768 || memory > policy.VMMaxMemoryMB {
		return vmResources{}, fmt.Errorf("VM_MEMORY_MB must be at least 768 and no more than VM_MAX_MEMORY_MB (%d)", policy.VMMaxMemoryMB)
	}
	if limits.MemoryLimitMB > 0 && memory > limits.MemoryLimitMB-vmHostReserveMB {
		return vmResources{}, fmt.Errorf("VM_MEMORY_MB must leave at least %d MB for QEMU and PCVM", vmHostReserveMB)
	}

	quotaCPUs := 0
	if limits.CPUQuota > 0 {
		quotaCPUs = int(limits.CPUQuota)
		if float64(quotaCPUs) < limits.CPUQuota {
			quotaCPUs++
		}
		if quotaCPUs < 1 {
			quotaCPUs = 1
		}
	}
	cpus := 0
	if req.VMCPUs == "" || req.VMCPUs == "auto" {
		cpus = 2
		if quotaCPUs > 0 && quotaCPUs < cpus {
			cpus = quotaCPUs
		}
		if policy.VMMaxCPUs < cpus {
			cpus = policy.VMMaxCPUs
		}
	} else {
		parsed, err := strconv.Atoi(req.VMCPUs)
		if err != nil {
			return vmResources{}, fmt.Errorf("VM_CPUS must be auto or an integer")
		}
		cpus = parsed
	}
	if cpus < 1 || cpus > 8 || cpus > policy.VMMaxCPUs {
		return vmResources{}, fmt.Errorf("VM_CPUS must be between 1 and VM_MAX_CPUS (%d)", policy.VMMaxCPUs)
	}
	if quotaCPUs > 0 && cpus > quotaCPUs {
		return vmResources{}, fmt.Errorf("VM_CPUS=%d exceeds the container CPU quota (%d)", cpus, quotaCPUs)
	}
	return vmResources{MemoryMB: memory, CPUs: cpus}, nil
}

func (p *catalogProvider) installVM(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	if ic.Artifact == "" {
		return resolved, fmt.Errorf("VM cloud image was not prepared")
	}
	image, ok := findVMImage(p.spec, resolved.Artifact.Version, resolved.Artifact.Build, ic.Request.Architecture)
	if !ok {
		return resolved, fmt.Errorf("resolved VM image is not pinned by the embedded catalog")
	}
	checksum := pinnedVMImageChecksum(image)
	if checksum == "" {
		return resolved, fmt.Errorf("resolved VM image has no pinned checksum")
	}
	meta := vmInstallMetadata{Schema: 1, Provider: p.spec.ID, Version: resolved.Artifact.Version, Build: resolved.Artifact.Build,
		Architecture: ic.Request.Architecture, Checksum: checksum,
		DiskGB: ic.Request.VMDiskGB, Hostname: ic.Request.VMHostname}
	finalDir := filepath.Join(ic.Home, "vm")
	stageDir := filepath.Join(ic.ControlDir, "staging", "vm")
	if image.SHA512 != "" && image.SHA256 == "" && strings.EqualFold(resolved.Artifact.SHA512, checksum) {
		for _, dir := range []string{finalDir, stageDir} {
			if _, err := repairLegacyVMMetadataFile(filepath.Join(dir, "install.json"), meta, resolved.Artifact.SHA256); err != nil {
				return resolved, fmt.Errorf("repair legacy VM staging metadata: %w", err)
			}
		}
	}
	if exists, matches, err := vmDirectoryStatus(finalDir, meta); err != nil {
		return resolved, err
	} else if exists {
		if !matches {
			return resolved, fmt.Errorf("existing VM install metadata does not match requested image; reset is required")
		}
		if err := validateCommittedVM(ctx, finalDir, ic.Out, ic.Err); err != nil {
			return resolved, err
		}
		if err := os.Remove(ic.Artifact); err != nil && !os.IsNotExist(err) && ic.Log != nil {
			ic.Log.Printf("WARNING: could not remove VM base image cache: %v", err)
		}
		return p.installedVMResult(ic, resolved), nil
	}
	if exists, matches, err := vmDirectoryStatus(stageDir, meta); err != nil {
		return resolved, err
	} else if exists && !matches {
		return resolved, fmt.Errorf("staged VM belongs to a different image; refusing to overwrite it")
	} else if !exists {
		if err := os.MkdirAll(stageDir, 0o750); err != nil {
			return resolved, err
		}
		if err := writeJSONAtomic(filepath.Join(stageDir, "install.json"), meta); err != nil {
			return resolved, err
		}
	}
	disk := filepath.Join(stageDir, "disk.qcow2")
	if err := ensureVMStandaloneDisk(ctx, ic.Artifact, disk, meta.DiskGB, ic.Out, ic.Err); err != nil {
		return resolved, err
	}
	seed := filepath.Join(stageDir, "seed.iso")
	if info, err := os.Lstat(seed); os.IsNotExist(err) {
		if err := createNoCloudSeed(ctx, stageDir, seed, meta.Hostname, meta.Architecture, ic.Out, ic.Err); err != nil {
			return resolved, err
		}
	} else if err == nil && !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("staged VM seed must be a regular file")
	} else if err != nil {
		return resolved, err
	}
	_, varsTemplate, err := vmFirmwareResolver(meta.Architecture)
	if err != nil {
		return resolved, err
	}
	vars := filepath.Join(stageDir, "uefi-vars.fd")
	if info, err := os.Lstat(vars); os.IsNotExist(err) {
		if err := copyFile(varsTemplate, vars, 0o600); err != nil {
			return resolved, fmt.Errorf("copy UEFI variables: %w", err)
		}
	} else if err == nil && !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("staged UEFI variables must be a regular file")
	} else if err != nil {
		return resolved, err
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		return resolved, fmt.Errorf("commit VM installation: %w", err)
	}
	if err := os.Remove(ic.Artifact); err != nil && !os.IsNotExist(err) && ic.Log != nil {
		ic.Log.Printf("WARNING: could not remove VM base image cache: %v", err)
	}
	return p.installedVMResult(ic, resolved), nil
}

func (p *catalogProvider) installedVMResult(ic InstallContext, resolved Resolved) Resolved {
	resolved.WorkDir = ic.Home
	resolved.Command = []string{qemuBinary(ic.Request.Architecture)}
	resolved.Environment = nil
	resolved.ReadyPatterns = []string{`\[PCVM-GUEST\] READY`}
	resolved.StopCommand = ""
	return resolved
}

func vmDirectoryStatus(dir string, want vmInstallMetadata) (bool, bool, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() {
		return true, false, fmt.Errorf("VM path %s is not a directory", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, "install.json"))
	if err != nil {
		return true, false, nil
	}
	var got vmInstallMetadata
	if json.Unmarshal(data, &got) != nil {
		return true, false, nil
	}
	return true, got == want, nil
}

func findVMImage(spec ProviderSpec, version, build, arch string) (VMImageSpec, bool) {
	for _, image := range spec.VMImages {
		if image.Version == version && image.Build == build && image.Architecture == arch {
			return image, true
		}
	}
	return VMImageSpec{}, false
}

// pinnedVMImageChecksum returns the checksum authored in the embedded catalog.
// SHA-512 takes precedence because Debian images are pinned only with SHA-512,
// while Download also populates a convenience SHA-256 of the received bytes.
func pinnedVMImageChecksum(image VMImageSpec) string {
	return strings.ToLower(firstNonEmpty(image.SHA512, image.SHA256))
}

func repairLegacyVMMetadataFile(path string, want vmInstallMetadata, legacySHA256 string) (bool, error) {
	legacySHA256 = strings.ToLower(legacySHA256)
	decoded, err := hex.DecodeString(legacySHA256)
	if err != nil || len(decoded) != sha256.Size {
		return false, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("VM install metadata must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var got vmInstallMetadata
	if err := json.Unmarshal(data, &got); err != nil || !strings.EqualFold(got.Checksum, legacySHA256) {
		return false, nil
	}
	got.Checksum = want.Checksum
	if got != want {
		return false, nil
	}
	if err := writeJSONAtomic(path, got); err != nil {
		return false, err
	}
	return true, nil
}

// repairLegacyVMInstallMetadata repairs the v1.4.0/v1.4.1 Debian metadata bug:
// Download added a computed SHA-256 and installVM accidentally persisted it
// instead of Debian's catalog-pinned SHA-512. This migration changes only the
// identity checksum; it never changes the disk, seed, firmware or process argv.
func repairLegacyVMInstallMetadata(home string, spec ProviderSpec, state State, arch string) (bool, error) {
	image, ok := findVMImage(spec, state.ResolvedVersion, state.ResolvedBuild, arch)
	if !ok || image.SHA512 == "" || image.SHA256 != "" {
		return false, nil
	}
	pinned := pinnedVMImageChecksum(image)
	legacySHA256 := strings.ToLower(state.Artifact.SHA256)
	decoded, err := hex.DecodeString(legacySHA256)
	if err != nil || len(decoded) != sha256.Size || !strings.EqualFold(state.Artifact.SHA512, pinned) ||
		state.Artifact.URL != image.URL || state.Artifact.Version != image.Version || state.Artifact.Build != image.Build {
		return false, nil
	}
	path := filepath.Join(home, "vm", "install.json")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("VM install metadata must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var meta vmInstallMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, nil
	}
	if meta.Provider != spec.ID || meta.Version != image.Version || meta.Build != image.Build || meta.Architecture != arch ||
		!strings.EqualFold(meta.Checksum, legacySHA256) {
		return false, nil
	}
	meta.Checksum = pinned
	if err := writeJSONAtomic(path, meta); err != nil {
		return false, err
	}
	return true, nil
}

func ensureVMStandaloneDisk(ctx context.Context, source, disk string, diskGB int, stdout, stderr anyWriter) error {
	if info, err := os.Lstat(disk); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("staged VM disk must be a regular file")
		}
		if vmRunTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk) == nil {
			if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "resize", disk, fmt.Sprintf("%dG", diskGB)); err != nil {
				return fmt.Errorf("resume VM disk resize: %w", err)
			}
			if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk); err != nil {
				return fmt.Errorf("validate resumed VM disk: %w", err)
			}
			return nil
		}
		if err := os.Remove(disk); err != nil {
			return fmt.Errorf("remove incomplete VM disk: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", source, disk); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("convert VM disk: %w", err)
	}
	if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "resize", disk, fmt.Sprintf("%dG", diskGB)); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("resize VM disk: %w", err)
	}
	if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("validate VM disk: %w", err)
	}
	return nil
}

func validateCommittedVM(ctx context.Context, dir string, stdout, stderr anyWriter) error {
	if err := validateVMFiles(dir); err != nil {
		return err
	}
	if err := vmRunTool(ctx, stdout, stderr, "qemu-img", "check", "-q", filepath.Join(dir, "disk.qcow2")); err != nil {
		return fmt.Errorf("committed VM disk failed validation: %w", err)
	}
	return nil
}

func validateVMFiles(dir string) error {
	for _, name := range []string{"install.json", "disk.qcow2", "seed.iso", "uefi-vars.fd"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("committed VM is missing valid regular file %s", name)
		}
	}
	return nil
}

type anyWriter interface {
	Write([]byte) (int, error)
}

func runVMTool(ctx context.Context, stdout, stderr anyWriter, name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("required tool %s is not installed", name)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func createNoCloudSeed(ctx context.Context, stageDir, output, hostname, arch string, stdout, stderr anyWriter) error {
	seedDir := filepath.Join(stageDir, "seed-data")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(seedDir)
	userData := cloudInitUserData(hostname, arch)
	metaData := fmt.Sprintf("instance-id: pcvm-%s\nlocal-hostname: %s\n", hostname, hostname)
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), []byte(userData), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return err
	}
	temp, err := os.CreateTemp(stageDir, ".seed-*.iso")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Remove(tempName); err != nil {
		return err
	}
	defer os.Remove(tempName)
	if err := vmRunTool(ctx, stdout, stderr, "genisoimage", "-quiet", "-output", tempName, "-volid", "cidata", "-joliet", "-rock",
		filepath.Join(seedDir, "user-data"), filepath.Join(seedDir, "meta-data")); err != nil {
		return err
	}
	info, err := os.Lstat(tempName)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("genisoimage did not create a valid seed ISO")
	}
	return os.Rename(tempName, output)
}

func cloudInitUserData(hostname, arch string) string {
	console := "ttyS0"
	if arch == "arm64" {
		console = "ttyAMA0"
	}
	autologin := `[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin pcvm --noclear %I $TERM
`
	readyUnit := `[Unit]
Description=PCVM guest readiness marker
After=network-online.target serial-getty@%s.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "[PCVM-GUEST] READY" > /dev/console'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
disable_root: true
ssh_pwauth: false
users:
  - name: pcvm
    gecos: PCVM Console User
    shell: /bin/bash
    lock_passwd: true
    sudo: ALL=(ALL) NOPASSWD:ALL
write_files:
  - path: /etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf
    permissions: '0644'
    encoding: b64
    content: %s
  - path: /etc/systemd/system/serial-getty@ttyAMA0.service.d/autologin.conf
    permissions: '0644'
    encoding: b64
    content: %s
  - path: /etc/systemd/system/pcvm-ready.service
    permissions: '0644'
    encoding: b64
    content: %s
runcmd:
  - [systemctl, daemon-reload]
  - [systemctl, restart, serial-getty@%s.service]
  - [systemctl, enable, --now, pcvm-ready.service]
`, hostname, encode(autologin), encode(autologin), encode(fmt.Sprintf(readyUnit, console)), console)
}

func (p *catalogProvider) buildVMProcess(cfg Config, state LaunchState) (ProcessSpec, error) {
	resources, err := calculateVMResources(cfg.Request, cfg.Policy, readHostCgroupLimits())
	if err != nil {
		return ProcessSpec{}, err
	}
	vmDir := filepath.Join(cfg.Home, "vm")
	if err := validateVMFiles(vmDir); err != nil {
		return ProcessSpec{}, err
	}
	metaData, err := os.ReadFile(filepath.Join(vmDir, "install.json"))
	if err != nil {
		return ProcessSpec{}, fmt.Errorf("read VM install metadata: %w", err)
	}
	var meta vmInstallMetadata
	image, found := findVMImage(p.spec, state.ResolvedVersion, state.ResolvedBuild, cfg.Arch)
	stateChecksum := pinnedVMImageChecksum(image)
	if !found || stateChecksum == "" {
		return ProcessSpec{}, fmt.Errorf("VM state does not reference an image pinned by the embedded catalog")
	}
	if err := json.Unmarshal(metaData, &meta); err != nil || meta.Provider != state.Provider || meta.Architecture != cfg.Arch ||
		meta.Version != state.ResolvedVersion || meta.Build != state.ResolvedBuild || meta.Checksum != stateChecksum {
		return ProcessSpec{}, fmt.Errorf("VM install metadata does not match state")
	}
	code, _, err := vmFirmwareResolver(cfg.Arch)
	if err != nil {
		return ProcessSpec{}, err
	}
	qmp := filepath.Join(vmDir, "qmp.sock")
	if info, err := os.Lstat(qmp); err == nil {
		if info.IsDir() {
			return ProcessSpec{}, fmt.Errorf("QMP socket path is a directory")
		}
		if err := os.Remove(qmp); err != nil {
			return ProcessSpec{}, fmt.Errorf("remove stale QMP socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return ProcessSpec{}, fmt.Errorf("inspect QMP socket: %w", err)
	}
	args := qemuArguments(cfg, resources, code, qmp)
	return ProcessSpec{Command: append([]string{qemuBinary(cfg.Arch)}, args...), Directory: cfg.Home,
		Readiness: ReadinessSpec{Mode: "regex", Patterns: []string{`\[PCVM-GUEST\] READY`}},
		Control:   ControlSpec{Mode: "qmp", SocketPath: qmp}, RawOutput: true, RepeatReadiness: true,
		ReadyTimeout: 15 * time.Minute, StopTimeout: 90 * time.Second}, nil
}

func qemuArguments(cfg Config, resources vmResources, code, qmp string) []string {
	vmDir := filepath.Join(cfg.Home, "vm")
	args := []string{"-name", "PCVM", "-accel", "tcg,thread=multi", "-m", strconv.Itoa(resources.MemoryMB), "-smp", strconv.Itoa(resources.CPUs),
		"-display", "none", "-serial", "stdio", "-monitor", "none", "-qmp", "unix:" + qmp + ",server=on,wait=off",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(vmDir, "uefi-vars.fd"),
		"-drive", "if=none,file=" + filepath.Join(vmDir, "disk.qcow2") + ",format=qcow2,id=osdisk,cache=writeback,discard=unmap",
		"-device", "virtio-blk-pci,drive=osdisk,bootindex=1", "-device", "virtio-scsi-pci,id=scsi0",
		"-drive", "if=none,media=cdrom,readonly=on,file=" + filepath.Join(vmDir, "seed.iso") + ",format=raw,id=seed",
		"-device", "scsi-cd,drive=seed,bootindex=99", "-netdev", "user,id=net0", "-device", "virtio-net-pci,netdev=net0"}
	if cfg.Arch == "amd64" {
		args = append([]string{"-machine", "q35", "-cpu", "max"}, args...)
	} else {
		args = append([]string{"-machine", "virt,gic-version=max", "-cpu", "max"}, args...)
	}
	return args
}

func qemuBinary(arch string) string {
	if arch == "arm64" {
		return "/usr/bin/qemu-system-aarch64"
	}
	return "/usr/bin/qemu-system-x86_64"
}

func vmFirmware(arch string) (string, string, error) {
	var codeCandidates, varsCandidates []string
	if arch == "arm64" {
		codeCandidates = []string{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd"}
		varsCandidates = []string{"/usr/share/AAVMF/AAVMF_VARS.fd", "/usr/share/AAVMF/AAVMF_VARS.ms.fd"}
	} else {
		codeCandidates = []string{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd"}
		varsCandidates = []string{"/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd"}
	}
	code := firstExisting(codeCandidates)
	vars := firstExisting(varsCandidates)
	if code == "" || vars == "" {
		return "", "", fmt.Errorf("QEMU UEFI firmware for %s is not installed", arch)
	}
	return code, vars, nil
}

func firstExisting(paths []string) string {
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
