package pcvm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const pcvmFiglet = `    ____  _______    ____  ___
   / __ \/ ____/ |  / /  |/  /
  / /_/ / /    | | / / /|_/ /
 / ____/ /___  | |/ / /  / /
/_/    \____/  |___/_/  /_/`

type menuCategory struct {
	ID          string
	Name        string
	Description string
}

var menuCategoryOrder = []menuCategory{
	{ID: "java", Name: "Minecraft Java", Description: "Vanilla, optimized servers and mod loaders"},
	{ID: "proxy", Name: "Minecraft Proxies", Description: "Route players between backend servers"},
	{ID: "bedrock", Name: "Minecraft Bedrock", Description: "Official Bedrock and PocketMine-MP"},
	{ID: "apps", Name: "Applications & Bots", Description: "Node.js, Python and Lavalink"},
}

func (a *App) menu() (string, error) {
	available := a.Catalog.Available(a.Config.Arch, a.Config.Policy.AllowedSoftware)
	filtered := available[:0]
	for _, provider := range available {
		if a.Config.Policy.AllowSystemPath || a.Catalog.HasRuntime(provider.Runtime, "", a.Config.Arch) {
			filtered = append(filtered, provider)
		}
	}
	if len(filtered) == 0 {
		return "", fmt.Errorf("host policy exposes no providers")
	}
	groups := make(map[string][]ProviderSpec)
	for _, provider := range filtered {
		category := providerMenuCategory(provider.ID)
		groups[category] = append(groups[category], provider)
	}
	categories := make([]menuCategory, 0, len(menuCategoryOrder))
	for _, category := range menuCategoryOrder {
		if len(groups[category.ID]) > 0 {
			categories = append(categories, category)
		}
	}
	reader := bufio.NewReader(a.In)
	a.renderMenuHeader()
	for {
		a.renderCategoryMenu(categories, groups)
		choice, err := readMenuChoice(reader)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(choice, "q") {
			return "", fmt.Errorf("software selection cancelled")
		}
		categoryIndex, err := strconv.Atoi(choice)
		if err != nil || categoryIndex < 1 || categoryIndex > len(categories) {
			a.menuWarning("Choose a category number from 1 to %d, or q to quit.", len(categories))
			continue
		}
		category := categories[categoryIndex-1]
		providers := groups[category.ID]
		for {
			a.renderProviderMenu(category, providers)
			choice, err = readMenuChoice(reader)
			if err != nil {
				return "", err
			}
			if strings.EqualFold(choice, "b") {
				break
			}
			if strings.EqualFold(choice, "q") {
				return "", fmt.Errorf("software selection cancelled")
			}
			providerIndex, parseErr := strconv.Atoi(choice)
			if parseErr != nil || providerIndex < 1 || providerIndex > len(providers) {
				a.menuWarning("Choose a software number from 1 to %d, b to go back, or q to quit.", len(providers))
				continue
			}
			selected := providers[providerIndex-1]
			fmt.Fprintf(a.Out, "\n%sSelected:%s %s %s[%s]%s\n\n", menuGreen(), menuReset(), selected.Name, menuDim(), selected.ID, menuReset())
			return selected.ID, nil
		}
	}
}

func (a *App) renderMenuHeader() {
	fmt.Fprintf(a.Out, "\n%s%s%s\n", menuCyan(), pcvmFiglet, menuReset())
	fmt.Fprintf(a.Out, "%s%s%s  %s%s%s  %s(%s)%s\n", menuBold(), a.Config.Policy.BrandName, menuReset(), menuDim(), "Pterodactyl multi-provider launcher", menuReset(), menuDim(), a.Config.Arch, menuReset())
}

func (a *App) renderCategoryMenu(categories []menuCategory, groups map[string][]ProviderSpec) {
	fmt.Fprintf(a.Out, "\n%s╭─ SELECT A SOFTWARE CATEGORY ─────────────────────────────╮%s\n", menuBlue(), menuReset())
	for index, category := range categories {
		fmt.Fprintf(a.Out, "%s│%s  %s[%d]%s %-22s %2d software                  %s│%s\n", menuBlue(), menuReset(), menuYellow(), index+1, menuReset(), category.Name, len(groups[category.ID]), menuBlue(), menuReset())
		fmt.Fprintf(a.Out, "%s│%s      %s%-52s%s%s│%s\n", menuBlue(), menuReset(), menuDim(), category.Description, menuReset(), menuBlue(), menuReset())
	}
	fmt.Fprintf(a.Out, "%s│%s  %s[q]%s Quit                                                %s│%s\n", menuBlue(), menuReset(), menuYellow(), menuReset(), menuBlue(), menuReset())
	fmt.Fprintf(a.Out, "%s╰──────────────────────────────────────────────────────────╯%s\n", menuBlue(), menuReset())
	fmt.Fprintf(a.Out, "%sSelect category%s › ", menuBold(), menuReset())
}

func (a *App) renderProviderMenu(category menuCategory, providers []ProviderSpec) {
	fmt.Fprintf(a.Out, "\n%s╭─ %-56s╮%s\n", menuBlue(), strings.ToUpper(category.Name)+" ", menuReset())
	for index, provider := range providers {
		fmt.Fprintf(a.Out, "%s│%s  %s[%d]%s %-31s %s%-20s%s%s│%s\n", menuBlue(), menuReset(), menuYellow(), index+1, menuReset(), provider.Name, menuDim(), provider.ID, menuReset(), menuBlue(), menuReset())
	}
	fmt.Fprintf(a.Out, "%s│%s  %s[b]%s Back        %s[q]%s Quit                                %s│%s\n", menuBlue(), menuReset(), menuYellow(), menuReset(), menuYellow(), menuReset(), menuBlue(), menuReset())
	fmt.Fprintf(a.Out, "%s╰──────────────────────────────────────────────────────────╯%s\n", menuBlue(), menuReset())
	fmt.Fprintf(a.Out, "%sSelect software%s › ", menuBold(), menuReset())
}

func (a *App) menuWarning(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.Out, "\n%s[PCVM] %s%s\n", menuYellow(), message, menuReset())
}

func readMenuChoice(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	choice := strings.TrimSpace(line)
	if err == nil || errors.Is(err, io.EOF) && choice != "" {
		return choice, nil
	}
	if errors.Is(err, io.EOF) {
		return "", fmt.Errorf("console input closed during software selection")
	}
	return "", err
}

func providerMenuCategory(providerID string) string {
	switch providerID {
	case "vanilla", "paper", "purpur", "pufferfish", "fabric", "forge", "neoforge":
		return "java"
	case "velocity", "bungeecord":
		return "proxy"
	case "bedrock", "pocketmine":
		return "bedrock"
	default:
		return "apps"
	}
}

func menuColors() bool { return os.Getenv("NO_COLOR") == "" && os.Getenv("PCVM_COLOR") != "0" }
func menuCode(code string) string {
	if menuColors() {
		return "\x1b[" + code + "m"
	}
	return ""
}
func menuReset() string  { return menuCode("0") }
func menuBold() string   { return menuCode("1") }
func menuDim() string    { return menuCode("2") }
func menuCyan() string   { return menuCode("96") }
func menuBlue() string   { return menuCode("94") }
func menuYellow() string { return menuCode("93") }
func menuGreen() string  { return menuCode("92") }
