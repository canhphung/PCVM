package pcvm

import "testing"

func TestDiskPlannerPeakByRollbackMode(t *testing.T) {
	staged := ProviderSpec{MinimumDisk: 1024, RollbackMode: "staged"}
	plan := planDisk(staged, 100<<20, 50<<20, false)
	want := int64(1024+100+50+128) << 20
	if plan.RequiredBytes != want {
		t.Fatalf("staged peak = %d, want %d", plan.RequiredBytes, want)
	}

	inPlace := ProviderSpec{MinimumDisk: 1024, RollbackMode: "in-place"}
	plan = planDisk(inPlace, 100<<20, 0, false)
	want = int64(512+128) << 20
	if plan.RequiredBytes != want {
		t.Fatalf("in-place peak = %d, want %d", plan.RequiredBytes, want)
	}

	plan = planDisk(inPlace, 700<<20, 0, false)
	want = int64(700+128) << 20
	if plan.RequiredBytes != want {
		t.Fatalf("large in-place download peak = %d, want %d", plan.RequiredBytes, want)
	}
}

func TestRuntimePackDownloadSizeUsesExactIdentity(t *testing.T) {
	catalog := Catalog{RuntimePacks: []RuntimePackSpec{{Kind: "java", Version: "21", Architecture: "amd64", Size: 42}}}
	if got := runtimePackDownloadSize(catalog, "java", "21", "amd64"); got != 42 {
		t.Fatalf("runtime size = %d", got)
	}
	if got := runtimePackDownloadSize(catalog, "java", "17", "amd64"); got != 0 {
		t.Fatalf("unexpected runtime size %d", got)
	}
}

func TestGitSourceDiskPlanUsesStagedPeak(t *testing.T) {
	spec := ProviderSpec{ID: "go-app", Resolver: "local-app", RollbackMode: "in-place", MinimumDisk: 512}
	planned := diskPlanningSpec(spec, Request{SourceMode: "git"})
	if planned.RollbackMode != "staged" {
		t.Fatalf("Git source rollback mode = %q", planned.RollbackMode)
	}
	plan := planDisk(planned, 32<<20, 64<<20, false)
	want := int64(32+64+512+128) << 20
	if plan.RequiredBytes != want {
		t.Fatalf("required=%d want=%d", plan.RequiredBytes, want)
	}
	if upload := diskPlanningSpec(spec, Request{SourceMode: "upload"}); upload.RollbackMode != "in-place" {
		t.Fatalf("upload mode unexpectedly changed to %q", upload.RollbackMode)
	}
}
