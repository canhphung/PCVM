# PCVM

PCVM v1.7.0 is one `PTDL_v2` Pterodactyl Egg that installs and runs one of 43 providers without modifying Panel or Wings. Four capability-aware Docker image profiles let hosts avoid shipping QEMU and native game libraries to servers that do not use them. The Go launcher owns provider selection, checksum-verified runtimes and cloud images, updates, state, safe switching, port validation and process supervision. The project is MIT licensed and has no telemetry.

## Provider catalog

| Menu | Providers |
|---|---|
| Minecraft Java | Vanilla, Paper, Purpur, Pufferfish, Fabric, Forge, NeoForge |
| Minecraft Proxies | Velocity, BungeeCord |
| Minecraft Bedrock | Bedrock Dedicated Server, PocketMine-MP, PowerNukkitX, Cloudburst Nukkit, Endstone |
| Games — GTA Multiplayer | SA-MP / open.mp, Multi Theft Auto |
| Games — Source & FPS | Counter-Strike 2, Garry's Mod, Left 4 Dead 2 |
| Games — Survival | Palworld, Rust, Rust + uMod, Project Zomboid, Valheim, Valheim + BepInEx, 7 Days to Die, Unturned |
| Games — Sandbox & Automation | Terraria, tModLoader, Satisfactory, Factorio |
| Web Servers | Nginx, Apache HTTP Server, Caddy |
| Applications & Bots | Node.js bot, Python bot, Lavalink, VS Code Server (code-server) |
| Virtual Machines â€” Lightweight Linux | Alpine Linux 3.23/3.24 |
| Virtual Machines â€” Debian Family | Ubuntu Minimal 24.04/26.04 LTS, Debian 12/13 |
| Virtual Machines â€” Enterprise Linux | AlmaLinux 9/10, Rocky Linux 9/10 |

All game providers are AMD64-only. `samp` installs the actively maintained open.mp server, which is backward-compatible with SA-MP scripts and clients, rather than an unmaintained third-party SA-MP binary. Web, Minecraft, code-server, bot and VM providers remain available on ARM64 when their embedded artifact metadata supports it. VM guests always match the host architecture. Endstone is AMD64-only because its official wheel is x86-64; PowerNukkitX and Cloudburst Nukkit support both PCVM architectures. LeviLamina is not listed because its upstream currently publishes Windows-only builds, while PCVM does not include Wine. Waterfall is excluded because upstream ended maintenance. One server runs one provider at a time.

## Install on Pterodactyl

1. Download `egg-pcvm-1.7.0.json` from [GitHub Releases](https://github.com/canhphung/PCVM/releases).
2. Import it into a Nest on Pterodactyl 1.12.x.
3. Keep the default release-pinned Full image, or select a smaller profile from the Egg's Docker image list.
4. Set `SOFTWARE`, required allocations and provider variables, then start the server.

### Docker image profiles

| Profile | Release tag | Provider capability | Catalog count |
|---|---|---|---:|
| Full | `ghcr.io/canhphung/pcvm:1.7.0` | Core + Games + Virtual Machines | 43 |
| Core | `ghcr.io/canhphung/pcvm:1.7.0-core` | Minecraft, proxies, Bedrock, web and applications | 21 |
| Games | `ghcr.io/canhphung/pcvm:1.7.0-games` | Core + native game servers | 38 |
| Virtual Machines | `ghcr.io/canhphung/pcvm:1.7.0-vm` | Core + QEMU virtual machines | 26 |

The Full image remains first and is the backward-compatible default. Provider counts describe profile capabilities before architecture filtering; all native game providers remain unavailable on ARM64. The image profile is compiled into the launcher and cannot be changed with a Startup Variable. Interactive menus hide providers that the selected image cannot run, while direct selection and existing state fail safely with an instruction to select the required image. Changing the Docker image profile does not reset or migrate server data.

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

## Game installation and control

Steam providers use the checksum-pinned AMD64 SteamCMD bootstrap, anonymous login and direct argv. PCVM stores the Steam build ID after `app_update ... validate`. Steam updates are in-place; if update or validation fails, state is not advanced and PCVM refuses to start the possibly partial installation.

Rust uMod and Valheim BepInEx use official GitHub release assets with SHA-256 digests and are reapplied after game updates. Switching Rust ↔ Rust uMod or Valheim ↔ Valheim BepInEx preserves saves, configuration and plugin directories. Terraria ↔ tModLoader is intentionally incompatible.

SA-MP/open.mp is installed from the official open.mp GitHub release and keeps gamemodes, filterscripts, models, scripts and configuration across in-family updates. Multi Theft Auto uses its official stable server, base configuration and resources archives; all three are pinned by SHA-256 in the embedded catalog. MTA uses the primary allocation for both UDP gameplay and TCP HTTP downloads, while its public-list query allocation must be `SERVER_PORT+123`.

Console input is forwarded through stdin, Source RCON or Telnet according to provider metadata. Providers such as Valheim and Satisfactory only support signal-based stopping; console commands return a visible warning. Shutdown tries the provider's graceful path before SIGTERM and SIGKILL.

`GAME_EXTRA_ARGS` is tokenized directly into argv and never passed through a shell. Managed ports, bind addresses, install paths and control credentials cannot be overridden. If `ADMIN_PASSWORD` is empty, PCVM creates a persistent random secret in `.pcvm/secrets` with mode `0600` and does not print it.

## Web servers

Nginx, Apache and Caddy run non-root on `0.0.0.0:${SERVER_PORT}`. `WEB_MODE=static` serves `WEB_ROOT` (default `public`). `WEB_MODE=proxy` requires an HTTP(S) `UPSTREAM_URL` without credentials. PCVM resolves every upstream address and rejects loopback, private, link-local, shared, reserved and metadata destinations; configuration metacharacters are rejected before the canonical URL is quoted into generated configs. PCVM canonicalizes the web root inside `/home/container`; symlink traversal and path escape are rejected. Caddy automatic HTTPS is disabled because public TLS remains the responsibility of the proxy in front of Pterodactyl.

PCVM owns the generated main configuration. Persistent extension snippets live under the Egg-denied internal `.pcvm/web/<provider>/conf.d` path and are not overwritten. Nginx, Apache and Caddy share compatibility family `web-static`, so switching between them preserves `public/`.

## VS Code Server

The `code-server` provider runs the official Coder standalone archive on `0.0.0.0:${SERVER_PORT}` with password authentication, telemetry and self-update disabled, and TLS delegated to Pterodactyl's external reverse proxy. Set `CODE_SERVER_PASSWORD` to at least 12 characters, or leave it blank and read the generated persistent password from `code-server-password.txt`. Extensions are kept in `code-server-extensions/`; editor data remains under the Egg-denied `.pcvm/code-server` directory. code-server intentionally gives the authenticated server owner an integrated terminal running as the container user; hosts that do not permit shell access should remove `code-server` from `ALLOWED_SOFTWARE`.

## State and safe switching

`.pcvm/state.json` schema 3 records only installation identity: provider, requested/resolved version, Steam or artifact build, runtime line, architecture, verified artifact and install time. Schema 1/2 state migrates atomically on read and drops all legacy command, environment, working-directory, readiness and stop metadata. Process argv is rebuilt from the embedded catalog, fixed installation layout and validated Startup Variables on every boot. The Egg denies user file access to the internal `.pcvm` control directory as defense in depth; launcher policy never relies on that UI restriction.

Paper, Purpur and Pufferfish share one compatibility family. Rust and Valheim variants share their respective families. A downgrade or incompatible family switch creates `.pcvm/pending-switch.json` and prints a 30-minute confirmation:

```text
RESET_CONFIRM=DELETE:0123456789abcdef...
```

After the exact confirmation, the launcher resets only canonical `/home/container`, does not follow symlinks and preserves the runtime cache. Back up data first. Hosts can disable user resets with `ALLOW_USER_RESET=0`.

## Linux virtual machines

Ubuntu Minimal, Debian, AlmaLinux, Rocky Linux and Alpine Linux run as real same-architecture QEMU system VMs using unprivileged multi-threaded TCG. No KVM, TAP, bridge, device passthrough or Wings changes are required. QEMU user-mode NAT provides outbound DHCP, DNS and HTTP, but v1.5 intentionally exposes no inbound guest port. The Pterodactyl console is the guest serial console; cloud-init creates the auto-login `pcvm` user with passwordless administration. Alpine uses OpenRC and `doas`, with a compatibility wrapper for common `sudo` commands.

The first boot downloads an immutable official cloud image, verifies SHA-256 or SHA-512, converts it to an independent sparse `vm/disk.qcow2`, resizes and checks it, and creates a NoCloud seed plus writable UEFI variables through a staged atomic install. The base image cache is then removed. `VM_DISK_COMPRESSION=zstd` optionally compresses initial QCOW2 clusters; it is off by default because decompression consumes TCG CPU and rewritten clusters become uncompressed. Alpine accepts a 2 GiB virtual disk, while the other VM providers require at least 8 GiB. `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB` and `VM_HOSTNAME` control initial resources; admin caps are `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS` and `VM_MAX_DISK_GB`.

PCVM v1.5.0 migrates VM install metadata to schema 2 and automatically repairs the checksum identity written by v1.4.0/v1.4.1 Debian installs, including interrupted staging. Existing Ubuntu Standard images remain embedded as deprecated boot-only identities, so upgrading PCVM does not force a reset. Migration preserves `vm/disk.qcow2`, the NoCloud seed, UEFI variables, disk size and hostname; unrelated metadata mismatches remain blocked.

VM image updates never modify an existing disk. Changing distro, version, pinned build or disk compression requires the reset nonce flow. `AUTO_UPDATE` and `UPDATE_REQUEST` are rejected for VM providers; update packages from inside the guest. Panel stop uses QMP ACPI powerdown and waits up to 90 seconds. Back up `vm/disk.qcow2` only while the VM is stopped; snapshots, shared backing images and online compaction are not supported in v1.5.

## Runtimes and downloads

Every Debian slim profile contains Nginx, Apache, Git, CA certificates, OpenSSL, archive tools and the native build toolchain used by application providers. The Games and Full profiles add common native game libraries; on AMD64 they also add the i386 libraries required by SteamCMD and Source-family servers. The Virtual Machines and Full profiles add QEMU system emulation, `qemu-img`, ISO tooling and architecture-specific UEFI firmware. Game binaries, cloud images, Wine and Proton are never built into an image.

`runtime-manifest.json` contains 27 architecture-specific, SHA-256-pinned packs:

- Java 8, 11, 17, 21 and 25 on AMD64/ARM64.
- Node.js 22 and 24 on AMD64/ARM64.
- Python 3.11–3.14 on AMD64/ARM64.
- PocketMine PHP on AMD64.
- Caddy 2 on AMD64/ARM64.
- .NET 8 and SteamCMD on AMD64.

Downloads require HTTPS, an allowlisted hostname, timeout/retry, a temporary file, checksum verification and atomic rename. Before every launch, PCVM derives a complete file/symlink manifest from the checksum-pinned runtime archive, verifies the extracted tree and repairs missing, changed, unexpected or escaping-symlink entries. ZIP and runtime extraction reject unsafe archive links, traversal, existing symlink targets and symlinked parent directories. The launcher never uses `curl | bash`, `eval`, arbitrary install commands or shell-composed startup commands.

## Variables

The release Egg defines the full public interface. Main groups are:

- Core: `SOFTWARE`, `SOFTWARE_VERSION`, `SOFTWARE_BUILD`, `RUNTIME_VERSION`, `AUTO_UPDATE`, `UPDATE_REQUEST` and `RESET_CONFIRM`. Minecraft providers use Pterodactyl's native EULA popup: PCVM installs the selected provider, emits the standard EULA trigger before starting it, and continues automatically after **I Accept** writes `eula=true` and restarts the server. The legacy `ACCEPT_MINECRAFT_EULA=1` environment override remains supported but is no longer exposed as an Egg variable.
- Games: `SERVER_NAME`, `SERVER_PASSWORD`, `ADMIN_PASSWORD`, `MAX_PLAYERS`, `GAME_MAP`, `GAME_WORLD`, `GAME_SEED`, `GAME_EXTRA_ARGS`, `STEAM_GSLT` and the port variables above.
- Web: `WEB_MODE`, `WEB_ROOT`, `UPSTREAM_URL`.
- Applications: bots use `SOURCE_MODE`, `GIT_URL`, `GIT_BRANCH`, `ENTRY_FILE`, `APP_ARGS`, `APP_READY_PATTERN`; code-server uses `CODE_SERVER_PASSWORD`.
- VMs: `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB`, `VM_DISK_COMPRESSION`, `VM_HOSTNAME`.
- Admin policy: `ALLOWED_SOFTWARE`, `ALLOW_USER_RESET`, `BRAND_NAME`, `SUPPORT_URL`, `RUNTIME_MIRROR_URL`, `GIT_ALLOWED_HOSTS`, `CACHE_LIMIT_MB`, `CLEAR_CONSOLE`, `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS`, `VM_MAX_DISK_GB`.

Pterodactyl view/edit flags are UI policy, not a secret store. Bot Git mode permits public credential-free HTTPS repositories only; upload mode runs files already present in the server directory.

## Development and release

```bash
go test -race ./...
go vet ./...
go run ./cmd/runtime-manifest -out runtime-manifest.json
docker build -t pcvm:dev .
docker build --build-arg PROFILE=core -t pcvm:core-dev .
docker build --build-arg PROFILE=games -t pcvm:games-dev .
docker build --build-arg PROFILE=vm -t pcvm:vm-dev .
```

Pull requests use local HTTP fixtures, a SteamCMD shim, fake QEMU/QMP and fake control servers. CI cross-compiles AMD64/ARM64, verifies the embedded profile and package boundaries, checks QEMU/firmware as non-root with a read-only container root, tests web static/proxy modes on Core and builds all four multi-architecture profiles with SBOM and provenance. Nightly checks live resolvers, runtime metadata, Steam App IDs and pinned cloud-image URLs without downloading full games. Manual application, game and VM smoke workflows build the matching lightweight profile and run one selected real provider per invocation.

Tags matching `v*.*.*` publish one version-pinned Egg, all four multi-architecture image profiles, checksums, provenance and a keyless Cosign signature for every image digest. Full additionally receives the unsuffixed version and `latest` tags for backward compatibility.
