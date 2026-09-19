#!/usr/bin/env bash
set -euo pipefail

if ! declare -p SHELLCRASH_IMAGE >/dev/null 2>&1 || test -z "$SHELLCRASH_IMAGE"; then
  echo "SHELLCRASH_IMAGE must be set to a digest-pinned official image"
  exit 1
fi
if ! declare -p GATEWAY_IMAGE >/dev/null 2>&1 || test -z "$GATEWAY_IMAGE"; then
  echo "GATEWAY_IMAGE must be set to a digest-pinned Nginx image"
  exit 1
fi

tmp=$(mktemp -d)
name="shellcrash-fnos-smoke-$$"
gateway_name="$name-gateway"
network="$name-network"
gateway_dir="$(cd "$(dirname "$0")/../app/docker/gateway" && pwd)"
secret=
smoke_passed=false
cleanup() {
  if test "$smoke_passed" != true && docker inspect "$gateway_name" >/dev/null 2>&1; then
    echo "Gateway container startup diagnostics (generated API secret redacted):" >&2
    docker inspect --format '{{.State.Status}} exit={{.State.ExitCode}}' "$gateway_name" >&2 || true
    docker logs "$gateway_name" 2>&1 | sed "s/$secret/[REDACTED]/g" | tail -100 >&2 || true
  fi
  docker rm -f "$gateway_name" "$name" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/etc" "$tmp/data" "$tmp/target"
TRIM_PKGETC="$tmp/etc" \
TRIM_PKGVAR="$tmp/data" \
bash cmd/install_init
secret=$(cat "$tmp/etc/api.secret")

docker network create "$network" >/dev/null
docker run -d --name "$name" \
  --network "$network" --network-alias shellcrash \
  -p 127.0.0.1::9999/tcp \
  -v "$tmp/data/ShellCrash/configs:/etc/ShellCrash/configs" \
  -v "$tmp/data/ShellCrash/yamls:/etc/ShellCrash/yamls" \
  -v "$tmp/data/ShellCrash/jsons:/etc/ShellCrash/jsons" \
  -v "$tmp/data/ShellCrash/configs/.autostart:/etc/s6-overlay/s6-rc.d/user/contents.d/shellcrash:ro" \
  -v "$tmp/data/ShellCrash/configs/.autostart:/etc/s6-overlay/s6-rc.d/user/contents.d/afstart" \
  -e TZ=Asia/Shanghai \
  "$SHELLCRASH_IMAGE" >/dev/null

port=
for _ in $(seq 1 36); do
  port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "9999/tcp") 0).HostPort}}' "$name")
  if test -n "$port" && curl --silent --fail --max-time 2 \
      -H "Authorization: Bearer $secret" "http://127.0.0.1:$port/version" >/dev/null; then
    break
  fi
  port=
  sleep 5
done
if test -z "$port"; then
  echo "ShellCrash API did not become ready"
  exit 1
fi

base="http://127.0.0.1:$port"
code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$base/version")
echo "Direct unauthenticated /version returned HTTP $code"
if test "$code" != 401; then
  echo "ShellCrash API did not reject a request without its token"
  exit 1
fi

docker run -d --name "$gateway_name" --network "$network" \
  -v "$tmp/target:/app/target" \
  -v "$tmp/etc/api.secret:/run/secrets/shellcrash-api.secret:ro" \
  -v "$gateway_dir:/etc/shellcrash-gateway:ro" \
  --entrypoint /bin/sh "$GATEWAY_IMAGE" \
  /etc/shellcrash-gateway/entrypoint.sh >/dev/null

socket="$tmp/target/app.sock"
socket_ready=false
for _ in $(seq 1 30); do
  if test -S "$socket"; then
    socket_ready=true
    break
  fi
  sleep 1
done
if test "$socket_ready" != true; then
  echo "fnOS gateway Unix socket did not become ready"
  exit 1
fi

gateway="http://localhost/app/shellcrash-fnos"
code=$(curl --silent --unix-socket "$socket" --output /dev/null \
  --write-out '%{http_code}' "$gateway/version")
echo "Gateway request without fnOS user headers returned HTTP $code"
if test "$code" != 401; then
  echo "Gateway accepted a request without fnOS identity headers"
  exit 1
fi

code=$(curl --silent --unix-socket "$socket" --output /dev/null \
  --write-out '%{http_code}' -H 'X-Trim-Userid: 1000' \
  -H 'X-Trim-Isadmin: false' "$gateway/version")
echo "Gateway request from a non-admin returned HTTP $code"
if test "$code" != 403; then
  echo "Gateway did not reject a non-admin request"
  exit 1
fi

code=$(curl --silent --show-error --unix-socket "$socket" \
  --output "$tmp/version.json" --write-out '%{http_code}' \
  -H 'X-Trim-Userid: 0' -H 'X-Trim-Isadmin: true' "$gateway/version")
echo "Admin gateway /version returned HTTP $code"
if test "$code" != 200 || ! grep -q '"version"' "$tmp/version.json"; then
  echo "Gateway did not inject the API token for an admin request"
  exit 1
fi

code=$(curl --silent --show-error --unix-socket "$socket" \
  --output "$tmp/dashboard.html" --write-out '%{http_code}' \
  -H 'X-Trim-Userid: 0' -H 'X-Trim-Isadmin: true' "$gateway/ui/")
echo "Admin gateway dashboard returned HTTP $code"
if test "$code" != 200 || ! grep -Eiq '<!doctype html|<html' "$tmp/dashboard.html"; then
  echo "Gateway did not return the dashboard HTML"
  exit 1
fi
if ! grep -Fq "q.set('secondaryPath','/app/shellcrash-fnos')" "$tmp/dashboard.html"; then
  echo "Dashboard did not receive the fnOS auto-connect bootstrap"
  exit 1
fi
if grep -Fq "$secret" "$tmp/dashboard.html"; then
  echo "Dashboard HTML leaked the ShellCrash API secret"
  exit 1
fi

asset=$(sed -n 's/.*src="\(\.\/assets\/[^" ]*\.js\)".*/\1/p' "$tmp/dashboard.html" | head -n 1)
if test -z "$asset"; then
  echo "Could not find the Dashboard JavaScript asset"
  exit 1
fi
asset_path=$(printf '%s' "$asset" | sed 's|^\./||')
code=$(curl --silent --show-error --unix-socket "$socket" \
  --output "$tmp/dashboard.js" --write-out '%{http_code}' \
  -H 'X-Trim-Userid: 0' -H 'X-Trim-Isadmin: true' \
  "$gateway/ui/$asset_path")
echo "Dashboard JavaScript through the gateway returned HTTP $code"
if test "$code" != 200 || ! grep -q 'secondaryPath' "$tmp/dashboard.js"; then
  echo "Dashboard static assets did not resolve through the gateway prefix"
  exit 1
fi

test -f "$tmp/data/ShellCrash/configs/.autostart"
smoke_passed=true
echo "ShellCrash API, fnOS identity gate, secret injection, dashboard bootstrap, and gateway assets passed smoke test"
