# PCVM

PCVM v2.0.0 is a secure `PTDL_v2` provider platform that installs and runs one of 53 providers without modifying Panel or Wings. A Universal Egg and four category Eggs share the same launcher contract while using capability-specific Minecraft, Games, Apps and VM images. The Go launcher owns provider selection, checksum-verified runtimes and cloud images, transactional lifecycle state, safe switching, cgroup-aware memory planning, port validation and process supervision. The project is MIT licensed and has no telemetry.

## Provider catalog

| Menu | Providers |
|---|---|
| Minecraft Java | Vanilla, Paper, Purpur, Pufferfish, Folia, Canvas, Fabric, Quilt, Forge, NeoForge, Paper + Geyser/Floodgate, Modrinth Modpack |
| Minecraft Proxies | Velocity, BungeeCord |
| Minecraft Bedrock | Bedrock Dedicated Server, PocketMine-MP, PowerNukkitX, Cloudburst Nukkit, Endstone |
| Games — GTA Multiplayer | SA-MP / open.mp, Multi Theft Auto |
| Games — Source & FPS | Counter-Strike 2, Garry's Mod, Left 4 Dead 2 |
| Games — Survival | Palworld, Rust, Rust + uMod, Project Zomboid, Valheim, Valheim + BepInEx, 7 Days to Die, Unturned |
| Games — Sandbox & Automation | Terraria, tModLoader, TShock, Satisfactory, Factorio |
| Web Servers | Nginx, Apache HTTP Server, Caddy |
| Applications & Bots | Node.js, Python, Bun, Deno, Go and .NET apps, Lavalink, VS Code Server (code-server) |
| Virtual Machines — Lightweight Linux | Alpine Linux 3.23/3.24 |
| Virtual Machines — Debian Family | Ubuntu Minimal 24.04/26.04 LTS, Debian 12/13 |
| Virtual Machines — Enterprise Linux | AlmaLinux 9/10, Rocky Linux 9/10 |

All game providers are AMD64-only. `samp` installs the actively maintained open.mp server, which is backward-compatible with SA-MP scripts and clients, rather than an unmaintained third-party SA-MP binary. Web, Minecraft, code-server, bot and VM providers remain available on ARM64 when their embedded artifact metadata supports it. VM guests always match the host architecture. Endstone is AMD64-only because its official wheel is x86-64; PowerNukkitX and Cloudburst Nukkit support both PCVM architectures. LeviLamina is not listed because its upstream currently publishes Windows-only builds, while PCVM does not include Wine. Waterfall is excluded because upstream ended maintenance. One server runs one provider at a time.

## Install on Pterodactyl

1. Download `egg-pcvm-2.0.0.json` (Universal) or one category Egg from [GitHub Releases](https://github.com/canhphung/PCVM/releases).
2. Import it into a Nest on Pterodactyl 1.12.x.
3. Keep the release-pinned image generated for that Egg. Universal uses Full; category Eggs use their smaller matching image.
4. Set `SOFTWARE`, required allocations and provider variables, then start the server.

### Docker image profiles

| Profile | Release tag | Provider capability | Catalog count |
|---|---|---|---:|
| Universal / Full | `ghcr.io/canhphung/pcvm:2.0.0` | Every provider | 53 |
| Minecraft | `ghcr.io/canhphung/pcvm:2.0.0-minecraft` | Java, proxies and Bedrock | 19 |
| Games | `ghcr.io/canhphung/pcvm:2.0.0-games` | Native game servers | 18 |
| Apps & Web | `ghcr.io/canhphung/pcvm:2.0.0-apps` | Web servers and applications | 11 |
| Virtual Machines | `ghcr.io/canhphung/pcvm:2.0.0-vm` | QEMU virtual machines | 5 |

The Universal Egg remains the broad default. Category images expose only their category; Games is AMD64-only, while Universal, Minecraft, Apps and VM are multi-architecture and still filter providers whose upstream artifact does not support the host. The image profile is compiled into the launcher and cannot be changed with a Startup Variable. The generated provider/image matrix is available in [`docs/provider-matrix.md`](docs/provider-matrix.md).

The Egg installation script only initializes `/mnt/server/.pcvm`. The immutable startup command is:

```text
/usr/local/bin/pcvm run
```

On first start, `SOFTWARE=interactive` opens a FIGlet menu with up to three levels. Host allowlists, image capability, architecture and runtime availability filter the choices. PCVM emits `[PCVM] READY` only after provider-specific regex, delay or TCP readiness succeeds.

`pcvm run` clears the visible terminal screen and scrollback before its first banner. Set the admin-only `CLEAR_CONSOLE=0` while debugging. This does not delete Wings logs, audit history or server files; `pcvm version` and other non-run commands never clear the terminal.

## Ports

PCVM reads Pterodactyl's primary `SERVER_PORT`, binds public services to `0.0.0.0` and validates every required secondary port. It cannot allocate ports in Panel; an administrator must add the allocation and expose its value through the matching Egg variable.

| Provider | Required secondary variable |
|---|---|
| Project Zomboid | `STEAM_PORT` |
| Satisfactory | `RELIABLE_PORT` |
| Valheim / Valheim + BepInEx | `QUERY_PORT=SERVER_PORT+1` |
| 7 Days to Die | `GAME_PORT_2=SERVER_PORT+1`, `GAME_PORT_3=SERVER_PORT+2` |
| Unturned | `QUERY_PORT=SERVER_PORT+1` |
| Multi Theft Auto | `QUERY_PORT=SERVER_PORT+123` |

Palworld and Rust RCON plus 7 Days to Die Telnet bind to loopback and use `RCON_PORT` or `TELNET_PORT`; they are launcher control channels, not public allocations. Missing, out-of-range, duplicate or invalid-offset ports stop startup with an exact error.

## Memory planning

PCVM calculates one memory budget on every boot from the container's cgroup v2 limit, cgroup v1 limit or `SERVER_MEMORY`, in that order. The cgroup always wins when both it and the Panel variable are available. The resulting `[PCVM] MEMORY ...` line records the source, allocation, runtime target, launcher reserve, strategy and recommended allocation without storing any of those values in server state.

JVM providers receive an 80% heap target rounded down to 64 MiB. Node.js, PHP and .NET providers receive 75% rounded down to 64 MiB through their native runtime controls. QEMU guests retain the 75%/128 MiB profile and at least 384 MiB for QEMU plus the launcher; `VM_MEMORY_MB` remains the only user override and is still bounded by the container and admin cap. Native games, Python, web servers, Bedrock and other `cgroup-only` providers are not given an artificial userspace limit because Wings remains the hard enforcement layer.

An allocation below a provider's recommendation emits a warning and still starts. Only the catalog's technical hard minimum prevents startup. If no finite allocation can be discovered, Java falls back to a 1024 MiB heap, VMs to a 1024 MiB guest and other runtimes keep their upstream defaults. PCVM samples cgroup OOM counters around the supervised process and reports a cgroup OOM kill explicitly; it does not resize memory, restart processes or run a background memory daemon.

## Game installation and control

Steam providers use the checksum-pinned AMD64 SteamCMD bootstrap, anonymous login and direct argv. PCVM stores the Steam build ID after `app_update ... validate`. Steam updates are in-place; if update or validation fails, state is not advanced and PCVM refuses to start the possibly partial installation.

Rust uMod and Valheim BepInEx use official GitHub release assets with SHA-256 digests and are reapplied after game updates. Switching Rust ↔ Rust uMod or Valheim ↔ Valheim BepInEx preserves saves, configuration and plugin directories. Terraria ↔ tModLoader is intentionally incompatible.

SA-MP/open.mp is installed from the official open.mp GitHub release and keeps gamemodes, filterscripts, models, scripts and configuration across in-family updates. Multi Theft Auto uses its official stable server, base configuration and resources archives; all three are pinned by SHA-256 in the embedded catalog. MTA uses the primary allocation for both UDP gameplay and TCP HTTP downloads, while its public-list query allocation must be `SERVER_PORT+123`.

Console input is forwarded through stdin, Source RCON or Telnet according to provider metadata. Providers such as Valheim and Satisfactory only support signal-based stopping; console commands return a visible warning. Shutdown tries the provider's graceful path before SIGTERM and SIGKILL.

`GAME_EXTRA_ARGS` is tokenized directly into argv and never passed through a shell. Every provider has a deny-by-default flag allowlist; providers without an explicit allowlist reject extra arguments. Managed ports, bind addresses, install paths, response/config files and control credentials cannot be overridden. If `ADMIN_PASSWORD` is empty, PCVM creates a persistent random secret in `.pcvm/secrets` with mode `0600` and does not print it.

## Web servers

Nginx, Apache and Caddy run non-root on `0.0.0.0:${SERVER_PORT}`. `WEB_MODE=static` serves `WEB_ROOT` (default `public`). `WEB_MODE=proxy` requires an HTTP(S) `UPSTREAM_URL` without credentials. PCVM resolves every upstream address and rejects loopback, private, link-local, shared, reserved and metadata destinations; configuration metacharacters are rejected before the canonical URL is quoted into generated configs. PCVM canonicalizes the web root inside `/home/container`; symlink traversal and path escape are rejected. Caddy automatic HTTPS is disabled because public TLS remains the responsibility of the proxy in front of Pterodactyl.

PCVM owns the complete declarative web configuration; raw Nginx, Apache and Caddy extension snippets are not executed. Proxy mode connects through PCVM's loopback SafeProxy, which resolves and validates the destination for every upstream connection to prevent DNS rebinding. Nginx, Apache and Caddy share compatibility family `web-static`, so switching between them preserves `public/`.

## VS Code Server

The `code-server` provider runs the official Coder standalone archive on `0.0.0.0:${SERVER_PORT}` with password authentication, telemetry and self-update disabled, and TLS delegated to Pterodactyl's external reverse proxy. Set `CODE_SERVER_PASSWORD` to at least 12 characters, or leave it blank and read the generated persistent password from `code-server-password.txt`. Extensions are kept in `code-server-extensions/`; editor data remains under the Egg-denied `.pcvm/code-server` directory. code-server intentionally gives the authenticated server owner an integrated terminal running as the container user; hosts that do not permit shell access should remove `code-server` from `ALLOWED_SOFTWARE`.

## State and safe switching

`.pcvm/state.json` schema 4 records only canonical installation identity; it never stores launch commands, environment, arbitrary URLs, readiness rules or secrets. Process argv is rebuilt from the compiled provider registry, embedded catalog and validated Startup Variables on every boot. State and install receipts are verified before launch, while every generated Egg denies SFTP/API modification of the internal `.pcvm` control directory.

PCVM 2.0 is a deliberate clean break. Schema 1–3 and `.multiegg` installations stop with `PCVM-E2001 LEGACY_STATE` without downloading or changing data. Back up the server and create a fresh 2.0 server, or explicitly request the nonce-confirmed legacy reset flow. PCVM never silently converts a 1.x installation.

Paper, Purpur, Pufferfish and Paper + Geyser/Floodgate share one compatibility family. Folia and Canvas share a separate regionized family; Quilt is separate, and each Modrinth project is bound to its immutable project ID. Rust and Valheim variants share their respective families. A downgrade or incompatible family switch creates `.pcvm/pending-switch.json` and prints a 30-minute confirmation:

```text
RESET_CONFIRM=DELETE:0123456789abcdef...
```

After the exact confirmation, the launcher stages and verifies the target, moves existing data into a same-filesystem quarantine, activates the new installation and only then purges the quarantine. Any failed installation restores the old data. Back up data first. Hosts can disable user resets with `ALLOW_USER_RESET=0`.

## Linux virtual machines

Ubuntu Minimal, Debian, AlmaLinux, Rocky Linux and Alpine Linux run as real same-architecture QEMU system VMs using unprivileged multi-threaded TCG. No KVM, TAP, bridge, device passthrough or Wings changes are required. QEMU user-mode NAT provides outbound DHCP, DNS and HTTP, but PCVM 2.0 intentionally exposes no inbound guest port. The Pterodactyl console is the guest serial console; cloud-init creates the auto-login `pcvm` user with passwordless administration. Alpine uses OpenRC and `doas`, with a compatibility wrapper for common `sudo` commands.

The first boot downloads an immutable official cloud image, verifies SHA-256 or SHA-512, converts it to an independent sparse `vm/disk.qcow2`, resizes and checks it, and creates a NoCloud seed plus writable UEFI variables through a staged atomic install. The base image cache is then removed. `VM_DISK_COMPRESSION=zstd` optionally compresses initial QCOW2 clusters; it is off by default because decompression consumes TCG CPU and rewritten clusters become uncompressed. Alpine accepts a 2 GiB virtual disk, while the other VM providers require at least 8 GiB. `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB` and `VM_HOSTNAME` control initial resources; admin caps are `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS` and `VM_MAX_DISK_GB`.

The 2.0 catalog contains only the 20 active lightweight VM image combinations. Deprecated Ubuntu Standard identities and all 1.x VM metadata migrations are removed as part of the clean break; an existing VM disk is never rewritten implicitly.

VM image updates never modify an existing disk. Changing distro, version, pinned build or disk compression requires the reset nonce flow. `AUTO_UPDATE` and `UPDATE_REQUEST` are rejected for VM providers; update packages from inside the guest. Panel stop uses QMP ACPI powerdown and waits up to 90 seconds. Back up `vm/disk.qcow2` only while the VM is stopped; snapshots, shared backing images and online compaction are not supported in 2.0.

## Runtimes and downloads

The common Debian slim layer contains only the launcher prerequisites, CA certificates, OpenSSL and archive tools. Minecraft adds no unrelated service packages. Apps adds Git, the native build toolchain, Nginx and Apache. Games is AMD64-only and adds native/i386 Steam and Source libraries. VM adds QEMU, `qemu-img`, ISO tooling and architecture-specific UEFI firmware. Full is their multi-architecture union. Game binaries, cloud images, Wine and Proton are never built into an image.

`runtime-manifest.json` schema 1 contains 38 architecture-specific packs. Every pack records the exact upstream release identifier separately from its selectable runtime line, and pins both the downloaded archive SHA-256 and deterministic installed-tree hash. TShock 6.1 is bound to the pinned .NET 9 runtime rather than relying on a host-global installation:

- Java 8, 11, 17, 21 and 25 on AMD64/ARM64.
- Node.js 22 and 24 on AMD64/ARM64.
- Python 3.11–3.14 on AMD64/ARM64.
- PocketMine PHP on AMD64.
- Caddy 2 on AMD64/ARM64.
- Bun 1, Deno 2 and Go 1.26 on AMD64/ARM64.
- .NET 8 and 10 on AMD64/ARM64.
- SteamCMD on AMD64.

For versioned upstreams, `upstream_version` is the exact release/tag selected by the generator; `version` remains the user-selectable compatibility line. Valve publishes SteamCMD only through an unversioned bootstrap URL, so that pack records `sha256:<digest>` as its exact upstream identifier. Rolling release names such as PocketMine's PHP tag remain content-locked by the adjacent archive SHA-256.

Downloads require HTTPS, an allowlisted hostname, timeout/retry, a temporary file, checksum verification and atomic rename. Before every launch, PCVM derives a complete file/symlink manifest from the checksum-pinned runtime archive, verifies the extracted tree and repairs missing, changed, unexpected or escaping-symlink entries. ZIP and runtime extraction reject unsafe archive links, traversal, existing symlink targets and symlinked parent directories. The launcher never uses `curl | bash`, `eval`, arbitrary install commands or shell-composed startup commands.

After a successful install, PCVM removes consumed software artifacts and prepared Git source caches. Runtime archives are deleted after the extracted tree is verified; the content-addressed cache keeps only the active runtime tree and, while the global `CACHE_LIMIT_MB` budget permits, one previous verified tree. VM base images are removed after the standalone QCOW2 disk is committed. npm and pip dependency installation uses disposable/no-download caches so package-manager archives do not accumulate in the server directory.

## Variables

The release Egg defines the full public interface. Main groups are:

- Common: `SOFTWARE`, `SOFTWARE_VERSION`, `SOFTWARE_BUILD`, `RUNTIME_VERSION`, `AUTO_UPDATE`, `UPDATE_REQUEST` and `RESET_CONFIRM`. Minecraft providers use Pterodactyl's native EULA popup: PCVM installs the selected provider, emits the standard EULA trigger before starting it, and continues automatically after **I Accept** writes `eula=true` and restarts the server. The legacy `ACCEPT_MINECRAFT_EULA` Egg variable is removed.
- Modrinth: `MODPACK_MODE`, `MODPACK_PROJECT` and `MODPACK_FILE` select a public project or uploaded `.mrpack`.
- Games: `SERVER_NAME`, `SERVER_PASSWORD`, `ADMIN_PASSWORD`, `MAX_PLAYERS`, `GAME_MAP`, `GAME_WORLD`, `GAME_SEED`, `GAME_EXTRA_ARGS`, `STEAM_GSLT` and the port variables above.
- Web: `WEB_MODE`, `WEB_ROOT`, `UPSTREAM_URL`.
- Applications: Node.js, Python, Bun, Deno, Go and .NET use `SOURCE_MODE`, `GIT_URL`, `GIT_BRANCH`, `ENTRY_FILE`, `APP_ARGS`, `APP_READY_PATTERN`; code-server uses `CODE_SERVER_PASSWORD`.
- VMs: `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB`, `VM_DISK_COMPRESSION`, `VM_HOSTNAME`.
- Admin policy: `ALLOWED_SOFTWARE`, `ALLOW_USER_RESET`, `BRAND_NAME`, `SUPPORT_URL`, `RUNTIME_MIRROR_URL`, `GIT_ALLOWED_HOSTS`, `CACHE_LIMIT_MB`, `CLEAR_CONSOLE`, `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS`, `VM_MAX_DISK_GB`.

Pterodactyl view/edit flags are UI policy, not a secret store. Bot Git mode permits public credential-free HTTPS repositories only; upload mode runs files already present in the server directory.

## Development and release

```bash
go test -race ./...
go vet ./...
go run ./cmd/runtime-manifest -out runtime-manifest.json
go run ./cmd/egggen
go run ./cmd/egggen -check
docker build -t pcvm:dev .
docker build --build-arg PROFILE=minecraft -t pcvm:minecraft-dev .
docker build --build-arg PROFILE=games -t pcvm:games-dev .
docker build --build-arg PROFILE=apps -t pcvm:apps-dev .
docker build --build-arg PROFILE=vm -t pcvm:vm-dev .
```

Pull requests use local HTTP fixtures, a SteamCMD shim, fake QEMU/QMP and fake control servers. CI regenerates all Eggs from the catalog and variable registry, cross-compiles AMD64/ARM64, verifies package boundaries, runs race/vulnerability/coverage gates and builds all five profiles with SBOM and provenance. Games builds only for AMD64. Nightly checks live resolvers, runtime metadata, Steam App IDs and pinned cloud-image URLs without downloading full games. Manual application, game and VM smoke workflows build the matching lightweight profile and run one selected real provider per invocation.

Before release is dispatched from the exact `main` HEAD, that commit must have successful Minecraft, application, game and VM full-smoke runs from the preceding seven days. The release workflow then builds candidate images once and boots all ten 2.0 flagship providers plus Alpine VM from those exact AMD64/ARM64 digests. It also runs race/fuzz/coverage gates, scans every supported descriptor, enforces the image-size budget, signs the tested digests and prepares deterministic signed assets. Stable image tags are promoted idempotently, and the GitHub `v<version>` tag is created only when the verified draft is published at the final step. Immutable SemVer tags are never overwritten. Releases contain Universal, Minecraft, Games, Apps and VM Eggs plus the `egg-pcvm-<version>.json` Universal alias, runtime lock, checksums and Sigstore bundles. Full additionally receives the unsuffixed version and `latest` tags.
