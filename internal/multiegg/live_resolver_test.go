//go:build live

package multiegg

import (
	"context"
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
			resolved, err := NewProvider(spec).Resolve(ctx, Request{Version: "latest", Build: "latest", RuntimeVersion: "auto"}, NewHTTPClient())
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("resolved version=%s build=%s runtime=%s", resolved.Artifact.Version, resolved.Artifact.Build, resolved.RuntimeVersion)
		})
	}
}
