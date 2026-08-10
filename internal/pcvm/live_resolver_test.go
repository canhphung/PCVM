//go:build live

package pcvm

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestLiveResolvers(t *testing.T) {
	catalog, err := LoadCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range catalog.Providers {
		if spec.Resolver == "local-app" {
			continue
		}
		spec := spec
		t.Run(spec.ID, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			httpc := NewHTTPClient()
			request := Request{Version: "latest", Build: "latest", RuntimeVersion: "auto", Architecture: runtime.GOARCH}
			if spec.ID == "modrinth-modpack" {
				// A small public, server-side Fabric pack maintained on Modrinth.
				// The generic live matrix must provide an explicit project because
				// PCVM deliberately never auto-detects or bypasses provider policy.
				request.ModpackMode = "project"
				request.ModpackProject = "sfs"
			}
			resolved, err := NewProvider(spec).Resolve(ctx, request, httpc)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("resolved version=%s build=%s runtime=%s", resolved.Artifact.Version, resolved.Artifact.Build, resolved.RuntimeVersion)
			// The official Bedrock CDN intentionally stalls automated range probes in
			// some regions; its first-party download-service response is the contract.
			if resolved.Artifact.URL != "" && spec.ID != "bedrock" {
				if err := httpc.Probe(ctx, resolved.Artifact.URL); err != nil {
					t.Fatalf("artifact URL is not reachable: %v", err)
				}
			}
		})
	}
}
