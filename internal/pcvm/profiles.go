package pcvm

import (
	"fmt"
	"strings"
)

func NormalizeImageProfile(raw string) (string, error) {
	profile := strings.ToLower(strings.TrimSpace(raw))
	if profile == "" {
		profile = ImageProfileFull
	}
	switch profile {
	case ImageProfileMinecraft, ImageProfileGames, ImageProfileApps, ImageProfileVM, ImageProfileFull:
		return profile, nil
	default:
		return "", fmt.Errorf("unsupported embedded image profile %q", raw)
	}
}

func ProviderImageCapability(spec ProviderSpec) string {
	if len(spec.MenuPath) == 0 {
		return ""
	}
	switch spec.MenuPath[0] {
	case "java", "proxy", "bedrock":
		return ImageProfileMinecraft
	case "games":
		return ImageProfileGames
	case "apps", "web":
		return ImageProfileApps
	case "vms":
		return ImageProfileVM
	default:
		return ""
	}
}

func ImageProfileSupports(profile string, spec ProviderSpec) bool {
	profile, err := NormalizeImageProfile(profile)
	if err != nil {
		return false
	}
	required := ProviderImageCapability(spec)
	return required != "" && (profile == ImageProfileFull || profile == required)
}

func ValidateProviderImageProfile(profile string, spec ProviderSpec) error {
	normalized, err := NormalizeImageProfile(profile)
	if err != nil {
		return err
	}
	if ImageProfileSupports(normalized, spec) {
		return nil
	}
	required := ProviderImageCapability(spec)
	if required == "" {
		return fmt.Errorf("provider %q does not declare a supported PCVM image capability", spec.ID)
	}
	return fmt.Errorf("provider %q requires the %s image; current PCVM image profile is %q (select the %s or universal image in Pterodactyl)", spec.ID, required, normalized, required)
}
