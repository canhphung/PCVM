package pcvm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const pcvmFiglet = `    ____  _______    ____  ___
   / __ \/ ____/ |  / /  |/  /
  / /_/ / /    | | / / /|_/ /
 / ____/ /___  | |/ / /  / /
/_/    \____/  |___/_/  /_/`

const menuFrameInnerWidth = 58

const defaultMenuSelectionTimeout = 5 * time.Minute

var errMenuSelectionTimeout = errors.New("software selection timed out")

type menuCategory struct {
	ID          string
	Name        string
	Description string
}

var menuCategories = map[string]menuCategory{
	"java":              {ID: "java", Name: "Minecraft Java", Description: "Vanilla, optimized servers and mod loaders"},
	"proxy":             {ID: "proxy", Name: "Minecraft Proxies", Description: "Route players between backend servers"},
	"bedrock":           {ID: "bedrock", Name: "Minecraft Bedrock", Description: "Official BDS, Nukkit-family and plugin platforms"},
	"games":             {ID: "games", Name: "Game Servers", Description: "Native Linux dedicated game servers"},
	"source":            {ID: "source", Name: "Source & FPS", Description: "Counter-Strike, Garry's Mod and Left 4 Dead"},
	"gta":               {ID: "gta", Name: "GTA Multiplayer", Description: "SA-MP/open.mp and Multi Theft Auto"},
	"survival":          {ID: "survival", Name: "Survival", Description: "Persistent survival and co-op worlds"},
	"sandbox":           {ID: "sandbox", Name: "Sandbox & Automation", Description: "Building, automation and sandbox games"},
	"web":               {ID: "web", Name: "Web Servers", Description: "Static hosting and reverse proxies"},
	"apps":              {ID: "apps", Name: "Applications & Bots", Description: "Node.js, Python, Lavalink and browser IDEs"},
	"vms":               {ID: "vms", Name: "Virtual Machines", Description: "Real Linux VMs using unprivileged QEMU TCG"},
	"debian-family":     {ID: "debian-family", Name: "Debian Family", Description: "Ubuntu Server and Debian cloud images"},
	"enterprise-linux":  {ID: "enterprise-linux", Name: "Enterprise Linux", Description: "AlmaLinux and Rocky Linux cloud images"},
	"lightweight-linux": {ID: "lightweight-linux", Name: "Lightweight Linux", Description: "Small cloud images for lightweight labs"},
}

var menuRootOrder = []string{"java", "proxy", "bedrock", "games", "vms", "web", "apps"}
var menuGameOrder = []string{"source", "gta", "survival", "sandbox"}
var menuVMOrder = []string{"lightweight-linux", "debian-family", "enterprise-linux"}

type menuNode struct {
	ID        string
	Providers []ProviderSpec
	Children  map[string]*menuNode
}

func (a *App) menu() (string, error) {
	return a.menuContext(context.Background())
}

func (a *App) menuContext(ctx context.Context) (string, error) {
	available := a.Catalog.Available(a.Config.Arch, a.Config.Policy.AllowedSoftware)
	filtered := available[:0]
	for _, provider := range available {
		if ImageProfileSupports(a.Config.ImageProfile, provider) && (a.Config.Policy.AllowSystemPath || a.Catalog.HasRuntime(provider.Runtime, "", a.Config.Arch)) {
			filtered = append(filtered, provider)
		}
	}
	if len(filtered) == 0 {
		return "", fmt.Errorf("host policy and %s image profile expose no providers", a.Config.ImageProfile)
	}
	root := buildMenuTree(filtered)
	reader := bufio.NewReader(a.In)
	a.renderMenuHeader()
	fmt.Fprintf(a.Out, "%sNo selection: server shuts down after %s.%s\n", menuDim(), formatMenuTimeout(a.menuSelectionTimeout()), menuReset())
	menuCtx, cancel := context.WithTimeoutCause(ctx, a.menuSelectionTimeout(), errMenuSelectionTimeout)
	defer cancel()
	return a.selectMenuNode(menuCtx, reader, root, true)
}

func (a *App) menuSelectionTimeout() time.Duration {
	if a.MenuTimeout > 0 {
		return a.MenuTimeout
	}
	return defaultMenuSelectionTimeout
}

func formatMenuTimeout(timeout time.Duration) string {
	if timeout > 0 && timeout%time.Minute == 0 {
		minutes := int(timeout / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return timeout.String()
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

func (a *App) selectMenuNode(ctx context.Context, reader *bufio.Reader, node *menuNode, root bool) (string, error) {
	if len(node.Children) == 0 {
		return a.selectProvider(ctx, reader, node)
	}
	for {
		children := orderedMenuChildren(node)
		a.renderCategoryMenu(node, children, root)
		choice, err := readMenuChoice(ctx, reader)
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
		selected, selectErr := a.selectMenuNode(ctx, reader, children[index-1], false)
		if errors.Is(selectErr, errMenuBack) {
			continue
		}
		return selected, selectErr
	}
}

var errMenuBack = errors.New("menu back")

func (a *App) selectProvider(ctx context.Context, reader *bufio.Reader, node *menuNode) (string, error) {
	for {
		a.renderProviderMenu(menuCategories[node.ID], node.Providers)
		choice, err := readMenuChoice(ctx, reader)
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
	} else if node.ID == "vms" {
		order = menuVMOrder
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

func menuFrameTop(title string) string {
	titleRunes := []rune(strings.TrimSpace(title))
	// Keep at least one rule character between the title and the right corner.
	maxTitleWidth := menuFrameInnerWidth - len([]rune("─ ")) - len([]rune(" ")) - 1
	if len(titleRunes) > maxTitleWidth {
		titleRunes = titleRunes[:maxTitleWidth]
	}
	title = string(titleRunes)
	ruleWidth := menuFrameInnerWidth - len([]rune("─ ")) - len(titleRunes) - len([]rune(" "))
	return "╭─ " + title + " " + strings.Repeat("─", ruleWidth) + "╮"
}

func menuFrameBottom() string {
	return "╰" + strings.Repeat("─", menuFrameInnerWidth) + "╯"
}

func (a *App) renderCategoryMenu(parent *menuNode, children []*menuNode, root bool) {
	title := "SELECT A SOFTWARE CATEGORY"
	if parent.ID != "" {
		title = strings.ToUpper(menuCategories[parent.ID].Name)
	}
	fmt.Fprintf(a.Out, "\n%s%s%s\n", menuBlue(), menuFrameTop(title), menuReset())
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
	fmt.Fprintf(a.Out, "%s%s%s\n", menuBlue(), menuFrameBottom(), menuReset())
	fmt.Fprintf(a.Out, "%sSelect category%s › ", menuBold(), menuReset())
}

func (a *App) renderProviderMenu(category menuCategory, providers []ProviderSpec) {
	fmt.Fprintf(a.Out, "\n%s%s%s\n", menuBlue(), menuFrameTop(strings.ToUpper(category.Name)), menuReset())
	for index, provider := range providers {
		fmt.Fprintf(a.Out, "%s│%s  %s[%d]%s %-31s %s%-20s%s%s│%s\n", menuBlue(), menuReset(), menuYellow(), index+1, menuReset(), provider.Name, menuDim(), provider.ID, menuReset(), menuBlue(), menuReset())
	}
	fmt.Fprintf(a.Out, "%s│%s  %s[b]%s Back        %s[q]%s Quit                                %s│%s\n", menuBlue(), menuReset(), menuYellow(), menuReset(), menuYellow(), menuReset(), menuBlue(), menuReset())
	fmt.Fprintf(a.Out, "%s%s%s\n", menuBlue(), menuFrameBottom(), menuReset())
	fmt.Fprintf(a.Out, "%sSelect software%s › ", menuBold(), menuReset())
}

func (a *App) menuWarning(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.Out, "\n%s[PCVM] %s%s\n", menuYellow(), message, menuReset())
}

func readMenuChoice(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		choice string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		if err == nil || errors.Is(err, io.EOF) && choice != "" {
			resultCh <- result{choice: choice}
			return
		}
		if errors.Is(err, io.EOF) {
			resultCh <- result{err: fmt.Errorf("console input closed during software selection")}
			return
		}
		resultCh <- result{err: err}
	}()

	select {
	case <-ctx.Done():
		return "", context.Cause(ctx)
	case result := <-resultCh:
		return result.choice, result.err
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
