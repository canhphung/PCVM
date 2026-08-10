package pcvm

import (
	"context"
	"net"
	"os"
)

type FileReadFunc func(string) ([]byte, error)
type EnvironmentReadFunc func(string) string
type DNSLookupFunc func(context.Context, string) ([]net.IPAddr, error)
type VMToolFunc func(context.Context, anyWriter, anyWriter, string, ...string) error
type VMFirmwareFunc func(string) (string, string, error)

// Dependencies contains the host-facing operations whose behavior must be
// replaceable in tests. It is carried by Config/InstallContext rather than
// mutated through process-global hooks, so parallel tests and concurrent
// reconciliations cannot change each other's filesystem, cgroup, DNS, or VM
// tool view.
type Dependencies struct {
	ReadFile   FileReadFunc
	Getenv     EnvironmentReadFunc
	LookupIP   DNSLookupFunc
	RunVMTool  VMToolFunc
	VMFirmware VMFirmwareFunc
}

func DefaultDependencies() Dependencies {
	return Dependencies{
		ReadFile:   os.ReadFile,
		Getenv:     os.Getenv,
		LookupIP:   net.DefaultResolver.LookupIPAddr,
		RunVMTool:  runVMTool,
		VMFirmware: vmFirmware,
	}
}

func (dependencies Dependencies) withDefaults() Dependencies {
	defaults := DefaultDependencies()
	if dependencies.ReadFile == nil {
		dependencies.ReadFile = defaults.ReadFile
	}
	if dependencies.Getenv == nil {
		dependencies.Getenv = defaults.Getenv
	}
	if dependencies.LookupIP == nil {
		dependencies.LookupIP = defaults.LookupIP
	}
	if dependencies.RunVMTool == nil {
		dependencies.RunVMTool = defaults.RunVMTool
	}
	if dependencies.VMFirmware == nil {
		dependencies.VMFirmware = defaults.VMFirmware
	}
	return dependencies
}
