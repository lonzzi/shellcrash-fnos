# Validation scope

The current package is a native fnOS host service: the root manager starts the official Mihomo binary, serves a bundled MetaCubeXD dashboard, and exposes the desktop app through an admin-gated Unix-socket gateway. The package source is checked in GitHub Actions. Its smoke test starts the native Core and manager on a Linux runner with TUN disabled, then checks the API, UI, identity gate, and static dashboard assets. A separate live fnOS test is required to validate host-wide routing and must be recorded below before calling TUN verified.

## Native revision 1.9.4-5: host-wide TUN live test (2026-09-20)

The FPK was upgraded through the fnOS Web App Center's manual package flow. The installed native service reports `1.9.4-5` and `running`; `docker container ls --filter name=shellcrash-fnos` returns no ShellCrash containers.

- The manager status endpoint reports `coreConnected=true`, `tunActive=true`, `tunMode=on`, `dnsMode=on`, `lastError=null`, and 2 configured providers.
- The host `Meta` interface has the `UP` flag. `ip route get 8.8.8.8` selects `dev Meta table 2022`; the route to the NAS LAN gateway remains on `end0`.
- The host has the `inet mihomo` nftables table with `output` and `prerouting` chains. A `curl` with proxy environment variables cleared and `--noproxy '*'` to Google's HTTPS `/generate_204` endpoint returned HTTP 204 with TLS verification successful. The resolved destination also selected `Meta` in the host route lookup.
- A separate 512 KiB HTTPS download from `speed.cloudflare.com`, also with proxy environment variables cleared and `--noproxy '*'`, returned HTTP 200 with TLS verification successful. While it was active, the Core `/connections` API showed the NAS-originated request on a non-`DIRECT` proxy chain; the proxy chain names were omitted from the record.
- The pre-upgrade and post-upgrade configuration comparison matched the API secret, persistent profile, DNS/TUN overlay, subscription settings, provider file, and ShellCrash settings. The status still reports 2 providers. The comparison emitted only match booleans; no secret or subscription URL was printed.
- The profile's saved proxy groups and YAML were not rewritten. TUN route excludes are generated in the runtime overlay; the fix removes only `240.0.0.0/4` and `ff00::/8`, retaining the local/private/LAN exclusions. These two address-family maximum ranges triggered an nftables `EEXIST` error in the bundled TUN redirect path; the failure and upstream fix are documented in [sing-box issue #4316](https://github.com/SagerNet/sing-box/issues/4316) and [sing-tun PR #83](https://github.com/SagerNet/sing-tun/pull/83).

This verifies that the fnOS **host** route is using Mihomo's TUN interface, rather than a TUN interface confined to a container. Mihomo's `auto-route` and Linux `auto-redirect` behavior is described in the [official TUN documentation](https://wiki.metacubex.one/en/config/inbound/tun/). Subscription rules still determine whether an individual request exits through a proxy node or uses `DIRECT`.

Historical container-based revisions `1.9.4-5` and `1.9.4-6` used Docker, S6, and an Nginx gateway. Their tests below describe those old packages and are not evidence that the native FPK upgrade or host TUN works.

The old container smoke check verified:

- ShellCrash starts with a persistent starter profile.
- The Web dashboard responds at /ui/.
- The direct Mihomo API accepts its generated bearer token and rejects a request without one.
- The gateway rejects requests without fnOS identity headers and denies non-admin users.
- The S6 subscription manager serves its embedded UI through the admin-only gateway, reads both persistent and active runtime profiles, and does not expose the ShellCrash API secret or a saved subscription URL in its status response.
- The internal manager service is not published as a NAS host port; its provider cache and subscription settings use separate persistent paths.
- An admin gateway request reaches the API because the gateway injects the token server-side.
- The injected dashboard bootstrap selects the fnOS gateway path without exposing the token, and relative dashboard assets resolve through that path.
- The ShellCrash autostart marker is present.

Revision `1.9.4-5` was built as an FPK with fnpack and then exercised on the live fnOS installation by replacing the mounted ARM64 manager binary and restarting its S6 service. The existing app was also restarted after the override was saved. This validates the running image, gateway, manager, and Core together; it does not claim a fresh App Center FPK install or upgrade.

Live device checks on 2026-09-20:

- The manager status endpoint through the fnOS Unix-socket gateway returned HTTP 200 and reported a configured subscription, 2 configured providers, 95 total proxy entries, a 24-hour refresh interval, and manager version `1.9.4-5`.
- The manager page, ShellCrash dashboard HTML, and all 7 dashboard assets returned HTTP 200 through the admin gateway.
- The Core `/proxies` API returned 135 entries including 36 groups. The source profile's 34 `proxy-groups` had the same SHA-256 before and after the manager refresh, so the manager did not add or rewrite groups.
- The saved DNS and TUN override remained enabled. Because the bundled Core reports that gVisor is not compiled in, the manager normalized an enabled `mixed`/`gvisor`/unspecified TUN stack to `system`.
- After restarting the app container, Core `/configs` reported TUN enabled with the `System` stack. The `Meta` TUN interface was up and 7 routes used it. A Core DNS query for `example.com` returned RCODE 0 with two answers.
- A mode-`600` backup of the persistent ShellCrash configuration, API key file, and manager binary was created before the override update and another before the container restart.

The logged-in fnOS browser opened the manager and advanced dashboard after the fix. The rendered proxy page showed 34 group cards with node counts, and its provider tab showed 2 providers; the reported blank state was not reproduced. The exact FPK was built, but it was not installed through App Center during this test, so a clean App Center install/upgrade remains unverified. Do not read CI or this direct runtime test as proof of that package-manager path.

The initial profile contains no proxy nodes. It exists so the API and Web panel can start before the user imports their own configuration.

## Revision 1.9.4-6

The updated manager binary and gateway template were deployed to the existing fnOS app after creating a mode-`600` rollback archive at `/vol1/@appdata/shellcrash-fnos/.codex-backups/shellcrash-manager-ui-before-1.9.4-6-20260920T124641Z.tar.gz`. The ShellCrash profile, saved subscription, DNS/TUN overrides, and policy groups were left in place.

- Built `dist/shellcrash-fnos-1.9.4-6-all.fpk` successfully with fnpack against the pinned official images. It has not been published or installed through App Center.
- `go test -race ./...`, `go vet ./...`, `python3 scripts/check.py`, `node --check manager/web/app.js`, and shell syntax checks passed.
- The running manager status endpoint reports version `1.9.4-6`, 95 proxy entries, 2 providers, a 24-hour refresh interval, and DNS/TUN modes both `on`.
- The logged-in fnOS desktop opened the app in its iframe window. The two feature selectors can be changed independently; the UI was toggled for inspection and reloaded without saving, so the live DNS/TUN settings remained on.
- The advanced panel showed provider and proxy-group entries. Its injected “返回 ShellCrash 订阅管理” link was clicked inside the fnOS iframe and returned to the manager in the same desktop window. The fnOS window close control remains available to return to the desktop.
- Core runtime config still has DNS enabled and TUN enabled with the supported `System` stack. The `Meta` interface has the `UP` flag and a TUN route.
- A NAS-side request through `http://127.0.0.1:17890` to Cloudflare returned HTTP 200 with 1 MiB downloaded. Core `/connections` captured that host on a non-`DIRECT` proxy chain, confirming that actual requests are flowing through a selected proxy; zero traffic while idle is expected.
- The saved `proxy-groups` section hash remained `87b4c6ecb08aace92ce159e738b9d2cf1bfea45efee1a6a49f578745deb82a42`.

These checks exercise the updated assets in the already-installed app and build the FPK, but do not replace a fresh App Center install/upgrade test.
