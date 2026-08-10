package pcvm

import (
	"context"
	"testing"
)

type dispatchTrackingProvider struct {
	installs int
	updates  int
}

func (p *dispatchTrackingProvider) Spec() ProviderSpec { return ProviderSpec{ID: "dispatch-test"} }
func (p *dispatchTrackingProvider) Resolve(context.Context, Request, *HTTPClient) (Resolved, error) {
	return Resolved{}, nil
}
func (p *dispatchTrackingProvider) Install(_ context.Context, _ InstallContext, resolved Resolved) (Resolved, error) {
	p.installs++
	return resolved, nil
}
func (p *dispatchTrackingProvider) Update(_ context.Context, _ InstallContext, resolved Resolved) (Resolved, error) {
	p.updates++
	return resolved, nil
}
func (p *dispatchTrackingProvider) BuildProcess(context.Context, Config, LaunchState, MemoryPlan) (ProcessSpec, error) {
	return ProcessSpec{}, nil
}
func (p *dispatchTrackingProvider) CompareVersions(a, b string) int { return CompareVersions(a, b) }

func TestInstallOrUpdateProviderDispatchesTypedLifecycle(t *testing.T) {
	provider := &dispatchTrackingProvider{}
	if _, err := installOrUpdateProvider(context.Background(), provider, InstallContext{}, Resolved{}, false); err != nil {
		t.Fatal(err)
	}
	if provider.installs != 1 || provider.updates != 0 {
		t.Fatalf("fresh dispatch: installs=%d updates=%d", provider.installs, provider.updates)
	}
	if _, err := installOrUpdateProvider(context.Background(), provider, InstallContext{}, Resolved{}, true); err != nil {
		t.Fatal(err)
	}
	if provider.installs != 1 || provider.updates != 1 {
		t.Fatalf("update dispatch: installs=%d updates=%d", provider.installs, provider.updates)
	}
}
