package pcvm

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var validModrinthProject = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)

type modrinthVersion struct {
	ID            string         `json:"id"`
	ProjectID     string         `json:"project_id"`
	VersionNumber string         `json:"version_number"`
	DatePublished string         `json:"date_published"`
	GameVersions  []string       `json:"game_versions"`
	Loaders       []string       `json:"loaders"`
	Files         []modrinthFile `json:"files"`
}

type modrinthFile struct {
	Hashes   map[string]string `json:"hashes"`
	URL      string            `json:"url"`
	Filename string            `json:"filename"`
	Primary  bool              `json:"primary"`
	Size     int64             `json:"size"`
}

func resolveModrinth(ctx context.Context, req Request, h *HTTPClient) (Artifact, error) {
	mode := strings.ToLower(strings.TrimSpace(req.ModpackMode))
	if mode == "" {
		mode = "project"
	}
	if mode == "upload" {
		file, err := cleanRelativeEntry(req.ModpackFile)
		if err != nil || !strings.EqualFold(filepath.Ext(file), ".mrpack") {
			return Artifact{}, fmt.Errorf("MODPACK_FILE must be a relative .mrpack file")
		}
		return Artifact{FileName: file, Kind: "mrpack-upload", Version: envLatest(req.Version), Build: envLatest(req.Build), Metadata: map[string]string{"modrinth_mode": "upload"}}, nil
	}
	if mode != "project" {
		return Artifact{}, fmt.Errorf("MODPACK_MODE must be project or upload")
	}
	project := strings.TrimSpace(req.ModpackProject)
	if !validModrinthProject.MatchString(project) {
		return Artifact{}, fmt.Errorf("MODPACK_PROJECT must be a public Modrinth project slug or ID")
	}
	var versions []modrinthVersion
	if err := h.JSON(ctx, "https://api.modrinth.com/v2/project/"+url.PathEscape(project)+"/version", &versions); err != nil {
		return Artifact{}, fmt.Errorf("Modrinth versions: %w", err)
	}
	wantVersion, wantBuild := strings.TrimSpace(req.Version), strings.TrimSpace(req.Build)
	for _, version := range versions {
		if wantVersion != "" && wantVersion != "latest" && version.VersionNumber != wantVersion && version.ID != wantVersion {
			continue
		}
		if wantBuild != "" && wantBuild != "latest" && version.ID != wantBuild {
			continue
		}
		published, err := time.Parse(time.RFC3339Nano, version.DatePublished)
		if err != nil {
			continue
		}
		for _, file := range orderedModrinthPackFiles(version.Files) {
			if !strings.EqualFold(filepath.Ext(file.Filename), ".mrpack") {
				continue
			}
			sha512Digest, sha1Digest := strings.ToLower(file.Hashes["sha512"]), strings.ToLower(file.Hashes["sha1"])
			if !validHexDigest(sha512Digest, 128) || sha1Digest != "" && !validHexDigest(sha1Digest, 40) {
				continue
			}
			parsed, err := url.Parse(file.URL)
			if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
				continue
			}
			metadata := map[string]string{
				"modrinth_mode":           "project",
				"modrinth_project_id":     version.ProjectID,
				"modrinth_version_id":     version.ID,
				"modrinth_order_revision": published.UTC().Format(time.RFC3339Nano),
			}
			if file.Size > 0 {
				metadata["size_bytes"] = fmt.Sprintf("%d", file.Size)
			}
			return Artifact{URL: file.URL, FileName: file.Filename, Kind: "mrpack", SHA512: sha512Digest, SHA1: sha1Digest,
				Version: version.VersionNumber, Build: version.ID, Metadata: metadata}, nil
		}
	}
	return Artifact{}, fmt.Errorf("Modrinth project %q has no matching server-installable .mrpack version", project)
}

// Modrinth may attach several files to one version. The primary .mrpack is the
// canonical server pack; API array order is not an identity contract.
func orderedModrinthPackFiles(files []modrinthFile) []modrinthFile {
	ordered := make([]modrinthFile, 0, len(files))
	for _, primary := range []bool{true, false} {
		for _, file := range files {
			if file.Primary == primary && strings.EqualFold(filepath.Ext(file.Filename), ".mrpack") {
				ordered = append(ordered, file)
			}
		}
	}
	return ordered
}

type mrpackIndex struct {
	FormatVersion int               `json:"formatVersion"`
	Game          string            `json:"game"`
	VersionID     string            `json:"versionId"`
	Name          string            `json:"name"`
	Dependencies  map[string]string `json:"dependencies"`
	Files         []struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		Env       map[string]string `json:"env"`
		Downloads []string          `json:"downloads"`
		FileSize  int64             `json:"fileSize"`
	} `json:"files"`
}

type modrinthInstallReceipt struct {
	Schema        int               `json:"schema"`
	ProjectID     string            `json:"project_id"`
	VersionID     string            `json:"version_id"`
	Minecraft     string            `json:"minecraft"`
	Loader        string            `json:"loader"`
	LoaderVersion string            `json:"loader_version,omitempty"`
	Managed       map[string]string `json:"managed_sha512"`
}

func (p *catalogProvider) installModrinth(ctx context.Context, ic InstallContext, resolved Resolved) (Resolved, error) {
	packPath := ic.Artifact
	if resolved.Artifact.Kind == "mrpack-upload" {
		clean, err := cleanRelativeEntry(resolved.Artifact.FileName)
		if err != nil {
			return resolved, err
		}
		packPath = filepath.Join(ic.Home, clean)
	}
	info, err := os.Stat(packPath)
	if err != nil || !info.Mode().IsRegular() {
		return resolved, fmt.Errorf("Modrinth pack: %w", err)
	}
	index, reader, err := readMRPack(packPath)
	if err != nil {
		return resolved, err
	}
	defer reader.Close()
	loader, loaderVersion, err := modrinthLoader(index.Dependencies)
	if err != nil {
		return resolved, err
	}
	minecraft := index.Dependencies["minecraft"]
	if resolved.Artifact.Metadata == nil {
		resolved.Artifact.Metadata = map[string]string{}
	}
	resolved.Artifact.Metadata["minecraft_version"] = minecraft
	resolved.Artifact.Metadata["modrinth_loader"] = loader
	resolved.Artifact.Metadata["modrinth_loader_version"] = loaderVersion
	managedRoot := filepath.Join(ic.ControlDir, "managed", p.spec.ID)
	if err := secureMkdirAll(ic.ControlDir, managedRoot, 0o750); err != nil {
		return resolved, err
	}
	receiptPath := filepath.Join(managedRoot, "install.json")
	previous := modrinthInstallReceipt{}
	if err := readJSON(receiptPath, &previous); err != nil && !os.IsNotExist(err) {
		return resolved, fmt.Errorf("read Modrinth install receipt: %w", err)
	}
	if previous.Schema != 0 && previous.Schema != 2 {
		return resolved, fmt.Errorf("unsupported Modrinth install receipt schema %d", previous.Schema)
	}
	if previous.Schema != 0 {
		state, err := LoadState(ic.ControlDir)
		if err != nil {
			return resolved, fmt.Errorf("load canonical Modrinth state: %w", err)
		}
		if state == nil || state.Provider != p.spec.ID {
			return resolved, fmt.Errorf("canonical Modrinth state is required to update an existing install")
		}
		sealed, err := LoadInstallReceipt(ic.ControlDir, state.Receipt)
		if err != nil {
			return resolved, fmt.Errorf("load current Modrinth receipt: %w", err)
		}
		if err := verifyInstallReceipt(ic.Home, *state, sealed); err != nil {
			return resolved, fmt.Errorf("verify current Modrinth install before update: %w", err)
		}
	}
	if previous.Schema != 0 && previous.ProjectID != "" && resolved.Artifact.Metadata["modrinth_project_id"] != "" && previous.ProjectID != resolved.Artifact.Metadata["modrinth_project_id"] {
		return resolved, fmt.Errorf("changing the Modrinth project requires reset")
	}
	if err := verifyManagedModrinthFiles(ic.Home, previous.Managed); err != nil {
		return resolved, err
	}

	transactionRoot := installOverlayRoot(ic.ControlDir, p.spec.ID)
	if _, err := os.Lstat(transactionRoot); err == nil {
		return resolved, fmt.Errorf("a pending Modrinth transaction must be recovered before installation")
	} else if !os.IsNotExist(err) {
		return resolved, err
	}
	if err := secureMkdirAll(ic.ControlDir, transactionRoot, 0o750); err != nil {
		return resolved, err
	}
	candidateHome := installOverlayNewRoot(ic.ControlDir, p.spec.ID)
	if err := os.Mkdir(candidateHome, 0o750); err != nil {
		_ = os.RemoveAll(transactionRoot)
		return resolved, err
	}
	transactionApplied := false
	defer func() {
		if !transactionApplied {
			_ = os.RemoveAll(transactionRoot)
		}
	}()
	if err := stageMRPackFiles(ctx, ic.HTTP, reader, index, candidateHome); err != nil {
		return resolved, err
	}
	candidateManagedRoot := filepath.Join(candidateHome, ".pcvm", "managed", p.spec.ID)
	stagedContext := ic
	stagedContext.Home = candidateHome
	if err := installModrinthBootstrap(ctx, stagedContext, minecraft, loader, loaderVersion, candidateManagedRoot); err != nil {
		return resolved, err
	}
	if err := preserveModrinthMutableFiles(ic.Home, candidateHome); err != nil {
		return resolved, err
	}
	managed, err := modrinthCandidateDigests(candidateHome, filepath.ToSlash(filepath.Join(".pcvm", "managed", p.spec.ID, "install.json")))
	if err != nil {
		return resolved, err
	}
	for _, relative := range modrinthMutableFiles {
		delete(managed, relative)
	}
	receipt := modrinthInstallReceipt{Schema: 2, ProjectID: resolved.Artifact.Metadata["modrinth_project_id"], VersionID: resolved.Artifact.Metadata["modrinth_version_id"], Minecraft: minecraft, Loader: loader, LoaderVersion: loaderVersion, Managed: managed}
	if receipt.ProjectID == "" {
		receipt.ProjectID, err = modrinthUploadProjectID(index)
		if err != nil {
			return resolved, err
		}
		receipt.VersionID = index.VersionID
	}
	candidateReceipt := filepath.Join(candidateManagedRoot, "install.json")
	if err := writeJSONAtomic(candidateReceipt, receipt); err != nil {
		return resolved, err
	}
	command, err := modrinthLaunchCommand(ic.Runtime, ic.Home, managedRoot, receipt)
	if err != nil {
		return resolved, err
	}
	resolved.WorkDir, resolved.Command = ic.Home, command
	resolved.RollbackMode = "staged"
	candidateState := newStateFromInstall(p.spec, ic.Request, resolved, ic.Request.Architecture, time.Now())
	previousManaged := make(map[string]string, len(previous.Managed)+1)
	for relative, checksum := range previous.Managed {
		previousManaged[relative] = checksum
	}
	for _, relative := range modrinthMutableFiles {
		if info, err := os.Lstat(filepath.Join(ic.Home, filepath.FromSlash(relative))); err == nil && info.Mode().IsRegular() {
			previousManaged[relative] = "mutable-config"
		} else if err != nil && !os.IsNotExist(err) {
			return resolved, err
		} else if err == nil {
			return resolved, fmt.Errorf("mutable Modrinth path %q must be a regular file", relative)
		}
	}
	if previous.Schema != 0 {
		previousManaged[filepath.ToSlash(filepath.Join(".pcvm", "managed", p.spec.ID, "install.json"))] = "receipt"
	}
	if err := applyInstallOverlay(ic.Home, ic.ControlDir, p.spec.ID, candidateState.InstallID, previousManaged); err != nil {
		return resolved, err
	}
	transactionApplied = true
	return resolved, nil
}

var modrinthMutableFiles = []string{
	"server.properties", "eula.txt", "ops.json", "whitelist.json", "banned-ips.json", "banned-players.json",
}

// preserveModrinthMutableFiles makes server configuration an input to the
// candidate transaction rather than a managed pack artifact. This lets PCVM
// reconcile server.properties on every boot without manufacturing an update
// conflict, while still rolling config writes back with the rest of the pack.
func preserveModrinthMutableFiles(home, candidate string) error {
	for _, relative := range modrinthMutableFiles {
		source := filepath.Join(home, filepath.FromSlash(relative))
		info, err := os.Lstat(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("mutable Modrinth path %q must be a regular non-symlink file", relative)
		}
		if err := copyRegularIntoTree(candidate, relative, source); err != nil {
			return err
		}
	}
	return nil
}

func readMRPack(path string) (mrpackIndex, *zip.ReadCloser, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return mrpackIndex{}, nil, fmt.Errorf("open .mrpack: %w", err)
	}
	for _, file := range reader.File {
		if file.Name != "modrinth.index.json" || !file.Mode().IsRegular() || file.UncompressedSize64 > 8<<20 {
			continue
		}
		if err := (&archiveBudget{limits: defaultArchiveLimits, total: int64(file.UncompressedSize64)}).checkCompression(file.Name, int64(file.CompressedSize64)); err != nil {
			reader.Close()
			return mrpackIndex{}, nil, err
		}
		in, err := file.Open()
		if err != nil {
			reader.Close()
			return mrpackIndex{}, nil, err
		}
		var index mrpackIndex
		err = json.NewDecoder(io.LimitReader(in, 8<<20)).Decode(&index)
		in.Close()
		if err != nil {
			reader.Close()
			return mrpackIndex{}, nil, fmt.Errorf("decode modrinth.index.json: %w", err)
		}
		if index.FormatVersion != 1 || index.Game != "minecraft" || strings.TrimSpace(index.Name) == "" || index.VersionID == "" || index.Dependencies["minecraft"] == "" {
			reader.Close()
			return mrpackIndex{}, nil, fmt.Errorf("unsupported or incomplete Modrinth pack index")
		}
		return index, reader, nil
	}
	reader.Close()
	return mrpackIndex{}, nil, fmt.Errorf(".mrpack contains no modrinth.index.json")
}

func modrinthUploadProjectID(index mrpackIndex) (string, error) {
	// The mrpack format has no project ID in upload mode. Its required, stable
	// pack name is therefore the only cross-version project identity available.
	// Hash the normalized value so it remains a safe state token.
	name := strings.ToLower(strings.Join(strings.Fields(index.Name), " "))
	if name == "" {
		return "", fmt.Errorf("uploaded Modrinth pack has no stable name")
	}
	digest := sha256.Sum256([]byte("modrinth-upload\x00" + name))
	return "upload-" + hex.EncodeToString(digest[:16]), nil
}

func modrinthLoader(dependencies map[string]string) (string, string, error) {
	loader, version := "vanilla", ""
	for _, candidate := range []struct{ key, name string }{{"fabric-loader", "fabric"}, {"quilt-loader", "quilt"}, {"forge", "forge"}, {"neoforge", "neoforge"}} {
		if value := strings.TrimSpace(dependencies[candidate.key]); value != "" {
			if loader != "vanilla" {
				return "", "", fmt.Errorf("Modrinth pack declares more than one mod loader")
			}
			loader, version = candidate.name, value
		}
	}
	if version != "" {
		if err := validateStatePathToken("Modrinth loader version", version); err != nil {
			return "", "", err
		}
	}
	return loader, version, nil
}

func stageMRPackFiles(ctx context.Context, h *HTTPClient, reader *zip.ReadCloser, index mrpackIndex, staging string) error {
	if len(index.Files) > 50_000 {
		return fmt.Errorf("Modrinth pack contains too many indexed files")
	}
	var declaredTotal int64
	var actualTotal int64
	for _, file := range index.Files {
		if strings.EqualFold(file.Env["server"], "unsupported") {
			continue
		}
		if err := rejectModrinthControlPath(file.Path); err != nil {
			return err
		}
		clean, target, err := archiveTarget(staging, file.Path)
		if err != nil {
			return err
		}
		if file.FileSize < 0 || file.FileSize > 2<<30 || declaredTotal > (4<<30)-file.FileSize {
			return fmt.Errorf("Modrinth pack exceeds extraction limits")
		}
		declaredTotal += file.FileSize
		sha512Digest, sha1Digest := strings.ToLower(file.Hashes["sha512"]), strings.ToLower(file.Hashes["sha1"])
		if !validHexDigest(sha512Digest, 128) && !validHexDigest(sha1Digest, 40) {
			return fmt.Errorf("Modrinth file %q has no supported integrity hash", clean)
		}
		if len(file.Downloads) == 0 {
			return fmt.Errorf("Modrinth file %q has no download URL", clean)
		}
		artifact := Artifact{URL: file.Downloads[0], FileName: filepath.Base(clean)}
		if validHexDigest(sha512Digest, 128) {
			artifact.SHA512 = sha512Digest
		} else {
			artifact.SHA1 = sha1Digest
		}
		if _, err := h.Download(ctx, artifact, target); err != nil {
			return fmt.Errorf("download Modrinth file %q: %w", clean, err)
		}
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("inspect downloaded Modrinth file %q: %w", clean, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("downloaded Modrinth file %q is not regular", clean)
		}
		if file.FileSize > 0 && info.Size() != file.FileSize {
			return fmt.Errorf("Modrinth file %q size mismatch: index declares %d bytes, downloaded %d", clean, file.FileSize, info.Size())
		}
		if info.Size() > 2<<30 || actualTotal > (4<<30)-info.Size() {
			return fmt.Errorf("downloaded Modrinth files exceed extraction limits")
		}
		actualTotal += info.Size()
	}
	var overrideBytes int64
	var overrideEntries int
	for _, prefix := range []string{"overrides/", "server-overrides/"} {
		limits := defaultArchiveLimits
		limits.MaxEntries = 0
		limits.MaxTotalBytes = 0
		budget := newArchiveBudget(limits)
		for _, file := range reader.File {
			name := strings.ReplaceAll(file.Name, "\\", "/")
			if !strings.HasPrefix(name, prefix) || name == prefix {
				continue
			}
			overrideEntries++
			if overrideEntries > defaultArchiveLimits.MaxEntries {
				return fmt.Errorf("Modrinth overrides contain more than %d entries", defaultArchiveLimits.MaxEntries)
			}
			rel := strings.TrimPrefix(name, prefix)
			if err := rejectModrinthControlPath(rel); err != nil {
				return err
			}
			clean, target, err := archiveTarget(staging, rel)
			if err != nil {
				return err
			}
			if file.FileInfo().IsDir() {
				if err := budget.add(clean, archiveEntryDirectory, 0); err != nil {
					return err
				}
				if err := secureMkdirAll(staging, target, 0o750); err != nil {
					return err
				}
				continue
			}
			if file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() || file.UncompressedSize64 > uint64(2<<30) {
				return fmt.Errorf("unsupported Modrinth override %q", file.Name)
			}
			size := int64(file.UncompressedSize64)
			if size > defaultArchiveLimits.MaxTotalBytes || overrideBytes > defaultArchiveLimits.MaxTotalBytes-size {
				return fmt.Errorf("Modrinth overrides expand beyond the %d byte total limit", defaultArchiveLimits.MaxTotalBytes)
			}
			overrideBytes += size
			if err := budget.add(clean, archiveEntryRegular, size); err != nil {
				return err
			}
			if err := (&archiveBudget{limits: defaultArchiveLimits, total: size}).checkCompression(file.Name, int64(file.CompressedSize64)); err != nil {
				return err
			}
			in, err := file.Open()
			if err != nil {
				return err
			}
			writeErr := writeArchiveRegular(staging, target, in, file.Mode(), size)
			closeErr := in.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return nil
}

func rejectModrinthControlPath(name string) error {
	normalized := strings.ReplaceAll(name, "\\", "/")
	for _, candidate := range []string{normalized, path.Clean(normalized)} {
		for _, component := range strings.Split(candidate, "/") {
			if component == "" || component == "." {
				continue
			}
			if strings.EqualFold(component, ".pcvm") {
				return fmt.Errorf("Modrinth payload path %q targets reserved .pcvm control data", name)
			}
			break
		}
	}
	return nil
}

func modrinthCandidateDigests(staging, excluded string) (map[string]string, error) {
	paths := []string{}
	if err := filepath.WalkDir(staging, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(staging, path)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	managed := make(map[string]string, len(paths))
	for _, rel := range paths {
		if filepath.ToSlash(rel) == excluded {
			continue
		}
		source := filepath.Join(staging, rel)
		info, err := os.Stat(source)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("inspect staged Modrinth file %q: %w", rel, err)
		}
		digest, err := regularFileSHA512(source)
		if err != nil {
			return nil, err
		}
		managed[filepath.ToSlash(rel)] = digest
	}
	return managed, nil
}

// applyMRPackStage remains a small fixture helper for the contract tests. The
// production installer never calls it: live updates go through the durable
// install overlay above so partial copies cannot escape rollback.
func applyMRPackStage(staging, home string) (map[string]string, error) {
	managed, err := modrinthCandidateDigests(staging, "")
	if err != nil {
		return nil, err
	}
	for relative := range managed {
		if err := copyRegularIntoTree(home, relative, filepath.Join(staging, filepath.FromSlash(relative))); err != nil {
			return nil, err
		}
	}
	return managed, nil
}

func verifyManagedModrinthFiles(home string, managed map[string]string) error {
	for rel, expected := range managed {
		if !validHexDigest(expected, 128) {
			return fmt.Errorf("Modrinth install receipt is invalid")
		}
		_, target, err := archiveTarget(home, rel)
		if err != nil {
			return err
		}
		digest, err := regularFileSHA512(target)
		if os.IsNotExist(err) {
			return fmt.Errorf("managed Modrinth file %q is missing; update aborted to preserve local changes", rel)
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(digest, expected) {
			return fmt.Errorf("managed Modrinth file %q was modified; update aborted to preserve local changes", rel)
		}
	}
	return nil
}

func regularFileSHA512(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha512.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func installModrinthBootstrap(ctx context.Context, ic InstallContext, minecraft, loader, loaderVersion, managedRoot string) error {
	request := Request{Version: minecraft, Build: "latest", Architecture: ic.Request.Architecture}
	switch loader {
	case "vanilla":
		artifact, err := resolveMojang(ctx, request, ic.HTTP)
		if err != nil {
			return err
		}
		return downloadBootstrap(ctx, ic, artifact, filepath.Join(managedRoot, "bootstrap", "server.jar"))
	case "fabric":
		var installers []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		if err := ic.HTTP.JSON(ctx, "https://meta.fabricmc.net/v2/versions/installer", &installers); err != nil {
			return err
		}
		installer := ""
		for _, candidate := range installers {
			if candidate.Stable {
				installer = candidate.Version
				break
			}
		}
		if installer == "" {
			return fmt.Errorf("Fabric returned no stable installer")
		}
		artifact := Artifact{URL: fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s/%s/server/jar", url.PathEscape(minecraft), url.PathEscape(loaderVersion), url.PathEscape(installer)), FileName: "server.jar", Kind: "jar"}
		return downloadBootstrap(ctx, ic, artifact, filepath.Join(managedRoot, "bootstrap", "server.jar"))
	case "quilt":
		artifact, err := resolveQuilt(ctx, Request{Version: minecraft, Build: loaderVersion}, ic.HTTP)
		if err != nil {
			return err
		}
		installer := filepath.Join(managedRoot, "bootstrap", artifact.FileName)
		if err := downloadBootstrap(ctx, ic, artifact, installer); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, ic.Runtime, "-jar", installer, "install", "server", minecraft, loaderVersion, "--download-server", "--install-dir="+ic.Home)
		cmd.Dir, cmd.Stdout, cmd.Stderr = ic.Home, ic.Out, ic.Err
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install Quilt modpack bootstrap: %w", err)
		}
		return nil
	case "forge":
		artifact, err := resolveMaven(ctx, Request{Version: minecraft + "-" + loaderVersion}, ic.HTTP, "https://maven.minecraftforge.net/net/minecraftforge/forge/maven-metadata.xml", "https://maven.minecraftforge.net/net/minecraftforge/forge/%s/forge-%s-installer.jar")
		if err != nil {
			return err
		}
		return runModrinthJavaInstaller(ctx, ic, artifact, managedRoot)
	case "neoforge":
		artifact, err := resolveMaven(ctx, Request{Version: loaderVersion}, ic.HTTP, "https://maven.neoforged.net/releases/net/neoforged/neoforge/maven-metadata.xml", "https://maven.neoforged.net/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar")
		if err != nil {
			return err
		}
		return runModrinthJavaInstaller(ctx, ic, artifact, managedRoot)
	default:
		return fmt.Errorf("unsupported Modrinth loader %q", loader)
	}
}

func downloadBootstrap(ctx context.Context, ic InstallContext, artifact Artifact, target string) error {
	root := ic.ControlDir
	if pathWithin(ic.Home, target) {
		root = ic.Home
	}
	if err := secureMkdirAll(root, filepath.Dir(target), 0o750); err != nil {
		return err
	}
	temporary := target + ".download"
	defer os.Remove(temporary)
	if _, err := ic.HTTP.Download(ctx, artifact, temporary); err != nil {
		return err
	}
	return copyFile(temporary, target, 0o640)
}

func runModrinthJavaInstaller(ctx context.Context, ic InstallContext, artifact Artifact, managedRoot string) error {
	installer := filepath.Join(managedRoot, "bootstrap", artifact.Version+"-installer.jar")
	if err := downloadBootstrap(ctx, ic, artifact, installer); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, ic.Runtime, "-jar", installer, "--installServer")
	cmd.Dir, cmd.Stdout, cmd.Stderr = ic.Home, ic.Out, ic.Err
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install Modrinth loader: %w", err)
	}
	return nil
}

func modrinthLaunchCommand(runtimePath, home, managedRoot string, receipt modrinthInstallReceipt) ([]string, error) {
	if receipt.Schema != 2 || receipt.Minecraft == "" {
		return nil, fmt.Errorf("Modrinth install receipt is invalid")
	}
	switch receipt.Loader {
	case "vanilla", "fabric":
		jar := filepath.Join(managedRoot, "bootstrap", "server.jar")
		return []string{runtimePath, "-jar", jar, "nogui"}, nil
	case "quilt":
		return []string{runtimePath, "-jar", filepath.Join(home, "quilt-server-launch.jar"), "nogui"}, nil
	case "forge":
		version := receipt.Minecraft + "-" + receipt.LoaderVersion
		if err := validateStatePathToken("Forge version", version); err != nil {
			return nil, err
		}
		return []string{runtimePath, "@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/" + version + "/unix_args.txt", "nogui"}, nil
	case "neoforge":
		if err := validateStatePathToken("NeoForge version", receipt.LoaderVersion); err != nil {
			return nil, err
		}
		return []string{runtimePath, "@user_jvm_args.txt", "@libraries/net/neoforged/neoforge/" + receipt.LoaderVersion + "/unix_args.txt", "nogui"}, nil
	default:
		return nil, fmt.Errorf("Modrinth install receipt has unsupported loader %q", receipt.Loader)
	}
}

func rebuildModrinthCommand(cfg Config, runtimePath string) ([]string, string, error) {
	managedRoot := filepath.Join(cfg.Control, "managed", "modrinth-modpack")
	var receipt modrinthInstallReceipt
	if err := readJSON(filepath.Join(managedRoot, "install.json"), &receipt); err != nil {
		return nil, "", fmt.Errorf("read Modrinth install receipt: %w", err)
	}
	command, err := modrinthLaunchCommand(runtimePath, cfg.Home, managedRoot, receipt)
	return command, cfg.Home, err
}
