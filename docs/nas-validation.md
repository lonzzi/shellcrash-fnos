# Validation scope

The package source and the release workflow are checked in GitHub Actions. The release job uses a Linux amd64 runner to start the official multi-architecture ShellCrash image and the digest-pinned Nginx gateway with the same persistent directories seeded by cmd/install_init.

The automated smoke check verifies:

- ShellCrash starts with a persistent starter profile.
- The Web dashboard responds at /ui/.
- The direct Mihomo API accepts its generated bearer token and rejects a request without one.
- The gateway rejects requests without fnOS identity headers and denies non-admin users.
- An admin gateway request reaches the API because the gateway injects the token server-side.
- The injected dashboard bootstrap selects the fnOS gateway path without exposing the token, and relative dashboard assets resolve through that path.
- The ShellCrash autostart marker is present.

The previous package was tested on a real fnOS installation, but this authenticated-gateway revision still needs a real FPK install/upgrade check. The desktop iframe, actual fnOS gateway forwarding, and dashboard rendering remain unconfirmed until that test passes. Do not read CI success as a real-device validation claim.

The initial profile contains no proxy nodes. It exists so the API and Web panel can start before the user imports their own configuration.
