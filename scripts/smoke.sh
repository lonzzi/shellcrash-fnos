#!/usr/bin/env bash
set -euo pipefail

: "${SHELLCRASH_IMAGE:?SHELLCRASH_IMAGE must be set to a digest-pinned official image}"
tmp=$(mktemp -d)
name=shellcrash-fnos-smoke
cleanup() {
  if [ "${smoke_passed:-false}" != true ]; then
    docker logs "$name" >&2 2>/dev/null || true
  fi
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/etc" "$tmp/data"
TRIM_PKGETC="$tmp/etc" \
TRIM_PKGVAR="$tmp/data" \
bash cmd/install_init
secret=$(cat "$tmp/etc/api.secret")

docker run -d --name "$name" \
  -p 127.0.0.1::9999/tcp \
  -v "$tmp/data/ShellCrash/configs:/etc/ShellCrash/configs" \
  -v "$tmp/data/ShellCrash/yamls:/etc/ShellCrash/yamls" \
  -v "$tmp/data/ShellCrash/jsons:/etc/ShellCrash/jsons" \
  -v "$tmp/data/ShellCrash/configs/.autostart:/etc/s6-overlay/s6-rc.d/user/contents.d/shellcrash:ro" \
  -v "$tmp/data/ShellCrash/configs/.autostart:/etc/s6-overlay/s6-rc.d/user/contents.d/afstart" \
  -e TZ=Asia/Shanghai \
  "$SHELLCRASH_IMAGE" >/dev/null

sleep 5
echo "S6 services and startup configuration:"
docker exec "$name" sh -c '
  echo "-- enabled runlevel files"
  ls -la /etc/s6-overlay/s6-rc.d/user/contents.d
  echo "-- active services"
  /command/s6-rc -a list 2>&1 || true
  echo "-- command.env"
  cat /etc/ShellCrash/configs/command.env 2>&1 || true
  echo "-- processes"
  ps 2>&1 || true
' >&2

port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "9999/tcp") 0).HostPort}}' "$name")
[[ "$port" =~ ^[0-9]+$ ]] || { echo "Could not resolve published Web port: [$port]"; exit 1; }
echo "Web/API published on 127.0.0.1:$port"
base="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 36); do
  if curl --silent --fail --max-time 2 \
      -H "Authorization: Bearer $secret" "$base/version" >/dev/null; then
    ready=true
    break
  fi
  sleep 5
done
if [ "$ready" != true ]; then
  echo "ShellCrash API did not become ready"
  echo "Trying the official ShellCrash start command for diagnostics:"
  timeout 30 docker exec "$name" /etc/ShellCrash/start.sh start 2>&1 || true
  sleep 5
  docker exec "$name" sh -c 'ps 2>&1; ls -la /run/service 2>&1' >&2 || true
  curl --silent --show-error --max-time 5 \
    -H "Authorization: Bearer $secret" "$base/version" || true
  exit 1
fi
echo "Authenticated /version request succeeded"

code=$(curl --silent --output /dev/null --write-out '%{http_code}' "$base/version")
echo "Unauthenticated /version returned HTTP $code"
if [ "$code" != 401 ]; then
  echo "ShellCrash API did not reject a request without its token"
  exit 1
fi

dashboard_code=$(curl --silent --show-error --output "$tmp/dashboard.html" --write-out '%{http_code}' "$base/ui/")
echo "Dashboard /ui/ returned HTTP $dashboard_code"
if [ "$dashboard_code" != 200 ] || ! grep -Eiq '<!doctype html|<html' "$tmp/dashboard.html"; then
  head -c 400 "$tmp/dashboard.html" >&2 || true
  echo
  echo "ShellCrash dashboard did not return an HTML page"
  exit 1
fi
test -s "$tmp/data/ShellCrash/configs/.autostart"
smoke_passed=true
echo "Official ShellCrash dashboard and token-protected API passed smoke test"
