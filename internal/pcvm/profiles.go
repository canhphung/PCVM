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
	case ImageProfileCore, ImageProfileGames, ImageProfileVM, ImageProfileFull:
		return profile, nil
	default:
		return "", fmt.Errorf("unsupported embedded image profile %q", raw)
	}
}

func ProviderImageCapability(spec ProviderSpec) string {
	if spec.Installer == "qemu-vm" {
		return ImageProfileVM
	}
	if len(spec.MenuPath) > 0 && spec.MenuPath[0] == "games" {
		return ImageProfileGames
	}
	return ImageProfileCore
}

func ImageProfileSupports(profile string, spec ProviderSpec) bool {
	profile, err := NormalizeImageProfile(profile)
	if err != nil {
		return false
	}
	required := ProviderImageCapability(spec)
	switch profile {
	case ImageProfileFull:
		return true
	case ImageProfileGames:
		return required == ImageProfileCore || required == ImageProfileGames
	case ImageProfileVM:
		return required == ImageProfileCore || required == ImageProfileVM
	default:
		return required == ImageProfileCore
	}
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
	return fmt.Errorf("provider %q requires the %s image capability; current PCVM image profile is %q (select the %s or full image in Pterodactyl)", spec.ID, required, normalized, required)
}
