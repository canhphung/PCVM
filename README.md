# PCVM

PCVM v1.4.2 is one `PTDL_v2` Pterodactyl Egg that installs and runs one of 39 providers without modifying Panel or Wings. Its Go launcher owns provider selection, checksum-verified runtimes and cloud images, updates, state, safe switching, port validation and process supervision. The project is MIT licensed and has no telemetry.

## Provider catalog

| Menu | Providers |
|---|---|
| Minecraft Java | Vanilla, Paper, Purpur, Pufferfish, Fabric, Forge, NeoForge |
| Minecraft Proxies | Velocity, BungeeCord |
| Minecraft Bedrock | Bedrock Dedicated Server, PocketMine-MP, PowerNukkitX, Cloudburst Nukkit, Endstone |
| Games — Source & FPS | Counter-Strike 2, Garry's Mod, Left 4 Dead 2 |
| Games — Survival | Palworld, Rust, Rust + uMod, Project Zomboid, Valheim, Valheim + BepInEx, 7 Days to Die, Unturned |
| Games — Sandbox & Automation | Terraria, tModLoader, Satisfactory, Factorio |
| Web Servers | Nginx, Apache HTTP Server, Caddy |
| Applications & Bots | Node.js bot, Python bot, Lavalink |
| Virtual Machines â€” Debian Family | Ubuntu Server 24.04/26.04 LTS, Debian 12/13 |
| Virtual Machines â€” Enterprise Linux | AlmaLinux 9/10, Rocky Linux 9/10 |

All game providers are AMD64-only. Web, Minecraft, application and VM providers remain available on ARM64 when their embedded artifact metadata supports it. VM guests always match the host architecture. Endstone is AMD64-only because its official wheel is x86-64; PowerNukkitX and Cloudburst Nukkit support both PCVM architectures. LeviLamina is not listed because its upstream currently publishes Windows-only builds, while PCVM does not include Wine. Waterfall is excluded because upstream ended maintenance. One server runs one provider at a time.

## Install on Pterodactyl

1. Download `egg-pcvm-1.4.2.json` from [GitHub Releases](https://github.com/canhphung/PCVM/releases).
2. Import it into a Nest on Pterodactyl 1.12.x.
3. Keep the release-pinned `ghcr.io/canhphung/pcvm:1.4.2` image.
4. Set `SOFTWARE`, required allocations and provider variables, then start the server.

The Egg installation script only initializes `/mnt/server/.pcvm`. The immutable startup command is:

```text
/usr/local/bin/pcvm run
```

On first start, `SOFTWARE=interactive` opens a FIGlet menu with up to three levels. Host allowlists, architecture and runtime availability filter the choices. PCVM emits `[PCVM] READY` only after provider-specific regex, delay or TCP readiness succeeds.

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

Palworld and Rust RCON plus 7 Days to Die Telnet bind to loopback and use `RCON_PORT` or `TELNET_PORT`; they are launcher control channels, not public allocations. Missing, out-of-range, duplicate or invalid-offset ports stop startup with an exact error.

## Game installation and control

Steam providers use the checksum-pinned AMD64 SteamCMD bootstrap, anonymous login and direct argv. PCVM stores the Steam build ID after `app_update ... validate`. Steam updates are in-place; if update or validation fails, state is not advanced and PCVM refuses to start the possibly partial installation.

Rust uMod and Valheim BepInEx use official GitHub release assets with SHA-256 digests and are reapplied after game updates. Switching Rust ↔ Rust uMod or Valheim ↔ Valheim BepInEx preserves saves, configuration and plugin directories. Terraria ↔ tModLoader is intentionally incompatible.

Console input is forwarded through stdin, Source RCON or Telnet according to provider metadata. Providers such as Valheim and Satisfactory only support signal-based stopping; console commands return a visible warning. Shutdown tries the provider's graceful path before SIGTERM and SIGKILL.

`GAME_EXTRA_ARGS` is tokenized directly into argv and never passed through a shell. Managed ports, bind addresses, install paths and control credentials cannot be overridden. If `ADMIN_PASSWORD` is empty, PCVM creates a persistent random secret in `.pcvm/secrets` with mode `0600` and does not print it.

## Web servers

Nginx, Apache and Caddy run non-root on `0.0.0.0:${SERVER_PORT}`. `WEB_MODE=static` serves `WEB_ROOT` (default `public`). `WEB_MODE=proxy` requires an HTTP(S) `UPSTREAM_URL` without credentials. PCVM resolves every upstream address and rejects loopback, private, link-local, shared, reserved and metadata destinations; configuration metacharacters are rejected before the canonical URL is quoted into generated configs. PCVM canonicalizes the web root inside `/home/container`; symlink traversal and path escape are rejected. Caddy automatic HTTPS is disabled because public TLS remains the responsibility of the proxy in front of Pterodactyl.

PCVM owns the generated main configuration. Persistent extension snippets live under the Egg-denied internal `.pcvm/web/<provider>/conf.d` path and are not overwritten. Nginx, Apache and Caddy share compatibility family `web-static`, so switching between them preserves `public/`.

## State and safe switching

`.pcvm/state.json` schema 3 records only installation identity: provider, requested/resolved version, Steam or artifact build, runtime line, architecture, verified artifact and install time. Schema 1/2 state migrates atomically on read and drops all legacy command, environment, working-directory, readiness and stop metadata. Process argv is rebuilt from the embedded catalog, fixed installation layout and validated Startup Variables on every boot. The Egg denies user file access to the internal `.pcvm` control directory as defense in depth; launcher policy never relies on that UI restriction.

Paper, Purpur and Pufferfish share one compatibility family. Rust and Valheim variants share their respective families. A downgrade or incompatible family switch creates `.pcvm/pending-switch.json` and prints a 30-minute confirmation:

```text
RESET_CONFIRM=DELETE:0123456789abcdef...
```

After the exact confirmation, the launcher resets only canonical `/home/container`, does not follow symlinks and preserves the runtime cache. Back up data first. Hosts can disable user resets with `ALLOW_USER_RESET=0`.

## Linux virtual machines

Ubuntu, Debian, AlmaLinux and Rocky Linux run as real same-architecture QEMU system VMs using unprivileged multi-threaded TCG. No KVM, TAP, bridge, device passthrough or Wings changes are required. QEMU user-mode NAT provides outbound DHCP, DNS and HTTP, but v1.4 intentionally exposes no inbound guest port. The Pterodactyl console is the guest serial console; cloud-init creates the auto-login `pcvm` user with passwordless `sudo`.

The first boot downloads an immutable official cloud image, verifies SHA-256 or SHA-512, converts it to an independent sparse `vm/disk.qcow2`, resizes and checks it, and creates a NoCloud seed plus writable UEFI variables through a staged atomic install. The base image cache is then removed. `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB` and `VM_HOSTNAME` control initial resources; admin caps are `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS` and `VM_MAX_DISK_GB`.

PCVM v1.4.2 automatically repairs the checksum identity written by v1.4.0/v1.4.1 Debian installs, including interrupted staging. The migration preserves `vm/disk.qcow2`, the NoCloud seed, UEFI variables, disk size and hostname; unrelated metadata mismatches remain blocked.

VM image updates never modify an existing disk. Changing distro, version or pinned build requires the reset nonce flow. `AUTO_UPDATE` and `UPDATE_REQUEST` are rejected for VM providers; update packages from inside the guest. Panel stop uses QMP ACPI powerdown and waits up to 90 seconds. Back up `vm/disk.qcow2` only while the VM is stopped; snapshots are not supported in v1.4.

## Runtimes and downloads

The Debian slim image contains Nginx, Apache, Git, CA certificates, archive/native build tools, QEMU system emulation, `qemu-img`, ISO tooling, OpenSSL, UEFI firmware and common native game libraries. AMD64 additionally contains the i386 libraries required by SteamCMD and Source-family servers. Game binaries, Wine and Proton are not built into the image.

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
- Bots: `SOURCE_MODE`, `GIT_URL`, `GIT_BRANCH`, `ENTRY_FILE`, `APP_ARGS`, `APP_READY_PATTERN`.
- VMs: `VM_MEMORY_MB`, `VM_CPUS`, `VM_DISK_GB`, `VM_HOSTNAME`.
- Admin policy: `ALLOWED_SOFTWARE`, `ALLOW_USER_RESET`, `BRAND_NAME`, `SUPPORT_URL`, `RUNTIME_MIRROR_URL`, `GIT_ALLOWED_HOSTS`, `CACHE_LIMIT_MB`, `CLEAR_CONSOLE`, `VM_MAX_MEMORY_MB`, `VM_MAX_CPUS`, `VM_MAX_DISK_GB`.

Pterodactyl view/edit flags are UI policy, not a secret store. Bot Git mode permits public credential-free HTTPS repositories only; upload mode runs files already present in the server directory.

## Development and release

```bash
go test -race ./...
go vet ./...
go run ./cmd/runtime-manifest -out runtime-manifest.json
docker build -t pcvm:dev .
```

Pull requests use local HTTP fixtures, a SteamCMD shim, fake QEMU/QMP and fake control servers. CI cross-compiles AMD64/ARM64, checks QEMU/firmware as non-root with a read-only container root, tests web static/proxy modes and builds the multi-architecture image with SBOM and provenance. Nightly checks live resolvers, runtime metadata, Steam App IDs and pinned cloud-image URLs without downloading full games. Manual `full-game-smoke` and `full-vm-smoke` workflows run one selected real provider per invocation.

Tags matching `v*.*.*` publish a version-pinned Egg and image, checksums, provenance and a keyless Cosign signature.
