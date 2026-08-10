package pcvm

import (
	"testing"
	"time"
)

func reconcileState() *State {
	req := Request{Version: "1.21.1", Build: "10", RuntimeVersion: "21"}
	spec := ProviderSpec{ID: "paper", Family: "paper", Installer: "jar", InstallFormat: 1}
	state := newStateFromInstall(spec, req, Resolved{Artifact: Artifact{Version: "1.21.1", Build: "10"}, RuntimeKind: "java", RuntimeVersion: "21"}, "amd64", testTime())
	state.Family = spec.Family
	return &state
}

func TestReconcileSelectionAndUpdateLifecycle(t *testing.T) {
	state := reconcileState()
	spec := ProviderSpec{ID: "paper", Family: "paper", Installer: "jar", InstallFormat: 1}
	base := Request{Version: "1.21.1", Build: "10", RuntimeVersion: "21"}
	if got := Reconcile(state, spec, base, nil); got.Kind != ActionRun || got.RequiresResolve {
		t.Fatalf("unchanged=%+v", got)
	}
	changed := base
	changed.RuntimeVersion = "25"
	if got := Reconcile(state, spec, changed, nil); got.Kind != ActionUpdate || !got.RequiresResolve {
		t.Fatalf("runtime change=%+v", got)
	}
	changed = base
	changed.Version = "latest"
	if got := Reconcile(state, spec, changed, nil); !got.RequiresResolve {
		t.Fatalf("pinned to latest=%+v", got)
	}
	changed = base
	changed.UpdateRequest = "new-token"
	if got := Reconcile(state, spec, changed, nil); !got.RequiresResolve {
		t.Fatalf("update request=%+v", got)
	}
}

func TestReconcileCrossProviderDowngradeRequiresReset(t *testing.T) {
	state := reconcileState()
	target := ProviderSpec{ID: "purpur", Family: "paper", Installer: "jar", VersionDomain: "minecraft"}
	resolved := Resolved{Artifact: Artifact{Version: "1.20.6", Build: "1"}}
	got := Reconcile(state, target, Request{Version: "1.20.6", Build: "1", RuntimeVersion: "21"}, &resolved)
	if got.Kind != ActionReset || got.Reason != "downgrade requires reset" {
		t.Fatalf("plan=%+v", got)
	}
}

func TestReconcileVMImmutableChangeRequiresReset(t *testing.T) {
	req := Request{Version: "13", Build: "1", RuntimeVersion: "native", VMHostname: "pcvm", VMDiskGB: 10, VMDiskCompression: "off"}
	spec := ProviderSpec{ID: "vm-debian", Family: "vm-debian", Installer: "qemu-vm", InstallFormat: 3}
	state := newStateFromInstall(spec, req, Resolved{Artifact: Artifact{Version: "13", Build: "1"}, RuntimeKind: "native", RuntimeVersion: "native"}, "amd64", testTime())
	state.Family = spec.Family
	req.VMHostname = "changed"
	got := Reconcile(&state, spec, req, nil)
	if got.Kind != ActionReset {
		t.Fatalf("plan=%+v", got)
	}
}

func testTime() (out time.Time) { return time.Unix(1, 0).UTC() }
