# PCVM

PCVM is a single `PTDL_v2` Pterodactyl egg for selecting and running one of 14 server or application providers without modifying the Panel. A small Go launcher owns provider resolution, checksum-verified runtime downloads, state, guarded switches and process supervision.

Provider catalog:

| Group | Providers |
|---|---|
| Minecraft Java | Vanilla, Paper, Purpur, Pufferfish, Fabric, Forge, NeoForge |
| Proxies | Velocity, BungeeCord |
| Bedrock | Bedrock Dedicated Server, PocketMine-MP |
| Applications | Node.js bot, Python bot, Lavalink |

Waterfall is intentionally excluded because upstream ended maintenance. Each Pterodactyl server runs exactly one provider at a time.

## Install

1. Download the versioned egg JSON from [Releases](https://github.com/canhphung/PCVM/releases).
2. In Pterodactyl 1.12.x, import it under an appropriate Nest.
3. Keep the release-pinned `ghcr.io/canhphung/pcvm:<version>` image selected.
4. Create a server, set `SOFTWARE`, version/build and EULA variables, then start it. With `SOFTWARE=interactive`, a first start displays the console menu.

The interactive console starts with a FIGlet PCVM banner and a two-level selector. Providers are grouped into Minecraft Java, Minecraft Proxies, Minecraft Bedrock, and Applications & Bots; categories disabled by host policy or unavailable on the current architecture are hidden.

The launcher reads Pterodactyl's built-in `SERVER_PORT` on every start and applies the primary allocation to the active provider. Java servers and PocketMine update `server.properties`; Bedrock updates its game port; Velocity and BungeeCord update their first listener; Lavalink updates `server.port`. Node.js and Python apps receive `SERVER_PORT`, `PORT` and `HOST=0.0.0.0`. Only allocation-owned bind/port keys are changed, and the remaining user configuration is preserved. Secondary allocations remain application-specific in v1.

The egg installation script only creates `/mnt/server/.pcvm`. Installation and later switching happen in the launcher, whose fixed startup command is:

```text
/usr/local/bin/pcvm run
```

Pterodactyl marks the server running only after `[PCVM] READY` appears.

## State and safe switching

`.pcvm/state.json` records the selected and resolved versions, build, runtime, architecture, command, artifact SHA-256 and install time. Paper, Purpur and Pufferfish share the only in-place compatibility family. A downgrade or any incompatible family switch creates `.pcvm/pending-switch.json` and prints a 30-minute confirmation such as:

```text
RESET_CONFIRM=DELETE:0123456789abcdef...
```

Back up the server, paste the exact value and start again. The launcher prepares and verifies target downloads before deleting. Reset canonicalizes the server root, rejects a symlink root, never follows child symlinks and preserves only `.pcvm/cache`. Hosts can disable all user resets with `ALLOW_USER_RESET=0`.

When upgrading from a pre-PCVM image, PCVM atomically migrates the legacy control directory into `.pcvm`, preserves state and cache, and rewrites stored absolute runtime and artifact paths before starting the existing provider.

## Runtimes and downloads

The core Debian image contains the launcher, Git, CA certificates, archive tools and native-module build tools. Java 8/11/17/21/25, Node.js 22/24, Python 3.11–3.14 and the PocketMine PHP pack are downloaded on demand. `runtime-manifest.json` is generated from official release metadata, embedded in the image and pins every URL to SHA-256. The current manifest has 23 architecture-specific packs; PocketMine's upstream PHP pack is AMD64-only, so it is hidden on ARM64.

Downloads require HTTPS, an allowlisted hostname, timeouts/retries and atomic renames. No `curl | bash`, `eval`, user-supplied install command or shell-composed application command is accepted. Bot Git mode permits only public credential-free HTTPS URLs on `GIT_ALLOWED_HOSTS`; upload mode runs the files already present in the server directory.

## Variables

User variables are `SOFTWARE`, `SOFTWARE_VERSION`, `SOFTWARE_BUILD`, `RUNTIME_VERSION`, `AUTO_UPDATE`, `UPDATE_REQUEST`, `ACCEPT_MINECRAFT_EULA`, `RESET_CONFIRM`, `SOURCE_MODE`, `GIT_URL`, `GIT_BRANCH`, `ENTRY_FILE`, `APP_ARGS` and `APP_READY_PATTERN`.

Admin-only policy variables are `ALLOWED_SOFTWARE`, `ALLOW_USER_RESET`, `BRAND_NAME`, `SUPPORT_URL`, `RUNTIME_MIRROR_URL`, `GIT_ALLOWED_HOSTS` and `CACHE_LIMIT_MB`. Pterodactyl's view/edit flags are UI controls, not a secret store; do not put credentials in any egg variable.

## Development

```bash
go test -race ./...
go vet ./...
go run ./cmd/runtime-manifest -out runtime-manifest.json
docker build -t pcvm:dev .
```

Pull requests use only local HTTP fixtures for provider contracts. The scheduled workflow resolves real upstream APIs and regenerates the runtime lock to detect schema or asset changes. CI tests, vets, cross-compiles AMD64/ARM64 and builds a multi-architecture image with SBOM and provenance. Tags matching `v*.*.*` publish a release-pinned egg, image, checksums and a keyless Cosign signature.

The project is MIT licensed and contains no telemetry.
