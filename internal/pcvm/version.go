package pcvm

import (
	"strconv"
	"strings"
)

func CompareVersions(a, b string) int {
	aa, bb := versionParts(a), versionParts(b)
	for i := 0; i < len(aa) || i < len(bb); i++ {
		var av, bv int
		if i < len(aa) {
			av = aa[i]
		}
		if i < len(bb) {
			bv = bb[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	if len(aa) > 0 && len(bb) > 0 {
		return 0
	}
	return strings.Compare(a, b)
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == '_' })
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func JavaVersionFor(softwareVersion string) string {
	if softwareVersion == "latest" || softwareVersion == "" {
		return "21"
	}
	parts := versionParts(softwareVersion)
	if len(parts) >= 2 {
		if parts[0] >= 26 {
			return "25"
		}
		if parts[0] >= 20 {
			return "21"
		}
		minor := parts[1]
		switch {
		case minor <= 12:
			return "8"
		case minor <= 16:
			return "11"
		case minor <= 20:
			return "17"
		default:
			return "21"
		}
	}
	return "21"
}
