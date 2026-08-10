package pcvm

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type gameArgRule struct {
	values int
}

func valueGameArgs(names ...string) map[string]gameArgRule {
	rules := make(map[string]gameArgRule, len(names))
	for _, name := range names {
		rules[strings.ToLower(name)] = gameArgRule{values: 1}
	}
	return rules
}

func flagGameArgs(rules map[string]gameArgRule, names ...string) map[string]gameArgRule {
	if rules == nil {
		rules = make(map[string]gameArgRule, len(names))
	}
	for _, name := range names {
		rules[strings.ToLower(name)] = gameArgRule{}
	}
	return rules
}

// gameExtraArgRules is intentionally provider-specific and deny-by-default.
// An option being harmless for one upstream does not make it safe for another
// binary whose parser may assign it a different meaning.
var gameExtraArgRules = map[string]map[string]gameArgRule{
	"cs2":  valueGameArgs("-tickrate", "+game_type", "+game_mode", "+mapgroup", "+sv_lan", "+tv_enable"),
	"gmod": valueGameArgs("+gamemode", "+host_workshop_collection", "+sv_lan", "+sv_region"),
	"l4d2": valueGameArgs("+mp_gamemode", "+sv_lan", "+sv_region"),
	"rust": valueGameArgs(
		"+server.description", "+server.url", "+server.headerimage", "+server.tags",
		"+server.saveinterval", "+server.worldsize", "+server.tickrate", "+server.secure",
		"+server.encryption", "+server.eac", "+server.radiation", "+server.stability",
	),
	"rust-umod": valueGameArgs(
		"+server.description", "+server.url", "+server.headerimage", "+server.tags",
		"+server.saveinterval", "+server.worldsize", "+server.tickrate", "+server.secure",
		"+server.encryption", "+server.eac", "+server.radiation", "+server.stability",
	),
	"project-zomboid": flagGameArgs(nil, "-nosteam"),
	"valheim":         flagGameArgs(valueGameArgs("-saveinterval", "-backups", "-backupshort", "-backuplong"), "-crossplay"),
	"valheim-bepinex": flagGameArgs(valueGameArgs("-saveinterval", "-backups", "-backupshort", "-backuplong"), "-crossplay"),
	"7dtd":            flagGameArgs(nil, "-disablecursorsnapping"),
	"unturned":        flagGameArgs(nil, "-noworkshop"),
	"terraria":        valueGameArgs("-difficulty", "-motd", "-lang"),
	"tmodloader":      valueGameArgs("-difficulty", "-motd", "-lang"),
	"tshock":          valueGameArgs("-difficulty", "-motd", "-lang", "-worldname"),
	"factorio":        flagGameArgs(valueGameArgs("--autosave-interval", "--autosave-slots", "--afk-autokick-interval"), "--use-server-whitelist", "--autosave-only-on-server", "--non-blocking-saving"),
}

func safeGameExtraArgs(providerID, raw string) ([]string, error) {
	args, err := SplitArgs(raw)
	if err != nil {
		return nil, fmt.Errorf("GAME_EXTRA_ARGS: %w", err)
	}
	if len(args) == 0 {
		return nil, nil
	}
	if len(args) > 64 {
		return nil, fmt.Errorf("GAME_EXTRA_ARGS permits at most 64 tokens")
	}
	rules := gameExtraArgRules[providerID]
	if len(rules) == 0 {
		return nil, fmt.Errorf("GAME_EXTRA_ARGS is not supported by provider %q", providerID)
	}
	for index := 0; index < len(args); index++ {
		token := args[index]
		if err := validateGameArgToken(token); err != nil {
			return nil, err
		}
		name, inlineValue, hasInlineValue := strings.Cut(token, "=")
		rule, ok := rules[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("GAME_EXTRA_ARGS option %q is not allowed for provider %q (allowed: %s)", name, providerID, allowedGameArgs(rules))
		}
		if rule.values == 0 {
			if hasInlineValue {
				return nil, fmt.Errorf("GAME_EXTRA_ARGS flag %q does not accept a value", name)
			}
			continue
		}
		if hasInlineValue {
			if inlineValue == "" {
				return nil, fmt.Errorf("GAME_EXTRA_ARGS option %q requires a value", name)
			}
			if err := validateGameArgToken(inlineValue); err != nil {
				return nil, err
			}
			continue
		}
		if index+1 >= len(args) {
			return nil, fmt.Errorf("GAME_EXTRA_ARGS option %q requires a value", name)
		}
		if strings.HasPrefix(args[index+1], "-") || strings.HasPrefix(args[index+1], "+") {
			return nil, fmt.Errorf("GAME_EXTRA_ARGS option %q requires a value", name)
		}
		index++
		if err := validateGameArgToken(args[index]); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func validateGameArgToken(token string) error {
	if token == "" || len(token) > 256 {
		return fmt.Errorf("GAME_EXTRA_ARGS tokens must contain 1 to 256 bytes")
	}
	if strings.HasPrefix(token, "@") {
		return fmt.Errorf("GAME_EXTRA_ARGS response-file tokens beginning with @ are not allowed")
	}
	for _, character := range token {
		if character == 0 || unicode.IsControl(character) {
			return fmt.Errorf("GAME_EXTRA_ARGS contains a control character")
		}
	}
	return nil
}

func allowedGameArgs(rules map[string]gameArgRule) string {
	values := make([]string, 0, len(rules))
	for value := range rules {
		values = append(values, value)
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}
