#!/usr/bin/env bash
set -euo pipefail

package_dir="${1:?usage: scripts/smoke.sh <staged-package-directory>}"
package_dir="$(cd "$package_dir" && pwd)"
manager_binary="$package_dir/app/bin/shellcrash-manager"
core_binary="$package_dir/app/bin/mihomo"
dashboard_dir="$package_dir/app/dashboard"
for path in "$manager_binary" "$core_binary" "$dashboard_dir/index.html"; do
  test -e "$path" || { echo "Native package file is missing: $path" >&2; exit 1; }
done
for command in curl python3; do
  command -v "$command" >/dev/null 2>&1 || { echo "$command is required for the native smoke test" >&2; exit 1; }
done

root="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
manager_pid=""
cleanup() {
  if test -n "$manager_pid" && kill -0 "$manager_pid" 2>/dev/null; then
    kill -TERM "$manager_pid" 2>/dev/null || true
    wait "$manager_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

read -r api_port manager_port proxy_port <<EOF_PORTS
$(python3 - <<'PY'
import socket
ports=[]
for _ in range(3):
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        ports.append(sock.getsockname()[1])
print(*ports)
PY
)
EOF_PORTS

mkdir -p "$tmp/etc" "$tmp/data"
TRIM_PKGETC="$tmp/etc" TRIM_PKGVAR="$tmp/data" bash "$root/cmd/install_init"
secret="$(cat "$tmp/etc/api.secret")"

CORE_BINARY="$core_binary" \
CORE_CONTROLLER="127.0.0.1:$api_port" \
CORE_DATA_DIR="$tmp/data/ShellCrash" \
CORE_LOG_PATH="$tmp/data/ShellCrash/native-core.log" \
CONFIG_PATH="$tmp/data/ShellCrash/yamls/config.yaml" \
RUNTIME_CONFIG_PATH="$tmp/data/ShellCrash/runtime/config.yaml" \
SETTINGS_PATH="$tmp/data/ShellCrash/configs/subscription-manager.json" \
OVERLAY_PATH="$tmp/data/ShellCrash/configs/fnos-overrides.yaml" \
PROFILE_BACKUP_PATH="$tmp/data/ShellCrash/configs/fnos-subscription-profile.backup.yaml" \
RUNTIME_BACKUP_PATH="$tmp/data/ShellCrash/configs/fnos-subscription-runtime.backup.yaml" \
SHELLCRASH_CONFIG_PATH="$tmp/data/ShellCrash/configs/ShellCrash.cfg" \
SHELLCRASH_BACKUP_PATH="$tmp/data/ShellCrash/configs/fnos-subscription-shellcrash-settings.backup" \
API_SECRET_FILE="$tmp/etc/api.secret" \
API_URL="http://127.0.0.1:$api_port" \
LISTEN_ADDR="127.0.0.1:$manager_port" \
GATEWAY_SOCKET="$tmp/app.sock" \
APP_PREFIX="/app/shellcrash-fnos" \
DASHBOARD_DIR="$dashboard_dir" \
MIXED_PORT="$proxy_port" \
MANAGER_VERSION="smoke-test" \
"$manager_binary" >"$tmp/manager.log" 2>&1 &
manager_pid=$!

ready=false
for _ in $(seq 1 90); do
  if test -S "$tmp/app.sock" && curl --silent --fail --max-time 2 "http://127.0.0.1:$manager_port/healthz" >/dev/null; then
    ready=true
    break
  fi
  if ! kill -0 "$manager_pid" 2>/dev/null; then
    cat "$tmp/manager.log" >&2
    echo "Native manager exited before Core and fnOS gateway became ready" >&2
    exit 1
  fi
  sleep 1
done
if test "$ready" != true; then
  cat "$tmp/manager.log" >&2
  echo "Native Core and fnOS gateway did not become ready" >&2
  exit 1
fi

api="http://127.0.0.1:$api_port"
code=$(curl --silent --output "$tmp/unauthorized.json" --write-out '%{http_code}' "$api/version")
echo "Direct unauthenticated Core API returned HTTP $code"
test "$code" = 401

gateway="http://localhost/app/shellcrash-fnos"
code=$(curl --silent --unix-socket "$tmp/app.sock" --output /dev/null --write-out '%{http_code}' "$gateway/manager/api/status")
echo "Gateway request without fnOS user headers returned HTTP $code"
test "$code" = 401
code=$(curl --silent --unix-socket "$tmp/app.sock" --output /dev/null --write-out '%{http_code}' \
  -H 'X-Trim-Userid: 1000' -H 'X-Trim-Isadmin: false' "$gateway/manager/api/status")
echo "Gateway request from a non-admin returned HTTP $code"
test "$code" = 403

headers=(-H 'X-Trim-Userid: 0' -H 'X-Trim-Isadmin: true')
code=$(curl --silent --show-error --unix-socket "$tmp/app.sock" --output "$tmp/status.json" --write-out '%{http_code}' \
  "${headers[@]}" "$gateway/manager/api/status")
echo "Admin manager status returned HTTP $code"
test "$code" = 200
python3 - "$tmp/status.json" "$proxy_port" <<'PY'
import json, sys
status=json.load(open(sys.argv[1], encoding="utf-8"))
assert status["coreConnected"] is True, status
assert status["tunActive"] is False, status
assert status["proxyPort"] == int(sys.argv[2]), status
assert any(group["name"] == "Proxy" for group in status["groups"]), status
PY
if grep -Fq "$secret" "$tmp/status.json"; then
  echo "Manager status leaked the API secret" >&2
  exit 1
fi

code=$(curl --silent --show-error --unix-socket "$tmp/app.sock" --output "$tmp/version.json" --write-out '%{http_code}' \
  "${headers[@]}" "$gateway/version")
echo "Admin Core API gateway returned HTTP $code"
test "$code" = 200
grep -q '"version"' "$tmp/version.json"

code=$(curl --silent --show-error --unix-socket "$tmp/app.sock" --output "$tmp/manager.html" --write-out '%{http_code}' \
  "${headers[@]}" "$gateway/manager/")
echo "Admin manager page returned HTTP $code"
test "$code" = 200
grep -Fq 'ShellCrash 订阅管理' "$tmp/manager.html"

code=$(curl --silent --show-error --unix-socket "$tmp/app.sock" --output "$tmp/dashboard.html" --write-out '%{http_code}' \
  "${headers[@]}" "$gateway/ui/")
echo "Admin MetaCubeXD dashboard returned HTTP $code"
test "$code" = 200
grep -Fq 'const base="/app/shellcrash-fnos"' "$tmp/dashboard.html"
grep -Fq "q.set('secondaryPath',base)" "$tmp/dashboard.html"
grep -Fq 'shellcrash-fnos-back' "$tmp/dashboard.html"
if grep -Fq "$secret" "$tmp/dashboard.html"; then
  echo "Dashboard HTML leaked the API secret" >&2
  exit 1
fi

asset_reference=$(grep -Eo '(src|href)="\./(_nuxt|assets)/[^"]+\.(js|css)"' "$tmp/dashboard.html" | head -n 1 || true)
if test -z "$asset_reference"; then
  echo "No relative dashboard JS/CSS asset was found" >&2
  exit 1
fi
asset_path="${asset_reference#*=}"
asset_path="${asset_path#\"./}"
asset_path="${asset_path%\"}"
code=$(curl --silent --show-error --unix-socket "$tmp/app.sock" --output "$tmp/dashboard.asset" --write-out '%{http_code}' \
  "${headers[@]}" "$gateway/ui/$asset_path")
echo "Dashboard static asset returned HTTP $code"
test "$code" = 200
test -s "$tmp/dashboard.asset"

echo "Native Mihomo API, manager, admin-only fnOS gateway, and bundled dashboard passed. TUN routing itself is validated on the live fnOS host."
