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

var menuCategories = map[string]menuCategory{
	"java":     {ID: "java", Name: "Minecraft Java", Description: "Vanilla, optimized servers and mod loaders"},
	"proxy":    {ID: "proxy", Name: "Minecraft Proxies", Description: "Route players between backend servers"},
	"bedrock":  {ID: "bedrock", Name: "Minecraft Bedrock", Description: "Official Bedrock and PocketMine-MP"},
	"games":    {ID: "games", Name: "Game Servers", Description: "Native Linux dedicated game servers"},
	"source":   {ID: "source", Name: "Source & FPS", Description: "Counter-Strike, Garry's Mod and Left 4 Dead"},
	"survival": {ID: "survival", Name: "Survival", Description: "Persistent survival and co-op worlds"},
	"sandbox":  {ID: "sandbox", Name: "Sandbox & Automation", Description: "Building, automation and sandbox games"},
	"web":      {ID: "web", Name: "Web Servers", Description: "Static hosting and reverse proxies"},
	"apps":     {ID: "apps", Name: "Applications & Bots", Description: "Node.js, Python and Lavalink"},
}

var menuRootOrder = []string{"java", "proxy", "bedrock", "games", "web", "apps"}
var menuGameOrder = []string{"source", "survival", "sandbox"}

type menuNode struct {
	ID        string
	Providers []ProviderSpec
	Children  map[string]*menuNode
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
	root := buildMenuTree(filtered)
	reader := bufio.NewReader(a.In)
	a.renderMenuHeader()
	return a.selectMenuNode(reader, root, true)
}

func buildMenuTree(providers []ProviderSpec) *menuNode {
	root := &menuNode{Children: map[string]*menuNode{}}
	for _, provider := range providers {
		node := root
		for _, id := range provider.MenuPath {
			if node.Children == nil {
				node.Children = map[string]*menuNode{}
			}
			if node.Children[id] == nil {
				node.Children[id] = &menuNode{ID: id, Children: map[string]*menuNode{}}
			}
			node = node.Children[id]
		}
		node.Providers = append(node.Providers, provider)
	}
	return root
}

func (a *App) selectMenuNode(reader *bufio.Reader, node *menuNode, root bool) (string, error) {
	if len(node.Children) == 0 {
		return a.selectProvider(reader, node)
	}
	for {
		children := orderedMenuChildren(node)
		a.renderCategoryMenu(node, children, root)
		choice, err := readMenuChoice(reader)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(choice, "q") {
			return "", fmt.Errorf("software selection cancelled")
		}
		if !root && strings.EqualFold(choice, "b") {
			return "", errMenuBack
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr != nil || index < 1 || index > len(children) {
			a.menuWarning("Choose a category number from 1 to %d, %s.", len(children), menuChoiceHelp(root))
			continue
		}
		selected, selectErr := a.selectMenuNode(reader, children[index-1], false)
		if errors.Is(selectErr, errMenuBack) {
			continue
		}
		return selected, selectErr
	}
}

var errMenuBack = errors.New("menu back")

func (a *App) selectProvider(reader *bufio.Reader, node *menuNode) (string, error) {
	for {
		a.renderProviderMenu(menuCategories[node.ID], node.Providers)
		choice, err := readMenuChoice(reader)
		if err != nil {
			return "", err
		}
		if strings.EqualFold(choice, "b") {
			return "", errMenuBack
		}
		if strings.EqualFold(choice, "q") {
			return "", fmt.Errorf("software selection cancelled")
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr != nil || index < 1 || index > len(node.Providers) {
			a.menuWarning("Choose a software number from 1 to %d, b to go back, or q to quit.", len(node.Providers))
			continue
		}
		selected := node.Providers[index-1]
		fmt.Fprintf(a.Out, "\n%sSelected:%s %s %s[%s]%s\n\n", menuGreen(), menuReset(), selected.Name, menuDim(), selected.ID, menuReset())
		return selected.ID, nil
	}
}

func orderedMenuChildren(node *menuNode) []*menuNode {
	order := menuRootOrder
	if node.ID == "games" {
		order = menuGameOrder
	}
	out := make([]*menuNode, 0, len(node.Children))
	for _, id := range order {
		if child := node.Children[id]; child != nil {
			out = append(out, child)
		}
	}
	return out
}

func menuLeafCount(node *menuNode) int {
	total := len(node.Providers)
	for _, child := range node.Children {
		total += menuLeafCount(child)
	}
	return total
}

func menuChoiceHelp(root bool) string {
	if root {
		return "or q to quit"
	}
	return "b to go back, or q to quit"
}

func (a *App) renderMenuHeader() {
	fmt.Fprintf(a.Out, "\n%s%s%s\n", menuCyan(), pcvmFiglet, menuReset())
	fmt.Fprintf(a.Out, "%s%s%s  %s%s%s  %s(%s)%s\n", menuBold(), a.Config.Policy.BrandName, menuReset(), menuDim(), "Pterodactyl multi-provider launcher", menuReset(), menuDim(), a.Config.Arch, menuReset())
}

func (a *App) renderCategoryMenu(parent *menuNode, children []*menuNode, root bool) {
	title := "SELECT A SOFTWARE CATEGORY"
	if parent.ID != "" {
		title = strings.ToUpper(menuCategories[parent.ID].Name)
	}
	fmt.Fprintf(a.Out, "\n%s╭─ %-56s╮%s\n", menuBlue(), title+" ", menuReset())
	for index, child := range children {
		category := menuCategories[child.ID]
		fmt.Fprintf(a.Out, "%s│%s  %s[%d]%s %-22s %2d software                  %s│%s\n", menuBlue(), menuReset(), menuYellow(), index+1, menuReset(), category.Name, menuLeafCount(child), menuBlue(), menuReset())
		fmt.Fprintf(a.Out, "%s│%s      %s%-52s%s%s│%s\n", menuBlue(), menuReset(), menuDim(), category.Description, menuReset(), menuBlue(), menuReset())
	}
	if root {
		fmt.Fprintf(a.Out, "%s│%s  %s[q]%s Quit                                                %s│%s\n", menuBlue(), menuReset(), menuYellow(), menuReset(), menuBlue(), menuReset())
	} else {
		fmt.Fprintf(a.Out, "%s│%s  %s[b]%s Back        %s[q]%s Quit                                %s│%s\n", menuBlue(), menuReset(), menuYellow(), menuReset(), menuYellow(), menuReset(), menuBlue(), menuReset())
	}
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
