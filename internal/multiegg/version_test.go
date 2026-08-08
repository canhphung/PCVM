package multiegg

import "testing"

func TestVersionsAndRuntimeMapping(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"1.21.4", "1.21.1", 1}, {"1.20", "1.20.0", 0}, {"1.19.4", "1.20", -1}} {
		got := CompareVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("CompareVersions(%q,%q)=%d", tc.a, tc.b, got)
		}
	}
	for version, want := range map[string]string{"1.8.9": "8", "1.16.5": "11", "1.20.4": "17", "1.21.1": "21", "21.11.2": "21", "26.1": "25"} {
		if got := JavaVersionFor(version); got != want {
			t.Errorf("JavaVersionFor(%s)=%s", version, got)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	args, err := SplitArgs(`--name "hello world" --count=2`)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[1] != "hello world" {
		t.Fatalf("%#v", args)
	}
	if _, err = SplitArgs(`"broken`); err == nil {
		t.Fatal("expected quote error")
	}
}
