package pcvm

import (
	"context"
	"encoding/base64"
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
	vmInstallSchema      = 3
)

var vmHostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

type vmInstallMetadata struct {
	Schema       int    `json:"schema"`
	ImageID      string `json:"image_id,omitempty"`
	Variant      string `json:"variant,omitempty"`
	Compression  string `json:"compression,omitempty"`
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
		if !image.Deprecated && image.Architecture == req.Architecture && (req.Version == "" || req.Version == "latest" || image.Version == req.Version) {
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
		Metadata: map[string]string{"architecture": selected.Architecture, "format": selected.Format,
			"vm_image_id": selected.ID, "vm_image_variant": selected.Variant, "disk_compression": normalizeVMCompression(req.VMDiskCompression)},
	}, nil
}

func validateVMRequest(spec ProviderSpec, cfg Config) error {
	if cfg.Request.AutoUpdate || strings.TrimSpace(cfg.Request.UpdateRequest) != "" {
		return fmt.Errorf("VM providers do not support AUTO_UPDATE or UPDATE_REQUEST; update packages inside the guest OS")
	}
	if !vmHostnamePattern.MatchString(cfg.Request.VMHostname) {
		return fmt.Errorf("VM_HOSTNAME must be a valid single-label Linux hostname")
	}
	minimumDiskGB := (spec.MinimumDisk + 1023) / 1024
	if minimumDiskGB < 2 {
		minimumDiskGB = 2
	}
	if cfg.Request.VMDiskGB < minimumDiskGB || cfg.Request.VMDiskGB > cfg.Policy.VMMaxDiskGB {
		return fmt.Errorf("VM_DISK_GB for %s must be between %d and VM_MAX_DISK_GB (%d)", spec.ID, minimumDiskGB, cfg.Policy.VMMaxDiskGB)
	}
	if _, err := validateVMCompression(cfg.Request.VMDiskCompression); err != nil {
		return err
	}
	if cfg.Request.VMMemoryMB != "" && cfg.Request.VMMemoryMB != "auto" {
		memory, err := strconv.Atoi(cfg.Request.VMMemoryMB)
		if err != nil {
			return fmt.Errorf("VM_MEMORY_MB must be auto or an integer")
		}
		if memory < 768 || memory > cfg.Policy.VMMaxMemoryMB {
			return fmt.Errorf("VM_MEMORY_MB must be at least 768 and no more than VM_MAX_MEMORY_MB (%d)", cfg.Policy.VMMaxMemoryMB)
		}
	}
	return nil
}

func normalizeVMCompression(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "off"
	}
	return value
}

func validateVMCompression(value string) (string, error) {
	value = normalizeVMCompression(value)
	if value != "off" && value != "zstd" {
		return "", fmt.Errorf("VM_DISK_COMPRESSION must be off or zstd")
	}
	return value, nil
}

type cgroupLimits struct {
	MemoryLimitMB int
	CPUQuota      float64
}

func readHostCgroupLimits() cgroupLimits {
	return readHostCgroupLimitsWith(DefaultDependencies())
}

func readHostCgroupLimitsWith(dependencies Dependencies) cgroupLimits {
	readFile := dependencies.withDefaults().ReadFile
	limits := cgroupLimits{}
	if raw, err := readFile("/sys/fs/cgroup/cpu.max"); err == nil {
		limits.CPUQuota = parseCgroupV2CPU(string(raw))
	} else {
		quotaRaw, quotaErr := readFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		periodRaw, periodErr := readFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
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

func calculateVMResources(req Request, policy Policy, limits cgroupLimits) (vmResources, error) {
	memory, err := planMemory(MemorySpec{Strategy: "qemu-guest", RecommendedMB: vmMinimumContainerMB, HardMinimumMB: vmMinimumContainerMB}, req, policy, MemorySnapshot{Source: "test", LimitMB: limits.MemoryLimitMB})
	if err != nil {
		return vmResources{}, err
	}
	return calculateVMResourcesWithPlan(req, policy, limits.CPUQuota, memory)
}

func calculateVMResourcesWithPlan(req Request, policy Policy, cpuQuota float64, memory MemoryPlan) (vmResources, error) {
	quotaCPUs := 0
	if cpuQuota > 0 {
		quotaCPUs = int(cpuQuota)
		if float64(quotaCPUs) < cpuQuota {
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
	return vmResources{MemoryMB: memory.TargetMB, CPUs: cpus}, nil
}

func (p *catalogProvider) installVM(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	dependencies := ic.Dependencies.withDefaults()
	if ic.Artifact == "" {
		return resolved, fmt.Errorf("VM cloud image was not prepared")
	}
	image, ok := findVMImageForArtifact(p.spec, resolved.Artifact, ic.Request.Architecture)
	if !ok {
		return resolved, fmt.Errorf("resolved VM image is not pinned by the embedded catalog")
	}
	checksum := pinnedVMImageChecksum(image)
	if checksum == "" {
		return resolved, fmt.Errorf("resolved VM image has no pinned checksum")
	}
	compression, err := validateVMCompression(ic.Request.VMDiskCompression)
	if err != nil {
		return resolved, err
	}
	meta := vmInstallMetadata{Schema: vmInstallSchema, ImageID: image.ID, Variant: image.Variant, Compression: compression,
		Provider: p.spec.ID, Version: resolved.Artifact.Version, Build: resolved.Artifact.Build,
		Architecture: ic.Request.Architecture, Checksum: checksum,
		DiskGB: ic.Request.VMDiskGB, Hostname: ic.Request.VMHostname}
	finalDir := filepath.Join(ic.Home, "vm")
	stageDir := filepath.Join(ic.ControlDir, "staging", "vm")
	if exists, matches, err := vmDirectoryStatus(finalDir, meta); err != nil {
		return resolved, err
	} else if exists {
		if !matches {
			return resolved, fmt.Errorf("existing VM install metadata does not match requested image; reset is required")
		}
		if err := validateCommittedVM(ctx, finalDir, ic.Out, ic.Err, dependencies.RunVMTool); err != nil {
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
	if err := ensureVMStandaloneDisk(ctx, ic.Artifact, disk, meta.DiskGB, meta.Compression, ic.Out, ic.Err, dependencies.RunVMTool); err != nil {
		return resolved, err
	}
	seed := filepath.Join(stageDir, "seed.iso")
	if info, err := os.Lstat(seed); os.IsNotExist(err) {
		if err := createNoCloudSeed(ctx, stageDir, seed, meta.Provider, meta.Hostname, meta.Architecture, ic.Out, ic.Err, dependencies.RunVMTool); err != nil {
			return resolved, err
		}
	} else if err == nil && !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("staged VM seed must be a regular file")
	} else if err != nil {
		return resolved, err
	}
	_, varsTemplate, err := dependencies.VMFirmware(meta.Architecture)
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
	if resolved.Artifact.Metadata == nil {
		resolved.Artifact.Metadata = map[string]string{}
	}
	resolved.Artifact.Metadata["disk_compression"] = normalizeVMCompression(ic.Request.VMDiskCompression)
	resolved.WorkDir = ic.Home
	resolved.Command = []string{qemuBinary(ic.Request.Architecture)}
	resolved.Environment = nil
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
		if !image.Deprecated && image.Version == version && image.Build == build && image.Architecture == arch {
			return image, true
		}
	}
	return VMImageSpec{}, false
}

func findVMImageForArtifact(spec ProviderSpec, artifact Artifact, arch string) (VMImageSpec, bool) {
	wantedID := ""
	if artifact.Metadata != nil {
		wantedID = artifact.Metadata["vm_image_id"]
	}
	for _, image := range spec.VMImages {
		if wantedID != "" && image.ID != wantedID {
			continue
		}
		if image.Architecture != arch || image.Version != artifact.Version || image.Build != artifact.Build {
			continue
		}
		// Persisted state stores only the catalog-authored image ID and integrity,
		// never an executable/download URL. Resolved install artifacts still carry
		// the URL and must match it exactly before download.
		if wantedID == "" && image.URL != artifact.URL {
			continue
		}
		if image.SHA512 != "" && !strings.EqualFold(image.SHA512, artifact.SHA512) {
			continue
		}
		if image.SHA256 != "" && !strings.EqualFold(image.SHA256, artifact.SHA256) {
			continue
		}
		return image, true
	}
	return VMImageSpec{}, false
}

// pinnedVMImageChecksum returns the checksum authored in the embedded catalog.
// SHA-512 takes precedence because Debian images are pinned only with SHA-512,
// while Download also populates a convenience SHA-256 of the received bytes.
func pinnedVMImageChecksum(image VMImageSpec) string {
	return strings.ToLower(firstNonEmpty(image.SHA512, image.SHA256))
}

func ensureVMStandaloneDisk(ctx context.Context, source, disk string, diskGB int, compression string, stdout, stderr anyWriter, runTool VMToolFunc) error {
	if runTool == nil {
		runTool = DefaultDependencies().RunVMTool
	}
	if info, err := os.Lstat(disk); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("staged VM disk must be a regular file")
		}
		if runTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk) == nil {
			if err := runTool(ctx, stdout, stderr, "qemu-img", "resize", disk, fmt.Sprintf("%dG", diskGB)); err != nil {
				return fmt.Errorf("resume VM disk resize: %w", err)
			}
			if err := runTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk); err != nil {
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
	convertArgs := []string{"convert", "-f", "qcow2", "-O", "qcow2"}
	if compression == "zstd" {
		convertArgs = append(convertArgs, "-c", "-o", "compat=1.1,compression_type=zstd")
	}
	convertArgs = append(convertArgs, source, disk)
	if err := runTool(ctx, stdout, stderr, "qemu-img", convertArgs...); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("convert VM disk: %w", err)
	}
	if err := runTool(ctx, stdout, stderr, "qemu-img", "resize", disk, fmt.Sprintf("%dG", diskGB)); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("resize VM disk: %w", err)
	}
	if err := runTool(ctx, stdout, stderr, "qemu-img", "check", "-q", disk); err != nil {
		_ = os.Remove(disk)
		return fmt.Errorf("validate VM disk: %w", err)
	}
	return nil
}

func validateCommittedVM(ctx context.Context, dir string, stdout, stderr anyWriter, runTool VMToolFunc) error {
	if err := validateVMFiles(dir); err != nil {
		return err
	}
	if err := runTool(ctx, stdout, stderr, "qemu-img", "check", "-q", filepath.Join(dir, "disk.qcow2")); err != nil {
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

func createNoCloudSeed(ctx context.Context, stageDir, output, provider, hostname, arch string, stdout, stderr anyWriter, runTool VMToolFunc) error {
	seedDir := filepath.Join(stageDir, "seed-data")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(seedDir)
	userData := cloudInitUserDataForProvider(provider, hostname, arch)
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
	if err := runTool(ctx, stdout, stderr, "genisoimage", "-quiet", "-output", tempName, "-volid", "cidata", "-joliet", "-rock",
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
	_ = arch // the guest selects whichever serial device firmware made active
	autologin := `[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin pcvm --noclear %I $TERM
`
	ready := vmGuestReadyScript()
	readyUnit := `[Unit]
Description=PCVM guest readiness marker
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/pcvm-ready
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`
	firstBoot := `#!/bin/sh
set -eu
active_consoles="$(cat /sys/class/tty/console/active 2>/dev/null || true)"
console=
for candidate in $active_consoles ttyAMA0 ttyS0; do
    case "$candidate" in
        ttyAMA0|ttyS0)
            if [ -c "/dev/$candidate" ]; then
                console="$candidate"
                break
            fi
            ;;
    esac
done
[ -n "$console" ]
systemctl daemon-reload
systemctl restart "serial-getty@$console.service"
systemctl enable --now pcvm-ready.service
`
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	return strings.TrimSpace(fmt.Sprintf(`#cloud-config
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
  - path: /usr/local/sbin/pcvm-ready
    permissions: '0755'
    encoding: b64
    content: %s
  - path: /usr/local/sbin/pcvm-firstboot
    permissions: '0755'
    encoding: b64
    content: %s
runcmd:
  - [/bin/sh, /usr/local/sbin/pcvm-firstboot]
	`, hostname, encode(autologin), encode(autologin), encode(readyUnit), encode(ready), encode(firstBoot))) + "\n"
}

func cloudInitUserDataForProvider(provider, hostname, arch string) string {
	if provider == "vm-alpine" {
		return alpineCloudInitUserData(hostname, arch)
	}
	return cloudInitUserData(hostname, arch)
}

func alpineCloudInitUserData(hostname, arch string) string {
	_ = arch // the guest selects whichever serial device firmware made active
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	autologin := "#!/bin/sh\nprintf '%s\\n' '[PCVM-GUEST] READY'\nexec /bin/login -f pcvm\n"
	sudoCompat := `#!/bin/sh
if [ "$#" -gt 0 ] && [ "$1" = "-i" ]; then
    shift
    if [ "$#" -gt 0 ] && [ "$1" = "-c" ]; then
        shift
        exec /usr/bin/doas /bin/ash -c "$@"
    fi
    exec /usr/bin/doas /bin/ash -l "$@"
fi
exec /usr/bin/doas "$@"
`
	firstBoot := `#!/bin/sh
set -eu
mkdir -p /etc/doas.d
rm -f /etc/doas.conf /etc/doas.d/*.conf
printf '%s\n' 'permit nopass pcvm as root' > /etc/doas.conf
chmod 0400 /etc/doas.conf
for console in ttyS0 ttyAMA0; do
    line="$console::respawn:/sbin/getty -n -l /usr/local/sbin/pcvm-autologin -L $console 115200 vt100"
    if grep -q "^$console::" /etc/inittab; then
        sed -i "\\|^$console::|c\\$line" /etc/inittab
    else
        printf '%s\n' "$line" >> /etc/inittab
    fi
done
passwd -l root >/dev/null 2>&1 || true
passwd -l alpine >/dev/null 2>&1 || true
kill -HUP 1
`
	return strings.TrimSpace(fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
disable_root: true
ssh_pwauth: false
users:
  - name: pcvm
    gecos: PCVM Console User
    groups: [wheel]
    shell: /bin/ash
    lock_passwd: true
write_files:
  - path: /etc/doas.conf
    permissions: '0400'
    content: |
      permit nopass pcvm as root
  - path: /usr/local/sbin/pcvm-autologin
    permissions: '0755'
    encoding: b64
    content: %s
  - path: /usr/local/bin/sudo
    permissions: '0755'
    encoding: b64
    content: %s
  - path: /usr/local/sbin/pcvm-firstboot
    permissions: '0755'
    encoding: b64
    content: %s
runcmd:
  - [/bin/sh, /usr/local/sbin/pcvm-firstboot]
	`, hostname, encode(autologin), encode(sudoCompat), encode(firstBoot))) + "\n"
}

func vmGuestReadyScript() string {
	return `#!/bin/sh
active_consoles="$(cat /sys/class/tty/console/active 2>/dev/null || true)"
console=
for candidate in $active_consoles ttyAMA0 ttyS0; do
    case "$candidate" in
        ttyAMA0|ttyS0)
            if [ -c "/dev/$candidate" ]; then
                console="$candidate"
                break
            fi
            ;;
    esac
done
[ -n "$console" ] || exit 1
printf '%s\n' '[PCVM-GUEST] READY' > "/dev/$console"
`
}

func (p *catalogProvider) buildVMProcess(cfg Config, state LaunchState, memory MemoryPlan) (ProcessSpec, error) {
	dependencies := cfg.Dependencies.withDefaults()
	resources, err := calculateVMResourcesWithPlan(cfg.Request, cfg.Policy, readHostCgroupLimitsWith(dependencies).CPUQuota, memory)
	if err != nil {
		return ProcessSpec{}, err
	}
	resources = stabilizeARMTCGResources(cfg.Arch, cfg.Request, resources)
	vmDir := filepath.Join(cfg.Home, "vm")
	if err := validateVMFiles(vmDir); err != nil {
		return ProcessSpec{}, err
	}
	metaData, err := os.ReadFile(filepath.Join(vmDir, "install.json"))
	if err != nil {
		return ProcessSpec{}, fmt.Errorf("read VM install metadata: %w", err)
	}
	var meta vmInstallMetadata
	if state.VMImageID == "" || state.VMImageChecksum == "" {
		return ProcessSpec{}, fmt.Errorf("VM state does not reference an image pinned by the embedded catalog")
	}
	if err := json.Unmarshal(metaData, &meta); err != nil || meta.Schema != vmInstallSchema || meta.Provider != state.Provider || meta.Architecture != cfg.Arch ||
		meta.Version != state.ResolvedVersion || meta.Build != state.ResolvedBuild || meta.ImageID != state.VMImageID ||
		meta.Variant != state.VMImageVariant || !strings.EqualFold(meta.Checksum, state.VMImageChecksum) || meta.Compression != state.VMDiskCompression {
		return ProcessSpec{}, fmt.Errorf("VM install metadata does not match state")
	}
	code, _, err := dependencies.VMFirmware(cfg.Arch)
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

func stabilizeARMTCGResources(arch string, req Request, resources vmResources) vmResources {
	// Debian bookworm ships QEMU 7.2. Its multi-vCPU AArch64 TCG path can
	// deadlock current Alpine kernels during early userspace mounts. Keep the
	// safe automatic default at one vCPU on ARM64 while preserving an explicit
	// administrator/user VM_CPUS choice. AMD64 remains capped at two by the
	// normal resource planner.
	if arch == "arm64" && (req.VMCPUs == "" || req.VMCPUs == "auto") && resources.CPUs > 1 {
		resources.CPUs = 1
	}
	return resources
}

func qemuArguments(cfg Config, resources vmResources, code, qmp string) []string {
	vmDir := filepath.Join(cfg.Home, "vm")
	args := []string{"-name", "PCVM", "-accel", "tcg,thread=multi", "-m", strconv.Itoa(resources.MemoryMB), "-smp", strconv.Itoa(resources.CPUs),
		"-display", "none", "-serial", "stdio", "-monitor", "none", "-qmp", "unix:" + qmp + ",server=on,wait=off",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(vmDir, "uefi-vars.fd"),
		"-drive", "if=none,file=" + filepath.Join(vmDir, "disk.qcow2") + ",format=qcow2,id=osdisk,cache=writeback,discard=unmap"}
	if cfg.Arch == "amd64" {
		args = append(args,
			"-device", "virtio-blk-pci,drive=osdisk,bootindex=1",
			"-device", "virtio-scsi-pci,id=scsi0",
		)
		args = append([]string{"-machine", "q35", "-cpu", "max"}, args...)
	} else {
		// Use ROMless PCI transports: virtio-MMIO can starve Alpine's page
		// allocator under QEMU 7.2 TCG. Cortex-A76 supplies ARMv8.2 LSE atomics,
		// avoiding the LL/SC lockup seen with Cortex-A72 without exposing the
		// expensive SVE surface of QEMU's max model.
		args = append(args,
			"-device", "virtio-blk-pci,drive=osdisk,bootindex=1,romfile=",
			"-device", "virtio-scsi-pci,id=scsi0,romfile=",
		)
		args = append([]string{"-machine", "virt,gic-version=max", "-cpu", "cortex-a72"}, args...)
	}
	args = append(args,
		"-drive", "if=none,media=cdrom,readonly=on,file="+filepath.Join(vmDir, "seed.iso")+",format=raw,id=seed",
		"-device", "scsi-cd,drive=seed,bus=scsi0.0,bootindex=99",
		"-netdev", "user,id=net0",
	)
	if cfg.Arch == "amd64" {
		args = append(args, "-device", "virtio-net-pci,netdev=net0")
	} else {
		args = append(args,
			"-device", "virtio-net-pci,netdev=net0,romfile=",
			"-object", "rng-random,filename=/dev/urandom,id=rng0",
			"-device", "virtio-rng-pci,rng=rng0,romfile=",
		)
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
