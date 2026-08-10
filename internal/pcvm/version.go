package pcvm

import (
	"strconv"
	"strings"
	"unicode"
)

func CompareVersions(a, b string) int {
	if aa, ok := parseSemanticVersion(a); ok {
		if bb, ok := parseSemanticVersion(b); ok {
			return compareSemanticVersion(aa, bb)
		}
	}
	return compareNaturalVersion(a, b)
}

type semanticVersion struct {
	core       []int
	prerelease []string
}

// parseSemanticVersion intentionally accepts shortened Minecraft-style cores
// such as 1.21 in addition to strict three-component SemVer. Build metadata is
// ignored for ordering, while every pre-release component participates.
func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if value == "" {
		return semanticVersion{}, false
	}
	if build := strings.IndexByte(value, '+'); build >= 0 {
		value = value[:build]
	}
	coreText, preText, hasPre := value, "", false
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		coreText, preText, hasPre = value[:dash], value[dash+1:], true
	}
	parts := strings.Split(coreText, ".")
	if len(parts) == 0 {
		return semanticVersion{}, false
	}
	parsed := semanticVersion{core: make([]int, 0, len(parts))}
	for _, part := range parts {
		if part == "" {
			return semanticVersion{}, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return semanticVersion{}, false
		}
		parsed.core = append(parsed.core, number)
	}
	if hasPre {
		if preText == "" {
			return semanticVersion{}, false
		}
		parsed.prerelease = strings.Split(preText, ".")
		for _, part := range parsed.prerelease {
			if part == "" {
				return semanticVersion{}, false
			}
		}
	}
	return parsed, true
}

func compareSemanticVersion(a, b semanticVersion) int {
	for index := 0; index < len(a.core) || index < len(b.core); index++ {
		av, bv := 0, 0
		if index < len(a.core) {
			av = a.core[index]
		}
		if index < len(b.core) {
			bv = b.core[index]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	if len(a.prerelease) == 0 || len(b.prerelease) == 0 {
		if len(a.prerelease) == len(b.prerelease) {
			return 0
		}
		if len(a.prerelease) == 0 {
			return 1
		}
		return -1
	}
	for index := 0; index < len(a.prerelease) || index < len(b.prerelease); index++ {
		if index >= len(a.prerelease) {
			return -1
		}
		if index >= len(b.prerelease) {
			return 1
		}
		av, aNumeric := decimalIdentifier(a.prerelease[index])
		bv, bNumeric := decimalIdentifier(b.prerelease[index])
		switch {
		case aNumeric && bNumeric:
			if av < bv {
				return -1
			}
			if av > bv {
				return 1
			}
		case aNumeric:
			return -1
		case bNumeric:
			return 1
		default:
			if compared := compareNaturalVersion(a.prerelease[index], b.prerelease[index]); compared != 0 {
				return compared
			}
		}
	}
	return 0
}

func decimalIdentifier(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}

type naturalVersionToken struct {
	number  int
	text    string
	numeric bool
}

func compareNaturalVersion(a, b string) int {
	aa, bb := naturalVersionTokens(a), naturalVersionTokens(b)
	for index := 0; index < len(aa) || index < len(bb); index++ {
		if index >= len(aa) {
			return -1
		}
		if index >= len(bb) {
			return 1
		}
		av, bv := aa[index], bb[index]
		if av.numeric && bv.numeric {
			if av.number < bv.number {
				return -1
			}
			if av.number > bv.number {
				return 1
			}
			continue
		}
		if av.numeric != bv.numeric {
			if av.numeric {
				return 1
			}
			return -1
		}
		if compared := strings.Compare(av.text, bv.text); compared != 0 {
			return compared
		}
	}
	return 0
}

func naturalVersionTokens(value string) []naturalVersionToken {
	value = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v")))
	var tokens []naturalVersionToken
	for index := 0; index < len(value); {
		character := rune(value[index])
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			index++
			continue
		}
		start, numeric := index, unicode.IsDigit(character)
		for index < len(value) {
			current := rune(value[index])
			if unicode.IsDigit(current) != numeric || !unicode.IsLetter(current) && !unicode.IsDigit(current) {
				break
			}
			index++
		}
		part := value[start:index]
		if numeric {
			number, err := strconv.Atoi(part)
			if err == nil {
				tokens = append(tokens, naturalVersionToken{number: number, numeric: true})
				continue
			}
		}
		tokens = append(tokens, naturalVersionToken{text: part})
	}
	return tokens
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
		case minor <= 19:
			return "17"
		case minor == 20:
			// Mojang raised the runtime floor to Java 21 in 1.20.5. Keep
			// 1.20.1-1.20.4 on Java 17 for loader/plugin compatibility.
			if len(parts) >= 3 && parts[2] >= 5 {
				return "21"
			}
			return "17"
		default:
			return "21"
		}
	}
	return "21"
}
