# Validation scope

The package source and the release workflow are checked in GitHub Actions. The release job uses a Linux amd64 runner to start the official multi-architecture ShellCrash image with the same persistent directories seeded by cmd/install_init.

The automated smoke check verifies:

- ShellCrash starts with a persistent starter profile.
- The Web dashboard responds at /ui/.
- The Mihomo API accepts its generated bearer token and rejects a request without one.
- The ShellCrash autostart marker is present.

No real fnOS installation has been performed for this repository yet. The desktop iframe configuration, FPK install/upgrade lifecycle, ARM64 runtime behavior, and actual dashboard rendering on fnOS remain to be confirmed on hardware. Do not read CI success as a real-device validation claim.

The initial profile contains no proxy nodes. It exists so the API and Web panel can start before the user imports their own configuration.
