package pcvm

import (
	"archive/zip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModrinthResolverPrefersPrimaryPackAndSealsPublicationOrder(t *testing.T) {
	digestA := strings.Repeat("a", 128)
	digestB := strings.Repeat("b", 128)
	endpoint := "https://api.modrinth.com/v2/project/example-pack/version"
	httpClient := NewHTTPClient()
	httpClient.Client = &http.Client{Transport: fixtureTransport{endpoint: `[{"id":"version-2","project_id":"project-1","version_number":"release-z","date_published":"2026-08-09T11:12:13.123Z","files":[{"filename":"fallback.mrpack","url":"https://cdn.modrinth.com/fallback.mrpack","primary":false,"hashes":{"sha512":"` + digestA + `"}},{"filename":"canonical.mrpack","url":"https://cdn.modrinth.com/canonical.mrpack","primary":true,"hashes":{"sha512":"` + digestB + `"}}]}]`}}

	artifact, err := resolveModrinth(context.Background(), Request{ModpackMode: "project", ModpackProject: "example-pack", Version: "latest", Build: "latest"}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.URL != "https://cdn.modrinth.com/canonical.mrpack" || artifact.SHA512 != digestB {
		t.Fatalf("resolver did not prefer primary .mrpack: %+v", artifact)
	}
	wantRevision := "2026-08-09T11:12:13.123Z"
	if artifact.Metadata["modrinth_order_revision"] != wantRevision || lockArtifact("modrinth-modpack", artifact).Revision != wantRevision {
		t.Fatalf("publication order was not preserved: artifact=%+v lock=%+v", artifact, lockArtifact("modrinth-modpack", artifact))
	}
}

func TestModrinthProjectDowngradeUsesPublicationTimeNotDisplayLabel(t *testing.T) {
	spec := catalogSpec(t, "modrinth-modpack")
	currentArtifact := Artifact{Version: "release-a", Build: "opaque-new", Metadata: map[string]string{
		"modrinth_project_id": "project-1", "modrinth_loader": "fabric", "modrinth_order_revision": "2026-08-09T00:00:00Z",
	}}
	current := &State{Provider: spec.ID, Family: spec.Family, ResolvedVersion: currentArtifact.Version, ResolvedBuild: currentArtifact.Build, ArtifactLock: lockArtifact(spec.ID, currentArtifact)}

	older := Resolved{Artifact: Artifact{Version: "release-z", Build: "opaque-old", Metadata: map[string]string{
		"modrinth_project_id": "project-1", "modrinth_loader": "fabric", "modrinth_order_revision": "2026-08-08T23:59:59Z",
	}}}
	if plan := Reconcile(current, spec, Request{}, &older); plan.Kind != ActionReset || !strings.Contains(plan.Reason, "downgrade") {
		t.Fatalf("older publication was not treated as downgrade: %+v", plan)
	}

	newer := Resolved{Artifact: Artifact{Version: "release-0", Build: "opaque-next", Metadata: map[string]string{
		"modrinth_project_id": "project-1", "modrinth_loader": "fabric", "modrinth_order_revision": "2026-08-10T00:00:00Z",
	}}}
	if plan := Reconcile(current, spec, Request{}, &newer); plan.Kind != ActionUpdate {
		t.Fatalf("newer publication was rejected because of its display label: %+v", plan)
	}

	unverifiable := newer
	unverifiable.Artifact.Metadata = map[string]string{"modrinth_project_id": "project-1", "modrinth_loader": "fabric"}
	if plan := Reconcile(current, spec, Request{}, &unverifiable); plan.Kind != ActionReset || !strings.Contains(plan.Reason, "cannot be verified") {
		t.Fatalf("changed project artifact without publication order was accepted: %+v", plan)
	}
}

func TestModrinthUploadIdentityIsStableAcrossVersions(t *testing.T) {
	first, err := modrinthUploadProjectID(mrpackIndex{Name: "  Example   Pack ", VersionID: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := modrinthUploadProjectID(mrpackIndex{Name: "example pack", VersionID: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	different, err := modrinthUploadProjectID(mrpackIndex{Name: "Another Pack", VersionID: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == different || !strings.HasPrefix(first, "upload-") {
		t.Fatalf("unexpected upload identities: first=%q second=%q different=%q", first, second, different)
	}

	spec := catalogSpec(t, "modrinth-modpack")
	currentArtifact := Artifact{Version: "2.0.0", Build: "content-a", Metadata: map[string]string{"modrinth_project_id": first, "modrinth_loader": "fabric"}}
	current := &State{Provider: spec.ID, Family: spec.Family, ResolvedVersion: "2.0.0", ResolvedBuild: "content-a", ArtifactLock: lockArtifact(spec.ID, currentArtifact)}
	older := Resolved{Artifact: Artifact{Version: "1.9.0", Build: "content-b", Metadata: map[string]string{"modrinth_project_id": second, "modrinth_loader": "fabric"}}}
	if plan := Reconcile(current, spec, Request{}, &older); plan.Kind != ActionReset {
		t.Fatalf("uploaded pack downgrade was accepted: %+v", plan)
	}
	differentLoader := Resolved{Artifact: Artifact{Version: "2.1.0", Build: "content-c", Metadata: map[string]string{"modrinth_project_id": second, "modrinth_loader": "quilt"}}}
	if plan := Reconcile(current, spec, Request{}, &differentLoader); plan.Kind != ActionReset || !strings.Contains(plan.Reason, "project") {
		t.Fatalf("uploaded pack loader change did not require reset: %+v", plan)
	}
}

func TestModrinthPayloadCannotTargetPCVMControlTree(t *testing.T) {
	tests := []struct {
		name       string
		indexFiles string
		override   string
	}{
		{name: "indexed operation journal", indexFiles: `[{"path":".pcvm/operation.json","hashes":{"sha512":"` + strings.Repeat("a", 128) + `"},"downloads":["https://cdn.modrinth.com/operation.json"],"fileSize":1}]`},
		{name: "mixed case transaction override", override: "server-overrides/.PcVm/transactions/modrinth-modpack/transaction.json"},
		{name: "canonicalized quarantine override", override: "overrides/config/../.PCVM/quarantine/restore.json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.indexFiles == "" {
				test.indexFiles = "[]"
			}
			pack := writeModrinthTestPack(t, `{"formatVersion":1,"game":"minecraft","name":"Security Fixture","versionId":"1.0.0","dependencies":{"minecraft":"1.21.8"},"files":`+test.indexFiles+`}`, test.override)
			index, reader, err := readMRPack(pack)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			staging := t.TempDir()
			sentinel := filepath.Join(staging, ".pcvm", "operation.json")
			if err := os.MkdirAll(filepath.Dir(sentinel), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinel, []byte("canonical-operation"), 0o640); err != nil {
				t.Fatal(err)
			}
			err = stageMRPackFiles(context.Background(), NewHTTPClient(), reader, index, staging)
			if err == nil || !strings.Contains(err.Error(), "reserved .pcvm") {
				t.Fatalf("reserved control path was accepted: %v", err)
			}
			body, readErr := os.ReadFile(sentinel)
			if readErr != nil || string(body) != "canonical-operation" {
				t.Fatalf("canonical operation journal was sabotaged: body=%q err=%v", body, readErr)
			}
		})
	}
}

func writeModrinthTestPack(t *testing.T, indexJSON, override string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.mrpack")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("modrinth.index.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(indexJSON)); err != nil {
		t.Fatal(err)
	}
	if override != "" {
		entry, err = writer.Create(override)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
