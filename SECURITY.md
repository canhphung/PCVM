# Security policy

Please report a vulnerability privately through GitHub Security Advisories instead of a public issue. Include the affected release, provider, architecture and a minimal reproduction when possible.

Only the latest 2.x SemVer release receives security fixes. The 1.x line is
EOL; its tags and images remain available only for rollback and migration.

PCVM has no telemetry. It treats Startup Variables, uploaded files, persisted
state and network responses as untrusted. The Egg denies Panel/SFTP access to
`.pcvm`, the launcher rebuilds every command from compiled drivers, and it
verifies runtime trees and managed launch files before execution.

Pterodactyl runs the launcher and selected server process under the same Unix
UID. Hidden Egg variables are therefore policy controls, not cryptographic
secrets from code already running inside that server. A node administrator who
needs isolation between mutually hostile workloads must use separate
containers/users and the normal Wings security boundary; PCVM does not claim
to create a privilege boundary inside one server container.
